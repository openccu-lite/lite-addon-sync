package jobs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
	"github.com/openccu-lite/lite-addon-sync/internal/servers"
)

// A run is `lite-sync run <id>`, started by the job's unit on the system (by its timer, by "Run
// now" on the addon's page or on the Services page). It takes the addon's run lock - one job at a
// time - builds the tool's arguments as a list, runs rsync or rclone as its child, reads what the
// tool says while it works, and writes the run's record and file list. It exits 0 only when the
// run succeeded, so a failed run is a failed unit on the Services page.

// The results of a run.
const (
	ResultOK              = "ok"
	ResultWarnings        = "warnings" // done, but some files could not be copied
	ResultFailed          = "failed"
	ResultStopped         = "stopped"
	ResultTooManyDeletes  = "too-many-deletions"
	ResultUnreachable     = "target-unreachable"
	ResultLoginExpired    = "login-expired"
	ResultHostKey         = "host-key-changed"
	ResultFull            = "quota-full"
	ResultSourceMissing   = "source-missing"
	ResultTargetMissing   = "target-missing"
	ResultBusy            = "busy"
	ResultNeedsDryRun     = "dry-run-first"
	maxLogBytes           = 5 << 20
	keepRuns              = 50
	keepDays              = 90
	lockWait              = 30 * time.Minute
	progressEvery         = 2 * time.Second
	recordSuffix          = ".json"
	logSuffix             = ".log.gz"
	requestPrefix         = "request-"
	progressPrefix        = "progress-"
	runTimeLayout         = "20060102T150405Z"
	triggerTimer          = "timer"
	TriggerPage           = "page"
	defaultRunDir         = "/run/addon-sync"
	maxRecordErrorLen     = 400
	sourceEmptyMirrorText = "the source is empty: a mirror run would delete everything on the target"
)

