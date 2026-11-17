// Package jobs keeps the copy jobs: a folder of the system's own storage, copied in one direction
// to an account (with rclone) or to an SSH server (with rsync). A job is a record here and, on the
// system, one timer with its service (local-sync-<id>), which runs `lite-sync run <id>`: the
// daemon never runs a job itself. The job's id is made once from its name and never changes - it
// is the job's identity on the system's Services page and in the journal.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/servers"
)

// The modes.
const (
	ModeCopy   = "copy"   // new and changed files; nothing is deleted on the target
	ModeMirror = "mirror" // the target becomes the source: files that are gone are deleted there
)

// The kinds of target.
const (
	TargetAccount = "account" // rclone
	TargetServer  = "server"  // rsync over ssh
)

// DefaultMaxDelete is how many files a mirror run may delete before it stops.
const DefaultMaxDelete = 50

// Target is where a job copies to.
type Target struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// Folder is the folder below the account's root or the server's base path.
	Folder string `json:"folder"`
}

// Job is one copy job.
type Job struct {
	// ID is the name of the job's units (local-sync-<id>) and of its history.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Source is a folder below /usr/local.
	Source string `json:"source"`
	// ReadAddon is the addon whose data the source is, when the job's run needs that addon's group
	// to read it; "" when the addon's own user can read the source.
	ReadAddon string `json:"read_addon,omitempty"`
	Target    Target `json:"target"`
	Mode      string `json:"mode"`
	// MaxDelete stops a mirror run that would delete more files.
	MaxDelete int `json:"max_delete"`
	// Excludes are the user's patterns, one per entry ("*.tmp", "/cache/**").
	Excludes []string `json:"excludes"`
	// NameFilters: the default filters for files that look like keys and tokens are on.
	NameFilters bool `json:"name_filters"`
	// BwLimit limits the transfer in KiB/s, 0 = none.
	BwLimit int `json:"bwlimit_kbps"`
	// Versions keeps what a run replaces or deletes on the target: it is moved into
	// <folder>/.versions/<time of the run>/ instead of being lost. VersionsDays removes the versions
	// of runs older than that many days after a run, 0 = they are kept until somebody removes them.
	Versions     bool `json:"versions"`
	VersionsDays int  `json:"versions_days"`
	// Schedule is what the job dialog chose last; the timer on the system is the truth.
	Schedule Schedule `json:"schedule"`
	// TimerHash is the SHA-256 of the timer text the addon wrote last: a different text on the
	// system was edited on the Services page.
	TimerHash string    `json:"timer_hash,omitempty"`
	Created   time.Time `json:"created"`
	// Changed is when what a run does last changed (source, target, mode).
	Changed time.Time `json:"changed"`
	// NeedsDryRun: a mirror job that nobody has tried since it changed. It does not run for real,
	// and its timer stays off, until a dry run's result has been looked at.
	NeedsDryRun bool `json:"needs_dry_run,omitempty"`
}

// Spec is what the job dialog sends.
type Spec struct {
	Name         string   `json:"name"`
	Source       string   `json:"source"`
	ReadAddon    string   `json:"read_addon"`
	Target       Target   `json:"target"`
	Mode         string   `json:"mode"`
	MaxDelete    *int     `json:"max_delete"`
	Excludes     []string `json:"excludes"`
	NameFilters  *bool    `json:"name_filters"`
	BwLimit      int      `json:"bwlimit_kbps"`
	Versions     bool     `json:"versions"`
	VersionsDays int      `json:"versions_days"`
	Schedule     Schedule `json:"schedule"`
}

// ValidationError is a field the user must correct.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, a...)} }

// ErrNotFound: no job of that id.
var ErrNotFound = errors.New("no such copy job")

// MaxIDLen keeps "sync-<id>" inside the 32 characters of an own unit's name on the system.
const MaxIDLen = 27

var (
	idRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,26}$`)
	notSlug    = regexp.MustCompile(`[^a-z0-9]+`)
	addonIDRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`)
	targetIDRe = regexp.MustCompile(`^[a-z0-9]{6,32}$`)
)

