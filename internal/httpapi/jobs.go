package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hobbyquaker/ccu-addon-kit/go/addonkit"

	"github.com/openccu-lite/lite-addon-sync/internal/jobs"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
	"github.com/openccu-lite/lite-addon-sync/internal/servers"
	"github.com/openccu-lite/lite-addon-sync/internal/systimers"
)

// Copy jobs, SSH servers, the folder picker and the history (docs/api.md). A job's schedule is a
// timer on the system: the routes here keep the job's record and the timer in step, and never run
// a job in the daemon - "run" starts the job's unit on the system.

// TimersAPI is the system's side of the jobs' timers (systimers.Client; a fake in the tests).
type TimersAPI interface {
	List(ctx context.Context) ([]systimers.Timer, []string, error)
	Put(ctx context.Context, name string, in systimers.Input) (systimers.Timer, error)
	Delete(ctx context.Context, name string) error
	Run(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	SetEnabled(ctx context.Context, name string, on bool) error
	Times(ctx context.Context) map[string]systimers.Times
}

// ServerView is an SSH server with the copy jobs that use it.
type ServerView struct {
	servers.Server
	Jobs []string `json:"jobs"`
}

// TimerView is what the system says about a job's timer.
type TimerView struct {
	Unit string `json:"unit"`
	// Exists false: the timer was deleted on the Services page (the job is then manual; a run or a
	// new schedule makes it again).
	Exists  bool   `json:"exists"`
	Enabled bool   `json:"enabled"`
	Next    string `json:"next,omitempty"`
	Last    string `json:"last,omitempty"`
}

// JobView is a copy job as the page shows it.
type JobView struct {
	jobs.Job
	// Schedule is read from the timer on the system, so an edit on the Services page shows here.
	Schedule   jobs.Schedule `json:"schedule"`
	Timer      TimerView     `json:"timer"`
	TargetName string        `json:"target_name"`
	// TargetGone: the account or server the job copies to no longer exists.
	TargetGone bool           `json:"target_gone,omitempty"`
	Last       *jobs.Record   `json:"last,omitempty"`
	Running    *jobs.Progress `json:"running,omitempty"`
}

// JobsView is the part of the state the Copy jobs tab shows.
type JobsView struct {
	// Supported: the system offers timers to this addon; Problem why not.
	Supported bool   `json:"supported"`
	Problem   string `json:"problem,omitempty"`
	// Readable are the addons whose data a job may read with their group.
	Readable []string  `json:"readable"`
	Jobs     []JobView `json:"jobs"`
	// Never are the fixed lists the picker and the dialog show.
	NameFilters []string `json:"name_filters"`
	Fixed       []string `json:"fixed"`
	Root        string   `json:"root"`
}

func (s *server) registerJobs(api *http.ServeMux) {
	handle(api, "POST /api/v1/servers", s.serverCreate)
	handle(api, "PUT /api/v1/servers/{id}", s.serverUpdate)
	handle(api, "DELETE /api/v1/servers/{id}", s.serverDelete)
	handle(api, "POST /api/v1/servers/{id}/key", s.serverNewKey)
	handle(api, "POST /api/v1/servers/{id}/hostkey", s.serverScan)
	handle(api, "PUT /api/v1/servers/{id}/hostkey", s.serverTrust)
	handle(api, "POST /api/v1/servers/{id}/test", s.serverTest)
	handle(api, "GET /api/v1/sources", s.sources)
	handle(api, "POST /api/v1/jobs", s.jobCreate)
	handle(api, "PUT /api/v1/jobs/{id}", s.jobUpdate)
	handle(api, "DELETE /api/v1/jobs/{id}", s.jobDelete)
	handle(api, "POST /api/v1/jobs/{id}/run", s.jobRun)
	handle(api, "POST /api/v1/jobs/{id}/stop", s.jobStop)
	handle(api, "GET /api/v1/runs", s.runs)
	handle(api, "GET /api/v1/runs/{job}/{run}", s.run)
	handle(api, "GET /api/v1/runs/{job}/{run}/log", s.runLog)
}

func (s *server) writeJobErr(w http.ResponseWriter, err error) {
	var jv *jobs.ValidationError
	var sv *servers.ValidationError
	var se *systimers.Error
	switch {
	case errors.As(err, &jv), errors.As(err, &sv):
		addonkit.WriteError(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case errors.Is(err, jobs.ErrNotFound), errors.Is(err, servers.ErrNotFound):
		addonkit.WriteError(w, http.StatusNotFound, "not-found", err.Error())
	case errors.Is(err, systimers.ErrUnsupported):
		addonkit.WriteError(w, http.StatusConflict, "unsupported", "this system does not offer timers to addons: copy jobs need a newer openccu-lite")
	case errors.As(err, &se):
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "system", "message": "the system refused the job's timer: " + se.Error(), "code": se.Code})
	default:
		addonkit.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *server) serverView(v servers.Server) ServerView {
	names := []string{}
	for _, j := range s.o.Jobs.UsesTarget(jobs.TargetServer, v.ID) {
		names = append(names, j.Name)
	}
	return ServerView{Server: v, Jobs: names}
}

func (s *server) serverCreate(w http.ResponseWriter, r *http.Request) {
	var sp servers.Spec
	if !decode(w, r, &sp) {
		return
	}
	v, err := s.o.Servers.Create(r.Context(), sp)
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.serverView(v))
}

