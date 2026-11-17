package jobs

import (
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// What a copy job may take as its source: a folder below the system's own storage (/usr/local),
// and of that not everything. The fixed list of what is never copied is checked when a job is
// saved, shown in the folder picker with its reason, and checked again by the run on the real
// path (after links are resolved), so a link planted later does not lead into it.

// Root is the system's own storage. A variable so the tests can use a directory of their own.
var Root = "/usr/local"

// The reasons a folder is never copied.
const (
	NeverConfig  = "config"  // /usr/local/etc: keys, passwords, the settings of the system and of every addon
	NeverTemp    = "temp"    // tmp, var, lost+found
	NeverBackups = "backups" // a folder that holds backups of the system
	NeverProgram = "program" // an addon's program files (.nobackup)
	NeverOwn     = "own"     // this addon's own tree
	NeverRoot    = "root"    // /usr/local itself
)

// neverBelow are the fixed trees, relative to Root.
var neverBelow = map[string]string{
	"etc":        NeverConfig,
	"tmp":        NeverTemp,
	"var":        NeverTemp,
	"lost+found": NeverTemp,
}

// OwnAddon is this addon's id: its own tree is no source.
var OwnAddon = "sync"

// CleanSource checks a source lexically and answers it cleaned: an absolute path below Root, with
// no "..", no control character, nothing a tool reads as an option, and not in the fixed list.
func CleanSource(p string) (string, error) {
	if p == "" || strings.ContainsAny(p, "\x00\n\r\t") || len(p) > 1000 {
		return "", invalid("source: a folder of the system")
	}
	if !filepath.IsAbs(p) {
		return "", invalid("source: an absolute path below %s", Root)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", invalid("source: no \"..\"")
		}
	}
	p = filepath.Clean(p)
	if why := NeverReason(p); why != "" {
		return "", invalid("source: %s is never copied (%s)", p, why)
	}
	return p, nil
}

// rel answers p relative to Root, false when p is not Root or below it.
func rel(p string) (string, bool) {
	if p == Root {
		return "", true
	}
	r, ok := strings.CutPrefix(p, strings.TrimRight(Root, "/")+"/")
	return r, ok
}

// NeverReason says why the cleaned path p is never a source, "" when it may be one: outside Root,
// Root itself, one of the fixed trees or below it, this addon's own tree.
func NeverReason(p string) string {
	r, ok := rel(p)
	if !ok || r == "" {
		return NeverRoot
	}
	first, _, _ := strings.Cut(r, "/")
	if why, never := neverBelow[first]; never {
		return why
	}
	if r == "addons/"+OwnAddon || strings.HasPrefix(r, "addons/"+OwnAddon+"/") {
		return NeverOwn
	}
	for _, seg := range strings.Split(r, "/") {
		if seg == "lost+found" {
			return NeverTemp
		}
	}
	return ""
}

// CheckSourceOnDisk is the run's check of the real folder: it exists, is a directory, and where
// its links lead is still a source that may be copied. It answers the resolved path.
func CheckSourceOnDisk(p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if why := NeverReason(real); why != "" {
		return "", invalid("the source leads to %s, which is never copied (%s)", real, why)
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", invalid("the source %s is not a folder", p)
	}
	if holdsBackups(real) {
		return "", invalid("the source %s holds backups of the system, which go through the Backup page", p)
	}
	if _, err := os.Lstat(filepath.Join(real, ".nobackup")); err == nil {
		return "", invalid("the source %s holds program files, not data", p)
	}
	return real, nil
}

// backupName says whether a file name is a backup of the system or the rest of one.
func backupName(name string) bool {
	return strings.HasSuffix(name, ".sbk") || strings.HasSuffix(name, ".sbk.age") || strings.HasSuffix(name, ".sbk.partial") || strings.HasSuffix(name, ".sbk.age.partial")
}

// holdsBackups says whether a folder directly holds a backup of the system.
func holdsBackups(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && backupName(e.Name()) {
			return true
		}
	}
	return false
}

// The states of a folder in the picker.
const (
	FolderOK         = "ok"         // the addon reads it: selectable
	FolderAddon      = "addon"      // another addon's data: selectable with that addon's group
	FolderUnreadable = "unreadable" // not readable by the addon, and no addon's group would help
	FolderNever      = "never"      // on the fixed list
)

// Folder is one entry of the picker.
type Folder struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
	// Why is the reason of a folder that is never copied.
	Why string `json:"why,omitempty"`
	// Addon is the addon whose data an "addon" folder is.
	Addon string `json:"addon,omitempty"`
	// Open: the picker can list what is inside.
	Open bool `json:"open"`
}