// ValidID says whether id has the shape of a job's id.
func ValidID(id string) bool { return idRe.MatchString(id) }

// Slug makes a job's id from its name: lower case, a-z 0-9 and dashes, at most MaxIDLen; taken
// says which ids exist, and a number is appended on a clash.
func Slug(name string, taken func(string) bool) string {
	s := strings.ToLower(name)
	s = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss").Replace(s)
	s = strings.Trim(notSlug.ReplaceAllString(s, "-"), "-")
	if s == "" {
		s = "job"
	}
	if len(s) > MaxIDLen {
		s = strings.Trim(s[:MaxIDLen], "-")
	}
	if !taken(s) {
		return s
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		base := s
		if len(base)+len(suffix) > MaxIDLen {
			base = strings.Trim(base[:MaxIDLen-len(suffix)], "-")
		}
		if c := base + suffix; !taken(c) {
			return c
		}
	}
}

// CheckFolder is the rule for a folder on a target: the servers' path rule, relative.
func CheckFolder(f string) error {
	f = strings.Trim(f, "/")
	if f == "" {
		return invalid("folder: the folder on the target the job writes into")
	}
	if err := servers.CheckPath(f); err != nil {
		return invalid("folder: letters, digits, / . _ - + @ and spaces; no \"..\", nothing that starts with a dash")
	}
	return nil
}

// CheckExclude is the rule for one of the user's patterns: a closed syntax - a name or path
// pattern with * ? and **, nothing that reads another file or changes the meaning of the list.
func CheckExclude(p string) error {
	if p == "" || len(p) > 200 || p != strings.TrimSpace(p) {
		return invalid("a pattern is 1 to 200 characters without spaces at its ends")
	}
	if strings.ContainsAny(p, "\x00\n\r\t{}[]\\!") || strings.HasPrefix(p, "-") || strings.HasPrefix(p, "+") || strings.HasPrefix(p, ".") && (strings.HasPrefix(p, ". ") || strings.HasPrefix(p, ": ")) || strings.HasPrefix(p, ":") || strings.HasPrefix(p, "#") || strings.HasPrefix(p, ";") {
		return invalid("pattern %q: a name or path with * and ? only", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return invalid("pattern %q: no \"..\"", p)
		}
	}
	return nil
}

// apply checks a spec and writes it into j; changed says whether what a run does changed.
func (j *Job) apply(sp Spec) (changed bool, err error) {
	sp.Name = strings.TrimSpace(sp.Name)
	if sp.Name == "" || len(sp.Name) > 60 || strings.ContainsAny(sp.Name, "\n\r\t\x00") {
		return false, invalid("name: 1 to 60 characters")
	}
	src, err := CleanSource(sp.Source)
	if err != nil {
		return false, err
	}
	if sp.ReadAddon != "" && !addonIDRe.MatchString(sp.ReadAddon) {
		return false, invalid("read_addon: an addon's id")
	}
	if sp.Target.Kind != TargetAccount && sp.Target.Kind != TargetServer {
		return false, invalid("target: an account or an SSH server")
	}
	if !targetIDRe.MatchString(sp.Target.ID) {
		return false, invalid("target: choose where the job copies to")
	}
	if err := CheckFolder(sp.Target.Folder); err != nil {
		return false, err
	}
	sp.Target.Folder = strings.Trim(sp.Target.Folder, "/")
	if sp.Mode == "" {
		sp.Mode = ModeCopy
	}
	if sp.Mode != ModeCopy && sp.Mode != ModeMirror {
		return false, invalid("mode: copy or mirror")
	}
	maxDelete := DefaultMaxDelete
	if sp.MaxDelete != nil {
		maxDelete = *sp.MaxDelete
	}
	if maxDelete < 0 || maxDelete > 1000000 {
		return false, invalid("max_delete: 0 to 1000000")
	}
	if len(sp.Excludes) > 100 {
		return false, invalid("excludes: at most 100 patterns")
	}
	ex := []string{}
	for _, p := range sp.Excludes {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if err := CheckExclude(p); err != nil {
			return false, err
		}
		ex = append(ex, p)
	}
	if sp.BwLimit < 0 || sp.BwLimit > 10000000 {
		return false, invalid("bwlimit_kbps: 0 or a rate in KiB/s")
	}
	if err := sp.Schedule.check(); err != nil {
		return false, err
	}
	if sp.VersionsDays < 0 || sp.VersionsDays > 3650 {
		return false, invalid("versions_days: 0 to 3650")
	}
	if !sp.Versions {
		sp.VersionsDays = 0
	}
	j.Versions, j.VersionsDays = sp.Versions, sp.VersionsDays
	changed = j.Source != src || j.Target != sp.Target || j.Mode != sp.Mode || j.ReadAddon != sp.ReadAddon
	j.Name, j.Source, j.ReadAddon, j.Target, j.Mode, j.MaxDelete, j.Excludes, j.BwLimit = sp.Name, src, sp.ReadAddon, sp.Target, sp.Mode, maxDelete, ex, sp.BwLimit
	j.NameFilters = sp.NameFilters == nil || *sp.NameFilters
	j.Schedule = sp.Schedule.stored()
	return changed, nil
}

