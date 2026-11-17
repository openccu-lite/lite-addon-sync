// Package servers keeps the SSH servers copy jobs send folders to with rsync: a place (host, port,
// user, base path), a key pair the addon makes for it, and the server's host key, which is read,
// shown and only then trusted. Logins are by key alone - there is no password field - and a run
// never accepts a host key on the way: an unknown or changed one fails it. The programs are the
// system's own (ssh, ssh-keygen, ssh-keyscan, rsync); every call is an argument list.
package servers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The states of a server's pill.
const (
	StateNotTested      = "not-tested"
	StateOK             = "ok"
	StateUnreachable    = "unreachable"
	StateAuthFailed     = "auth-failed"
	StateHostKeyUnknown = "hostkey-unknown"
	StateHostKeyChanged = "hostkey-changed"
	StateNoRsync        = "no-rsync"
	StateReadOnly       = "read-only"
	StateError          = "error"
)

// HostKey is one key a server showed.
type HostKey struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"` // SHA256:…, as ssh prints it
}

// Server is one SSH server.
type Server struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	// Path is the base path on the server: a job writes below it, never beside it.
	Path    string    `json:"path"`
	Created time.Time `json:"created"`
	// PublicKey is the line for the server's authorized_keys, with "restrict" in front.
	PublicKey string `json:"public_key"`
	// HostKeys are the trusted keys (the server's known_hosts file holds them); Pending the ones the
	// last scan read and nobody has confirmed yet.
	HostKeys []HostKey `json:"host_keys"`
	Pending  []HostKey `json:"pending,omitempty"`
	State    string    `json:"state"`
	Text     string    `json:"text,omitempty"`
	Checked  time.Time `json:"checked,omitzero"`
}

// Spec is what the user enters.
type Spec struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	Path string `json:"path"`
}

// ValidationError is a field the user must correct.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, a...)} }

// ErrNotFound: no server of that id.
var ErrNotFound = errors.New("no such server")

var (
	hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,251}[A-Za-z0-9])?$|^[0-9A-Fa-f:]{2,45}$`)
	userRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`)
	// a path on the server: nothing a remote shell or rsync reads as more than a path
	pathRe = regexp.MustCompile(`^[A-Za-z0-9_./@+ -]{1,200}$`)
)

// CheckPath is the rule for a path on a server (the base path, a job's folder): plain characters,
// no leading dash, no "..", no leading or trailing space.
func CheckPath(p string) error {
	if !pathRe.MatchString(p) || strings.HasPrefix(p, "-") || p != strings.TrimSpace(p) {
		return invalid("path: letters, digits, / . _ - + @ and spaces, not starting with a dash")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || strings.HasPrefix(seg, "-") {
			return invalid("path: no \"..\" and no part that starts with a dash")
		}
	}
	return nil
}

func (sp *Spec) clean() error {
	sp.Name, sp.Host, sp.User, sp.Path = strings.TrimSpace(sp.Name), strings.TrimSpace(sp.Host), strings.TrimSpace(sp.User), strings.TrimSpace(sp.Path)
	if sp.Name == "" || len(sp.Name) > 60 || strings.ContainsAny(sp.Name, "\n\r\t\x00") {
		return invalid("name: 1 to 60 characters")
	}
	if !hostRe.MatchString(sp.Host) || strings.HasPrefix(sp.Host, "-") {
		return invalid("host: a host name or an address")
	}
	if sp.Port == 0 {
		sp.Port = 22
	}
	if sp.Port < 1 || sp.Port > 65535 {
		return invalid("port: 1 to 65535")
	}
	if !userRe.MatchString(sp.User) {
		return invalid("user: the account's name on the server")
	}
	if sp.Path == "" {
		return invalid("path: the folder on the server everything is written below")
	}
	return CheckPath(sp.Path)
}

// Tools are the programs; empty names are looked up on the PATH.
type Tools struct {
	SSH, Keygen, Keyscan, Rsync string
}

func (t Tools) bin(set, name string) string {
	if set != "" {
		return set
	}
	return name
}

// File is the servers' file in the configuration directory.
const File = "servers.json"

