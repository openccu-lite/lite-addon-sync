package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/jobs"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
	"github.com/openccu-lite/lite-addon-sync/internal/servers"
)

// Copy jobs, run as the job's unit runs them (jobs.Run), with the real tools: rclone against the
// WebDAV server of the other tests, and rsync over ssh against an sshd started here as this user
// (skipped where rsync or sshd is missing; SYNC_REQUIRE_RSYNC=1 makes that a failure, as in CI).

type tree map[string]string

func writeTree(t *testing.T, dir string, files tree) {
	t.Helper()
	for name, text := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func listTree(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			r, _ := filepath.Rel(dir, p)
			out = append(out, r)
		}
		return nil
	})
	return strings.Join(out, " ")
}

// jobFixture is a storage with a source, a job store, and what a run needs.
type jobFixture struct {
	root, source string
	store        *jobs.Store
	env          jobs.Env
}

func newJobFixture(t *testing.T, confDir string) *jobFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := jobs.Root
	jobs.Root = root
	t.Cleanup(func() { jobs.Root = old })
	f := &jobFixture{root: root, source: filepath.Join(root, "sdcard", "export")}
	writeTree(t, f.source, tree{
		"a.txt": "alpha", "sub dir/b.txt": "beta", "sub dir/deep/c.bin": strings.Repeat("c", 70000),
		"cache/x.tmp": "tmp", "server.pem": "KEY", "my-token.json": "TOKEN", "old/ccu-2026.sbk": "BACKUP", "old/keep.txt": "no",
	})
	if f.store, err = jobs.Open(confDir); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	f.env = jobs.Env{AddonDir: filepath.Join(base, "addon"), ConfDir: confDir, RunDir: filepath.Join(base, "run"), Log: t.Logf}
	return f
}