func (s *server) serverUpdate(w http.ResponseWriter, r *http.Request) {
	var sp servers.Spec
	if !decode(w, r, &sp) {
		return
	}
	v, err := s.o.Servers.Update(r.PathValue("id"), sp)
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	writeJSON(w, 200, s.serverView(v))
}

func (s *server) serverDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if used := s.o.Jobs.UsesTarget(jobs.TargetServer, id); len(used) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "in-use", "message": "a copy job copies to this server: delete the job first", "jobs": jobNames(used)})
		return
	}
	if err := s.o.Servers.Delete(id); err != nil {
		s.writeJobErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func jobNames(list []jobs.Job) []string {
	out := []string{}
	for _, j := range list {
		out = append(out, j.Name)
	}
	return out
}

func (s *server) serverNewKey(w http.ResponseWriter, r *http.Request) {
	v, err := s.o.Servers.NewKey(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	writeJSON(w, 200, s.serverView(v))
}

func (s *server) serverScan(w http.ResponseWriter, r *http.Request) {
	v, err := s.o.Servers.Scan(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	writeJSON(w, 200, s.serverView(v))
}

func (s *server) serverTrust(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Fingerprints []string `json:"fingerprints"`
	}
	if !decode(w, r, &in) {
		return
	}
	v, err := s.o.Servers.Trust(r.PathValue("id"), in.Fingerprints)
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	s.o.Log("the host key of the SSH server %q (%s) was confirmed: %s", v.Name, v.Host, fingerprints(v.HostKeys))
	writeJSON(w, 200, s.serverView(v))
}

func fingerprints(keys []servers.HostKey) string {
	var out []string
	for _, k := range keys {
		out = append(out, k.Type+" "+k.Fingerprint)
	}
	return strings.Join(out, ", ")
}

func (s *server) serverTest(w http.ResponseWriter, r *http.Request) {
	v, err := s.o.Servers.Test(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	writeJSON(w, 200, s.serverView(v))
}

// timerState is what the system says about the jobs' timers, read once per request.
type timerState struct {
	err      error
	timers   map[string]systimers.Timer
	readable []string
	times    map[string]systimers.Times
}

func (s *server) timerState(ctx context.Context, withTimes bool) timerState {
	st := timerState{timers: map[string]systimers.Timer{}, readable: []string{}, times: map[string]systimers.Times{}}
	list, readable, err := s.o.Timers.List(ctx)
	if err != nil {
		st.err = err
		return st
	}
	for _, t := range list {
		st.timers[t.Name] = t
	}
	if readable != nil {
		st.readable = readable
	}
	if withTimes && len(list) > 0 {
		st.times = s.o.Timers.Times(ctx)
	}
	return st
}

func (s *server) targetName(j jobs.Job) (string, bool) {
	switch j.Target.Kind {
	case jobs.TargetAccount:
		if a, ok := s.o.Accounts.Get(j.Target.ID); ok {
			return a.Name, true
		}
	case jobs.TargetServer:
		if v, ok := s.o.Servers.Get(j.Target.ID); ok {
			return v.Name, true
		}
	}
	return "", false
}

func (s *server) jobView(j jobs.Job, st timerState) JobView {
	v := JobView{Job: j, Timer: TimerView{Unit: "local-" + s.o.AddonID + "-" + j.ID + ".timer"}}
	if t, ok := st.timers[j.ID]; ok {
		v.Timer.Exists, v.Timer.Enabled = true, t.Enabled
		v.Schedule = jobs.ReadTimer(j.ID, t.TimerFile, t.Enabled)
		tm := st.times[t.Unit]
		v.Timer.Last = tm.Last
		if t.Enabled {
			v.Timer.Next = tm.Next
		}
	} else {
		v.Schedule = jobs.Schedule{Kind: jobs.Manual}
	}
	name, ok := s.targetName(j)
	v.TargetName, v.TargetGone = name, !ok
	if hist := jobs.History(s.o.AddonDir, j.ID); len(hist) > 0 {
		v.Last = &hist[0]
	}
	if p, ok := jobs.ReadProgress(s.o.RunDir, j.ID); ok {
		v.Running = &p
	}
	if v.Excludes == nil {
		v.Excludes = []string{}
	}
	return v
}

func (s *server) jobsView(ctx context.Context) JobsView {
	st := s.timerState(ctx, true)
	v := JobsView{Supported: st.err == nil, Readable: st.readable, Jobs: []JobView{}, NameFilters: jobs.NameFilterPatterns, Fixed: jobs.FixedPatterns, Root: jobs.Root}
	if st.err != nil {
		v.Problem = st.err.Error()
	}
	for _, j := range s.o.Jobs.List() {
		v.Jobs = append(v.Jobs, s.jobView(j, st))
	}
	return v
}

func (s *server) sources(w http.ResponseWriter, r *http.Request) {
	st := s.timerState(r.Context(), false)
	l, err := jobs.List(r.URL.Query().Get("path"), func(addon string) bool {
		for _, id := range st.readable {
			if id == addon {
				return true
			}
		}
		return false
	})
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	writeJSON(w, 200, l)
}

// jobSpec is a job as the dialog sends it.
type jobSpec struct {
	jobs.Spec
	// ReplaceCustom: the user confirmed that a timer edited on the Services page is replaced.
	ReplaceCustom bool `json:"replace_custom"`
}

// checkTarget makes sure the job's target exists and, for a read grant, that the system allows it.
func (s *server) checkJob(sp jobs.Spec, st timerState) error {
	switch sp.Target.Kind {
	case jobs.TargetAccount:
		if _, ok := s.o.Accounts.Get(sp.Target.ID); !ok {
			return &jobs.ValidationError{Msg: "target: this account no longer exists"}
		}
	case jobs.TargetServer:
		if _, ok := s.o.Servers.Get(sp.Target.ID); !ok {
			return &jobs.ValidationError{Msg: "target: this SSH server no longer exists"}
		}
	}
	if sp.ReadAddon != "" {
		ok := false
		for _, id := range st.readable {
			ok = ok || id == sp.ReadAddon
		}
		if !ok {
			return &jobs.ValidationError{Msg: "read_addon: the system does not let a job read the data of " + sp.ReadAddon}
		}
	}
	return nil
}

func readList(j jobs.Job) *[]string {
	list := []string{}
	if j.ReadAddon != "" {
		list = append(list, j.ReadAddon)
	}
	return &list
}

func (s *server) jobCreate(w http.ResponseWriter, r *http.Request) {
	var in jobSpec
	if !decode(w, r, &in) {
		return
	}
	st := s.timerState(r.Context(), false)
	if st.err != nil {
		s.writeJobErr(w, st.err)
		return
	}
	if in.Schedule.Kind == jobs.Custom {
		in.Schedule = jobs.Schedule{Kind: jobs.Manual}
	}
	if err := s.checkJob(in.Spec, st); err != nil {
		s.writeJobErr(w, err)
		return
	}
	j, err := s.o.Jobs.Create(in.Spec)
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	// an encrypted job gets a crypt remote of its own over its folder, with a password made here
	// and answered once: it is never read back
	password := ""
	if j.Encrypt {
		acc, _ := s.o.Accounts.Get(j.Target.ID)
		password = newPassword()
		if err := s.o.Accounts.Rclone.ConfigCreate(r.Context(), jobs.CryptRemote(j.ID), "crypt", map[string]string{
			"remote": rclone.Path(acc.Remote(), acc.Root, j.Target.Folder), "password": password, "filename_encryption": "standard", "directory_name_encryption": "true"}); err != nil {
			_ = s.o.Jobs.Delete(j.ID)
			writeErr(w, err)
			return
		}
	}
	text := jobs.TimerText(j.ID, j.Schedule)
	enabled := j.Schedule.Kind != jobs.Manual && !j.NeedsDryRun
	if _, err := s.o.Timers.Put(r.Context(), j.ID, systimers.Input{Timer: text, Description: &j.Name, Read: readList(j), Enabled: &enabled}); err != nil {
		_ = s.o.Jobs.Delete(j.ID)
		if j.Encrypt {
			_ = s.o.Accounts.Rclone.ConfigDelete(r.Context(), jobs.CryptRemote(j.ID))
		}
		s.writeJobErr(w, err)
		return
	}
	j, _ = s.o.Jobs.Set(j.ID, func(j *jobs.Job) { j.TimerHash = jobs.Hash(text) })
	s.o.Log("the copy job %q was made: %s → %s %s (%s)", j.Name, j.Source, j.Target.Kind, j.Target.Folder, j.Mode)
	v := s.jobView(j, s.timerState(r.Context(), true))
	if password != "" {
		writeJSON(w, http.StatusCreated, struct {
			JobView
			CryptPassword string `json:"crypt_password"`
		}{v, password})
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// newPassword makes the password of an encrypted job: 32 characters that are easy to write down
// (no 0/O, 1/l/I), about 160 bits.
func newPassword() string {
	const letters = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

func (s *server) jobUpdate(w http.ResponseWriter, r *http.Request) {
	var in jobSpec
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	old, ok := s.o.Jobs.Get(id)
	if !ok {
		s.writeJobErr(w, jobs.ErrNotFound)
		return
	}
	st := s.timerState(r.Context(), false)
	if st.err != nil {
		s.writeJobErr(w, st.err)
		return
	}
	if err := s.checkJob(in.Spec, st); err != nil {
		s.writeJobErr(w, err)
		return
	}
	cur, exists := st.timers[id]
	// what the timer on the system is now, and whether a person edited it on the Services page
	var actual, text jobs.Schedule
	if exists {
		actual = jobs.ReadTimer(id, cur.TimerFile, cur.Enabled)
		text = jobs.ReadTimer(id, cur.TimerFile, true)
	}
	want := in.Schedule
	simple := want.Kind == jobs.Hourly || want.Kind == jobs.Daily || want.Kind == jobs.Weekly
	if want.Kind == jobs.Custom && !exists {
		want = jobs.Schedule{Kind: jobs.Manual}
	}
	if exists && simple && text.Kind == jobs.Custom && !in.ReplaceCustom {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "custom-timer", "message": "the timer was edited on the Services page: confirm that it is replaced", "calendar": text.Calendar})
		return
	}
	if want.Kind == jobs.Custom {
		// the dialog left the edited timer alone: the record keeps what it had
		in.Spec.Schedule = old.Schedule
	}
	j, err := s.o.Jobs.Update(id, in.Spec)
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	// an encrypted job that was pointed at another folder or account: its crypt remote follows,
	// the password stays
	if j.Encrypt && j.Target != old.Target {
		acc, _ := s.o.Accounts.Get(j.Target.ID)
		if err := s.o.Accounts.Rclone.ConfigUpdate(r.Context(), jobs.CryptRemote(id), map[string]string{"remote": rclone.Path(acc.Remote(), acc.Root, j.Target.Folder)}); err != nil {
			writeErr(w, err)
			return
		}
	}
	put := systimers.Input{Description: &j.Name, Read: readList(j)}
	// the timer's text is written only when the schedule itself was chosen anew (or the timer is gone)
	switch {
	case !exists:
		put.Timer = jobs.TimerText(id, j.Schedule)
	case simple && (actual.Kind != want.Kind || actual.Time != want.Time || actual.Weekday != want.Weekday || text.Kind == jobs.Custom):
		if t := jobs.TimerText(id, want); t != cur.TimerFile {
			put.Timer = t
		}
	}
	var enabled *bool
	switch {
	case j.NeedsDryRun, want.Kind == jobs.Manual:
		off := false
		enabled = &off
	case simple:
		on := true
		enabled = &on
	}
	put.Enabled = enabled
	if _, err := s.o.Timers.Put(r.Context(), id, put); err != nil {
		s.writeJobErr(w, err)
		return
	}
	if put.Timer != "" {
		j, _ = s.o.Jobs.Set(id, func(j *jobs.Job) { j.TimerHash = jobs.Hash(put.Timer) })
	}
	writeJSON(w, 200, s.jobView(j, s.timerState(r.Context(), true)))
}

func (s *server) jobDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, ok := s.o.Jobs.Get(id)
	if !ok {
		s.writeJobErr(w, jobs.ErrNotFound)
		return
	}
	if err := s.o.Timers.Delete(r.Context(), id); err != nil && !errors.Is(err, systimers.ErrUnsupported) {
		s.writeJobErr(w, err)
		return
	}
	if err := s.o.Jobs.Delete(id); err != nil {
		s.writeJobErr(w, err)
		return
	}
	jobs.DeleteHistory(s.o.AddonDir, id)
	if j.Encrypt {
		// the password goes with the job: who wants to read the copies later has written it down
		_ = s.o.Accounts.Rclone.ConfigDelete(r.Context(), jobs.CryptRemote(id))
	}
	s.o.Log("the copy job %q was deleted (nothing on its target was touched)", j.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) jobRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DryRun bool `json:"dry_run"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	j, ok := s.o.Jobs.Get(id)
	if !ok {
		s.writeJobErr(w, jobs.ErrNotFound)
		return
	}
	if j.NeedsDryRun && !in.DryRun {
		addonkit.WriteError(w, http.StatusConflict, "dry-run-first", "a mirror job deletes on its target: try it first and look at what it would do")
		return
	}
	if _, running := jobs.ReadProgress(s.o.RunDir, id); running {
		addonkit.WriteError(w, http.StatusConflict, "running", "this job is running")
		return
	}
	st := s.timerState(r.Context(), false)
	if st.err != nil {
		s.writeJobErr(w, st.err)
		return
	}
	if _, exists := st.timers[id]; !exists {
		// deleted on the Services page: made again, switched off
		off := false
		text := jobs.TimerText(id, jobs.Schedule{Kind: jobs.Manual})
		if _, err := s.o.Timers.Put(r.Context(), id, systimers.Input{Timer: text, Description: &j.Name, Read: readList(j), Enabled: &off}); err != nil {
			s.writeJobErr(w, err)
			return
		}
	}
	if err := jobs.WriteRequest(s.o.RunDir, id, jobs.Request{DryRun: in.DryRun, Trigger: jobs.TriggerPage, At: time.Now().UTC()}); err != nil {
		s.writeJobErr(w, err)
		return
	}
	if err := s.o.Timers.Run(r.Context(), id); err != nil {
		s.writeJobErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "dry_run": in.DryRun})
}

func (s *server) jobStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.o.Jobs.Get(id); !ok {
		s.writeJobErr(w, jobs.ErrNotFound)
		return
	}
	if err := s.o.Timers.Stop(r.Context(), id); err != nil {
		s.writeJobErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// RunView is a run with its job's name.
type RunView struct {
	jobs.Record
	JobName string `json:"job_name"`
}

func (s *server) runs(w http.ResponseWriter, r *http.Request) {
	only := r.URL.Query().Get("job")
	out := []RunView{}
	for _, j := range s.o.Jobs.List() {
		if only != "" && j.ID != only {
			continue
		}
		for _, rec := range jobs.History(s.o.AddonDir, j.ID) {
			out = append(out, RunView{Record: rec, JobName: j.Name})
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].Start.After(out[k].Start) })
	if len(out) > 300 {
		out = out[:300]
	}
	writeJSON(w, 200, map[string]any{"runs": out})
}

// run is GET /api/v1/runs/{job}/{run}. With ?seen=1 the page says its user looked at the run: a
// mirror job's dry run that was looked at lets the job run for real, and switches its timer on
// when the job has a schedule.
func (s *server) run(w http.ResponseWriter, r *http.Request) {
	jobID, runID := r.PathValue("job"), r.PathValue("run")
	rec, err := jobs.ReadRecord(s.o.AddonDir, jobID, runID)
	if err != nil {
		s.writeJobErr(w, jobs.ErrNotFound)
		return
	}
	j, ok := s.o.Jobs.Get(jobID)
	released := false
	if ok && r.URL.Query().Get("seen") == "1" && j.NeedsDryRun && rec.DryRun && rec.OK() && rec.Start.After(j.Changed.Add(-time.Second)) {
		if j, err = s.o.Jobs.Set(jobID, func(j *jobs.Job) { j.NeedsDryRun = false }); err == nil {
			released = true
			if k := j.Schedule.Kind; k == jobs.Hourly || k == jobs.Daily || k == jobs.Weekly {
				if err := s.o.Timers.SetEnabled(r.Context(), jobID, true); err != nil {
					s.o.Log("the timer of the copy job %q could not be switched on: %v", j.Name, err)
				}
			}
			s.o.Log("the dry run of the mirror job %q was looked at: the job may run", j.Name)
		}
	}
	writeJSON(w, 200, map[string]any{"run": RunView{Record: rec, JobName: j.Name}, "released": released})
}

func (s *server) runLog(w http.ResponseWriter, r *http.Request) {
	lines, more, err := jobs.ReadLog(s.o.AddonDir, r.PathValue("job"), r.PathValue("run"), 5000)
	if err != nil {
		s.writeJobErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"lines": lines, "more": more})
}
