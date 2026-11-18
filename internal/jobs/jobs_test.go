package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/servers"
)

func TestSlug(t *testing.T) {
	taken := map[string]bool{}
	is := func(id string) bool { return taken[id] }
	for name, want := range map[string]string{
		"Fotos Klingel":         "fotos-klingel",
		"Verlauf-DB":            "verlauf-db",
		"  Größe & Maße!  ":     "groesse-masse",
		"--":                    "job",
		"../../etc/passwd":      "etc-passwd",
		"-rf":                   "rf",
		strings.Repeat("a", 60): strings.Repeat("a", 27),
		"a\nb; rm -rf /":        "a-b-rm-rf",
		"ÄÖÜ":                   "aeoeue",
		"日本語":                   "job",
	} {
		got := Slug(name, is)
		if got != want || !ValidID(got) {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
	taken["export"], taken["export-2"] = true, true
	if got := Slug("Export", is); got != "export-3" {
		t.Errorf("a taken name: %q", got)
	}
	long := strings.Repeat("b", 27)
	taken[long] = true
	if got := Slug(long, is); got != strings.Repeat("b", 25)+"-2" || len(got) > MaxIDLen {
		t.Errorf("a taken long name: %q", got)
	}
}

func withRoot(t *testing.T) string {
	t.Helper()
	old := Root
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	Root = dir
	t.Cleanup(func() { Root = old })
	for _, d := range []string{"etc/config/addons/hmm", "tmp", "var/lib", "lost+found", "sdcard/doorbell", "sdcard/backup", "addons/sync/var", "addons/history/var", "addons/history/bin", "export/sub", "export/cache", "export/bin"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for f, text := range map[string]string{
		"sdcard/backup/ccu-2026-11-17.sbk.age": "x", "addons/history/bin/.nobackup": "", "export/a.txt": "a", "export/sub/b.txt": "b", "export/cache/c.tmp": "c",
		// .nobackup alone says "not in the system's backup", not "program files": what is kept out of
		// the backup because it is large is what a copy job is for
		"sdcard/.nobackup": "", "addons/history/var/.nobackup": "", "addons/history/var/history.db": "db", "export/bin/.nobackup": "", "export/bin/tool": "x",
		"etc/config/keys": "secret", "sdcard/doorbell/1.jpg": "jpg",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The fixed list of what is never copied holds for the path as written and for where its links
// lead: the configuration, the temporary trees, backups, program files, the addon's own tree.
func TestSources(t *testing.T) {
	root := withRoot(t)
	for p, ok := range map[string]bool{
		root + "/export":              true,
		root + "/export/sub/":         true,
		root + "/sdcard/doorbell":     true,
		root + "/addons/history/var":  true,
		root:                          false,
		root + "/":                    false,
		root + "/etc":                 false,
		root + "/etc/config":          false,
		root + "/etc/config/addons/x": false,
		root + "/tmp":                 false,
		root + "/var/lib":             false,
		root + "/lost+found":          false,
		root + "/export/lost+found":   false,
		root + "/addons/sync":         false,
		root + "/addons/sync/var":     false,
		root + "/export/../etc":       false,
		"/etc":                        false,
		"/":                           false,
		"export":                      false,
		"":                            false,
		root + "/export\n/x":          false,
		filepath.Dir(root):            false,
		root + "x/export":             false,
	} {
		got, err := CleanSource(p)
		if ok != (err == nil) {
			t.Errorf("%q: %q %v", p, got, err)
		}
	}
	// on disk: a link that leads into the configuration, a folder of backups, program files
	if err := os.Symlink(filepath.Join(root, "etc", "config"), filepath.Join(root, "export", "sneaky")); err != nil {
		t.Fatal(err)
	}
	for p, ok := range map[string]bool{
		root + "/export":             true,
		root + "/export/sneaky":      false,
		root + "/sdcard/backup":      false,
		root + "/addons/history/bin": false,
		root + "/export/a.txt":       false,
		root + "/missing":            false,
	} {
		if _, err := CheckSourceOnDisk(p); ok != (err == nil) {
			t.Errorf("on disk %q: %v", p, err)
		}
	}

	// the picker
	l, err := List("", nil)
	if err != nil || l.Path != root || l.Parent != "" {
		t.Fatal(l, err)
	}
	state := map[string]Folder{}
	for _, f := range l.Folders {
		state[f.Name] = f
	}
	for name, want := range map[string]string{"etc": FolderNever, "tmp": FolderNever, "var": FolderNever, "lost+found": FolderNever, "export": FolderOK, "sdcard": FolderOK, "addons": FolderOK} {
		if state[name].State != want {
			t.Errorf("picker %s: %+v", name, state[name])
		}
	}
	if state["etc"].Why != NeverConfig || state["tmp"].Why != NeverTemp {
		t.Errorf("reasons: %+v %+v", state["etc"], state["tmp"])
	}
	l, _ = List(root+"/sdcard", nil)
	for _, f := range l.Folders {
		if f.Name == "backup" && (f.State != FolderNever || f.Why != NeverBackups) {
			t.Errorf("a folder of backups: %+v", f)
		}
	}
	l, _ = List(root+"/addons/history", nil)
	for _, f := range l.Folders {
		if f.Name == "bin" && (f.State != FolderNever || f.Why != NeverProgram) {
			t.Errorf("program files: %+v", f)
		}
	}
	if l, _ := List(root+"/addons", nil); len(l.Folders) != 2 || l.Folders[1].Name != "sync" || l.Folders[1].Why != NeverOwn {
		t.Errorf("the addon's own tree: %+v", l.Folders)
	}
	if l, _ := List(root+"/etc", nil); l.Self.State != FolderNever || len(l.Folders) != 0 {
		t.Errorf("inside the configuration: %+v", l)
	}
	for _, p := range []string{"/etc", filepath.Dir(root), "relative", root + "/../.."} {
		if l, err := List(p, nil); err == nil && strings.HasPrefix(l.Path, root) == false {
			t.Errorf("the picker left the storage: %q -> %+v", p, l)
		}
	}

	// tagged .nobackup, and still data: the SD card's folder, an addon's var, a bin outside the addons
	for _, p := range []string{root + "/sdcard", root + "/sdcard/doorbell", root + "/addons/history/var", root + "/export/bin"} {
		if _, err := CheckSourceOnDisk(p); err != nil {
			t.Errorf("a .nobackup folder that is data: %s: %v", p, err)
		}
	}
	if l, _ := List(root, nil); len(l.Folders) == 0 {
		t.Fatal("no folders")
	} else {
		for _, f := range l.Folders {
			if f.Name == "sdcard" && f.State != FolderOK {
				t.Errorf("the SD card's folder: %+v", f)
			}
		}
	}

	// the exclude list of a run: fixed, the name filters, the user's, and what the walk finds
	if err := os.MkdirAll(filepath.Join(root, "addons", "history", "var", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "addons", "history", "var", "lib", ".nobackup"), nil, 0o644)
	if ex := strings.Join(Excludes(Job{}, root+"/addons/history/var"), "\n"); !strings.Contains(ex, "\n/lib/") {
		t.Errorf("program files inside an addon's data:\n%s", ex)
	}
	_ = os.MkdirAll(filepath.Join(root, "export", "old", "backups"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "export", "old", "backups", "x.sbk"), nil, 0o644)
	ex := strings.Join(Excludes(Job{NameFilters: true, Excludes: []string{"/cache/**"}}, root+"/export"), "\n")
	for _, want := range []string{"*.sbk", "*.sbk.age", "*.partial", "*.pem", "id_*", "*token*", "/cache/**", "/old/backups/"} {
		if !strings.Contains("\n"+ex+"\n", "\n"+want+"\n") {
			t.Errorf("the exclude list lacks %q:\n%s", want, ex)
		}
	}
	if ex := strings.Join(Excludes(Job{}, root+"/export"), "\n"); strings.Contains(ex, "*.pem") || !strings.Contains(ex, "*.sbk") {
		t.Errorf("without the name filters:\n%s", ex)
	}
}

func TestSpecRules(t *testing.T) {
	root := withRoot(t)
	good := Spec{Name: "Export", Source: root + "/export", Target: Target{Kind: TargetAccount, ID: "aone000001", Folder: "exp"}, Schedule: Schedule{Kind: Daily, Time: "03:00"}}
	s := &Store{Dir: t.TempDir()}
	j, err := s.Create(good)
	if err != nil || j.ID != "export" || j.Mode != ModeCopy || j.MaxDelete != DefaultMaxDelete || !j.NameFilters || j.NeedsDryRun {
		t.Fatal(j, err)
	}
	for what, change := range map[string]func(*Spec){
		"no name":                    func(sp *Spec) { sp.Name = " " },
		"a two-line name":            func(sp *Spec) { sp.Name = "a\nb" },
		"a source outside":           func(sp *Spec) { sp.Source = "/etc" },
		"the configuration":          func(sp *Spec) { sp.Source = root + "/etc/config" },
		"an unknown kind of target":  func(sp *Spec) { sp.Target.Kind = "ftp" },
		"a target id with a slash":   func(sp *Spec) { sp.Target.ID = "../x" },
		"no folder":                  func(sp *Spec) { sp.Target.Folder = "/" },
		"a folder that is an option": func(sp *Spec) { sp.Target.Folder = "-e sh" },
		"a folder with ..":           func(sp *Spec) { sp.Target.Folder = "a/../../b" },
		"a folder with a newline":    func(sp *Spec) { sp.Target.Folder = "a\nb" },
		"a folder with a quote":      func(sp *Spec) { sp.Target.Folder = "a'b" },
		"a folder with a dollar":     func(sp *Spec) { sp.Target.Folder = "$(id)" },
		"an unknown mode":            func(sp *Spec) { sp.Mode = "bisync" },
		"an exclude that merges":     func(sp *Spec) { sp.Excludes = []string{". /etc/passwd"} },
		"an exclude that includes":   func(sp *Spec) { sp.Excludes = []string{"+ /etc/**"} },
		"an exclude with a newline":  func(sp *Spec) { sp.Excludes = []string{"a\n+ b"} },
		"an exclude that is a flag":  func(sp *Spec) { sp.Excludes = []string{"--files-from=/etc/shadow"} },
		"an exclude with braces":     func(sp *Spec) { sp.Excludes = []string{"{a,b}"} },
		"a negative limit":           func(sp *Spec) { sp.BwLimit = -1 },
		"a time that is none":        func(sp *Spec) { sp.Schedule = Schedule{Kind: Daily, Time: "25:00"} },
		"a calendar expression":      func(sp *Spec) { sp.Schedule = Schedule{Kind: Daily, Time: "03:00\nUnit=x"} },
		"a weekday that is none":     func(sp *Spec) { sp.Schedule = Schedule{Kind: Weekly, Time: "03:00", Weekday: "Mo"} },
		"an unknown schedule":        func(sp *Spec) { sp.Schedule = Schedule{Kind: "minutely"} },
		"a read grant with a slash":  func(sp *Spec) { sp.ReadAddon = "../occulite" },
		"encryption on a server":     func(sp *Spec) { sp.Target.Kind, sp.Encrypt = TargetServer, true },
	} {
		sp := good
		change(&sp)
		if _, err := s.Create(sp); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	if len(s.List()) != 1 {
		t.Fatalf("a refused job was kept: %+v", s.List())
	}
	// a mirror job waits for its dry run; a change of what it does asks again; copy never does
	mirror := good
	mirror.Name, mirror.Mode = "Spiegel", ModeMirror
	m, err := s.Create(mirror)
	if err != nil || !m.NeedsDryRun || m.ID != "spiegel" {
		t.Fatal(m, err)
	}
	m, _ = s.Set(m.ID, func(j *Job) { j.NeedsDryRun = false })
	mirror.BwLimit = 100
	if m, _ = s.Update(m.ID, mirror); m.NeedsDryRun || m.ID != "spiegel" {
		t.Errorf("a change of the rate asked for a dry run: %+v", m)
	}
	mirror.Name, mirror.Target.Folder = "Spiegel 2", "other"
	if m, _ = s.Update(m.ID, mirror); !m.NeedsDryRun || m.ID != "spiegel" || m.Name != "Spiegel 2" {
		t.Errorf("another folder did not ask for a dry run, or the id followed the name: %+v", m)
	}
	mirror.Mode = ModeCopy
	if m, _ = s.Update(m.ID, mirror); m.NeedsDryRun {
		t.Errorf("a copy job waits for a dry run: %+v", m)
	}
	// encryption is chosen when a job is made, and stays
	plain := good
	plain.Encrypt = true
	if _, err := s.Update("export", plain); err == nil {
		t.Error("an existing job was switched to encrypted")
	}
	// the file is read again as it was written
	s2, err := Open(s.Dir)
	if err != nil || len(s2.List()) != 2 {
		t.Fatal(s2, err)
	}
	if got := s.UsesTarget(TargetAccount, "aone000001"); len(got) != 2 {
		t.Errorf("uses: %+v", got)
	}
}

// The timer text is exactly what the Services page's form writes (occulited ui/src/lib/timers.ts,
// timerText), so a job's timer opens there as its schedule and follows that form.
func TestScheduleAndTimerText(t *testing.T) {
	for _, c := range []struct {
		s    Schedule
		text string
	}{
		{Schedule{Kind: Daily, Time: "03:00"}, "[Unit]\nDescription=sync-fotos\n\n[Timer]\nOnCalendar=*-*-* 03:00:00\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"},
		{Schedule{Kind: Weekly, Time: "23:59", Weekday: "Sun"}, "[Unit]\nDescription=sync-fotos\n\n[Timer]\nOnCalendar=Sun *-*-* 23:59:00\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"},
		{Schedule{Kind: Hourly}, "[Unit]\nDescription=sync-fotos\n\n[Timer]\nOnCalendar=hourly\nPersistent=true\nRandomizedDelaySec=5min\n\n[Install]\nWantedBy=timers.target\n"},
	} {
		if got := TimerText("fotos", c.s); got != c.text {
			t.Errorf("%+v:\n%q\nwant\n%q", c.s, got, c.text)
		}
		if back := ReadTimer("fotos", c.text, true); back.Kind != c.s.Kind || back.Time != c.s.Time || back.Weekday != c.s.Weekday || back.Calendar != nil && back.Kind == Custom {
			t.Errorf("read back %+v as %+v", c.s, back)
		}
		if back := ReadTimer("fotos", c.text, false); back.Kind != Manual || back.Calendar != nil {
			t.Errorf("switched off: %+v", back)
		}
	}
	daily := TimerText("fotos", Schedule{Kind: Daily, Time: "03:00"})
	// manual gets a sensible text, switched off by the caller
	if TimerText("fotos", Schedule{Kind: Manual}) != daily {
		t.Error("a manual job's timer text")
	}
	// edits on the Services page: another time is still simple, a delay too; anything else is custom
	if s := ReadTimer("fotos", strings.Replace(daily, "03:00:00", "04:30:00", 1), true); s.Kind != Daily || s.Time != "04:30" {
		t.Errorf("another time: %+v", s)
	}
	if s := ReadTimer("fotos", strings.Replace(daily, "Persistent=true\n", "Persistent=true\nRandomizedDelaySec=10min\n", 1), true); s.Kind != Daily {
		t.Errorf("a random delay: %+v", s)
	}
	for what, text := range map[string]string{
		"working days":        strings.Replace(daily, "*-*-* 03:00:00", "Mon..Fri *-*-* 02:30:00", 1),
		"two calendars":       strings.Replace(daily, "Persistent=true\n", "OnCalendar=*-*-* 15:00:00\nPersistent=true\n", 1),
		"not persistent":      strings.Replace(daily, "Persistent=true\n", "", 1),
		"another directive":   strings.Replace(daily, "Persistent=true\n", "Persistent=true\nAccuracySec=1h\n", 1),
		"another description": strings.Replace(daily, "Description=sync-fotos", "Description=Meine Fotos", 1),
		"a boot timer":        strings.Replace(daily, "OnCalendar=*-*-* 03:00:00", "OnBootSec=5min", 1),
	} {
		s := ReadTimer("fotos", text, true)
		if s.Kind != Custom {
			t.Errorf("%s: read as %+v", what, s)
		}
		if off := ReadTimer("fotos", text, false); off.Kind != Manual || (what != "a boot timer" && len(off.Calendar) == 0) {
			t.Errorf("%s, switched off: %+v", what, off)
		}
	}
	if s := ReadTimer("fotos", strings.Replace(daily, "*-*-* 03:00:00", "Mon..Fri *-*-* 02:30:00", 1), true); len(s.Calendar) != 1 || s.Calendar[0] != "Mon..Fri *-*-* 02:30:00" {
		t.Errorf("the custom calendar: %+v", s)
	}
	if Hash(daily) == Hash(daily+" ") || len(Hash(daily)) != 64 {
		t.Error("hash")
	}
}

// The tools' arguments are lists; nothing a job holds becomes an option or a second argument.
func TestToolArguments(t *testing.T) {
	j := Job{ID: "x", Mode: ModeMirror, MaxDelete: 50, BwLimit: 512, Target: Target{Kind: TargetServer, ID: "s", Folder: "my photos/2026"}}
	srv := servers.Server{ID: "abcdefghij", Host: "nas.example.org", Port: 2222, User: "backup", Path: "/volume1/ccu/"}
	args := RsyncArgs(j, "/usr/local/sdcard/door bell/", srv, "/run/addon-sync/ex.txt", "20261117T030000Z", true)
	want := "-rlt|-s|--out-format=%i %n|--info=progress2|--stats|--timeout=120|--exclude-from=/run/addon-sync/ex.txt|--delete|--max-delete=50|--bwlimit=512|--dry-run|--|/usr/local/sdcard/door bell/|backup@nas.example.org:/volume1/ccu/my photos/2026/"
	if got := strings.Join(args, "|"); got != want {
		t.Errorf("rsync:\n%s\nwant\n%s", got, want)
	}
	j.Mode, j.BwLimit = ModeCopy, 0
	if got := strings.Join(RsyncArgs(j, "/usr/local/x", srv, "/e", "s", false), "|"); strings.Contains(got, "--delete") || strings.Contains(got, "--dry-run") || strings.Contains(got, "bwlimit") || strings.Contains(got, "--backup") {
		t.Errorf("a copy run: %s", got)
	}
	// versions: what is replaced or deleted goes into the run's folder below the destination
	j.Versions = true
	if got := strings.Join(RsyncArgs(j, "/usr/local/x", srv, "/e", "20261117T030000Z", false), "|"); !strings.Contains(got, "|--backup|--backup-dir=.versions/20261117T030000Z|--|") {
		t.Errorf("rsync with versions: %s", got)
	}
	if got := strings.Join(RsyncListArgs(j, srv), "|"); got != "--list-only|-s|--timeout=60|--|backup@nas.example.org:/volume1/ccu/my photos/2026/.versions/" {
		t.Errorf("list: %s", got)
	}
	if got := strings.Join(RsyncPruneArgs(j, srv, "/run/e", "20260101T030000Z"), "|"); got != "-r|-s|--timeout=120|--delete|--include=/20260101T030000Z/***|--exclude=*|--|/run/e/|backup@nas.example.org:/volume1/ccu/my photos/2026/.versions/" {
		t.Errorf("prune: %s", got)
	}
	j.Versions = false
	for _, a := range RsyncArgs(j, "/usr/local/x", srv, "/e", "s", false) {
		if a == "-e" || strings.HasPrefix(a, "--rsh") || strings.HasPrefix(a, "--rsync-path") || strings.HasPrefix(a, "--files-from") {
			t.Errorf("rsync gets %q", a)
		}
	}
	if got := servers.Dest(servers.Server{Host: "fd00::5", User: "u", Path: "/b"}, "f"); got != "u@[fd00::5]:/b/f/" {
		t.Errorf("an IPv6 server: %s", got)
	}
	if got := strings.Join(RsyncMkdirArgs("/run/addon-sync/mkdir-1/", srv), "|"); got != "-r|-s|--timeout=60|--|/run/addon-sync/mkdir-1/|backup@nas.example.org:/volume1/ccu/" {
		t.Errorf("mkdir: %s", got)
	}

	j = Job{ID: "x", Mode: ModeMirror, MaxDelete: 50, BwLimit: 512, Target: Target{Kind: TargetAccount, ID: "a", Folder: "db/history"}}
	got := strings.Join(RcloneArgs(j, "/usr/local/addons/history/var", RcloneDest(j, "acc-aone000001", "my-bucket"), "/e", "s", true), "|")
	if got != "sync|--exclude-from|/e|--skip-links|--max-delete|50|--bwlimit|512k|--dry-run|/usr/local/addons/history/var|acc-aone000001:my-bucket/db/history" {
		t.Errorf("rclone: %s", got)
	}
	j.Mode = ModeCopy
	if got := strings.Join(RcloneArgs(j, "/usr/local/x", RcloneDest(j, "acc-a", ""), "/e", "s", false), "|"); got != "copy|--exclude-from|/e|--skip-links|--bwlimit|512k|/usr/local/x|acc-a:db/history" {
		t.Errorf("rclone copy: %s", got)
	}
	j.Versions = true
	if got := strings.Join(RcloneArgs(j, "/usr/local/x", RcloneDest(j, "acc-a", "bucket"), "/e", "20261117T030000Z", false), "|"); got != "copy|--exclude-from|/e|--skip-links|--bwlimit|512k|--backup-dir|acc-a:bucket/db/history/.versions/20261117T030000Z|/usr/local/x|acc-a:bucket/db/history" {
		t.Errorf("rclone with versions: %s", got)
	}
	// an encrypted job goes through its own crypt remote, versions too
	j.Encrypt = true
	if got := strings.Join(RcloneArgs(j, "/usr/local/x", RcloneDest(j, "acc-a", "bucket"), "/e", "20261117T030000Z", false), "|"); got != "copy|--exclude-from|/e|--skip-links|--bwlimit|512k|--backup-dir|job-x:.versions/20261117T030000Z|/usr/local/x|job-x:" {
		t.Errorf("rclone encrypted: %s", got)
	}
	j.Encrypt = false
	if ex := strings.Join(Excludes(j, t.TempDir()), "\n"); !strings.Contains(ex, "\n/.versions/") {
		t.Errorf("the versions' folder is not left out of the copy:\n%s", ex)
	}
	// only folders named as a run's time, and only the old ones, are ever removed
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	old := OldVersions([]string{"20261001T030000Z", "20261110T030000Z/", "20261117T030000Z", "20250101T000000Z", "photos", "..", "20261001", "."}, 30, now)
	if strings.Join(old, ",") != "20261001T030000Z,20250101T000000Z" {
		t.Errorf("old versions: %v", old)
	}
	if OldVersions([]string{"20200101T000000Z"}, 0, now) != nil {
		t.Error("0 days removes versions")
	}
}

func TestParseRsync(t *testing.T) {
	rec := Record{}
	prog := Progress{}
	rl := newRunLog(&rec, nil)
	for _, l := range []string{
		">f+++++++++ a.txt",
		">f+++++++++ dir with space/b.txt",
		">f.st...... changed.txt",
		"cd+++++++++ newdir/",
		".d..t...... ./",
		"cL+++++++++ link -> target",
		"*deleting   gone.txt",
		"          1,234  45%    1.23MB/s    0:00:10 (xfr#3, to-chk=5/20)",
		"Number of files: 20 (reg: 17, dir: 3)",
		"Total transferred file size: 12,345 bytes",
		"sending incremental file list",
	} {
		parseRsync(l, false, rl, &rec, &prog)
	}
	parseRsync(`rsync: [sender] send_files failed to open "/usr/local/x/secret": Permission denied (13)`, true, rl, &rec, &prog)
	parseRsync("rsync error: some files/attrs were not transferred (see previous errors) (code 23) at main.c(1338) [sender=3.5.0]", true, rl, &rec, &prog)
	if rec.Copied != 3 || rec.Replaced != 1 || rec.Deleted != 1 || rec.Errors != 1 || rec.Bytes != 12345 || prog.Percent != 45 || prog.Bytes != 1234 || prog.Speed != "1.23MB/s" || !strings.Contains(rec.Error, "Permission denied") {
		t.Errorf("%+v %+v", rec, prog)
	}
	for exit, want := range map[int]string{0: ResultOK, 23: ResultWarnings, 24: ResultWarnings, 25: ResultTooManyDeletes, 20: ResultStopped, 30: ResultUnreachable, 1: ResultFailed} {
		if got := rsyncResult(exit, ""); got != want {
			t.Errorf("exit %d: %s", exit, got)
		}
	}
	for text, want := range map[string]string{
		"Host key verification failed.":                                        ResultHostKey,
		"@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @":                 ResultHostKey,
		"backup@nas: Permission denied (publickey,password).":                  ResultLoginRefused,
		"ssh: connect to host nas port 22: Connection refused":                 ResultUnreachable,
		"ssh: Could not resolve hostname nas: Name or service not known":       ResultUnreachable,
		`rsync: [receiver] write failed on "/x": No space left on device (28)`: ResultFull,
		"something else": ResultFailed,
	} {
		if got := rsyncResult(255, text); got != want {
			t.Errorf("%q: %s", text, got)
		}
	}
}

func TestParseRclone(t *testing.T) {
	rec := Record{}
	prog := Progress{}
	rl := newRunLog(&rec, nil)
	for _, l := range []string{
		`{"level":"info","msg":"Copied (new)","object":"a.txt","objectType":"*local.Object","source":"operations/copy.go:368","time":"2026-11-17T21:00:00+01:00"}`,
		`{"level":"info","msg":"Copied (replaced existing)","object":"b.txt","time":"x"}`,
		`{"level":"info","msg":"Deleted","object":"gone.txt","time":"x"}`,
		`{"level":"notice","msg":"Skipped copy as --dry-run is set (size 12)","object":"c.txt","skipped":"copy","time":"x"}`,
		`{"level":"notice","msg":"Skipped delete as --dry-run is set (size 5)","object":"old.txt","time":"x"}`,
		`{"level":"info","msg":"Updated modification time in destination","object":"d.txt","time":"x"}`,
		`{"level":"error","msg":"Failed to copy: 507 Insufficient Storage","object":"big.bin","time":"x"}`,
		`{"level":"notice","msg":"\nTransferred: ...","stats":{"bytes":2048,"totalBytes":4096,"speed":1048576,"eta":3,"errors":1},"time":"x"}`,
		`{"level":"error","msg":"Attempt 1/1 failed with 1 errors and: 507 Insufficient Storage","time":"x"}`,
		`{"level":"info","msg":"There was nothing to transfer","time":"x"}`,
		// with a backup directory: a replaced file is moved away and then copied "new", a deleted one
		// is moved into the backup directory
		`{"level":"info","msg":"Moved (server-side)","object":"v.txt","time":"x"}`,
		`{"level":"info","msg":"Copied (new)","object":"v.txt","time":"x"}`,
		`{"level":"info","msg":"Moved (server-side)","object":"vgone.txt","time":"x"}`,
		`{"level":"info","msg":"Moved into backup dir","object":"vgone.txt","time":"x"}`,
		`not json at all`,
		``,
	} {
		parseRclone(l, true, rl, &rec, &prog)
	}
	parseRclone(`{"level":"info","msg":"Copied (new)","object":"stdout.txt"}`, false, rl, &rec, &prog)
	if rec.Copied != 2 || rec.Replaced != 2 || rec.Deleted != 3 || rec.Errors != 2 || rec.Bytes != 2048 || prog.Percent != 50 || prog.Speed != "1024 KiB/s" || !strings.Contains(rec.Error, "big.bin") {
		t.Errorf("%+v %+v", rec, prog)
	}
	for _, c := range []struct {
		exit int
		text string
		want string
	}{
		{0, "", ResultOK}, {9, "", ResultOK}, {1, "boom", ResultFailed},
		{7, "Fatal error: --max-delete threshold reached", ResultTooManyDeletes},
		{1, "invalid_grant: token expired", ResultLoginExpired},
		{1, "507 Insufficient Storage", ResultFull},
		{5, "dial tcp: lookup x: no such host", ResultUnreachable},
	} {
		if got := rcloneResult(c.exit, c.text); got != c.want {
			t.Errorf("%d %q: %s", c.exit, c.text, got)
		}
	}
}

// A job with a schedule whose last good run is older than twice its interval is overdue; a manual
// job never is, a failed or a dry run does not count as a run, and a job that was just made or
// pointed elsewhere starts anew.
func TestOverdue(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	job := Job{ID: "x", Created: ago(100 * 24 * time.Hour), Changed: ago(100 * 24 * time.Hour)}
	ok := func(d time.Duration) Record { return Record{Result: ResultOK, End: ago(d)} }
	for what, c := range map[string]struct {
		s    Schedule
		hist []Record
		job  Job
		late bool
	}{
		"daily, ran yesterday":                {Schedule{Kind: Daily}, []Record{ok(30 * time.Hour)}, job, false},
		"daily, ran three days ago":           {Schedule{Kind: Daily}, []Record{ok(72 * time.Hour)}, job, true},
		"daily, only failures since":          {Schedule{Kind: Daily}, []Record{{Result: ResultFailed, End: ago(time.Hour)}, ok(72 * time.Hour)}, job, true},
		"daily, only a dry run since":         {Schedule{Kind: Daily}, []Record{{Result: ResultOK, DryRun: true, End: ago(time.Hour)}, ok(72 * time.Hour)}, job, true},
		"daily, done with warnings":           {Schedule{Kind: Daily}, []Record{{Result: ResultWarnings, End: ago(time.Hour)}}, job, false},
		"hourly, ran three hours ago":         {Schedule{Kind: Hourly}, []Record{ok(3 * time.Hour)}, job, true},
		"weekly, ran ten days ago":            {Schedule{Kind: Weekly}, []Record{ok(10 * 24 * time.Hour)}, job, false},
		"custom, ran four days ago":           {Schedule{Kind: Custom}, []Record{ok(4 * 24 * time.Hour)}, job, true},
		"manual, never ran":                   {Schedule{Kind: Manual}, nil, job, false},
		"daily, never ran, old job":           {Schedule{Kind: Daily}, nil, job, true},
		"daily, never ran, made today":        {Schedule{Kind: Daily}, nil, Job{Created: ago(time.Hour), Changed: ago(time.Hour)}, false},
		"daily, pointed elsewhere today":      {Schedule{Kind: Daily}, []Record{ok(72 * time.Hour)}, Job{Created: ago(100 * 24 * time.Hour), Changed: ago(time.Hour)}, false},
		"a mirror job that waits to be tried": {Schedule{Kind: Daily}, nil, Job{Created: ago(100 * 24 * time.Hour), NeedsDryRun: true}, false},
	} {
		if _, late := Overdue(c.job, c.s, c.hist, now); late != c.late {
			t.Errorf("%s: overdue %v", what, late)
		}
	}
}
