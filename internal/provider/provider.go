// Package provider is the addon's side of openccu-lite's backup-provider contract (occulited's
// docs/backup-provider.md, protocol 1): six HTTP calls on a Unix socket, through which the system
// hands each encrypted backup to one of the addon's accounts and lists, reads and deletes what is
// there. The addon gets bytes, never a path; it only ever sees *.sbk.age; and it serves the
// system's own service alone (the peer's uid is checked on every connection).
package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
)

// Protocol is the contract's version.
const Protocol = 1

// DefaultSocket is where the system expects the addon's socket.
const DefaultSocket = "/run/occulite/backup-targets/sync/target.sock"

// Text is a sentence in German and English.
type Text map[string]string

// Action is a link the system's card shows as a button.
type Action struct {
	Label Text   `json:"label"`
	Href  string `json:"href"`
}

// Remote is what the provider needs of rclone (rclone.Runner; a fake in the tests).
type Remote interface {
	Rcat(ctx context.Context, path string, size int64, in io.Reader, kbps int) error
	Moveto(ctx context.Context, src, dst string) error
	DeleteFile(ctx context.Context, path string) error
	Mkdir(ctx context.Context, path string) error
	List(ctx context.Context, path string) ([]rclone.File, error)
	Cat(ctx context.Context, path string) (io.ReadCloser, error)
	About(ctx context.Context, remote string) (rclone.Space, error)
	HasHashes(ctx context.Context, remote string) bool
}

// Server serves the contract.
type Server struct {
	Accounts *accounts.Store
	Remote   Remote
	// SetupHref is the addon's page, where a login is renewed (the action's link).
	SetupHref string
	// PeerOK says whether a connecting uid may speak; nil = occulite's and root's.
	PeerOK func(uid uint32) bool
	Log    func(format string, args ...any)

	mu      sync.Mutex
	hashes  map[string]bool
	running map[string]bool
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}\.sbk\.age$`)
	folderRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._ -]{0,63}(/[A-Za-z0-9_][A-Za-z0-9._ -]{0,63}){0,5}$`)
	shaRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidFolder is the contract's folder rule: one to six segments, none hidden, no "..".
func ValidFolder(f string) bool {
	if !folderRe.MatchString(f) {
		return false
	}
	for _, p := range strings.Split(f, "/") {
		if p == ".." || p == "." || strings.HasSuffix(p, " ") {
			return false
		}
	}
	return true
}

// messages are the provider's own sentences for a state, per kind of account.
func message(state string, a accounts.Account) Text {
	who := accounts.Label(a.Kind)
	switch state {
	case rclone.StateAuthFailed:
		if accounts.OAuth(a.Kind) && a.Auth != "service-account" {
			return Text{"de": who + " nimmt die Anmeldung dieses Systems nicht mehr an.", "en": who + " no longer accepts this system's login."}
		}
		return Text{"de": who + " lehnt die Zugangsdaten ab.", "en": who + " refuses the credentials."}
	case rclone.StateUnreachable:
		return Text{"de": who + " ist nicht erreichbar.", "en": who + " cannot be reached."}
	case rclone.StateFull:
		return Text{"de": "Der Speicher bei " + who + " ist voll.", "en": "The storage at " + who + " is full."}
	case rclone.StateReadOnly:
		return Text{"de": who + " lässt das Schreiben in diesen Ordner nicht zu.", "en": who + " does not allow writing to this folder."}
	}
	return nil
}

func (s *Server) action(state string, a accounts.Account) *Action {
	if state != rclone.StateAuthFailed || s.SetupHref == "" {
		return nil
	}
	label := Text{"de": "Zugangsdaten ändern", "en": "Change the credentials"}
	if accounts.OAuth(a.Kind) && a.Auth != "service-account" {
		label = Text{"de": "Erneut anmelden", "en": "Log in again"}
	}
	return &Action{Label: label, Href: s.SetupHref}
}