type file struct {
	Version int      `json:"version"`
	Servers []Server `json:"servers"`
}

// Store keeps the servers.
type Store struct {
	// Dir is the configuration directory (0700); a server's key and known_hosts lie in ssh/<id>/.
	Dir   string
	Tools Tools
	// Home is a directory ssh may treat as its home (it wants one; nothing is read from it).
	Home string
	Log  func(format string, args ...any)

	mu   sync.Mutex
	list []Server
}

// Open reads the servers.
func Open(dir string, tools Tools, home string) (*Store, error) {
	s := &Store{Dir: dir, Tools: tools, Home: home}
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
	s.list = f.Servers
	return s, nil
}

// Load reads the servers for a reader that changes nothing (a job's run).
func Load(dir string) ([]Server, error) {
	s, err := Open(dir, Tools{}, "")
	if err != nil {
		return nil, err
	}
	return s.list, nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(file{Version: 1, Servers: s.list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, File+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, File))
}

// List answers the servers, oldest first.
func (s *Store) List() []Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Server{}, s.list...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Get answers one server.
func (s *Store) Get(id string) (Server, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.list {
		if v.ID == id {
			return v, true
		}
	}
	return Server{}, false
}

func (s *Store) update(id string, f func(*Server)) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].ID == id {
			f(&s.list[i])
			return s.list[i], s.save()
		}
	}
	return Server{}, ErrNotFound
}

var idRe = regexp.MustCompile(`^[a-z0-9]{10}$`)

func newID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// KeyDir is where a server's key pair and known_hosts lie.
func KeyDir(confDir, id string) string { return filepath.Join(confDir, "ssh", id) }

// Create adds a server and makes its key pair.
func (s *Store) Create(ctx context.Context, sp Spec) (Server, error) {
	if err := sp.clean(); err != nil {
		return Server{}, err
	}
	v := Server{ID: newID(), Name: sp.Name, Host: sp.Host, Port: sp.Port, User: sp.User, Path: sp.Path, Created: time.Now().UTC(), State: StateNotTested, HostKeys: []HostKey{}}
	pub, err := s.keygen(ctx, v.ID)
	if err != nil {
		return Server{}, err
	}
	v.PublicKey = pub
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, v)
	return v, s.save()
}

// keygen makes a fresh ed25519 pair for the server, replacing one that is there, and answers the
// authorized_keys line.
func (s *Store) keygen(ctx context.Context, id string) (string, error) {
	dir := KeyDir(s.Dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	key := filepath.Join(dir, "id_ed25519")
	_ = os.Remove(key)
	_ = os.Remove(key + ".pub")
	out, err := exec.CommandContext(ctx, s.Tools.bin(s.Tools.Keygen, "ssh-keygen"), "-q", "-t", "ed25519", "-N", "", "-C", "openccu-lite sync", "-f", key).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(key, 0o600); err != nil {
		return "", err
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", err
	}
	return "restrict " + strings.TrimSpace(string(pub)), nil
}

// NewKey replaces the server's key pair: the old key stops working.
func (s *Store) NewKey(ctx context.Context, id string) (Server, error) {
	if _, ok := s.Get(id); !ok {
		return Server{}, ErrNotFound
	}
	pub, err := s.keygen(ctx, id)
	if err != nil {
		return Server{}, err
	}
	return s.update(id, func(v *Server) { v.PublicKey, v.State, v.Text = pub, StateNotTested, "" })
}

// Update changes a server. Another host or port is another machine: its trusted host keys go.
func (s *Store) Update(id string, sp Spec) (Server, error) {
	if err := sp.clean(); err != nil {
		return Server{}, err
	}
	return s.update(id, func(v *Server) {
		if v.Host != sp.Host || v.Port != sp.Port {
			v.HostKeys, v.Pending = []HostKey{}, nil
			_ = os.Remove(filepath.Join(KeyDir(s.Dir, id), "known_hosts"))
			_ = os.Remove(filepath.Join(KeyDir(s.Dir, id), "known_hosts.pending"))
		}
		if v.Host != sp.Host || v.Port != sp.Port || v.User != sp.User || v.Path != sp.Path {
			v.State, v.Text = StateNotTested, ""
		}
		v.Name, v.Host, v.Port, v.User, v.Path = sp.Name, sp.Host, sp.Port, sp.User, sp.Path
	})
}

// Delete removes a server with its key and known_hosts; nothing on the server is touched.
func (s *Store) Delete(id string) error {
	if !idRe.MatchString(id) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].ID == id {
			s.list = append(s.list[:i], s.list[i+1:]...)
			_ = os.RemoveAll(KeyDir(s.Dir, id))
			return s.save()
		}
	}
	return ErrNotFound
}