// Listing is the picker's answer for one folder.
type Listing struct {
	Path string `json:"path"`
	// Parent is the folder above, "" at Root.
	Parent string `json:"parent"`
	// Self is the listed folder's own state: whether it can be chosen.
	Self    Folder   `json:"self"`
	Folders []Folder `json:"folders"`
}

// ownerAddon answers the addon a folder belongs to by its group (addon-<id>), "" for none.
func ownerAddon(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	g, err := user.LookupGroupId(strconv.FormatUint(uint64(st.Gid), 10))
	if err != nil {
		return ""
	}
	id, ok := strings.CutPrefix(g.Name, "addon-")
	if !ok || id == OwnAddon {
		return ""
	}
	return id
}

// classify answers a folder's state. readable says whether an addon's data may be read by a job
// (the system's list of readable addons).
func classify(p string, readable func(addon string) bool) Folder {
	f := Folder{Name: filepath.Base(p), Path: p}
	if why := NeverReason(p); why != "" {
		f.State, f.Why = FolderNever, why
		return f
	}
	info, err := os.Lstat(p)
	if err != nil || !info.IsDir() {
		f.State = FolderUnreadable
		return f
	}
	if _, err := os.Lstat(filepath.Join(p, ".nobackup")); err == nil {
		f.State, f.Why = FolderNever, NeverProgram
		return f
	}
	if entries, err := os.ReadDir(p); err == nil {
		for _, e := range entries {
			if !e.IsDir() && backupName(e.Name()) {
				f.State, f.Why = FolderNever, NeverBackups
				return f
			}
		}
		f.State, f.Open = FolderOK, true
		return f
	}
	if id := ownerAddon(info); id != "" && readable != nil && readable(id) {
		f.State, f.Addon = FolderAddon, id
		return f
	}
	f.State = FolderUnreadable
	return f
}

// List answers the picker's view of the folder p (Root when empty).
func List(p string, readable func(addon string) bool) (Listing, error) {
	if p == "" {
		p = Root
	}
	if strings.ContainsAny(p, "\x00\n\r") || !filepath.IsAbs(p) {
		return Listing{}, invalid("path: a folder below %s", Root)
	}
	p = filepath.Clean(p)
	if _, ok := rel(p); !ok {
		return Listing{}, invalid("path: a folder below %s", Root)
	}
	l := Listing{Path: p, Folders: []Folder{}}
	if p != Root {
		l.Parent = filepath.Dir(p)
		l.Self = classify(p, readable)
	} else {
		l.Self = Folder{Name: filepath.Base(p), Path: p, State: FolderNever, Why: NeverRoot, Open: true}
	}
	if l.Self.State == FolderNever && p != Root {
		return l, nil // nothing inside a folder that is never copied is offered
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return l, nil
	}
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		l.Folders = append(l.Folders, classify(filepath.Join(p, e.Name()), readable))
	}
	sort.Slice(l.Folders, func(i, j int) bool { return strings.ToLower(l.Folders[i].Name) < strings.ToLower(l.Folders[j].Name) })
	return l, nil
}

// NameFilterPatterns are the default filters a job starts with and the user can switch off: files
// that look like keys, tokens and passwords.
var NameFilterPatterns = []string{"*.pem", "*.key", "id_*", ".ssh/", "*token*", "*secret*", "*.age"}

// FixedPatterns are never copied, whatever the job says: backups of the system and their rests,
// and what the filesystem keeps for itself.
var FixedPatterns = []string{"*.sbk", "*.sbk.age", "*.partial", "lost+found/"}

// Excludes builds a run's exclude list for the resolved source: the fixed patterns, the default
// name filters when the job has them, the user's patterns, and - found by walking the source -
// every folder that holds program files (.nobackup) or backups of the system, as paths anchored at
// the source. The walk stops at 200000 entries; what lies beyond is not looked at (and the fixed
// patterns still hold there).
func Excludes(j Job, source string) []string {
	out := append([]string{}, FixedPatterns...)
	if j.NameFilters {
		out = append(out, NameFilterPatterns...)
	}
	out = append(out, j.Excludes...)
	seen := 0
	_ = filepath.WalkDir(source, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if seen++; seen > 200000 {
			return filepath.SkipAll
		}
		if !d.IsDir() || p == source {
			return nil
		}
		if _, err := os.Lstat(filepath.Join(p, ".nobackup")); err == nil || holdsBackups(p) {
			r, _ := filepath.Rel(source, p)
			if !strings.ContainsAny(r, "\n\r*?[]{}\\") {
				out = append(out, "/"+filepath.ToSlash(r)+"/")
			}
			return filepath.SkipDir
		}
		return nil
	})
	return out
}