// File is the jobs' file in the configuration directory.
const File = "jobs.json"

type file struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

// Store keeps the jobs.
type Store struct {
	Dir string
	Now func() time.Time

	mu   sync.Mutex
	list []Job
}

// Open reads the jobs.
func Open(dir string) (*Store, error) {
	s := &Store{Dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, File))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	s.list = f.Jobs
	return s, nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(file{Version: 1, Jobs: s.list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, File+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, File))
}

// List answers the jobs, oldest first.
func (s *Store) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Job{}, s.list...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Get answers one job.
func (s *Store) Get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.list {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

// Create adds a job; its id is made from the name.
func (s *Store) Create(sp Spec) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var j Job
	if _, err := j.apply(sp); err != nil {
		return Job{}, err
	}
	if len(s.list) >= 30 {
		return Job{}, invalid("at most 30 copy jobs")
	}
	j.ID = Slug(j.Name, func(id string) bool {
		for _, o := range s.list {
			if o.ID == id {
				return true
			}
		}
		return false
	})
	j.Created, j.Changed = s.now(), s.now()
	j.NeedsDryRun = j.Mode == ModeMirror
	s.list = append(s.list, j)
	return j, s.save()
}

// Update changes a job; the id stays. A change of what a mirror run does asks for a new dry run.
func (s *Store) Update(id string, sp Spec) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].ID != id {
			continue
		}
		j := s.list[i]
		changed, err := j.apply(sp)
		if err != nil {
			return Job{}, err
		}
		if changed {
			j.Changed = s.now()
		}
		switch {
		case j.Mode != ModeMirror:
			j.NeedsDryRun = false
		case changed || s.list[i].Mode != ModeMirror:
			j.NeedsDryRun = true
		}
		s.list[i] = j
		return j, s.save()
	}
	return Job{}, ErrNotFound
}

// Set changes a job's bookkeeping (the timer's hash, the schedule as read back, the dry run).
func (s *Store) Set(id string, f func(*Job)) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].ID == id {
			f(&s.list[i])
			return s.list[i], s.save()
		}
	}
	return Job{}, ErrNotFound
}

// Delete removes a job's record; its history and its timer are the caller's to remove.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].ID == id {
			s.list = append(s.list[:i], s.list[i+1:]...)
			return s.save()
		}
	}
	return ErrNotFound
}

// UsesTarget lists the jobs that copy to a target.
func (s *Store) UsesTarget(kind, id string) []Job {
	var out []Job
	for _, j := range s.List() {
		if j.Target.Kind == kind && j.Target.ID == id {
			out = append(out, j)
		}
	}
	return out
}
