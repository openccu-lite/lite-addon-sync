// Package rclone runs the rclone binary the addon ships: every call is an argument list (never a
// shell), with the addon's own configuration file and nothing of rclone's written elsewhere, a
// bounded number of retries, and the limits a Raspberry Pi 3 needs. What rclone says on stderr is
// mapped onto the states the system's backup targets know.
package rclone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The states of the system's backup targets a provider may answer (occulited's
// docs/backup-provider.md).
const (
	StateWritable    = "writable"
	StateUnreachable = "unreachable"
	StateAuthFailed  = "auth-failed"
	StateFull        = "full"
	StateReadOnly    = "read-only"
	StateError       = "error"
)

// Runner runs rclone.
type Runner struct {
	// Bin is the rclone binary.
	Bin string
	// Config is the addon's rclone.conf (0600): the remotes with their tokens and keys.
	Config string
	// Home is where rclone may keep its cache and temporary files (the addon's var directory).
	Home string
	// Log gets one line per failed call; nil = none.
	Log func(format string, args ...any)
}

// Error is a failed rclone call.
type Error struct {
	// State is what the failure means for a target.
	State string
	// Text is rclone's last error line (never a secret: rclone prints none).
	Text string
	Exit int
}

func (e *Error) Error() string { return e.Text }

// StateOf is the state an error means; error for anything that is no *Error.
func StateOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.State
	}
	if err == nil {
		return StateWritable
	}
	return StateError
}

// ErrNotFound: the file or folder is not there.
var ErrNotFound = errors.New("not found")

func (r *Runner) env() []string {
	return append(os.Environ(),
		"RCLONE_CONFIG="+r.Config,
		"HOME="+r.Home,
		"XDG_CACHE_HOME="+filepath.Join(r.Home, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(r.Home, "config"),
		"TMPDIR="+filepath.Join(r.Home, "tmp"),
		// a Raspberry Pi 3 has 1 GB: the garbage collector works harder instead
		"GOGC=50", "GOMEMLIMIT=192MiB",
		// never ask anything, never look for a newer version
		"RCLONE_ASK_PASSWORD=false", "RCLONE_AUTO_CONFIRM=true",
	)
}

// global are the flags every call carries: few retries (the system retries a run itself), bounded
// timeouts, small buffers, plain log lines.
func global() []string {
	return []string{"--retries", "1", "--low-level-retries", "3", "--contimeout", "15s", "--timeout", "2m",
		"--transfers", "1", "--checkers", "2", "--buffer-size", "4M", "--use-mmap", "--log-level", "NOTICE", "--stats", "0"}
}

func (r *Runner) cmd(ctx context.Context, args ...string) *exec.Cmd {
	_ = os.MkdirAll(filepath.Join(r.Home, "tmp"), 0o700)
	c := exec.CommandContext(ctx, r.Bin, append(global(), args...)...)
	c.Env = r.env()
	c.Dir = r.Home
	c.WaitDelay = 5 * time.Second
	return c
}

// run runs a call and answers its stdout.
func (r *Runner) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	c := r.cmd(ctx, args...)
	var out, errb bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = stdin, &out, &limited{w: &errb, n: 64 << 10}
	err := c.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	return out.Bytes(), r.failure(ctx, args, err, errb.String())
}

type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		q := p
		if len(q) > l.n {
			q = q[:l.n]
		}
		_, _ = l.w.Write(q)
		l.n -= len(q)
	}
	return len(p), nil
}

func (r *Runner) failure(ctx context.Context, args []string, err error, stderr string) error {
	exit := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	}
	text := lastLine(stderr)
	if text == "" {
		text = err.Error()
	}
	e := &Error{State: Classify(stderr, exit), Text: text, Exit: exit}
	if ctx.Err() != nil {
		e.State, e.Text = StateUnreachable, "the transfer was stopped: it took longer than its limit"
	}
	if r.Log != nil {
		r.Log("rclone %s: exit %d, %s: %s", verb(args), exit, e.State, e.Text)
	}
	if e.State == "not-found" {
		return fmt.Errorf("%w: %s", ErrNotFound, text)
	}
	return e
}

