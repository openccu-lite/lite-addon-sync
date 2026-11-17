// Package fakesys is the slice of openccu-lite the addon talks to, for the tests and the page's
// development: the auth state of the addon's token and the system's backup targets (which of
// them use which account, whether encryption is set up). It also stands in for an OAuth provider
// (/oauth/authorize, /oauth/token), so a login can be walked through without a cloud account.
package fakesys

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Server is the fake system.
type Server struct {
	mu sync.Mutex
	// Token is the one the addon must send; "" accepts any bearer.
	Token string
	// Encryption: a recovery key is set up. Provider: the system knows the extension.
	Encryption bool
	Provider   bool
	// Used maps an account id to the name of the backup target that uses it.
	Used map[string]string
	// Timers: the system offers timers to the addon (false: an older system, 404). The timers are
	// kept in memory; RunJob is called for a timer's "run" (the tests start the addon's runner).
	Timers   bool
	Readable []string
	RunJob   func(name string)
	StopJob  func(name string)
	timers   map[string]*FakeTimer
}

// FakeTimer is one of the addon's timers in the fake.
type FakeTimer struct {
	Name        string   `json:"name"`
	Unit        string   `json:"unit"`
	Service     string   `json:"service"`
	TimerFile   string   `json:"timer_file"`
	ServiceFile string   `json:"service_file"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description"`
	Read        []string `json:"read"`
	Addon       string   `json:"addon"`
}

// Timer answers a copy of the addon's timer of that name.
func (s *Server) Timer(name string) (FakeTimer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.timers[name]
	if !ok {
		return FakeTimer{}, false
	}
	return *t, true
}

// EditTimer changes a timer as a person on the Services page would; false when there is none.
func (s *Server) EditTimer(name string, f func(*FakeTimer)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.timers[name]
	if ok {
		f(t)
	}
	return ok
}

// DropTimer deletes a timer as the Services page would.
func (s *Server) DropTimer(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.timers, name)
}

// New makes a fake that accepts the token.
func New(token string) *Server {
	return &Server{Token: token, Encryption: true, Provider: true, Used: map[string]string{}, Timers: true, Readable: []string{}, timers: map[string]*FakeTimer{}}
}

