// Package httpapi is the addon's own HTTP server: the Svelte page's files and its JSON API under
// one base path (/addons/sync/), which lighttpd proxies to the loopback port. Every request
// carries the shell's session, confirmed through the kit (administrators only); writes carry the
// kit's CSRF header. docs/api.md documents the routes. No answer ever holds a secret: a token, a
// key or a password goes in and is never read back.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/hobbyquaker/ccu-addon-kit/go/addonkit"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/jobs"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
	"github.com/openccu-lite/lite-addon-sync/internal/servers"
	"github.com/openccu-lite/lite-addon-sync/internal/sysinfo"
)

// FactsReader answers the system's facts (sysinfo.Client; a fake in the tests).
type FactsReader interface {
	Facts(ctx context.Context, fresh bool) sysinfo.Facts
}

// Options configure the server.
type Options struct {
	// BasePath is where the page is served, /addons/sync (no trailing slash).
	BasePath string
	// WWW is the directory with the built page.
	WWW      string
	Accounts *accounts.Store
	Flows    *accounts.Flows
	Facts    FactsReader
	// Wrap is the session check; nil: none (development only).
	Wrap    func(http.Handler) http.Handler
	Version string
	// Rclone is the version of the rclone inside.
	Rclone  string
	Started time.Time
	// Provider says whether the backup provider's socket is being served, and why not.
	Provider func() (ok bool, problem string)
	// Servers, Jobs and Timers are the copy jobs' side: the SSH servers, the jobs' records, and the
	// system's timers. AddonDir holds the history (var/runs), RunDir a running job's progress.
	Servers  *servers.Store
	Jobs     *jobs.Store
	Timers   TimersAPI
	AddonID  string
	AddonDir string
	RunDir   string
	// Log writes a line to the journal; nil = none.
	Log func(format string, args ...any)
}

// Routes lists every "METHOD /path" the API serves; the documentation is held to it by a test.
var Routes []string

type server struct{ o Options }

func handle(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	Routes = append(Routes, pattern)
	mux.HandleFunc(pattern, h)
}

// New builds the handler.
func New(o Options) http.Handler {
	o.BasePath = strings.TrimRight(o.BasePath, "/")
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.AddonID == "" {
		o.AddonID = "sync"
	}
	s := &server{o: o}
	Routes = nil
	api := http.NewServeMux()
	handle(api, "GET /api/v1/health", s.health)
	handle(api, "GET /api/v1/state", s.state)
	handle(api, "POST /api/v1/accounts", s.create)
	handle(api, "PUT /api/v1/accounts/{id}", s.rename)
	handle(api, "PUT /api/v1/accounts/{id}/credentials", s.credentials)
	handle(api, "DELETE /api/v1/accounts/{id}", s.remove)
	handle(api, "POST /api/v1/accounts/{id}/test", s.test)
	handle(api, "POST /api/v1/login", s.loginStart)
	handle(api, "POST /api/v1/login/finish", s.loginFinish)
	handle(api, "DELETE /api/v1/login", s.loginCancel)
	s.registerJobs(api)
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		addonkit.WriteError(w, http.StatusNotFound, "not-found", r.URL.Path)
	})

	root := http.NewServeMux()
	root.Handle("/api/", addonkit.CSRF(api))
	root.Handle("/", addonkit.AppFiles(o.WWW))

	var h http.Handler = http.StripPrefix(o.BasePath, root)
	if o.Wrap != nil {
		h = o.Wrap(h)
	}
	return guard(h)
}

func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p != "/" && (path.Clean(p) != strings.TrimSuffix(p, "/") && path.Clean(p) != p) {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		addonkit.WriteError(w, http.StatusBadRequest, "invalid", err.Error())
		return false
	}
	return true
}

