package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/fakesys"
	"github.com/openccu-lite/lite-addon-sync/internal/jobs"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
	"github.com/openccu-lite/lite-addon-sync/internal/servers"
	"github.com/openccu-lite/lite-addon-sync/internal/sysinfo"
	"github.com/openccu-lite/lite-addon-sync/internal/systimers"
)

type fakeFacts struct{ f sysinfo.Facts }

func (f *fakeFacts) Facts(context.Context, bool) sysinfo.Facts { return f.f }

func rig(t *testing.T) (*httptest.Server, *fakeFacts) {
	t.Helper()
	dir := t.TempDir()
	rec := `{"version":1,"accounts":[{"id":"aone000001","name":"Privat","kind":"onedrive","auth":"oauth","detail":"OneDrive · max@example.org","created":"2026-11-01T10:00:00Z","state":"writable","upload_limit_kbps":0}]}`
	if err := os.WriteFile(filepath.Join(dir, accounts.File), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	// rclone is not there: every call of it fails, which is what these tests need of it
	store, err := accounts.Open(dir, &rclone.Runner{Bin: filepath.Join(dir, "no-rclone"), Config: filepath.Join(dir, "rclone.conf"), Home: dir})
	if err != nil {
		t.Fatal(err)
	}
	facts := &fakeFacts{f: sysinfo.Facts{Reachable: true, Provider: true, Encryption: true, Targets: map[string][]sysinfo.Target{"aone000001": {{ID: "t1", Name: "OneDrive"}}}}}
	ts, _ := rigWith(t, dir, store, facts)
	return ts, facts
}

// jobRig is what the copy jobs' tests reach besides the server: the fake system with the addon's
// timers, the stores, and the directories.
type jobRig struct {
	sys      *fakesys.Server
	jobs     *jobs.Store
	servers  *servers.Store
	addonDir string
	runDir   string
	ran      []string
}

func rigWith(t *testing.T, dir string, store *accounts.Store, facts *fakeFacts) (*httptest.Server, *jobRig) {
	t.Helper()
	jr := &jobRig{sys: fakesys.New("tok"), addonDir: filepath.Join(dir, "addon"), runDir: filepath.Join(dir, "run")}
	jr.sys.RunJob = func(name string) { jr.ran = append(jr.ran, name) }
	sys := httptest.NewServer(jr.sys.Handler())
	t.Cleanup(sys.Close)
	var err error
	if jr.servers, err = servers.Open(dir, servers.Tools{}, dir); err != nil {
		t.Fatal(err)
	}
	if jr.jobs, err = jobs.Open(dir); err != nil {
		t.Fatal(err)
	}
	timers := &systimers.Client{Base: sys.URL, HTTP: &http.Client{Transport: bearer("tok")}}
	h := New(Options{BasePath: "/addons/sync", WWW: t.TempDir(), Accounts: store, Flows: &accounts.Flows{Bin: filepath.Join(dir, "no-rclone")}, Facts: facts, Version: "test", Rclone: "v1.76.0", Started: time.Now(),
		Provider: func() (bool, string) { return true, "" }, Servers: jr.servers, Jobs: jr.jobs, Timers: timers, AddonDir: jr.addonDir, RunDir: jr.runDir})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, jr
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func call(t *testing.T, method, url, body string, csrf bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if csrf {
		req.Header.Set("X-CCU-Addon-Request", "1")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 64<<10)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res.StatusCode, sb.String()
}

func TestStateNamesTheTargetsAndNoSecret(t *testing.T) {
	ts, _ := rig(t)
	code, out := call(t, "GET", ts.URL+"/addons/sync/api/v1/state", "", false)
	var v StateView
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if len(v.Accounts) != 1 || len(v.Accounts[0].UsedBy) != 1 || v.Accounts[0].UsedBy[0].Name != "OneDrive" || !v.Provider || !v.System.Encryption || len(v.Kinds) != 5 {
		t.Fatalf("%+v", v)
	}
	for _, word := range []string{"token", "secret", "password", "client_secret"} {
		if strings.Contains(strings.ToLower(out), `"`+word+`"`) {
			t.Errorf("the state carries a field %q", word)
		}
	}
}

func TestWritesNeedTheRequestHeader(t *testing.T) {
	ts, _ := rig(t)
	if code, _ := call(t, "PUT", ts.URL+"/addons/sync/api/v1/accounts/aone000001", `{"name":"x","upload_limit_kbps":0}`, false); code != http.StatusForbidden {
		t.Fatalf("a write without the header: %d", code)
	}
	code, out := call(t, "PUT", ts.URL+"/addons/sync/api/v1/accounts/aone000001", `{"name":"Mein OneDrive","upload_limit_kbps":512}`, true)
	if code != 200 || !strings.Contains(out, `"name":"Mein OneDrive"`) || !strings.Contains(out, `"upload_limit_kbps":512`) {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ = call(t, "PUT", ts.URL+"/addons/sync/api/v1/accounts/aone000001", `{"name":"","upload_limit_kbps":0}`, true); code != http.StatusUnprocessableEntity {
		t.Fatalf("an empty name: %d", code)
	}
	if code, _ = call(t, "PUT", ts.URL+"/addons/sync/api/v1/accounts/nobody", `{"name":"x"}`, true); code != http.StatusNotFound {
		t.Fatalf("an unknown account: %d", code)
	}
}

func TestAnAccountInUseIsNotRemovedUnseen(t *testing.T) {
	ts, facts := rig(t)
	code, out := call(t, "DELETE", ts.URL+"/addons/sync/api/v1/accounts/aone000001", "", true)
	if code != http.StatusConflict || !strings.Contains(out, `"error":"in-use"`) || !strings.Contains(out, `"name":"OneDrive"`) {
		t.Fatalf("%d %s", code, out)
	}
	// with ?force=1, or once no target uses it, the removal goes on (and fails here on the missing rclone)
	if code, out = call(t, "DELETE", ts.URL+"/addons/sync/api/v1/accounts/aone000001?force=1", "", true); code == http.StatusConflict {
		t.Fatalf("force: %d %s", code, out)
	}
	facts.f.Targets = map[string][]sysinfo.Target{}
	if code, _ = call(t, "DELETE", ts.URL+"/addons/sync/api/v1/accounts/aone000001", "", true); code == http.StatusConflict {
		t.Fatalf("unused: %d", code)
	}
}

func TestKindsAndLogins(t *testing.T) {
	ts, _ := rig(t)
	// an OAuth kind is not created with keys; a kind with keys has no login in a browser
	if code, out := call(t, "POST", ts.URL+"/addons/sync/api/v1/accounts", `{"name":"x","kind":"onedrive"}`, true); code != http.StatusUnprocessableEntity {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := call(t, "POST", ts.URL+"/addons/sync/api/v1/login", `{"name":"x","kind":"webdav","url":"https://c.example/","user":"u","password":"p"}`, true); code != http.StatusUnprocessableEntity {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := call(t, "POST", ts.URL+"/addons/sync/api/v1/login", `{"name":"x","kind":"drive"}`, true); code != http.StatusUnprocessableEntity || !strings.Contains(out, "own client ID") {
		t.Fatalf("%d %s", code, out)
	}
	// way B: a paste that holds no token; a login that is not waiting
	if code, out := call(t, "POST", ts.URL+"/addons/sync/api/v1/login/finish", `{"token":"hello","spec":{"name":"x","kind":"dropbox"}}`, true); code != http.StatusUnprocessableEntity || !strings.Contains(out, "marker lines") {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := call(t, "POST", ts.URL+"/addons/sync/api/v1/login/finish", `{"id":"nope","address":"http://localhost:53682/?state=a&code=b"}`, true); code != http.StatusConflict || !strings.Contains(out, "login-gone") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ := call(t, "DELETE", ts.URL+"/addons/sync/api/v1/login", "", true); code != http.StatusNoContent {
		t.Fatalf("%d", code)
	}
	if code, _ := call(t, "GET", ts.URL+"/addons/sync/api/v1/nope", "", false); code != http.StatusNotFound {
		t.Fatalf("%d", code)
	}
}

// docs/api.md names every route, and no route that is not there.
func TestEveryRouteDocumented(t *testing.T) {
	rig(t)
	doc, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Routes {
		if !strings.Contains(string(doc), "`"+r+"`") {
			t.Errorf("docs/api.md does not name %s", r)
		}
	}
	for _, line := range strings.Split(string(doc), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		route := strings.SplitN(strings.TrimPrefix(line, "| `"), "`", 2)[0]
		found := false
		for _, r := range Routes {
			if r == route {
				found = true
			}
		}
		if !found {
			t.Errorf("docs/api.md names %s, which the API does not serve", route)
		}
	}
}