func verb(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if args[0] == "config" && len(args) > 1 {
		return "config " + args[1]
	}
	return args[0]
}

var logPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} (ERROR|NOTICE|CRITICAL|INFO|DEBUG)\s*: `)

// lastLine is rclone's last line that says something, without its time stamp.
func lastLine(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(logPrefixRe.ReplaceAllString(lines[i], ""))
		if l == "" || strings.HasPrefix(l, "Failed to ") && i > 0 {
			// "Failed to copy: …" repeats the line above with less in it
			if l != "" && i == 0 {
				return clip(l)
			}
			continue
		}
		return clip(l)
	}
	if len(lines) > 0 {
		return clip(strings.TrimSpace(logPrefixRe.ReplaceAllString(lines[len(lines)-1], "")))
	}
	return ""
}

func clip(s string) string {
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}

// Classify maps rclone's stderr and exit code onto a state. rclone's exit codes: 3 directory not
// found, 4 file not found, 5 temporary error, 6 less serious, 7 fatal, 8 transfer limit, 9 no files.
func Classify(stderr string, exit int) string {
	s := strings.ToLower(stderr)
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(s, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("invalid_grant", "invalid_client", "unauthorized", "401 ", "status 401", "token expired", "expired_access_token", "invalid_access_token", "couldn't fetch token", "failed to refresh token",
		"empty token found", "invalidaccesskeyid", "signaturedoesnotmatch", "accessdenied", "invalid credentials", "authentication failed", "login failed", "aadsts", "reauth"):
		return StateAuthFailed
	case has("quota", "insufficient storage", "insufficient_space", "no space left", "storage limit", "507 ", "status 507", "storagequotaexceeded", "entitytoolarge"):
		return StateFull
	case has("no such host", "dial tcp", "connection refused", "network is unreachable", "i/o timeout", "context deadline exceeded", "tls handshake", "connection reset", "eof", "temporary failure in name resolution", "502 ", "503 ", "504 ", "too many requests", "429 "):
		return StateUnreachable
	case has("permission denied", "403 ", "status 403", "forbidden", "read-only", "read only", "insufficient_scope", "405 "):
		return StateReadOnly
	case exit == 3 || exit == 4 || has("directory not found", "object not found", "file not found"):
		return "not-found"
	}
	return StateError
}

// Version is rclone's version ("v1.76.0").
func (r *Runner) Version(ctx context.Context) string {
	out, err := r.run(ctx, nil, "version")
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(strings.TrimPrefix(first, "rclone "))
}

// question is one step of rclone's non-interactive configuration.
type question struct {
	State  string `json:"State"`
	Option *struct {
		Name       string `json:"Name"`
		DefaultStr string `json:"DefaultStr"`
		Examples   []struct {
			Value string `json:"Value"`
		} `json:"Examples"`
	} `json:"Option"`
	Error string `json:"Error"`
}

// ConfigCreate creates (or replaces) a remote: its type and parameters, the passwords obscured by
// rclone, and the questions a backend asks after its login answered with their defaults - never
// "replace the token", which was just given.
func (r *Runner) ConfigCreate(ctx context.Context, name, typ string, params map[string]string) error {
	_ = r.ConfigDelete(ctx, name)
	args := []string{"config", "create", name, typ}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		if params[k] != "" {
			args = append(args, k+"="+params[k])
		}
	}
	args = append(args, "--non-interactive", "--obscure")
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		_ = r.ConfigDelete(ctx, name)
		return err
	}
	for step := 0; step < 12; step++ {
		var q question
		if json.Unmarshal(out, &q) != nil || q.State == "" {
			return os.Chmod(r.Config, 0o600)
		}
		if q.Error != "" {
			_ = r.ConfigDelete(ctx, name)
			return &Error{State: Classify(q.Error, 1), Text: clip(q.Error)}
		}
		answer := ""
		if q.Option != nil {
			answer = q.Option.DefaultStr
			if q.Option.Name == "config_refresh_token" {
				answer = "false"
			} else if answer == "" && len(q.Option.Examples) > 0 {
				answer = q.Option.Examples[0].Value
			}
		}
		out, err = r.run(ctx, nil, "config", "update", name, "--continue", "--state", q.State, "--result", answer, "--non-interactive")
		if err != nil {
			_ = r.ConfigDelete(ctx, name)
			return err
		}
	}
	_ = r.ConfigDelete(ctx, name)
	return &Error{State: StateError, Text: "rclone's configuration of this account did not come to an end"}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ConfigDelete removes a remote; a missing one is no error.
func (r *Runner) ConfigDelete(ctx context.Context, name string) error {
	_, err := r.run(ctx, nil, "config", "delete", name)
	return err
}

// Remotes lists the remotes in the configuration.
func (r *Runner) Remotes(ctx context.Context) ([]string, error) {
	out, err := r.run(ctx, nil, "listremotes")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSuffix(strings.TrimSpace(l), ":"); l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// HasHashes says whether the remote's backend keeps a hash rclone can compare an upload with.
func (r *Runner) HasHashes(ctx context.Context, remote string) bool {
	out, err := r.run(ctx, nil, "backend", "features", remote+":")
	if err != nil {
		return false
	}
	var f struct {
		Hashes []string `json:"Hashes"`
	}
	return json.Unmarshal(out, &f) == nil && len(f.Hashes) > 0
}

// Space is what a remote says about its room; zero where it does not say.
type Space struct {
	Total, Used, Free int64
}

// About asks the remote for its room.
func (r *Runner) About(ctx context.Context, remote string) (Space, error) {
	out, err := r.run(ctx, nil, "about", remote+":", "--json")
	if err != nil {
		return Space{}, err
	}
	var a struct {
		Total *int64 `json:"total"`
		Used  *int64 `json:"used"`
		Free  *int64 `json:"free"`
	}
	if json.Unmarshal(out, &a) != nil {
		return Space{}, nil
	}
	var s Space
	if a.Total != nil {
		s.Total = *a.Total
	}
	if a.Used != nil {
		s.Used = *a.Used
	}
	if a.Free != nil {
		s.Free = *a.Free
	}
	return s, nil
}

// Path is remote:path, the form every command takes; the parts are joined with single slashes.
func Path(remote string, parts ...string) string {
	var clean []string
	for _, p := range parts {
		if p = strings.Trim(p, "/"); p != "" {
			clean = append(clean, p)
		}
	}
	return remote + ":" + strings.Join(clean, "/")
}

// Rcat uploads a stream of a known size to path. kbps > 0 limits the rate (KiB/s).
func (r *Runner) Rcat(ctx context.Context, path string, size int64, in io.Reader, kbps int) error {
	args := []string{"rcat", "--size", strconv.FormatInt(size, 10), path}
	if kbps > 0 {
		args = append(args, "--bwlimit", strconv.Itoa(kbps)+"k")
	}
	_, err := r.run(ctx, in, args...)
	return err
}

// Moveto renames src to dst on the remote.
func (r *Runner) Moveto(ctx context.Context, src, dst string) error {
	_, err := r.run(ctx, nil, "moveto", src, dst)
	return err
}

// DeleteFile removes one file; a missing one is no error.
func (r *Runner) DeleteFile(ctx context.Context, path string) error {
	_, err := r.run(ctx, nil, "deletefile", path)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Mkdir makes a folder (and its parents).
func (r *Runner) Mkdir(ctx context.Context, path string) error {
	_, err := r.run(ctx, nil, "mkdir", path)
	return err
}

// File is one entry of a listing.
type File struct {
	Name    string    `json:"Name"`
	Size    int64     `json:"Size"`
	ModTime time.Time `json:"ModTime"`
	IsDir   bool      `json:"IsDir"`
}

// List lists the files in a folder; a folder that is not there is an empty list.
func (r *Runner) List(ctx context.Context, path string) ([]File, error) {
	out, err := r.run(ctx, nil, "lsjson", "--files-only", "--no-mimetype", path)
	if errors.Is(err, ErrNotFound) {
		return []File{}, nil
	}
	if err != nil {
		return nil, err
	}
	files := []File{}
	if err := json.Unmarshal(out, &files); err != nil {
		return nil, &Error{State: StateError, Text: "rclone's listing could not be read"}
	}
	return files, nil
}

// Cat opens a file on the remote as a stream. Close waits for rclone and answers its error.
func (r *Runner) Cat(ctx context.Context, path string) (io.ReadCloser, error) {
	c := r.cmd(ctx, "cat", path)
	var errb bytes.Buffer
	c.Stderr = &limited{w: &errb, n: 64 << 10}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &catReader{r: out, c: c, errb: &errb, run: r, ctx: ctx}, nil
}

type catReader struct {
	r    io.ReadCloser
	c    *exec.Cmd
	errb *bytes.Buffer
	run  *Runner
	ctx  context.Context
	done bool
	err  error
}

func (c *catReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		if werr := c.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (c *catReader) wait() error {
	if c.done {
		return c.err
	}
	c.done = true
	if err := c.c.Wait(); err != nil {
		c.err = c.run.failure(c.ctx, []string{"cat"}, err, c.errb.String())
	}
	return c.err
}

// Close ends the read. A reader that is closed before the end (the system reads only a file's
// first bytes to see which key it needs) stops rclone; that is no failure and is not logged.
func (c *catReader) Close() error {
	if !c.done {
		c.done = true
		if c.c.Process != nil {
			_ = c.c.Process.Kill()
		}
		_ = c.r.Close()
		_ = c.c.Wait()
		return nil
	}
	return c.r.Close()
}

// ListDirs lists the folders at path; it is the cheap question "does this account answer".
func (r *Runner) ListDirs(ctx context.Context, path string) ([]File, error) {
	out, err := r.run(ctx, nil, "lsjson", "--dirs-only", "--no-mimetype", "--no-modtime", path)
	if err != nil {
		return nil, err
	}
	files := []File{}
	_ = json.Unmarshal(out, &files)
	return files, nil
}

// UserInfo answers who a remote is logged in as (an e-mail address or a name), "" when the backend
// does not tell.
func (r *Runner) UserInfo(ctx context.Context, remote string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := r.run(ctx, nil, "config", "userinfo", remote+":", "--json")
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		return ""
	}
	for _, k := range []string{"email", "mail", "userPrincipalName", "preferred_username", "name", "displayName"} {
		if v, ok := m[k].(string); ok && v != "" && len(v) <= 100 && !strings.ContainsAny(v, "\n\r\t") {
			return v
		}
	}
	return ""
}

// ConfigRename gives a remote another name, replacing a remote of that name: the remote's section
// is rewritten by rclone itself (config create with the old one's values would lose what the
// backend stored after its login), so the file is read and written here - the one place the addon
// touches rclone.conf directly, section headers only.
func (r *Runner) ConfigRename(ctx context.Context, from, to string) error {
	b, err := os.ReadFile(r.Config)
	if err != nil {
		return err
	}
	var out []string
	skip := false
	found := false
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			name := t[1 : len(t)-1]
			skip = name == to
			if name == from {
				l, found = "["+to+"]", true
			}
		}
		if !skip {
			out = append(out, l)
		}
	}
	if !found {
		return &Error{State: StateError, Text: "the new login was not written"}
	}
	tmp := r.Config + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.Config)
}

// JobCommand is an rclone call for a copy job's run: the addon's configuration and limits as every
// call has them, but two transfers, lines as JSON on stderr (one per file and a figure line every
// few seconds), and the output left to the caller to read while it runs.
func (r *Runner) JobCommand(ctx context.Context, args ...string) *exec.Cmd {
	_ = os.MkdirAll(filepath.Join(r.Home, "tmp"), 0o700)
	base := []string{"--retries", "1", "--low-level-retries", "3", "--contimeout", "15s", "--timeout", "2m",
		"--transfers", "2", "--checkers", "4", "--buffer-size", "4M", "--use-mmap",
		"--use-json-log", "--log-level", "INFO", "--stats", "5s", "--stats-log-level", "NOTICE"}
	c := exec.CommandContext(ctx, r.Bin, append(base, args...)...)
	c.Env = r.env()
	c.Dir = r.Home
	c.WaitDelay = 10 * time.Second
	return c
}