// writeErr maps an error onto the API's answers: a field to correct (422 invalid), an account
// that is not there (404), a login that is not waiting (409), the account's own refusal (502 with
// the state: auth-failed, unreachable, full, read-only, error).
func writeErr(w http.ResponseWriter, err error) {
	var ve *accounts.ValidationError
	var re *rclone.Error
	switch {
	case errors.As(err, &ve):
		addonkit.WriteError(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case errors.Is(err, accounts.ErrNotFound):
		addonkit.WriteError(w, http.StatusNotFound, "not-found", err.Error())
	case errors.Is(err, accounts.ErrBusy):
		addonkit.WriteError(w, http.StatusConflict, "login-busy", err.Error())
	case errors.Is(err, accounts.ErrNoFlow):
		addonkit.WriteError(w, http.StatusConflict, "login-gone", err.Error())
	case errors.As(err, &re):
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "account", "state": re.State, "message": re.Text})
	case errors.Is(err, rclone.ErrNotFound):
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "account", "state": rclone.StateError, "message": "the bucket or folder does not exist: " + err.Error()})
	default:
		addonkit.WriteError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "version": s.o.Version, "rclone": s.o.Rclone, "uptime_s": int(time.Since(s.o.Started).Seconds())})
}

// KindView is a kind of account the page offers.
type KindView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	OAuth bool   `json:"oauth"`
}

// AccountView is an account with the backup targets that use it.
type AccountView struct {
	accounts.Account
	UsedBy []sysinfo.Target `json:"used_by"`
	// Jobs are the names of the copy jobs that copy to the account.
	Jobs []string `json:"jobs"`
}

// LoginView is the login that waits for its paste.
type LoginView struct {
	ID      string    `json:"id"`
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"`
}

// StateView is GET /api/v1/state.
type StateView struct {
	Version  string        `json:"version"`
	Rclone   string        `json:"rclone"`
	Kinds    []KindView    `json:"kinds"`
	Accounts []AccountView `json:"accounts"`
	Login    *LoginView    `json:"login,omitempty"`
	System   sysinfo.Facts `json:"system"`
	// Provider: the socket the system reaches the addon through is served; ProviderProblem why not.
	Provider        bool   `json:"provider"`
	ProviderProblem string `json:"provider_problem,omitempty"`
	Now             int64  `json:"now"`
	// Servers are the SSH servers copy jobs send to with rsync; Copy the copy jobs.
	Servers []ServerView `json:"servers"`
	Copy    JobsView     `json:"copy"`
}

func (s *server) view(ctx context.Context, fresh bool) StateView {
	facts := s.o.Facts.Facts(ctx, fresh)
	v := StateView{Version: s.o.Version, Rclone: s.o.Rclone, System: facts, Accounts: []AccountView{}, Now: time.Now().UnixMilli()}
	for _, k := range accounts.Kinds {
		v.Kinds = append(v.Kinds, KindView{ID: k, Label: accounts.Label(k), OAuth: accounts.OAuth(k)})
	}
	for _, a := range s.o.Accounts.List() {
		used := facts.Targets[a.ID]
		if used == nil {
			used = []sysinfo.Target{}
		}
		v.Accounts = append(v.Accounts, AccountView{Account: a, UsedBy: used, Jobs: jobNames(s.o.Jobs.UsesTarget(jobs.TargetAccount, a.ID))})
	}
	v.Servers = []ServerView{}
	for _, sv := range s.o.Servers.List() {
		v.Servers = append(v.Servers, s.serverView(sv))
	}
	v.Copy = s.jobsView(ctx)
	if f := s.o.Flows.Pending(); f != nil {
		v.Login = &LoginView{ID: f.ID, URL: f.URL, Expires: f.Expires}
	}
	if s.o.Provider != nil {
		v.Provider, v.ProviderProblem = s.o.Provider()
	}
	return v
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.view(r.Context(), r.URL.Query().Get("refresh") == "1"))
}

func (s *server) one(ctx context.Context, a accounts.Account) AccountView {
	used := s.o.Facts.Facts(ctx, false).Targets[a.ID]
	if used == nil {
		used = []sysinfo.Target{}
	}
	return AccountView{Account: a, UsedBy: used, Jobs: jobNames(s.o.Jobs.UsesTarget(jobs.TargetAccount, a.ID))}
}

// create is POST /api/v1/accounts: an account that needs no login in a browser (S3, WebDAV, Google
// Drive with a service account).
func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var sp accounts.Spec
	if !decode(w, r, &sp) {
		return
	}
	if accounts.OAuth(sp.Kind) && sp.ServiceAccount == "" {
		addonkit.WriteError(w, http.StatusUnprocessableEntity, "invalid", "this kind of account logs in at its provider: POST /api/v1/login")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	a, err := s.o.Accounts.Create(ctx, sp, "")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.one(r.Context(), a))
}