// scenario runs the same story against either tool: target is where the files land on this machine.
//
// plant puts a file onto the target as somebody else would: through the server for the WebDAV
// account (a file written behind the server's back is not seen by it for minutes - and telling it
// to keep no directory cache made every move slow), straight into the folder for the SSH server.
func (f *jobFixture) scenario(t *testing.T, tg jobs.Target, target string, tool string, plant func(rel, text string)) {
	t.Helper()
	ctx := context.Background()
	j, err := f.store.Create(jobs.Spec{Name: "Export " + tool, Source: f.source, Target: tg, Excludes: []string{"/cache/**"}, Schedule: jobs.Schedule{Kind: jobs.Manual}})
	if err != nil {
		t.Fatal(err)
	}
	const want = "a.txt sub dir/b.txt sub dir/deep/c.bin"

	// a dry run copies nothing and lists what a run would
	if err := jobs.WriteRequest(f.env.RunDir, j.ID, jobs.Request{DryRun: true, Trigger: jobs.TriggerPage, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rec, err := jobs.Run(ctx, f.env, j.ID)
	if err != nil || !rec.DryRun || rec.Trigger != jobs.TriggerPage || rec.Result != jobs.ResultOK || rec.Copied != 3 || rec.Tool != tool {
		t.Fatalf("dry run: %+v %v", rec, err)
	}
	if got := listTree(t, target); got != "" {
		t.Fatalf("the dry run copied: %s", got)
	}
	lines, _, _ := jobs.ReadLog(f.env.AddonDir, j.ID, rec.ID, 100)
	var names []string
	for _, l := range lines {
		if l.Kind == "C" {
			names = append(names, l.Text)
		}
	}
	if len(names) != 3 {
		t.Errorf("the dry run's list: %+v", lines)
	}

	// the run: what is data arrives; the cache, the key, the token and the backup's folder do not. The request is gone: this one is the timer's
	rec, err = jobs.Run(ctx, f.env, j.ID)
	if err != nil || rec.DryRun || rec.Trigger != "timer" || rec.Result != jobs.ResultOK || rec.Copied != 3 || rec.Deleted != 0 || rec.Bytes < 70000 {
		t.Fatalf("run: %+v %v", rec, err)
	}
	if got := listTree(t, target); got != want {
		t.Fatalf("on the target: %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "sub dir", "deep", "c.bin")); len(b) != 70000 {
		t.Errorf("c.bin has %d bytes", len(b))
	}
	// again: nothing to do
	if rec, err = jobs.Run(ctx, f.env, j.ID); err != nil || rec.Copied+rec.Replaced+rec.Deleted != 0 {
		t.Errorf("a second run: %+v %v", rec, err)
	}
	// a changed file is replaced, a new one copied; in copy mode nothing is deleted
	time.Sleep(1100 * time.Millisecond)
	writeTree(t, f.source, tree{"a.txt": "alpha changed", "new.txt": "new"})
	_ = os.Remove(filepath.Join(f.source, "sub dir", "b.txt"))
	if rec, err = jobs.Run(ctx, f.env, j.ID); err != nil || rec.Copied != 1 || rec.Replaced != 1 || rec.Deleted != 0 {
		t.Errorf("after changes: %+v %v", rec, err)
	}
	if got := listTree(t, target); got != "a.txt new.txt sub dir/b.txt sub dir/deep/c.bin" {
		t.Errorf("copy mode deleted: %q", got)
	}

	// mirror: refused until its dry run was looked at; then it deletes what is gone
	sp := jobs.Spec{Name: j.Name, Source: f.source, Target: tg, Mode: jobs.ModeMirror, Excludes: []string{"/cache/**"}, Schedule: jobs.Schedule{Kind: jobs.Manual}}
	if j, err = f.store.Update(j.ID, sp); err != nil || !j.NeedsDryRun {
		t.Fatal(j, err)
	}
	if rec, err = jobs.Run(ctx, f.env, j.ID); err == nil || rec.Result != jobs.ResultNeedsDryRun {
		t.Fatalf("a mirror run before its dry run: %+v %v", rec, err)
	}
	_ = jobs.WriteRequest(f.env.RunDir, j.ID, jobs.Request{DryRun: true, At: time.Now()})
	if rec, err = jobs.Run(ctx, f.env, j.ID); err != nil || rec.Deleted != 1 {
		t.Fatalf("the mirror's dry run: %+v %v", rec, err)
	}
	if got := listTree(t, target); !strings.Contains(got, "sub dir/b.txt") {
		t.Fatalf("the dry run deleted: %q", got)
	}
	_, _ = f.store.Set(j.ID, func(j *jobs.Job) { j.NeedsDryRun = false })
	if rec, err = jobs.Run(ctx, f.env, j.ID); err != nil || rec.Deleted != 1 {
		t.Fatalf("the mirror run: %+v %v", rec, err)
	}
	if got := listTree(t, target); got != "a.txt new.txt sub dir/deep/c.bin" {
		t.Errorf("after the mirror run: %q", got)
	}
	// versions: what a run replaces or deletes is kept in the run's folder, and old runs' go
	sp.Versions, sp.VersionsDays = true, 30
	j, _ = f.store.Update(j.ID, sp)
	_, _ = f.store.Set(j.ID, func(j *jobs.Job) { j.NeedsDryRun = false })
	time.Sleep(1100 * time.Millisecond)
	writeTree(t, f.source, tree{"a.txt": "alpha once more"})
	_ = os.Remove(filepath.Join(f.source, "new.txt"))
	plant(".versions/20200101T000000Z/old.txt", "old")
	plant(".versions/notes/keep.txt", "not a run's folder")
	if rec, err = jobs.Run(ctx, f.env, j.ID); err != nil || rec.Replaced != 1 || rec.Deleted != 1 {
		t.Fatalf("a run with versions: %+v %v", rec, err)
	}
	kept := filepath.Join(target, ".versions", rec.ID)
	if b, _ := os.ReadFile(filepath.Join(kept, "a.txt")); string(b) != "alpha changed" {
		t.Errorf("the replaced file's version: %q (%s)", b, listTree(t, filepath.Join(target, ".versions")))
	}
	if b, _ := os.ReadFile(filepath.Join(kept, "new.txt")); string(b) != "new" {
		t.Errorf("the deleted file's version: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "a.txt")); string(b) != "alpha once more" {
		t.Errorf("the target's a.txt: %q", b)
	}
	if got := listTree(t, filepath.Join(target, ".versions")); strings.Contains(got, "20200101T000000Z") || !strings.Contains(got, "notes/keep.txt") {
		t.Errorf("old versions: %q", got)
	}
	// the next run neither copies the versions back nor deletes them
	if rec, err = jobs.Run(ctx, f.env, j.ID); err != nil || rec.Copied+rec.Replaced+rec.Deleted != 0 {
		t.Errorf("a run after versions were kept: %+v %v", rec, err)
	}
	if _, err := os.Stat(filepath.Join(kept, "a.txt")); err != nil {
		t.Errorf("the versions went with the next mirror run: %v", err)
	}
	sp.Versions, sp.VersionsDays = false, 0
	_ = os.RemoveAll(filepath.Join(target, ".versions"))
	// more deletions than allowed: the run stops
	max := 1
	sp.MaxDelete = &max
	j, _ = f.store.Update(j.ID, sp)
	_, _ = f.store.Set(j.ID, func(j *jobs.Job) { j.NeedsDryRun = false })
	for _, n := range []string{"x1", "x2", "x3"} {
		plant(n, n)
	}
	if rec, err = jobs.Run(ctx, f.env, j.ID); err == nil || rec.Result != jobs.ResultTooManyDeletes {
		t.Errorf("past the deletion limit: %+v %v", rec, err)
	}
	if got := listTree(t, target); strings.Count(got, "x") < 2 {
		t.Errorf("the limit did not hold: %q", got)
	}
	// an empty source never mirrors; a source that is gone fails without touching the target
	empty := filepath.Join(f.root, "sdcard", "empty")
	_ = os.MkdirAll(empty, 0o755)
	sp.Source = empty
	j, _ = f.store.Update(j.ID, sp)
	_, _ = f.store.Set(j.ID, func(j *jobs.Job) { j.NeedsDryRun = false })
	before := listTree(t, target)
	if rec, err = jobs.Run(ctx, f.env, j.ID); err == nil || rec.Result != jobs.ResultSourceMissing {
		t.Errorf("an empty source: %+v %v", rec, err)
	}
	_ = os.Remove(empty)
	if rec, err = jobs.Run(ctx, f.env, j.ID); err == nil || rec.Result != jobs.ResultSourceMissing {
		t.Errorf("a missing source: %+v %v", rec, err)
	}
	if got := listTree(t, target); got != before {
		t.Errorf("the target changed without a source: %q", got)
	}
	// a link that leads into the configuration is no source
	_ = os.MkdirAll(filepath.Join(f.root, "etc", "config"), 0o755)
	_ = os.Symlink(filepath.Join(f.root, "etc", "config"), empty)
	if rec, err = jobs.Run(ctx, f.env, j.ID); err == nil || rec.Result != jobs.ResultFailed || !strings.Contains(rec.Error, "never copied") {
		t.Errorf("a source that leads into the configuration: %+v %v", rec, err)
	}
	_ = os.Remove(empty)
	// the history holds every run, newest first, and nothing running is left behind
	hist := jobs.History(f.env.AddonDir, j.ID)
	if len(hist) < 10 || hist[0].ID < hist[len(hist)-1].ID {
		t.Errorf("history: %d runs", len(hist))
	}
	if _, running := jobs.ReadProgress(f.env.RunDir, j.ID); running {
		t.Error("a progress file is left")
	}
	if err := f.store.Delete(j.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCopyJobWithRclone(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	acc, err := r.store.Create(ctx, accounts.Spec{Name: "DAV", Kind: accounts.KindWebDAV, URL: r.url, Vendor: "other", User: "backup", Password: "s3cret-pass"}, "")
	if err != nil {
		t.Fatal(err)
	}
	f := newJobFixture(t, r.store.Dir)
	f.env.Rclone = r.runner
	f.scenario(t, jobs.Target{Kind: jobs.TargetAccount, ID: acc.ID, Folder: "copies/export"}, filepath.Join(r.remote, "copies", "export"), "rclone", func(rel, text string) {
		if err := r.runner.Rcat(ctx, rclone.Path(acc.Remote(), "copies/export", rel), int64(len(text)), strings.NewReader(text), 0); err != nil {
			t.Fatal(err)
		}
	})

	// a target that is gone, and a login that is refused, are said as what they are
	j, _ := f.store.Create(jobs.Spec{Name: "Weg", Source: f.source, Target: jobs.Target{Kind: jobs.TargetAccount, ID: "zzzz000009", Folder: "x"}, Schedule: jobs.Schedule{Kind: jobs.Manual}})
	if rec, err := jobs.Run(ctx, f.env, j.ID); err == nil || rec.Result != jobs.ResultTargetMissing {
		t.Errorf("an account that is gone: %+v %v", rec, err)
	}
	if rec, err := jobs.Run(ctx, f.env, "no-such-job"); err == nil || rec.Result != jobs.ResultFailed {
		t.Errorf("a job that is not there: %+v %v", rec, err)
	}
}

// sshd starts an OpenSSH server as this user on a free port and answers the port.
func sshd(t *testing.T, authorizedKeys string) int {
	t.Helper()
	bin, err := exec.LookPath("sshd")
	if err != nil {
		for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd"} {
			if _, serr := os.Stat(p); serr == nil {
				bin, err = p, nil
			}
		}
	}
	if err != nil {
		needRsync(t, "no sshd here")
	}
	dir := t.TempDir()
	hostKey := filepath.Join(dir, "host_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	port := freePort(t)
	conf := filepath.Join(dir, "sshd_config")
	text := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s\nAuthorizedKeysFile %s\nPidFile %s\nStrictModes no\nUsePAM no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nPubkeyAuthentication yes\nLogLevel ERROR\n",
		port, hostKey, authorizedKeys, filepath.Join(dir, "sshd.pid"))
	if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-D", "-e", "-f", conf)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			c.Close()
			return port
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("sshd did not come up")
	return 0
}

func needRsync(t *testing.T, why string) {
	t.Helper()
	if os.Getenv("SYNC_REQUIRE_RSYNC") != "" {
		t.Fatal(why + " (SYNC_REQUIRE_RSYNC is set)")
	}
	t.Skip(why)
}

func TestCopyJobWithRsyncOverSSH(t *testing.T) {
	for _, tool := range []string{"rsync", "ssh", "ssh-keygen", "ssh-keyscan"} {
		if _, err := exec.LookPath(tool); err != nil {
			needRsync(t, "no "+tool+" here")
		}
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conf := t.TempDir()
	store, err := servers.Open(conf, servers.Tools{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.Log = t.Logf
	authorized := filepath.Join(t.TempDir(), "authorized_keys")
	port := sshd(t, authorized)
	base := filepath.Join(t.TempDir(), "srv")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	srv, err := store.Create(ctx, servers.Spec{Name: "Hier", Host: "127.0.0.1", Port: port, User: me.Username, Path: base})
	if err != nil {
		t.Fatal(err)
	}
	// the host key is not confirmed: nothing is tried, and a run fails on it
	if v, _ := store.Test(ctx, srv.ID); v.State != servers.StateHostKeyUnknown {
		t.Fatalf("before the host key: %+v", v)
	}
	f := newJobFixture(t, conf)
	early, _ := f.store.Create(jobs.Spec{Name: "Zu früh", Source: f.source, Target: jobs.Target{Kind: jobs.TargetServer, ID: srv.ID, Folder: "x"}, Schedule: jobs.Schedule{Kind: jobs.Manual}})
	if rec, err := jobs.Run(ctx, f.env, early.ID); err == nil || rec.Result != jobs.ResultHostKey {
		t.Errorf("a run without a confirmed host key: %+v %v", rec, err)
	}
	// read, shown, and only then trusted
	v, err := store.Scan(ctx, srv.ID)
	if err != nil || len(v.Pending) == 0 || len(v.HostKeys) != 0 || !strings.HasPrefix(v.Pending[0].Fingerprint, "SHA256:") {
		t.Fatalf("scan: %+v %v", v, err)
	}
	if _, err := store.Trust(srv.ID, []string{"SHA256:not-this-one"}); err == nil {
		t.Fatal("another fingerprint was trusted")
	}
	var fps []string
	for _, k := range v.Pending {
		fps = append(fps, k.Fingerprint)
	}
	if v, err = store.Trust(srv.ID, fps); err != nil || len(v.HostKeys) == 0 || len(v.Pending) != 0 {
		t.Fatalf("trust: %+v %v", v, err)
	}
	// the key is not on the server yet: the login is refused
	if v, _ = store.Test(ctx, srv.ID); v.State != servers.StateAuthFailed {
		t.Fatalf("before the key is on the server: %+v", v)
	}
	if rec, err := jobs.Run(ctx, f.env, early.ID); err == nil || rec.Result != jobs.ResultLoginRefused {
		t.Errorf("a run with a refused key: %+v %v", rec, err)
	}
	// the line the page shows goes into authorized_keys as it is, "restrict" and all
	if err := os.WriteFile(authorized, []byte(v.PublicKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, _ = store.Test(ctx, srv.ID); v.State != servers.StateOK {
		t.Fatalf("the test: %+v", v)
	}
	if got := listTree(t, base); got != "" {
		t.Errorf("the test left something: %q", got)
	}
	_ = f.store.Delete(early.ID)
	f.scenario(t, jobs.Target{Kind: jobs.TargetServer, ID: srv.ID, Folder: "copies/export"}, filepath.Join(base, "copies", "export"), "rsync", func(rel, text string) {
		writeTree(t, filepath.Join(base, "copies", "export"), tree{rel: text})
	})

	// a server that shows another host key afterwards is refused: nothing is accepted on the way
	port2 := sshd(t, authorized)
	if _, err := store.Update(srv.ID, servers.Spec{Name: "Hier", Host: "127.0.0.1", Port: port, User: me.Username, Path: base}); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(servers.KeyDir(conf, srv.ID), "known_hosts")
	b, _ := os.ReadFile(kh)
	_ = os.WriteFile(kh, []byte(strings.ReplaceAll(string(b), fmt.Sprintf(":%d ", port), fmt.Sprintf(":%d ", port2))), 0o600)
	list, _ := servers.Load(conf)
	list[0].Port = port2
	changed, _ := servers.Open(conf, servers.Tools{}, t.TempDir())
	_ = changed
	j, _ := f.store.Create(jobs.Spec{Name: "Anderer", Source: f.source, Target: jobs.Target{Kind: jobs.TargetServer, ID: srv.ID, Folder: "y"}, Schedule: jobs.Schedule{Kind: jobs.Manual}})
	// point the stored server at the second sshd, whose host key is another one
	raw, _ := os.ReadFile(filepath.Join(conf, servers.File))
	_ = os.WriteFile(filepath.Join(conf, servers.File), []byte(strings.Replace(string(raw), fmt.Sprintf(`"port": %d`, port), fmt.Sprintf(`"port": %d`, port2), 1)), 0o600)
	if rec, err := jobs.Run(ctx, f.env, j.ID); err == nil || rec.Result != jobs.ResultHostKey {
		t.Errorf("a changed host key: %+v %v", rec, err)
	}
	if got := listTree(t, filepath.Join(base, "y")); got != "" {
		t.Errorf("a run against a changed host key copied: %q", got)
	}
}