var keyLineRe = regexp.MustCompile(`^(\S+) (ssh-ed25519|ecdsa-sha2-nistp(256|384|521)|ssh-rsa|rsa-sha2-(256|512)) ([A-Za-z0-9+/=]+)$`)

// Fingerprint is the SHA256 fingerprint of a key blob in base64, as ssh prints it.
func Fingerprint(blob string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// Scan reads the server's host keys with ssh-keyscan and keeps them as pending: nothing is trusted
// until Trust names what the user compared.
func (s *Store) Scan(ctx context.Context, id string) (Server, error) {
	v, ok := s.Get(id)
	if !ok {
		return Server{}, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, s.Tools.bin(s.Tools.Keyscan, "ssh-keyscan"), "-T", "5", "-p", strconv.Itoa(v.Port), "-t", "ed25519,ecdsa,rsa", "--", v.Host)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	_ = c.Run()
	var lines []string
	var keys []HostKey
	for _, l := range strings.Split(out.String(), "\n") {
		l = strings.TrimSpace(l)
		m := keyLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		fp, err := Fingerprint(m[5])
		if err != nil {
			continue
		}
		lines = append(lines, l)
		keys = append(keys, HostKey{Type: m[2], Fingerprint: fp})
	}
	if len(keys) == 0 {
		text := lastLine(errb.String())
		if text == "" {
			text = "the server showed no host key"
		}
		return s.update(id, func(v *Server) { v.State, v.Text, v.Checked, v.Pending = StateUnreachable, text, time.Now().UTC(), nil })
	}
	dir := KeyDir(s.Dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Server{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "known_hosts.pending"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return Server{}, err
	}
	return s.update(id, func(v *Server) { v.Pending = keys })
}

// Trust makes the pending host keys the trusted ones, when fingerprints names every one of them
// (what the page showed and the user compared); anything else trusts nothing.
func (s *Store) Trust(id string, fingerprints []string) (Server, error) {
	v, ok := s.Get(id)
	if !ok {
		return Server{}, ErrNotFound
	}
	if len(v.Pending) == 0 {
		return Server{}, invalid("no host key was read: read the host key first")
	}
	want := map[string]bool{}
	for _, f := range fingerprints {
		want[f] = true
	}
	for _, k := range v.Pending {
		if !want[k.Fingerprint] {
			return Server{}, invalid("the host keys changed since they were shown: read them again")
		}
	}
	dir := KeyDir(s.Dir, id)
	if err := os.Rename(filepath.Join(dir, "known_hosts.pending"), filepath.Join(dir, "known_hosts")); err != nil {
		return Server{}, err
	}
	return s.update(id, func(v *Server) {
		v.HostKeys, v.Pending = v.Pending, nil
		if v.State == StateHostKeyUnknown || v.State == StateHostKeyChanged {
			v.State, v.Text = StateNotTested, ""
		}
	})
}

// RSH is the remote shell rsync uses for the server: ssh with the server's key and known_hosts and
// nothing else - no agent, no other identity, no question, an unknown or changed host key refused.
// The paths are the addon's own (no spaces); rsync splits the string at spaces.
func RSH(tools Tools, confDir string, v Server) string {
	dir := KeyDir(confDir, v.ID)
	return strings.Join([]string{tools.bin(tools.SSH, "ssh"), "-p", strconv.Itoa(v.Port), "-i", filepath.Join(dir, "id_ed25519"),
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + filepath.Join(dir, "known_hosts"), "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=4", "-o", "LogLevel=ERROR",
		"-F", "/dev/null"}, " ")
}

// Dest is user@host:path for rsync; an IPv6 address goes in brackets.
func Dest(v Server, folder string) string {
	host := v.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	p := strings.TrimRight(v.Path, "/")
	if f := strings.Trim(folder, "/"); f != "" {
		p += "/" + f
	}
	return v.User + "@" + host + ":" + p + "/"
}

// Classify maps what rsync and ssh said, and rsync's exit code, onto a state.
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
	case has("remote host identification has changed", "host key for", "offending"):
		return StateHostKeyChanged
	case has("host key verification failed", "no ed25519 host key is known", "no ecdsa host key is known", "no rsa host key is known", "host key is known for"):
		return StateHostKeyUnknown
	case has("permission denied (publickey", "permission denied (password", "permission denied (keyboard", "too many authentication failures", "no supported authentication"):
		return StateAuthFailed
	case has("could not resolve hostname", "connection refused", "connection timed out", "no route to host", "network is unreachable", "connection closed by", "connection reset", "operation timed out", "timed out", "name or service not known"):
		return StateUnreachable
	case has("rsync: command not found", "rsync: not found", "remote command not found", "command not found"), exit == 127:
		return StateNoRsync
	case has("permission denied (13)", "read-only file system", "operation not permitted"):
		return StateReadOnly
	case exit == 0:
		return StateOK
	}
	return StateError
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "rsync error:") {
			if r := []rune(l); len(r) > 300 {
				return string(r[:300]) + "…"
			}
			return l
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// Test tries the server as a job would use it: rsync over ssh writes a small file below the base
// path and removes it again. So it proves the key, the host key, rsync on the server and the right
// to write - also on a server that allows rsync and nothing else.
func (s *Store) Test(ctx context.Context, id string) (Server, error) {
	v, ok := s.Get(id)
	if !ok {
		return Server{}, ErrNotFound
	}
	if len(v.HostKeys) == 0 {
		return s.update(id, func(v *Server) {
			v.State, v.Text, v.Checked = StateHostKeyUnknown, "the server's host key is not confirmed yet", time.Now().UTC()
		})
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	tmp, err := os.MkdirTemp(s.Home, "test-")
	if err != nil {
		return Server{}, err
	}
	defer os.RemoveAll(tmp)
	const name = ".lite-sync-test"
	if err := os.WriteFile(filepath.Join(tmp, name), []byte("openccu-lite sync: a test file, safe to delete\n"), 0o600); err != nil {
		return Server{}, err
	}
	run := func(args ...string) (string, int) {
		c := exec.CommandContext(ctx, s.Tools.bin(s.Tools.Rsync, "rsync"), args...)
		c.Env = append(os.Environ(), "HOME="+s.Home, "RSYNC_RSH="+RSH(s.Tools, s.Dir, v))
		var errb bytes.Buffer
		c.Stderr = &errb
		err := c.Run()
		exit := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else if err != nil {
			return err.Error(), -1
		}
		return errb.String(), exit
	}
	dest := Dest(v, "")
	// write it, then remove it: an empty directory as the source, with only the test file allowed
	// to go. (No --mkpath: the rsync of many a NAS does not know it; rsync makes the last part of
	// the base path itself, the folders above it must be there.)
	stderr, exit := run("-s", "--timeout=30", "--", filepath.Join(tmp, name), dest)
	if exit == 0 {
		empty := filepath.Join(tmp, "empty")
		_ = os.Mkdir(empty, 0o700)
		stderr, exit = run("-s", "-r", "--timeout=30", "--delete", "--include=/"+name, "--exclude=*", "--", empty+"/", dest)
	}
	state, text := Classify(stderr, exit), ""
	if state != StateOK {
		text = lastLine(stderr)
		if s.Log != nil {
			s.Log("the test of the server %s failed: rsync exit %d, %s: %s", v.Name, exit, state, text)
		}
	}
	return s.update(id, func(v *Server) { v.State, v.Text, v.Checked = state, text, time.Now().UTC() })
}

// Note records what a job's run learned about the server.
func (s *Store) Note(id, state, text string) {
	_, _ = s.update(id, func(v *Server) { v.State, v.Text, v.Checked = state, text, time.Now().UTC() })
}
