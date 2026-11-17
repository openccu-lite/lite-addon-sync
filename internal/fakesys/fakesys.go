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
}

// New makes a fake that accepts the token.
func New(token string) *Server {
	return &Server{Token: token, Encryption: true, Provider: true, Used: map[string]string{}}
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