// Record is what is kept of one run.
type Record struct {
	ID    string    `json:"id"`
	Job   string    `json:"job"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Trigger is "timer" (the timer, or a start on the Services page) or "page".
	Trigger string `json:"trigger"`
	DryRun  bool   `json:"dry_run"`
	// Tool is "rsync" or "rclone".
	Tool     string `json:"tool"`
	Result   string `json:"result"`
	Copied   int    `json:"copied"`
	Replaced int    `json:"replaced"`
	Deleted  int    `json:"deleted"`
	Errors   int    `json:"errors"`
	Bytes    int64  `json:"bytes"`
	Exit     int    `json:"exit"`
	// Error is the first line that says what went wrong.
	Error string `json:"error,omitempty"`
	// LogCut: the file list was cut at its size limit.
	LogCut bool `json:"log_cut,omitempty"`
}

// OK says whether the run did what it was to do.
func (r Record) OK() bool { return r.Result == ResultOK || r.Result == ResultWarnings }

// Progress is a running job's figures, written every few seconds.
type Progress struct {
	Job     string    `json:"job"`
	Run     string    `json:"run"`
	Started time.Time `json:"started"`
	DryRun  bool      `json:"dry_run"`
	Waiting bool      `json:"waiting,omitempty"` // for another job to end
	Files   int       `json:"files"`
	Bytes   int64     `json:"bytes"`
	Percent int       `json:"percent"`
	Speed   string    `json:"speed,omitempty"`
	ETA     string    `json:"eta,omitempty"`
	Updated time.Time `json:"updated"`
	PID     int       `json:"pid"`
}

// Request is what the page leaves for a run it starts: the unit's command line is fixed, so a dry
// run is asked for through this file, which the run takes (and removes) at its start.
type Request struct {
	DryRun  bool      `json:"dry_run"`
	Trigger string    `json:"trigger"`
	At      time.Time `json:"at"`
}

// Env is where a run finds things.
type Env struct {
	// AddonDir is the addon's directory (var/runs holds the history); ConfDir its configuration.
	AddonDir, ConfDir string
	// RunDir is the runtime directory: the run lock, a running job's progress, the page's request.
	RunDir string
	Rclone *rclone.Runner
	Tools  servers.Tools
	// Log writes a line to the journal.
	Log func(format string, args ...any)
}

// DefaultRunDir is the runtime directory on the system.
func DefaultRunDir() string { return defaultRunDir }

// HistoryDir is where a job's runs are kept.
func HistoryDir(addonDir, job string) string { return filepath.Join(addonDir, "var", "runs", job) }

// WriteRequest leaves the page's request for the job's next run.
func WriteRequest(runDir, job string, rq Request) error {
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(rq)
	return os.WriteFile(filepath.Join(runDir, requestPrefix+job+".json"), b, 0o600)
}

func takeRequest(runDir, job string) Request {
	p := filepath.Join(runDir, requestPrefix+job+".json")
	b, err := os.ReadFile(p)
	_ = os.Remove(p)
	var rq Request
	// a request that was never taken (the start failed) must not turn a later timer run into a dry run
	if err != nil || json.Unmarshal(b, &rq) != nil || time.Since(rq.At) > 2*time.Minute {
		return Request{Trigger: triggerTimer}
	}
	if rq.Trigger == "" {
		rq.Trigger = TriggerPage
	}
	return rq
}

// ReadProgress answers a running job's figures; false when it is not running (no file, or one a
// process left that is gone).
func ReadProgress(runDir, job string) (Progress, bool) {
	b, err := os.ReadFile(filepath.Join(runDir, progressPrefix+job+".json"))
	if err != nil {
		return Progress{}, false
	}
	var p Progress
	if json.Unmarshal(b, &p) != nil || p.PID <= 0 {
		return Progress{}, false
	}
	if err := syscall.Kill(p.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return Progress{}, false
	}
	return p, true
}

func writeProgress(runDir string, p Progress) {
	p.Updated, p.PID = time.Now().UTC(), os.Getpid()
	b, _ := json.Marshal(p)
	tmp := filepath.Join(runDir, progressPrefix+p.Job+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(runDir, progressPrefix+p.Job+".json"))
	}
}

// History lists a job's runs, newest first.
func History(addonDir, job string) []Record {
	dir := HistoryDir(addonDir, job)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []Record{}
	}
	out := []Record{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), recordSuffix) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(b, &r) == nil && r.ID != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

var runIDRe = regexp.MustCompile(`^\d{8}T\d{6}Z(-\d+)?$`)

// ReadRecord answers one run.
func ReadRecord(addonDir, job, run string) (Record, error) {
	if !ValidID(job) || !runIDRe.MatchString(run) {
		return Record{}, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(HistoryDir(addonDir, job), run+recordSuffix))
	if err != nil {
		return Record{}, ErrNotFound
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, err
	}
	return r, nil
}

// LogLine is one line of a run's file list: Kind is C (copied), R (replaced), D (deleted) or E (an
// error's text).
type LogLine struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// ReadLog answers a run's file list, at most limit lines.
func ReadLog(addonDir, job, run string, limit int) ([]LogLine, bool, error) {
	if !ValidID(job) || !runIDRe.MatchString(run) {
		return nil, false, ErrNotFound
	}
	f, err := os.Open(filepath.Join(HistoryDir(addonDir, job), run+logSuffix))
	if err != nil {
		return []LogLine{}, false, nil
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return []LogLine{}, false, nil
	}
	lines := []LogLine{}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		kind, text, ok := strings.Cut(sc.Text(), " ")
		if !ok || len(kind) != 1 {
			continue
		}
		if len(lines) >= limit {
			return lines, true, nil
		}
		lines = append(lines, LogLine{Kind: kind, Text: text})
	}
	return lines, false, nil
}

// DeleteHistory removes a job's runs.
func DeleteHistory(addonDir, job string) {
	if ValidID(job) {
		_ = os.RemoveAll(HistoryDir(addonDir, job))
	}
}

func prune(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), recordSuffix); ok {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	limit := time.Now().UTC().AddDate(0, 0, -keepDays).Format(runTimeLayout)
	for i, id := range ids {
		if i >= keepRuns || id < limit {
			_ = os.Remove(filepath.Join(dir, id+recordSuffix))
			_ = os.Remove(filepath.Join(dir, id+logSuffix))
		}
	}
}

// runLog collects a run's file list, counts it, and keeps the first error.
type runLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	zw    *gzip.Writer
	n     int
	cut   bool
	rec   *Record
	files int
	log   func(format string, args ...any)
}

func newRunLog(rec *Record, log func(string, ...any)) *runLog {
	l := &runLog{rec: rec, log: log}
	l.zw = gzip.NewWriter(&l.buf)
	return l
}

func (l *runLog) add(kind, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch kind {
	case "C":
		l.rec.Copied++
		l.files++
	case "R":
		l.rec.Replaced++
		l.files++
	case "D":
		l.rec.Deleted++
	case "E":
		l.rec.Errors++
		if l.rec.Error == "" {
			l.rec.Error = clip(text, maxRecordErrorLen)
		}
		if l.log != nil && l.rec.Errors <= 20 {
			l.log("%s", text)
		}
	}
	if l.n > maxLogBytes {
		l.cut = true
		return
	}
	line := kind + " " + strings.ReplaceAll(text, "\n", " ") + "\n"
	l.n += len(line)
	_, _ = io.WriteString(l.zw, line)
}

func (l *runLog) bytes() ([]byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.zw.Close()
	return l.buf.Bytes(), l.cut
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// lockRun takes the addon's run lock, waiting for another job to end; false when the wait ran out
// or the run was stopped meanwhile.
func lockRun(ctx context.Context, runDir string, waiting func()) (*os.File, bool) {
	f, err := os.OpenFile(filepath.Join(runDir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, true // no lock file: run without one rather than not at all
	}
	deadline := time.Now().Add(lockWait)
	told := false
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return f, true
		}
		if !told {
			told = true
			waiting()
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, false
		case <-time.After(2 * time.Second):
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, false
		}
	}
}

// Run runs the job once and writes its record. The error is nil when the run did what it was to
// do; the record is written either way.
func Run(ctx context.Context, env Env, jobID string) (Record, error) {
	logf := env.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	rec := Record{Job: jobID, Start: time.Now().UTC(), Trigger: triggerTimer, Result: ResultFailed, Exit: -1}
	rec.ID = rec.Start.Format(runTimeLayout)
	if !ValidID(jobID) {
		return rec, fmt.Errorf("not a copy job's id: %q", jobID)
	}
	store, err := Open(env.ConfDir)
	if err != nil {
		return rec, err
	}
	job, ok := store.Get(jobID)
	if !ok {
		return rec, fmt.Errorf("no copy job %s: its timer is left over and can be deleted on the Services page", jobID)
	}
	_ = os.MkdirAll(env.RunDir, 0o700)
	rq := takeRequest(env.RunDir, jobID)
	rec.DryRun, rec.Trigger = rq.DryRun, rq.Trigger
	rl := newRunLog(&rec, logf)
	hist := HistoryDir(env.AddonDir, jobID)
	finish := func(result, text string) (Record, error) {
		rec.Result, rec.End = result, time.Now().UTC()
		if text != "" && rec.Error == "" {
			rec.Error = clip(text, maxRecordErrorLen)
		}
		logBytes, cut := rl.bytes()
		rec.LogCut = cut
		if err := os.MkdirAll(hist, 0o700); err == nil {
			_ = os.WriteFile(filepath.Join(filepath.Dir(hist), ".nobackup"), nil, 0o600)
			for n := 2; ; n++ { // two runs in one second keep both records
				if _, err := os.Stat(filepath.Join(hist, rec.ID+recordSuffix)); err != nil {
					break
				}
				rec.ID = rec.Start.Format(runTimeLayout) + "-" + strconv.Itoa(n)
			}
			b, _ := json.MarshalIndent(rec, "", "  ")
			_ = os.WriteFile(filepath.Join(hist, rec.ID+logSuffix), logBytes, 0o600)
			_ = os.WriteFile(filepath.Join(hist, rec.ID+recordSuffix), append(b, '\n'), 0o600)
			prune(hist)
		}
		_ = os.Remove(filepath.Join(env.RunDir, progressPrefix+jobID+".json"))
		what := "run"
		if rec.DryRun {
			what = "dry run"
		}
		logf("%s of %q: %s - %d copied, %d replaced, %d deleted, %d errors, %d bytes in %s", what, job.Name, result, rec.Copied, rec.Replaced, rec.Deleted, rec.Errors, rec.Bytes, rec.End.Sub(rec.Start).Round(time.Second))
		if rec.OK() {
			return rec, nil
		}
		if rec.Error != "" {
			return rec, fmt.Errorf("%s: %s", result, rec.Error)
		}
		return rec, errors.New(result)
	}
	if job.Mode == ModeMirror && job.NeedsDryRun && !rec.DryRun {
		return finish(ResultNeedsDryRun, "a mirror job runs only after a dry run of it has been looked at on the addon's page")
	}

	prog := Progress{Job: jobID, Run: rec.ID, Started: rec.Start, DryRun: rec.DryRun}
	lock, got := lockRun(ctx, env.RunDir, func() {
		prog.Waiting = true
		writeProgress(env.RunDir, prog)
		logf("%q waits for another copy job to end", job.Name)
	})
	if !got {
		if ctx.Err() != nil {
			return finish(ResultStopped, "stopped while it waited for another copy job")
		}
		return finish(ResultBusy, "another copy job ran for longer than this one could wait")
	}
	if lock != nil {
		defer lock.Close()
	}
	prog.Waiting = false
	writeProgress(env.RunDir, prog)

	source, err := CheckSourceOnDisk(job.Source)
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			return finish(ResultFailed, err.Error())
		}
		return finish(ResultSourceMissing, fmt.Sprintf("the source %s cannot be read: %v", job.Source, err))
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return finish(ResultSourceMissing, fmt.Sprintf("the source %s cannot be read: %v", job.Source, err))
	}
	if job.Mode == ModeMirror && len(entries) == 0 {
		return finish(ResultSourceMissing, sourceEmptyMirrorText)
	}
	exFile := filepath.Join(env.RunDir, "excludes-"+jobID+".txt")
	if err := os.WriteFile(exFile, []byte(strings.Join(Excludes(job, source), "\n")+"\n"), 0o600); err != nil {
		return finish(ResultFailed, err.Error())
	}
	defer os.Remove(exFile)

	var cmd *exec.Cmd
	var parse func(line string, stderr bool)
	var classify func(exit int, stderr string) string
	switch job.Target.Kind {
	case TargetServer:
		list, err := servers.Load(env.ConfDir)
		if err != nil {
			return finish(ResultFailed, err.Error())
		}
		var srv *servers.Server
		for i := range list {
			if list[i].ID == job.Target.ID {
				srv = &list[i]
			}
		}
		if srv == nil {
			return finish(ResultTargetMissing, "the SSH server this job copies to no longer exists")
		}
		rec.Tool = "rsync"
		bin := env.Tools.Rsync
		if bin == "" {
			bin = "rsync"
		}
		rsyncEnv := append(os.Environ(), "HOME="+filepath.Join(env.AddonDir, "var"), "RSYNC_RSH="+servers.RSH(env.Tools, env.ConfDir, *srv))
		// the job's folder below the base path, made when it is missing (empty folders, nothing else)
		if tree, err := os.MkdirTemp(env.RunDir, "mkdir-"); err == nil {
			if os.MkdirAll(filepath.Join(tree, filepath.FromSlash(job.Target.Folder)), 0o755) == nil {
				mk := exec.CommandContext(ctx, bin, RsyncMkdirArgs(tree, *srv)...)
				mk.Env = rsyncEnv
				var mkErr bytes.Buffer
				mk.Stderr = &mkErr
				if err := mk.Run(); err != nil {
					_ = os.RemoveAll(tree)
					exit := -1
					var ee *exec.ExitError
					if errors.As(err, &ee) {
						exit = ee.ExitCode()
					}
					rec.Exit = exit
					for _, l := range strings.Split(strings.TrimSpace(mkErr.String()), "\n") {
						if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "rsync error:") {
							rl.add("E", l)
						}
					}
					if ctx.Err() != nil {
						return finish(ResultStopped, "the run was stopped")
					}
					res := rsyncResult(exit, mkErr.String())
					if res == ResultOK || res == ResultWarnings {
						res = ResultFailed
					}
					return finish(res, "the folder on the server could not be made")
				}
			}
			_ = os.RemoveAll(tree)
		}
		cmd = exec.CommandContext(ctx, bin, RsyncArgs(job, source, *srv, exFile, rec.DryRun)...)
		cmd.Env = rsyncEnv
		parse = func(line string, stderr bool) { parseRsync(line, stderr, rl, &rec, &prog) }
		classify = rsyncResult
	case TargetAccount:
		acc, ok := loadAccount(env.ConfDir, job.Target.ID)
		if !ok {
			return finish(ResultTargetMissing, "the account this job copies to no longer exists")
		}
		if env.Rclone == nil {
			return finish(ResultFailed, "rclone is not there")
		}
		rec.Tool = "rclone"
		cmd = env.Rclone.JobCommand(ctx, RcloneArgs(job, source, acc.Remote(), acc.Root, exFile, rec.DryRun)...)
		parse = func(line string, stderr bool) { parseRclone(line, stderr, rl, &rec, &prog) }
		classify = rcloneResult
	default:
		return finish(ResultFailed, "the job has no target")
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = 10 * time.Second
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return finish(ResultFailed, err.Error())
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return finish(ResultFailed, err.Error())
	}
	what := "run"
	if rec.DryRun {
		what = "dry run"
	}
	logf("%s of %q: %s → %s (%s, %s)", what, job.Name, job.Source, job.Target.Folder, job.Mode, rec.Tool)
	if err := cmd.Start(); err != nil {
		return finish(ResultFailed, fmt.Sprintf("%s does not start: %v", rec.Tool, err))
	}
	var errText bytes.Buffer
	var emu sync.Mutex
	var wg sync.WaitGroup
	read := func(r io.Reader, isErr bool) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		sc.Split(scanLinesCR)
		for sc.Scan() {
			line := sc.Text()
			if isErr {
				emu.Lock()
				if errText.Len() < 64<<10 {
					errText.WriteString(line + "\n")
				}
				emu.Unlock()
			}
			parse(line, isErr)
		}
	}
	wg.Add(2)
	go read(stdout, false)
	go read(stderr, true)
	tick := time.NewTicker(progressEvery)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				rl.mu.Lock()
				prog.Files = rl.files
				p := prog
				rl.mu.Unlock()
				writeProgress(env.RunDir, p)
			}
		}
	}()
	wg.Wait()
	werr := cmd.Wait()
	tick.Stop()
	close(done)
	rec.Exit = 0
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		rec.Exit = ee.ExitCode()
	} else if werr != nil {
		rec.Exit = -1
	}
	if ctx.Err() != nil {
		return finish(ResultStopped, "the run was stopped")
	}
	emu.Lock()
	text := errText.String()
	emu.Unlock()
	return finish(classify(rec.Exit, text), "")
}

// scanLinesCR splits at \n and at \r: rsync rewrites its progress line with carriage returns.
func scanLinesCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// loadAccount reads one account's record for a run: the run changes nothing in the accounts' file.
func loadAccount(confDir, id string) (accounts.Account, bool) {
	b, err := os.ReadFile(filepath.Join(confDir, accounts.File))
	if err != nil {
		return accounts.Account{}, false
	}
	var f struct {
		Accounts []accounts.Account `json:"accounts"`
	}
	if json.Unmarshal(b, &f) != nil {
		return accounts.Account{}, false
	}
	for _, a := range f.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return accounts.Account{}, false
}