// Set changes the fake's state under its lock.
func (s *Server) Set(f func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// Handler is the fake's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/v1/state", func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeJSON(w, 401, map[string]any{"error": "unauthenticated", "authenticated": false})
			return
		}
		writeJSON(w, 200, map[string]any{"authenticated": true, "sid": "", "user": "token:addon:sync", "scopes": []string{"system:read"}, "level": "read", "role": "user"})
	})
	mux.HandleFunc("GET /api/system/v1/backup/targets", func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		targets := []map[string]any{{"id": "directory", "name": "USB", "kind": "directory", "enabled": true}}
		for acc, name := range s.Used {
			targets = append(targets, map[string]any{"id": "t" + acc[:min(7, len(acc))], "name": name, "kind": "addon", "enabled": true, "addon": map[string]any{"id": "sync", "destination": acc, "folder": "openccu-lite/ccu"}})
		}
		providers := []map[string]any{}
		if s.Provider {
			providers = append(providers, map[string]any{"addon": "sync", "available": true})
		}
		writeJSON(w, 200, map[string]any{"encryption": s.Encryption, "hostname": "ccu", "targets": targets, "providers": providers})
	})
	// the addon's own timers (openccu-lite's scope timers:addon), in memory
	timerRoute := func(pattern string, h func(w http.ResponseWriter, r *http.Request, name string)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !s.authed(r) {
				writeJSON(w, 401, map[string]any{"error": "unauthenticated"})
				return
			}
			s.mu.Lock()
			on := s.Timers
			s.mu.Unlock()
			if !on {
				writeJSON(w, 404, map[string]any{"error": "not-found"})
				return
			}
			h(w, r, r.PathValue("name"))
		})
	}
	timerRoute("GET /api/system/v1/addon-timers", func(w http.ResponseWriter, r *http.Request, _ string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		list := []FakeTimer{}
		for _, t := range s.timers {
			list = append(list, *t)
		}
		writeJSON(w, 200, map[string]any{"addon": "sync", "timers": list, "max": 32, "readable": s.Readable})
	})
	timerRoute("PUT /api/system/v1/addon-timers/{name}", func(w http.ResponseWriter, r *http.Request, name string) {
		var in struct {
			Timer       string    `json:"timer"`
			Description *string   `json:"description"`
			Read        *[]string `json:"read"`
			Enabled     *bool     `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 422, map[string]any{"error": "invalid-body", "message": err.Error()})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		t, had := s.timers[name]
		if !had {
			if strings.TrimSpace(in.Timer) == "" {
				writeJSON(w, 422, map[string]any{"error": "invalid", "message": "a new timer needs its timer text"})
				return
			}
			t = &FakeTimer{Name: name, Unit: "local-sync-" + name + ".timer", Service: "local-sync-" + name + ".service", Enabled: true, Read: []string{}, Addon: "sync"}
			t.ServiceFile = "[Service]\nType=oneshot\nExecStart=/usr/local/addons/sync/bin/lite-sync run " + name + "\nUser=addon-sync\n"
		}
		if strings.Contains(in.Timer, "Unit=") || (in.Timer != "" && !strings.Contains(in.Timer, "[Timer]")) {
			writeJSON(w, 422, map[string]any{"error": "invalid", "message": "timer: not allowed in an addon's timer"})
			return
		}
		if in.Timer != "" {
			t.TimerFile = in.Timer
		}
		if in.Description != nil {
			t.Description = *in.Description
		}
		if in.Read != nil {
			for _, id := range *in.Read {
				ok := false
				for _, r := range s.Readable {
					ok = ok || r == id
				}
				if !ok {
					writeJSON(w, 422, map[string]any{"error": "invalid", "message": "read: the addon " + id + " is not installed"})
					return
				}
			}
			t.Read = *in.Read
		}
		if in.Enabled != nil {
			t.Enabled = *in.Enabled
		}
		s.timers[name] = t
		st := 200
		if !had {
			st = 201
		}
		writeJSON(w, st, t)
	})
	timerRoute("DELETE /api/system/v1/addon-timers/{name}", func(w http.ResponseWriter, r *http.Request, name string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.timers[name]; !ok {
			writeJSON(w, 404, map[string]any{"error": "not-found"})
			return
		}
		delete(s.timers, name)
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	action := func(what string) {
		timerRoute("POST /api/system/v1/addon-timers/{name}/"+what, func(w http.ResponseWriter, r *http.Request, name string) {
			s.mu.Lock()
			t, ok := s.timers[name]
			run, stop := s.RunJob, s.StopJob
			if ok {
				switch what {
				case "enable":
					t.Enabled = true
				case "disable":
					t.Enabled = false
				}
			}
			s.mu.Unlock()
			if !ok {
				writeJSON(w, 404, map[string]any{"error": "not-found"})
				return
			}
			if what == "run" && run != nil {
				run(name)
			}
			if what == "stop" && stop != nil {
				stop(name)
			}
			writeJSON(w, 200, map[string]any{"ok": true})
		})
	}
	for _, what := range []string{"run", "stop", "enable", "disable"} {
		action(what)
	}
	mux.HandleFunc("GET /api/system/v1/timers", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		list := []map[string]any{}
		for _, t := range s.timers {
			row := map[string]any{"unit": t.Unit, "activates": t.Service, "own": true, "addon": "sync", "enabled": t.Enabled, "active": t.Enabled}
			if t.Enabled {
				row["next"] = "2026-11-18T03:00:00Z"
			}
			list = append(list, row)
		}
		writeJSON(w, 200, map[string]any{"systemd": true, "timers": list})
	})
	// the stand-in OAuth provider: the consent page is skipped, the browser is sent straight back
	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		back, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || back.Host == "" {
			http.Error(w, "no redirect_uri", http.StatusBadRequest)
			return
		}
		back.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"FAKE-CODE"}}.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"access_token": "FAKE-AT", "token_type": "Bearer", "refresh_token": "FAKE-RT", "expires_in": 3600})
	})
	// the dev routes drive the fake: /dev/set?encryption=0&provider=1&used=<account id>&name=OneDrive
	mux.HandleFunc("/dev/set", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.mu.Lock()
		if v := q.Get("encryption"); v != "" {
			s.Encryption = v == "1"
		}
		if v := q.Get("provider"); v != "" {
			s.Provider = v == "1"
		}
		if v := q.Get("used"); v != "" {
			name := q.Get("name")
			if name == "" {
				name = "Cloud"
			}
			s.Used[v] = name
		}
		if q.Get("unused") != "" {
			s.Used = map[string]string{}
		}
		if v := q.Get("timers"); v != "" {
			s.Timers = v == "1"
		}
		if v := q.Get("readable"); v != "" {
			s.Readable = strings.Split(v, ",")
		}
		// what a person does on the Services page: edit a timer's text, switch it, delete it
		if name := q.Get("timer"); name != "" {
			if t, ok := s.timers[name]; ok {
				if v := q.Get("text"); v != "" {
					t.TimerFile = v
				}
				if v := q.Get("enabled"); v != "" {
					t.Enabled = v == "1"
				}
				if q.Get("delete") != "" {
					delete(s.timers, name)
				}
			}
		}
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	return mux
}

func (s *Server) authed(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == s.Token
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
