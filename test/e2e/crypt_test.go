package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/fakesys"
	"github.com/openccu-lite/lite-addon-sync/internal/httpapi"
	"github.com/openccu-lite/lite-addon-sync/internal/jobs"
	"github.com/openccu-lite/lite-addon-sync/internal/servers"
	"github.com/openccu-lite/lite-addon-sync/internal/sysinfo"
	"github.com/openccu-lite/lite-addon-sync/internal/systimers"
)

type noFacts struct{}

func (noFacts) Facts(context.Context, bool) sysinfo.Facts {
	return sysinfo.Facts{Targets: map[string][]sysinfo.Target{}}
}

type bearerRT string

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// An encrypted copy job, through the API and the real rclone: the password is answered once and
// never again, what lies on the account has neither the files' names nor their contents, rclone
// reads it back through the job's crypt remote, versions are encrypted too, the remote follows a
// change of the folder and goes with the job.
func TestEncryptedCopyJob(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	acc, err := r.store.Create(ctx, accounts.Spec{Name: "DAV", Kind: accounts.KindWebDAV, URL: r.url, Vendor: "other", User: "backup", Password: "s3cret-pass"}, "")
	if err != nil {
		t.Fatal(err)
	}
	f := newJobFixture(t, r.store.Dir)
	f.env.Rclone = r.runner
	sys := fakesys.New("tok")
	sysSrv := httptest.NewServer(sys.Handler())
	defer sysSrv.Close()
	srvStore, _ := servers.Open(r.store.Dir, servers.Tools{}, t.TempDir())
	h := httpapi.New(httpapi.Options{BasePath: "/addons/sync", WWW: t.TempDir(), Accounts: r.store, Flows: &accounts.Flows{Bin: r.bin}, Facts: noFacts{}, Version: "test", Started: time.Now(),
		Servers: srvStore, Jobs: f.store, Timers: &systimers.Client{Base: sysSrv.URL, HTTP: &http.Client{Transport: bearerRT("tok")}}, AddonDir: f.env.AddonDir, RunDir: f.env.RunDir})
	api := httptest.NewServer(h)
	defer api.Close()
	call := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, api.URL+"/addons/sync/api/v1"+path, bytes.NewReader([]byte(body)))
		req.Header.Set("X-CCU-Addon-Request", "1")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	body := func(folder string, encrypt bool) string {
		return fmt.Sprintf(`{"name":"Geheim","source":%q,"target":{"kind":"account","id":%q,"folder":%q},"versions":true,"versions_days":30,"encrypt":%v,"schedule":{"kind":"manual"}}`, f.source, acc.ID, folder, encrypt)
	}
	code, out := call("POST", "/jobs", body("secret/one", true))
	var made struct {
		ID            string `json:"id"`
		Encrypt       bool   `json:"encrypt"`
		CryptPassword string `json:"crypt_password"`
	}
	if err := json.Unmarshal([]byte(out), &made); err != nil || code != 201 || !made.Encrypt || len(made.CryptPassword) != 32 {
		t.Fatalf("%d %s", code, out)
	}
	// never again: not in the state, not in a later answer, not in the job's record
	if _, st := call("GET", "/state", ""); strings.Contains(st, made.CryptPassword) || strings.Contains(st, "crypt_password") {
		t.Error("the state carries the password")
	}
	if b, _ := os.ReadFile(filepath.Join(r.store.Dir, jobs.File)); strings.Contains(string(b), made.CryptPassword) {
		t.Error("jobs.json carries the password")
	}
	if b, _ := os.ReadFile(r.runner.Config); strings.Contains(string(b), made.CryptPassword) || !strings.Contains(string(b), "[job-geheim]") {
		t.Errorf("rclone.conf: the password in clear, or no crypt remote")
	}
	rec, err := jobs.Run(ctx, f.env, made.ID)
	if err != nil || rec.Copied != 4 {
		t.Fatalf("run: %+v %v", rec, err)
	}
	target := filepath.Join(r.remote, "secret", "one")
	on := listTree(t, target)
	if on == "" || strings.Contains(on, "a.txt") || strings.Contains(on, "sub dir") || strings.Contains(on, "c.bin") {
		t.Fatalf("on the account: %q", on)
	}
	_ = filepath.WalkDir(target, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("alpha")) || bytes.Contains(b, []byte("ccccccccccccccccc")) {
				t.Errorf("%s holds the content in clear", p)
			}
		}
		return nil
	})
	// rclone reads it back through the crypt remote
	rd, err := r.runner.Cat(ctx, "job-geheim:a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rd)
	_ = rd.Close()
	if string(got) != "alpha" {
		t.Errorf("read back: %q", got)
	}
	// a replaced file's version is encrypted as well, and readable through the remote
	time.Sleep(1100 * time.Millisecond)
	writeTree(t, f.source, tree{"a.txt": "alpha changed"})
	if rec, err = jobs.Run(ctx, f.env, made.ID); err != nil || rec.Replaced != 1 {
		t.Fatalf("second run: %+v %v", rec, err)
	}
	if on := listTree(t, target); strings.Contains(on, ".versions") || strings.Contains(on, "a.txt") {
		t.Errorf("the versions' names are in clear: %q", on)
	}
	rd, _ = r.runner.Cat(ctx, "job-geheim:.versions/"+rec.ID+"/a.txt")
	got, _ = io.ReadAll(rd)
	_ = rd.Close()
	if string(got) != "alpha" {
		t.Errorf("the version read back: %q", got)
	}
	// encryption is not switched off afterwards; another folder moves the remote, the password stays
	if code, out = call("PUT", "/jobs/"+made.ID, body("secret/one", false)); code != 422 {
		t.Errorf("switching encryption off: %d %s", code, out)
	}
	if code, out = call("PUT", "/jobs/"+made.ID, body("secret/two", true)); code != 200 || strings.Contains(out, "crypt_password") {
		t.Fatalf("another folder: %d %s", code, out)
	}
	if rec, err = jobs.Run(ctx, f.env, made.ID); err != nil || rec.Copied != 4 {
		t.Fatalf("run into the other folder: %+v %v", rec, err)
	}
	if on := listTree(t, filepath.Join(r.remote, "secret", "two")); on == "" || strings.Contains(on, "a.txt") {
		t.Errorf("the other folder: %q", on)
	}
	// encryption on an SSH server is refused; the remote goes with the job
	if code, _ = call("POST", "/jobs", strings.Replace(body("x", true), `"kind":"account"`, `"kind":"server"`, 1)); code != 422 {
		t.Errorf("an encrypted job to a server: %d", code)
	}
	if code, _ = call("DELETE", "/jobs/"+made.ID, ""); code != 204 {
		t.Fatal(code)
	}
	if remotes, _ := r.runner.Remotes(ctx); strings.Contains(strings.Join(remotes, ","), "job-geheim") {
		t.Errorf("the crypt remote stays after the job: %v", remotes)
	}
}