// answer is the body every call shares for a state.
type answer struct {
	State   string  `json:"state"`
	Message Text    `json:"message,omitempty"`
	Action  *Action `json:"action,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail answers a failed call: the state rclone's error means, the sentence and the action for it,
// and - for a state without a sentence of its own - rclone's line.
func (s *Server) fail(w http.ResponseWriter, a accounts.Account, err error) {
	state := rclone.StateOf(err)
	msg := message(state, a)
	if msg == nil {
		line := err.Error()
		msg = Text{"de": "Fehler: " + line, "en": "Error: " + line}
	}
	writeJSON(w, http.StatusOK, answer{State: state, Message: msg, Action: s.action(state, a)})
}

func refuse(w http.ResponseWriter, status int, de, en string) {
	writeJSON(w, status, answer{State: rclone.StateError, Message: Text{"de": de, "en": en}})
}

// Handler is the contract's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.info)
	mux.HandleFunc("POST /v1/destinations/{id}/test", s.test)
	mux.HandleFunc("PUT /v1/destinations/{id}/files/{name}", s.put)
	mux.HandleFunc("GET /v1/destinations/{id}/files", s.list)
	mux.HandleFunc("GET /v1/destinations/{id}/files/{name}", s.get)
	mux.HandleFunc("DELETE /v1/destinations/{id}/files/{name}", s.del)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		refuse(w, http.StatusNotFound, "Diesen Aufruf kennt das Addon nicht.", "The addon does not know this call.")
	})
	return mux
}

type destination struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Detail  string  `json:"detail,omitempty"`
	State   string  `json:"state"`
	Message Text    `json:"message,omitempty"`
	Action  *Action `json:"action,omitempty"`
}

// info names the accounts with the state the last test or transfer left them in: nothing is
// contacted for it.
func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	out := []destination{}
	for _, a := range s.Accounts.List() {
		d := destination{ID: a.ID, Name: a.Name, Detail: a.Detail, State: rclone.StateWritable}
		// only a refused login is the account's own lasting state; an unreachable network last
		// night says nothing about now
		if a.State == rclone.StateAuthFailed {
			d.State, d.Message, d.Action = a.State, message(a.State, a), s.action(a.State, a)
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"protocol": Protocol, "destinations": out})
}

// target reads the account and the folder of a call.
func (s *Server) target(w http.ResponseWriter, r *http.Request, folder string) (accounts.Account, string, bool) {
	a, ok := s.Accounts.Get(r.PathValue("id"))
	if !ok {
		refuse(w, http.StatusNotFound, "Dieses Konto gibt es nicht mehr.", "This account no longer exists.")
		return a, "", false
	}
	if !ValidFolder(folder) {
		refuse(w, http.StatusBadRequest, "Der Ordner ist kein gültiger Ordnername.", "The folder is not a valid folder name.")
		return a, "", false
	}
	return a, rclone.Path(a.Remote(), a.Root, folder), true
}

func (s *Server) fileName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) || strings.Contains(name, "..") {
		refuse(w, http.StatusBadRequest, "Das ist nicht der Name einer verschlüsselten Sicherung.", "That is not the name of an encrypted backup.")
		return "", false
	}
	return name, true
}

func (s *Server) hasHashes(ctx context.Context, a accounts.Account) bool {
	s.mu.Lock()
	v, ok := s.hashes[a.Kind]
	s.mu.Unlock()
	if ok {
		return v
	}
	v = s.Remote.HasHashes(ctx, a.Remote())
	s.mu.Lock()
	if s.hashes == nil {
		s.hashes = map[string]bool{}
	}
	s.hashes[a.Kind] = v
	s.mu.Unlock()
	return v
}

// test writes a small file into the folder, reads it back and deletes it.
func (s *Server) test(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, "Die Anfrage ist kein JSON.", "The request is not JSON.")
		return
	}
	a, dir, ok := s.target(w, r, body.Folder)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	err := s.probe(ctx, dir)
	var space rclone.Space
	if err == nil {
		space, _ = s.Remote.About(ctx, a.Remote())
	}
	_, _ = s.Accounts.Note(a.ID, err, &space)
	if err != nil {
		s.logf("test of %s (%s) in %s: %v", a.Name, a.ID, body.Folder, err)
		s.fail(w, a, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": rclone.StateWritable, "free_bytes": space.Free, "total_bytes": space.Total})
}

func (s *Server) probe(ctx context.Context, dir string) error {
	if err := s.Remote.Mkdir(ctx, dir); err != nil {
		return err
	}
	content := make([]byte, 1024)
	_, _ = rand.Read(content)
	tag := make([]byte, 6)
	_, _ = rand.Read(tag)
	path := dir + "/openccu-lite-test-" + hex.EncodeToString(tag) + ".tmp"
	if err := s.Remote.Rcat(ctx, path, int64(len(content)), bytes.NewReader(content), 0); err != nil {
		return err
	}
	rc, err := s.Remote.Cat(ctx, path)
	if err != nil {
		_ = s.Remote.DeleteFile(context.WithoutCancel(ctx), path)
		return err
	}
	back, err := io.ReadAll(io.LimitReader(rc, 2048))
	rc.Close()
	if err == nil && !bytes.Equal(back, content) {
		err = &rclone.Error{State: rclone.StateError, Text: "the test file read back differs from the one written"}
	}
	if derr := s.Remote.DeleteFile(context.WithoutCancel(ctx), path); err == nil {
		err = derr
	}
	return err
}

// lock keeps one upload per file name at a time.
func (s *Server) lock(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]bool{}
	}
	if s.running[key] {
		return false
	}
	s.running[key] = true
	return true
}

func (s *Server) unlock(key string) {
	s.mu.Lock()
	delete(s.running, key)
	s.mu.Unlock()
}

// put takes the backup as the body: uploaded as <name>.partial while its SHA-256 is computed,
// refused when the body is not what the header said, verified (rclone compares the destination's
// own hash with the one it computed while reading, where the destination has one; the size
// otherwise) and renamed.
func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	name, ok := s.fileName(w, r)
	if !ok {
		return
	}
	a, dir, ok := s.target(w, r, r.URL.Query().Get("folder"))
	if !ok {
		return
	}
	want := strings.ToLower(r.Header.Get("X-SHA256"))
	size := r.ContentLength
	if size <= 0 || !shaRe.MatchString(want) {
		refuse(w, http.StatusBadRequest, "Die Sicherung kam ohne Länge oder ohne Prüfsumme an.", "The backup arrived without its length or its checksum.")
		return
	}
	key := a.ID + "|" + dir + "|" + name
	if !s.lock(key) {
		refuse(w, http.StatusConflict, "Diese Sicherung wird gerade schon hochgeladen.", "This backup is being uploaded already.")
		return
	}
	defer s.unlock(key)
	start := time.Now()
	ctx := r.Context()
	final, partial := dir+"/"+name, dir+"/"+name+".partial"
	clean := func() { _ = s.Remote.DeleteFile(context.WithoutCancel(ctx), partial) }
	done := func(err error) {
		_, _ = s.Accounts.Note(a.ID, err, nil)
		s.Accounts.NoteUpload(a.ID, accounts.Upload{At: time.Now().UTC(), OK: err == nil, Name: name, Size: size})
	}
	h := sha256.New()
	body := &countReader{r: io.TeeReader(io.LimitReader(r.Body, size), h)}
	if err := s.Remote.Rcat(ctx, partial, size, body, a.UploadLimit); err != nil {
		clean()
		done(err)
		s.logf("upload of %s to %s (%s) failed after %s: %v", name, a.Name, a.ID, time.Since(start).Round(time.Second), err)
		s.fail(w, a, err)
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); body.n != size || got != want {
		clean()
		err := &rclone.Error{State: rclone.StateError, Text: fmt.Sprintf("the backup arrived damaged (%d of %d bytes, the checksum differs)", body.n, size)}
		done(err)
		s.logf("upload of %s to %s (%s): %v", name, a.Name, a.ID, err)
		writeJSON(w, http.StatusOK, answer{State: rclone.StateError, Message: Text{"de": "Die Sicherung kam beschädigt beim Addon an.", "en": "The backup arrived damaged at the addon."}})
		return
	}
	if err := s.Remote.Moveto(ctx, partial, final); err != nil {
		clean()
		done(err)
		s.fail(w, a, err)
		return
	}
	stored := int64(-1)
	files, err := s.Remote.List(ctx, dir)
	for _, f := range files {
		if f.Name == name {
			stored = f.Size
		}
	}
	if err == nil && stored != size {
		err = &rclone.Error{State: rclone.StateError, Text: fmt.Sprintf("the file on the account has %d bytes, %d were sent", stored, size)}
	}
	if err != nil {
		done(err)
		s.fail(w, a, err)
		return
	}
	verified := "size"
	if s.hasHashes(ctx, a) {
		verified = "hash"
	}
	done(nil)
	s.logf("uploaded %s (%d bytes) to %s (%s) in %s, verified by %s", name, size, a.Name, a.ID, time.Since(start).Round(time.Millisecond), verified)
	writeJSON(w, http.StatusOK, map[string]any{"state": rclone.StateWritable, "size": stored, "verified": verified, "duration_ms": time.Since(start).Milliseconds()})
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	a, dir, ok := s.target(w, r, r.URL.Query().Get("folder"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	files, err := s.Remote.List(ctx, dir)
	if err != nil {
		_, _ = s.Accounts.Note(a.ID, err, nil)
		writeErr(s, w, a, err)
		return
	}
	type file struct {
		Name string    `json:"name"`
		Size int64     `json:"size"`
		Time time.Time `json:"time"`
	}
	out := []file{}
	for _, f := range files {
		if nameRe.MatchString(f.Name) {
			out = append(out, file{Name: f.Name, Size: f.Size, Time: f.ModTime})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

// writeErr answers a failed list, get or delete with a status that is not 2xx.
func writeErr(s *Server, w http.ResponseWriter, a accounts.Account, err error) {
	state := rclone.StateOf(err)
	msg := message(state, a)
	if msg == nil {
		msg = Text{"de": "Fehler: " + err.Error(), "en": "Error: " + err.Error()}
	}
	status := http.StatusBadGateway
	if errors.Is(err, rclone.ErrNotFound) {
		status, state = http.StatusNotFound, rclone.StateError
		msg = Text{"de": "Diese Datei gibt es dort nicht.", "en": "There is no such file there."}
	}
	writeJSON(w, status, answer{State: state, Message: msg, Action: s.action(state, a)})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	name, ok := s.fileName(w, r)
	if !ok {
		return
	}
	a, dir, ok := s.target(w, r, r.URL.Query().Get("folder"))
	if !ok {
		return
	}
	// the size first: it is the answer's length, and a file that is not there is a 404 before any byte
	files, err := s.Remote.List(r.Context(), dir)
	if err != nil {
		writeErr(s, w, a, err)
		return
	}
	size := int64(-1)
	for _, f := range files {
		if f.Name == name {
			size = f.Size
		}
	}
	if size < 0 {
		writeErr(s, w, a, rclone.ErrNotFound)
		return
	}
	rc, err := s.Remote.Cat(r.Context(), dir+"/"+name)
	if err != nil {
		writeErr(s, w, a, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	cw := &clientWriter{w: w}
	n, err := io.Copy(cw, rc)
	// the length was promised: a short body is the reader's sign that the file did not arrive. The
	// system often wants only the first bytes (an encrypted backup's header names its key) and
	// hangs up; that is no failed download and is not logged.
	if (err != nil || n != size) && cw.err == nil && r.Context().Err() == nil {
		s.logf("download of %s from %s (%s) ended after %d of %d bytes: %v", name, a.Name, a.ID, n, size, err)
	}
}

// clientWriter remembers whether a write to the client failed: then the client went away.
type clientWriter struct {
	w   io.Writer
	err error
}

func (c *clientWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err != nil && c.err == nil {
		c.err = err
	}
	return n, err
}

func (s *Server) del(w http.ResponseWriter, r *http.Request) {
	name, ok := s.fileName(w, r)
	if !ok {
		return
	}
	a, dir, ok := s.target(w, r, r.URL.Query().Get("folder"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	if err := s.Remote.DeleteFile(ctx, dir+"/"+name); err != nil {
		writeErr(s, w, a, err)
		return
	}
	s.logf("deleted %s on %s (%s)", name, a.Name, a.ID)
	w.WriteHeader(http.StatusNoContent)
}

// Listen binds the socket: a socket a previous run left is removed, the mode is 0666 - the
// directory, which the system made for the addon and its own service alone, is what keeps
// everyone else out, and a connecting process needs the write bit on the socket itself.
func Listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

type peerKey struct{}

// Serve serves the contract on l until ctx ends. Every connection's peer is asked for its uid
// (SO_PEERCRED); a peer that is not the system's service gets 403 on every request.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	h := s.Handler()
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			uid, ok := peerUID(c)
			return context.WithValue(ctx, peerKey{}, peer{uid: uid, ok: ok})
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := r.Context().Value(peerKey{}).(peer)
			if !p.ok || !s.peerOK(p.uid) {
				s.logf("refused a connection from uid %d: only the system's service speaks here", p.uid)
				refuse(w, http.StatusForbidden, "Nur der Systemdienst darf hier sprechen.", "Only the system's service may speak here.")
				return
			}
			h.ServeHTTP(w, r)
		}),
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type peer struct {
	uid uint32
	ok  bool
}

func (s *Server) peerOK(uid uint32) bool {
	if s.PeerOK != nil {
		return s.PeerOK(uid)
	}
	return uid == 0
}

// peerUID reads the uid of a Unix connection's peer.
func peerUID(c net.Conn) (uint32, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || cerr != nil || cred == nil {
		return 0, false
	}
	return cred.Uid, true
}