// credentials is PUT /api/v1/accounts/{id}/credentials: new keys for an account that has no login
// in a browser. The id stays, and with it every backup target that names the account.
func (s *server) credentials(w http.ResponseWriter, r *http.Request) {
	var sp accounts.Spec
	if !decode(w, r, &sp) {
		return
	}
	old, ok := s.o.Accounts.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, accounts.ErrNotFound)
		return
	}
	if accounts.OAuth(old.Kind) && sp.ServiceAccount == "" {
		addonkit.WriteError(w, http.StatusUnprocessableEntity, "invalid", "this kind of account logs in at its provider: POST /api/v1/login with account")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	a, err := s.o.Accounts.Relogin(ctx, old.ID, sp, "")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.one(r.Context(), a))
}

func (s *server) rename(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		UploadLimit int    `json:"upload_limit_kbps"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, err := s.o.Accounts.Rename(r.PathValue("id"), in.Name, in.UploadLimit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.one(r.Context(), a))
}

// remove is DELETE /api/v1/accounts/{id}: an account a backup target uses is refused with the
// targets' names (409 in-use) unless ?force=1 says the user has read them.
func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.o.Accounts.Get(id); !ok {
		writeErr(w, accounts.ErrNotFound)
		return
	}
	if used := s.o.Jobs.UsesTarget(jobs.TargetAccount, id); len(used) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "in-use", "message": "a copy job copies to this account: delete the job first", "jobs": jobNames(used)})
		return
	}
	if used := s.o.Facts.Facts(r.Context(), true).Targets[id]; len(used) > 0 && r.URL.Query().Get("force") != "1" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "in-use", "message": "a backup target uses this account", "targets": used})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.o.Accounts.Delete(ctx, id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) test(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	a, err := s.o.Accounts.Test(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.one(r.Context(), a))
}

// LoginStart is POST /api/v1/login: the account to connect (or, with account, the one to log in
// again) - the provider's address for the browser, and the command for the other way.
type LoginStart struct {
	accounts.Spec
	Account string `json:"account,omitempty"`
}

func (s *server) loginStart(w http.ResponseWriter, r *http.Request) {
	var in LoginStart
	if !decode(w, r, &in) {
		return
	}
	if in.Account != "" {
		old, ok := s.o.Accounts.Get(in.Account)
		if !ok {
			writeErr(w, accounts.ErrNotFound)
			return
		}
		in.Spec.Kind, in.Spec.Name = old.Kind, old.Name
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	f, err := s.o.Flows.Start(ctx, in.Spec, in.Account)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": f.ID, "url": f.URL, "expires": f.Expires, "command": accounts.Command(in.Spec)})
}

// LoginFinish is POST /api/v1/login/finish: way A's pasted address ({id, address}), or way B's
// pasted token with the account it is for ({token, spec, account?}).
type LoginFinish struct {
	ID      string         `json:"id,omitempty"`
	Address string         `json:"address,omitempty"`
	Token   string         `json:"token,omitempty"`
	Spec    *accounts.Spec `json:"spec,omitempty"`
	Account string         `json:"account,omitempty"`
}

func (s *server) loginFinish(w http.ResponseWriter, r *http.Request) {
	var in LoginFinish
	if !decode(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()
	var token, account string
	var sp accounts.Spec
	if in.Token != "" {
		if in.Spec == nil {
			addonkit.WriteError(w, http.StatusUnprocessableEntity, "invalid", "spec: the account the token is for")
			return
		}
		token = accounts.ParseToken(in.Token)
		if token == "" {
			addonkit.WriteError(w, http.StatusUnprocessableEntity, "invalid", "that is not what rclone authorize prints: copy everything between the two marker lines")
			return
		}
		sp, account = *in.Spec, in.Account
		s.o.Flows.Cancel()
	} else {
		var err error
		token, sp, account, err = s.o.Flows.Finish(ctx, in.ID, in.Address)
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	var a accounts.Account
	var err error
	if account != "" {
		a, err = s.o.Accounts.Relogin(ctx, account, sp, token)
	} else {
		a, err = s.o.Accounts.Create(ctx, sp, token)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.one(r.Context(), a))
}

func (s *server) loginCancel(w http.ResponseWriter, r *http.Request) {
	s.o.Flows.Cancel()
	w.WriteHeader(http.StatusNoContent)
}
