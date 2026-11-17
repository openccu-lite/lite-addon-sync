package httpapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/fakesys"
	"github.com/openccu-lite/lite-addon-sync/internal/jobs"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
	"github.com/openccu-lite/lite-addon-sync/internal/sysinfo"
)

// jobsRig is the server with an account, the fake system's timers and a storage of its own.
func jobsRig(t *testing.T) (string, *jobRig, string) {
	t.Helper()
	dir := t.TempDir()
	rec := `{"version":1,"accounts":[{"id":"aone000001","name":"Privat","kind":"onedrive","auth":"oauth","detail":"OneDrive","created":"2026-11-01T10:00:00Z","state":"writable","upload_limit_kbps":0}]}`
	if err := os.WriteFile(filepath.Join(dir, accounts.File), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := accounts.Open(dir, &rclone.Runner{Bin: filepath.Join(dir, "no-rclone"), Config: filepath.Join(dir, "rclone.conf"), Home: dir})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	old := jobs.Root
	jobs.Root = root
	t.Cleanup(func() { jobs.Root = old })
	for _, d := range []string{"export/sub", "etc/config", "addons/history/var"} {
		_ = os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	ts, jr := rigWith(t, dir, store, &fakeFacts{f: sysinfo.Facts{Reachable: true, Provider: true, Encryption: true, Targets: map[string][]sysinfo.Target{}}})
	return ts.URL + "/addons/sync/api/v1", jr, root
}

func jobBody(root, name, mode, schedule string) string {
	return fmt.Sprintf(`{"name":%q,"source":%q,"target":{"kind":"account","id":"aone000001","folder":"exp"},"mode":%q,"schedule":%s}`, name, root+"/export", mode, schedule)
}

func decodeJob(t *testing.T, out string) JobView {
	t.Helper()
	var v JobView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return v
}

// A job is a record here and a timer on the system: made together, the timer's text the Services
// page's own, switched off for "manual only", never rewritten unless the schedule was chosen anew,
// and a timer edited over there replaced only after the user said so.
func TestJobAndItsTimer(t *testing.T) {
	api, jr, root := jobsRig(t)
	code, out := call(t, "POST", api+"/jobs", jobBody(root, "Export", "copy", `{"kind":"daily","time":"03:00"}`), true)
	if code != 201 {
		t.Fatalf("%d %s", code, out)
	}
	j := decodeJob(t, out)
	if j.ID != "export" || j.Schedule.Kind != jobs.Daily || j.Schedule.Time != "03:00" || !j.Timer.Exists || !j.Timer.Enabled || j.Timer.Unit != "local-sync-export.timer" || j.Timer.Next == "" || j.TargetName != "Privat" {
		t.Fatalf("%+v", j)
	}
	tm, ok := jr.sys.Timer("export")
	if !ok || tm.TimerFile != jobs.TimerText("export", jobs.Schedule{Kind: jobs.Daily, Time: "03:00"}) || tm.Description != "Export" || !tm.Enabled || len(tm.Read) != 0 {
		t.Fatalf("the timer on the system: %+v", tm)
	}
	// the state carries it; the account says a job uses it and cannot be removed
	_, out = call(t, "GET", api+"/state", "", false)
	var st StateView
	_ = json.Unmarshal([]byte(out), &st)
	if !st.Copy.Supported || len(st.Copy.Jobs) != 1 || len(st.Accounts[0].Jobs) != 1 || st.Accounts[0].Jobs[0] != "Export" || st.Copy.Root != root {
		t.Fatalf("state: %s", out)
	}
	if code, out := call(t, "DELETE", api+"/accounts/aone000001?force=1", "", true); code != 409 || !strings.Contains(out, "Export") {
		t.Errorf("removing an account a job uses: %d %s", code, out)
	}

	// a person edits the time on the Services page: still a simple schedule, read from the timer
	edited := strings.Replace(tm.TimerFile, "03:00:00", "04:30:00", 1)
	jr.sys.EditTimer("export", func(t *fakesys.FakeTimer) { t.TimerFile = edited })
	// renaming the job with the schedule as the dialog read it writes no timer text
	code, out = call(t, "PUT", api+"/jobs/export", jobBody(root, "Export neu", "copy", `{"kind":"daily","time":"04:30"}`), true)
	if j = decodeJob(t, out); code != 200 || j.ID != "export" || j.Name != "Export neu" || j.Schedule.Time != "04:30" {
		t.Fatalf("%d %s", code, out)
	}
	if tm, _ = jr.sys.Timer("export"); tm.TimerFile != edited || tm.Description != "Export neu" {
		t.Errorf("a rename rewrote the timer: %+v", tm)
	}

	// a custom edit: shown as custom, kept by a save that leaves the schedule alone
	custom := strings.Replace(edited, "*-*-* 04:30:00", "Mon..Fri *-*-* 02:30:00", 1)
	jr.sys.EditTimer("export", func(t *fakesys.FakeTimer) { t.TimerFile = custom })
	_, out = call(t, "GET", api+"/state", "", false)
	_ = json.Unmarshal([]byte(out), &st)
	if s := st.Copy.Jobs[0].Schedule; s.Kind != jobs.Custom || len(s.Calendar) != 1 || s.Calendar[0] != "Mon..Fri *-*-* 02:30:00" {
		t.Fatalf("a custom timer reads as %+v", s)
	}
	if code, out = call(t, "PUT", api+"/jobs/export", jobBody(root, "Export", "copy", `{"kind":"custom"}`), true); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if tm, _ = jr.sys.Timer("export"); tm.TimerFile != custom || !tm.Enabled || tm.Description != "Export" {
		t.Errorf("a save with the custom schedule left alone changed the timer: %+v", tm)
	}
	// choosing a simple schedule over it asks first, and replaces it after the yes
	body := jobBody(root, "Export", "copy", `{"kind":"daily","time":"03:00"}`)
	if code, out = call(t, "PUT", api+"/jobs/export", body, true); code != 409 || !strings.Contains(out, "custom-timer") || !strings.Contains(out, "Mon..Fri") {
		t.Fatalf("replacing a custom timer unasked: %d %s", code, out)
	}
	if tm, _ = jr.sys.Timer("export"); tm.TimerFile != custom {
		t.Error("the refused save changed the timer")
	}
	if code, out = call(t, "PUT", api+"/jobs/export", strings.Replace(body, `{"name"`, `{"replace_custom":true,"name"`, 1), true); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if tm, _ = jr.sys.Timer("export"); tm.TimerFile != jobs.TimerText("export", jobs.Schedule{Kind: jobs.Daily, Time: "03:00"}) {
		t.Errorf("after the yes: %+v", tm)
	}

	// manual only: the timer is switched off and its text stays (also a custom one, without asking)
	jr.sys.EditTimer("export", func(t *fakesys.FakeTimer) { t.TimerFile = custom })
	if code, out = call(t, "PUT", api+"/jobs/export", jobBody(root, "Export", "copy", `{"kind":"manual"}`), true); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if tm, _ = jr.sys.Timer("export"); tm.Enabled || tm.TimerFile != custom {
		t.Errorf("manual only: %+v", tm)
	}
	if j = decodeJob(t, out); j.Schedule.Kind != jobs.Manual || len(j.Schedule.Calendar) != 1 || j.Timer.Enabled || j.Timer.Next != "" {
		t.Errorf("manual only reads as %+v %+v", j.Schedule, j.Timer)
	}

	// run now: the page's request is left for the run, the job's unit is started on the system
	if code, out = call(t, "POST", api+"/jobs/export/run", `{"dry_run":true}`, true); code != 202 || len(jr.ran) != 1 || jr.ran[0] != "export" {
		t.Fatalf("run: %d %s %v", code, out, jr.ran)
	}
	b, err := os.ReadFile(filepath.Join(jr.runDir, "request-export.json"))
	if err != nil || !strings.Contains(string(b), `"dry_run":true`) {
		t.Errorf("the request: %s %v", b, err)
	}
	// the timer deleted on the Services page: the job reads as manual, and a run makes it again
	jr.sys.DropTimer("export")
	_, out = call(t, "GET", api+"/state", "", false)
	_ = json.Unmarshal([]byte(out), &st)
	if jv := st.Copy.Jobs[0]; jv.Timer.Exists || jv.Schedule.Kind != jobs.Manual {
		t.Errorf("a deleted timer reads as %+v", jv)
	}
	if code, _ = call(t, "POST", api+"/jobs/export/run", `{}`, true); code != 202 {
		t.Fatalf("run after the delete: %d", code)
	}
	if tm, ok = jr.sys.Timer("export"); !ok || tm.Enabled {
		t.Errorf("the timer made again: %+v %v", tm, ok)
	}

	// deleting the job takes the timer and the history
	_ = os.MkdirAll(jobs.HistoryDir(jr.addonDir, "export"), 0o700)
	if code, _ = call(t, "DELETE", api+"/jobs/export", "", true); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if _, ok := jr.sys.Timer("export"); ok {
		t.Error("the timer of a deleted job stays")
	}
	if _, err := os.Stat(jobs.HistoryDir(jr.addonDir, "export")); err == nil {
		t.Error("the history of a deleted job stays")
	}
	if code, _ = call(t, "DELETE", api+"/jobs/export", "", true); code != 404 {
		t.Errorf("delete twice: %d", code)
	}
}

// A mirror job deletes on its target: it does not run, and its timer stays off, until a dry run
// of it has been looked at.
func TestMirrorWaitsForItsDryRun(t *testing.T) {
	api, jr, root := jobsRig(t)
	code, out := call(t, "POST", api+"/jobs", jobBody(root, "Spiegel", "mirror", `{"kind":"hourly"}`), true)
	j := decodeJob(t, out)
	if code != 201 || !j.NeedsDryRun {
		t.Fatalf("%d %s", code, out)
	}
	if tm, _ := jr.sys.Timer("spiegel"); tm.Enabled || !strings.Contains(tm.TimerFile, "OnCalendar=hourly") {
		t.Fatalf("the timer of an untried mirror job: %+v", tm)
	}
	if code, out = call(t, "POST", api+"/jobs/spiegel/run", `{}`, true); code != 409 || !strings.Contains(out, "dry-run-first") || len(jr.ran) != 0 {
		t.Fatalf("a real run before the dry run: %d %s", code, out)
	}
	if code, _ = call(t, "POST", api+"/jobs/spiegel/run", `{"dry_run":true}`, true); code != 202 {
		t.Fatalf("the dry run: %d", code)
	}
	// the run's record, as the runner writes it
	hist := jobs.HistoryDir(jr.addonDir, "spiegel")
	_ = os.MkdirAll(hist, 0o700)
	write := func(id string, rec jobs.Record) {
		rec.ID, rec.Job = id, "spiegel"
		b, _ := json.Marshal(rec)
		_ = os.WriteFile(filepath.Join(hist, id+".json"), b, 0o600)
	}
	write("20200101T000000Z", jobs.Record{Start: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), DryRun: true, Result: jobs.ResultOK})
	write("20991231T235958Z", jobs.Record{Start: time.Now().Add(time.Minute), DryRun: true, Result: jobs.ResultFailed})
	write("20991231T235959Z", jobs.Record{Start: time.Now().Add(time.Minute), DryRun: true, Result: jobs.ResultOK, Deleted: 3})
	// an old dry run, a failed one, and one that is only fetched release nothing
	for _, u := range []string{"/runs/spiegel/20200101T000000Z?seen=1", "/runs/spiegel/20991231T235958Z?seen=1", "/runs/spiegel/20991231T235959Z"} {
		if code, out = call(t, "GET", api+u, "", false); code != 200 || strings.Contains(out, `"released":true`) {
			t.Errorf("%s: %d %s", u, code, out)
		}
	}
	if tm, _ := jr.sys.Timer("spiegel"); tm.Enabled {
		t.Fatal("the timer was switched on unseen")
	}
	if code, out = call(t, "GET", api+"/runs/spiegel/20991231T235959Z?seen=1", "", false); code != 200 || !strings.Contains(out, `"released":true`) || !strings.Contains(out, `"job_name":"Spiegel"`) {
		t.Fatalf("looking at the dry run: %d %s", code, out)
	}
	if tm, _ := jr.sys.Timer("spiegel"); !tm.Enabled {
		t.Error("the timer stays off after the dry run was looked at")
	}
	if code, _ = call(t, "POST", api+"/jobs/spiegel/run", `{}`, true); code != 202 {
		t.Errorf("a real run after the dry run: %d", code)
	}
	// the history lists the runs, newest first; a run that is not there, a path as an id
	_, out = call(t, "GET", api+"/runs?job=spiegel", "", false)
	var h struct {
		Runs []RunView `json:"runs"`
	}
	if _ = json.Unmarshal([]byte(out), &h); len(h.Runs) != 3 || h.Runs[2].ID != "20200101T000000Z" {
		t.Errorf("history: %s", out)
	}
	for _, u := range []string{"/runs/spiegel/20000101T000000Z", "/runs/spiegel/..%2F..%2Fjobs", "/runs/nope/20991231T235959Z", "/runs/spiegel/20991231T235959Z%00"} {
		if code, _ = call(t, "GET", api+u, "", false); code != 404 && code != 400 {
			t.Errorf("%s: %d", u, code)
		}
	}
	if code, out = call(t, "GET", api+"/runs/spiegel/20991231T235959Z/log", "", false); code != 200 || !strings.Contains(out, `"lines":[]`) {
		t.Errorf("a run without a file list: %d %s", code, out)
	}
	// changing where it mirrors to asks for a new dry run and switches the timer off again
	body := strings.Replace(jobBody(root, "Spiegel", "mirror", `{"kind":"hourly"}`), `"folder":"exp"`, `"folder":"other"`, 1)
	if code, out = call(t, "PUT", api+"/jobs/spiegel", body, true); code != 200 || !decodeJob(t, out).NeedsDryRun {
		t.Fatalf("%d %s", code, out)
	}
	if tm, _ := jr.sys.Timer("spiegel"); tm.Enabled {
		t.Error("the timer of a changed mirror job stays on")
	}
}

func TestJobRefusalsSourcesAndServers(t *testing.T) {
	api, jr, root := jobsRig(t)
	for what, body := range map[string]string{
		"the configuration as the source": strings.Replace(jobBody(root, "x", "copy", `{"kind":"manual"}`), "/export", "/etc/config", 1),
		"an account that is not there":    strings.Replace(jobBody(root, "x", "copy", `{"kind":"manual"}`), "aone000001", "zzzz000009", 1),
		"a read grant the system lacks":   strings.Replace(jobBody(root, "x", "copy", `{"kind":"manual"}`), `{"name"`, `{"read_addon":"history","name"`, 1),
		"a folder that is an option":      strings.Replace(jobBody(root, "x", "copy", `{"kind":"manual"}`), `"folder":"exp"`, `"folder":"--config=/x"`, 1),
	} {
		if code, out := call(t, "POST", api+"/jobs", body, true); code != 422 {
			t.Errorf("%s: %d %s", what, code, out)
		}
	}
	if len(jr.jobs.List()) != 0 {
		t.Fatalf("a refused job was kept: %+v", jr.jobs.List())
	}
	// with the system's word the read grant goes into the timer
	jr.sys.Set(func(s *fakesys.Server) { s.Readable = []string{"history"} })
	body := strings.Replace(strings.Replace(jobBody(root, "Verlauf", "copy", `{"kind":"manual"}`), `{"name"`, `{"read_addon":"history","name"`, 1), "/export", "/addons/history/var", 1)
	if code, out := call(t, "POST", api+"/jobs", body, true); code != 201 {
		t.Fatalf("%d %s", code, out)
	}
	if tm, _ := jr.sys.Timer("verlauf"); len(tm.Read) != 1 || tm.Read[0] != "history" || tm.Enabled {
		t.Errorf("the read grant: %+v", tm)
	}
	// a system that refuses the timer leaves no job behind
	jr.sys.Set(func(s *fakesys.Server) { s.Readable = nil })
	if code, out := call(t, "PUT", api+"/jobs/verlauf", body, true); code != 422 {
		t.Errorf("a grant the system took back: %d %s", code, out)
	}
	// an older system: the tab says so, and no job is made
	jr.sys.Set(func(s *fakesys.Server) { s.Timers = false })
	_, out := call(t, "GET", api+"/state", "", false)
	var st StateView
	_ = json.Unmarshal([]byte(out), &st)
	if st.Copy.Supported || st.Copy.Problem == "" {
		t.Errorf("an older system: %s", out)
	}
	if code, out := call(t, "POST", api+"/jobs", jobBody(root, "y", "copy", `{"kind":"manual"}`), true); code != 409 || !strings.Contains(out, "unsupported") {
		t.Errorf("a job on an older system: %d %s", code, out)
	}
	jr.sys.Set(func(s *fakesys.Server) { s.Timers = true })

	// the picker stays inside the storage and marks what is never copied
	code, out := call(t, "GET", api+"/sources", "", false)
	var l jobs.Listing
	if _ = json.Unmarshal([]byte(out), &l); code != 200 || l.Path != root || len(l.Folders) != 3 {
		t.Fatalf("%d %s", code, out)
	}
	for _, f := range l.Folders {
		if (f.Name == "etc") != (f.State == jobs.FolderNever) {
			t.Errorf("picker: %+v", f)
		}
	}
	for _, p := range []string{"/etc", "/", "..", root + "/../.."} {
		code, out := call(t, "GET", api+"/sources?path="+p, "", false)
		_ = json.Unmarshal([]byte(out), &l)
		if code == 200 && !strings.HasPrefix(l.Path, root) {
			t.Errorf("the picker left the storage for %q: %s", p, out)
		}
	}

	// SSH servers: made with a key of their own, never a private key in an answer, in use = kept
	code, out = call(t, "POST", api+"/servers", `{"name":"NAS","host":"nas.example.org","user":"backup","path":"/volume1/ccu"}`, true)
	var sv ServerView
	if _ = json.Unmarshal([]byte(out), &sv); code != 201 || !strings.HasPrefix(sv.PublicKey, "restrict ssh-ed25519 ") || strings.Contains(out, "PRIVATE") || sv.Port != 22 {
		t.Fatalf("%d %s", code, out)
	}
	if code, out = call(t, "POST", api+"/servers", `{"name":"x","host":"-oProxyCommand=id","user":"u","path":"/p"}`, true); code != 422 {
		t.Errorf("a host that is an option: %d %s", code, out)
	}
	if code, out = call(t, "PUT", api+"/servers/"+sv.ID+"/hostkey", `{"fingerprints":["SHA256:x"]}`, true); code != 422 {
		t.Errorf("trusting a host key nobody read: %d %s", code, out)
	}
	if code, out = call(t, "POST", api+"/servers/"+sv.ID+"/test", "", true); code != 200 || !strings.Contains(out, `"state":"hostkey-unknown"`) {
		t.Errorf("a test without a host key: %d %s", code, out)
	}
	body = fmt.Sprintf(`{"name":"Zum NAS","source":%q,"target":{"kind":"server","id":%q,"folder":"ccu"},"schedule":{"kind":"manual"}}`, root+"/export", sv.ID)
	if code, out = call(t, "POST", api+"/jobs", body, true); code != 201 || decodeJob(t, out).TargetName != "NAS" {
		t.Fatalf("%d %s", code, out)
	}
	if code, out = call(t, "DELETE", api+"/servers/"+sv.ID, "", true); code != 409 || !strings.Contains(out, "Zum NAS") {
		t.Errorf("removing a server a job uses: %d %s", code, out)
	}
	if code, _ = call(t, "DELETE", api+"/jobs/zum-nas", "", true); code != 204 {
		t.Fatal(code)
	}
	if code, _ = call(t, "DELETE", api+"/servers/"+sv.ID, "", true); code != 204 {
		t.Errorf("removing an unused server: %d", code)
	}
	// every write needs the request header
	if code, _ = call(t, "POST", api+"/jobs", jobBody(root, "z", "copy", `{"kind":"manual"}`), false); code != 403 {
		t.Errorf("a job without the request header: %d", code)
	}
}
