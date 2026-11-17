// Package e2e runs the addon's Go side against the real rclone it ships and a real remote: a WebDAV
// server on this machine (rclone's own `serve webdav` over a temporary directory), so no cloud
// account is needed. It covers the accounts (create, a wrong password, an unreachable server, new
// credentials), the backup provider's six calls over its Unix socket as the system makes them,
// and the OAuth login's way A against a stand-in provider.
//
// The rclone binary is RCLONE, or the pinned one in the cache (scripts/fetch-rclone.sh x86_64);
// without either the tests are skipped. With OCCULITED set to an occulited binary, the system's
// own conformance run (`occulited -backup provider-check`) is held against the provider too.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/provider"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
)

func rcloneBin(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("RCLONE"); b != "" {
		return b
	}
	out, err := exec.Command("sh", "-c", ". ../../scripts/rclone.pin && echo ${RCLONE_CACHE:-$HOME/.cache/lite-addon-sync/rclone}/$RCLONE_VERSION-x86_64/rclone").Output()
	bin := strings.TrimSpace(string(out))
	if err != nil || bin == "" {
		t.Skip("no rclone: set RCLONE or run scripts/fetch-rclone.sh x86_64")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("no rclone at %s: run scripts/fetch-rclone.sh x86_64", bin)
	}
	return bin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// webdav serves dir over WebDAV with a user and a password and answers its URL.
func webdav(t *testing.T, bin, dir string) string {
	t.Helper()
	port := freePort(t)
	cmd := exec.Command(bin, "serve", "webdav", dir, "--addr", fmt.Sprintf("127.0.0.1:%d", port), "--user", "backup", "--pass", "s3cret-pass", "--dir-cache-time", "0s", "--config", filepath.Join(t.TempDir(), "none.conf"))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	u := fmt.Sprintf("http://127.0.0.1:%d/", port)
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			c.Close()
			return u
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the WebDAV server did not come up")
	return ""
}

type rig struct {
	bin    string
	store  *accounts.Store
	runner *rclone.Runner
	remote string // the WebDAV server's directory
	url    string
	socket string
	hc     *http.Client
}

func newRig(t *testing.T) *rig {
	t.Helper()
	bin := rcloneBin(t)
	base, err := os.MkdirTemp("", "sync-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	r := &rig{bin: bin, remote: filepath.Join(base, "remote"), socket: filepath.Join(base, "target.sock")}
	_ = os.MkdirAll(r.remote, 0o755)
	r.url = webdav(t, bin, r.remote)
	r.runner = &rclone.Runner{Bin: bin, Config: filepath.Join(base, "conf", "rclone.conf"), Home: filepath.Join(base, "var"), Log: t.Logf}
	r.store, err = accounts.Open(filepath.Join(base, "conf"), r.runner)
	if err != nil {
		t.Fatal(err)
	}
	srv := &provider.Server{Accounts: r.store, Remote: r.runner, SetupHref: "/addon-settings/sync#accounts", PeerOK: func(uid uint32) bool { return uid == uint32(os.Getuid()) }, Log: t.Logf}
	l, err := provider.Listen(r.socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, l) }()
	r.hc = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", r.socket)
	}}}
	return r
}

func (r *rig) call(t *testing.T, method, path string, body io.Reader, size int64, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://provider"+path, body)
	if size > 0 {
		req.ContentLength = size
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := r.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestAccountsAndProviderAgainstWebDAV(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	spec := accounts.Spec{Name: "NAS", Kind: accounts.KindWebDAV, URL: r.url, Vendor: "other", User: "backup", Password: "s3cret-pass"}

	// a wrong password and a server that is not there leave no account
	bad := spec
	bad.Password = "wrong"
	if _, err := r.store.Create(ctx, bad, ""); rclone.StateOf(err) != rclone.StateAuthFailed {
		t.Fatalf("a wrong password: %v (%s)", err, rclone.StateOf(err))
	}
	gone := spec
	gone.URL = fmt.Sprintf("http://127.0.0.1:%d/", freePort(t))
	if _, err := r.store.Create(ctx, gone, ""); rclone.StateOf(err) != rclone.StateUnreachable {
		t.Fatalf("a server that is not there: %v (%s)", err, rclone.StateOf(err))
	}
	if n := len(r.store.List()); n != 0 {
		t.Fatalf("%d accounts after two failed attempts", n)
	}
	if names, _ := r.runner.Remotes(ctx); len(names) != 0 {
		t.Fatalf("remotes left in rclone.conf: %v", names)
	}

	a, err := r.store.Create(ctx, spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if a.State != rclone.StateWritable || a.Detail != "WebDAV · backup@"+strings.TrimSuffix(strings.TrimPrefix(r.url, "http://"), "/") {
		t.Fatalf("%+v", a)
	}
	// the password is in rclone.conf only, obscured, and the file is the addon's alone
	conf, _ := os.ReadFile(r.runner.Config)
	if strings.Contains(string(conf), "s3cret-pass") {
		t.Fatal("the password is in rclone.conf in plain")
	}
	if fi, _ := os.Stat(r.runner.Config); fi.Mode().Perm() != 0o600 {
		t.Fatalf("rclone.conf is %v", fi.Mode().Perm())
	}
	rec, _ := os.ReadFile(filepath.Join(r.store.Dir, accounts.File))
	if strings.Contains(string(rec), "s3cret") || strings.Contains(string(rec), "pass") {
		t.Fatalf("a secret in %s: %s", accounts.File, rec)
	}

	// info names the account
	code, b := r.call(t, "GET", "/v1/info", nil, 0, nil)
	var info struct {
		Protocol     int `json:"protocol"`
		Destinations []struct{ ID, Name, Detail, State string }
	}
	if json.Unmarshal(b, &info); code != 200 || info.Protocol != 1 || len(info.Destinations) != 1 || info.Destinations[0].ID != a.ID || info.Destinations[0].State != "writable" {
		t.Fatalf("info: %d %s", code, b)
	}
	folder := "openccu-lite/ccu-test"
	q := "?folder=" + url.QueryEscape(folder)
	dest := "/v1/destinations/" + a.ID

	// the test: a file written, read back, deleted - nothing left
	code, b = r.call(t, "POST", dest+"/test", strings.NewReader(`{"folder":"`+folder+`"}`), 0, nil)
	if code != 200 || !strings.Contains(string(b), `"state":"writable"`) {
		t.Fatalf("test: %d %s", code, b)
	}
	if left, _ := os.ReadDir(filepath.Join(r.remote, folder)); len(left) != 0 {
		t.Fatalf("the test left %d files", len(left))
	}

	// put: the backup arrives under its name, with its bytes, and no .partial stays
	body := make([]byte, 3<<20+123)
	_, _ = rand.Read(body)
	name := "ccu-test-2.0.0-2026-11-17-0007.sbk.age"
	code, b = r.call(t, "PUT", dest+"/files/"+name+q, bytes.NewReader(body), int64(len(body)), map[string]string{"X-SHA256": sum(body)})
	var put struct {
		State, Verified string
		Size            int64
	}
	if json.Unmarshal(b, &put); code != 200 || put.State != "writable" || put.Size != int64(len(body)) || put.Verified == "" {
		t.Fatalf("put: %d %s", code, b)
	}
	stored, err := os.ReadFile(filepath.Join(r.remote, folder, name))
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("the file on the server is not the backup: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(r.remote, folder)); len(left) != 1 {
		t.Fatalf("beside the backup: %v", left)
	}
	if got, _ := r.store.Get(a.ID); got.LastUpload == nil || !got.LastUpload.OK || got.LastUpload.Name != name {
		t.Fatalf("the upload is not noted: %+v", got.LastUpload)
	}

	// a body that is not what the header said is refused and leaves nothing
	other := "ccu-test-2.0.0-2026-11-18-0007.sbk.age"
	code, b = r.call(t, "PUT", dest+"/files/"+other+q, bytes.NewReader(body), int64(len(body)), map[string]string{"X-SHA256": sum([]byte("x"))})
	if code != 200 || !strings.Contains(string(b), `"state":"error"`) {
		t.Fatalf("a damaged body: %d %s", code, b)
	}
	if left, _ := os.ReadDir(filepath.Join(r.remote, folder)); len(left) != 1 {
		t.Fatalf("a refused upload left a file: %v", left)
	}
	// names that are no encrypted backup, folders that lead out
	for _, p := range []string{dest + "/files/notes.txt" + q, dest + "/files/x.sbk" + q, dest + "/files/" + name + "?folder=..%2Fup", dest + "/files/" + name + "?folder=", "/v1/destinations/nope/files/" + name + q} {
		if code, b = r.call(t, "PUT", p, bytes.NewReader(body), int64(len(body)), map[string]string{"X-SHA256": sum(body)}); code < 400 {
			t.Errorf("PUT %s: %d %s", p, code, b)
		}
	}

	// list: the backup, and nothing that is none
	_ = os.WriteFile(filepath.Join(r.remote, folder, "notes.txt"), []byte("x"), 0o644)
	code, b = r.call(t, "GET", dest+"/files"+q, nil, 0, nil)
	var list struct {
		Files []struct {
			Name string
			Size int64
		}
	}
	if json.Unmarshal(b, &list); code != 200 || len(list.Files) != 1 || list.Files[0].Name != name || list.Files[0].Size != int64(len(body)) {
		t.Fatalf("list: %d %s", code, b)
	}
	if code, b = r.call(t, "GET", dest+"/files?folder=never%2Fmade", nil, 0, nil); code != 200 || !strings.Contains(string(b), `"files":[]`) {
		t.Fatalf("a folder that does not exist: %d %s", code, b)
	}

	// get, a file that is not there, delete
	code, b = r.call(t, "GET", dest+"/files/"+name+q, nil, 0, nil)
	if code != 200 || !bytes.Equal(b, body) {
		t.Fatalf("get: %d, %d bytes", code, len(b))
	}
	if code, _ = r.call(t, "GET", dest+"/files/"+other+q, nil, 0, nil); code != 404 {
		t.Fatalf("get of a missing file: %d", code)
	}
	if code, _ = r.call(t, "DELETE", dest+"/files/"+name+q, nil, 0, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if _, err := os.Stat(filepath.Join(r.remote, folder, name)); !os.IsNotExist(err) {
		t.Fatal("the file is still on the server")
	}

	// the system's own conformance run, when its binary is at hand
	if occ := os.Getenv("OCCULITED"); occ != "" {
		out, err := exec.Command(occ, "-backup", "provider-check", "sync", a.ID, folder, "--socket", r.socket).CombinedOutput()
		t.Logf("%s", out)
		if err != nil {
			t.Fatalf("occulited's provider-check: %v", err)
		}
	}

	// the password changed on the server: the account says so, info carries the action, and new
	// credentials under the same id put it right
	if _, err := r.store.Relogin(ctx, a.ID, bad, ""); rclone.StateOf(err) != rclone.StateAuthFailed {
		t.Fatalf("new credentials that are wrong: %v", err)
	}
	if got, err := r.store.Test(ctx, a.ID); err != nil || got.State != rclone.StateWritable {
		t.Fatalf("a failed change of credentials broke the account: %v %+v", err, got)
	}
	_, _ = r.store.Note(a.ID, &rclone.Error{State: rclone.StateAuthFailed, Text: "401"}, nil)
	_, b = r.call(t, "GET", "/v1/info", nil, 0, nil)
	if !strings.Contains(string(b), `"state":"auth-failed"`) || !strings.Contains(string(b), `"href":"/addon-settings/sync#accounts"`) || !strings.Contains(string(b), "Zugangsdaten") {
		t.Fatalf("info after a refused login: %s", b)
	}
	again, err := r.store.Relogin(ctx, a.ID, spec, "")
	if err != nil || again.ID != a.ID || again.State != rclone.StateWritable {
		t.Fatalf("relogin: %v %+v", err, again)
	}
	if names, _ := r.runner.Remotes(ctx); len(names) != 1 || names[0] != a.Remote() {
		t.Fatalf("remotes after the relogin: %v", names)
	}

	// removing the account removes its remote; the files on the server stay
	if err := r.store.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if names, _ := r.runner.Remotes(ctx); len(names) != 0 {
		t.Fatalf("remotes after the delete: %v", names)
	}
	if _, b = r.call(t, "GET", "/v1/info", nil, 0, nil); !strings.Contains(string(b), `"destinations":[]`) {
		t.Fatalf("info after the delete: %s", b)
	}
}

// A peer that is not the system's service is refused on every call.
func TestProviderRefusesOtherPeers(t *testing.T) {
	bin := rcloneBin(t)
	base, _ := os.MkdirTemp("", "sync-peer")
	defer os.RemoveAll(base)
	runner := &rclone.Runner{Bin: bin, Config: filepath.Join(base, "rclone.conf"), Home: base}
	store, _ := accounts.Open(filepath.Join(base, "conf"), runner)
	srv := &provider.Server{Accounts: store, Remote: runner, PeerOK: func(uint32) bool { return false }}
	sock := filepath.Join(base, "t.sock")
	l, err := provider.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, l) }()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	res, err := hc.Get("http://provider/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a foreign peer got %d", res.StatusCode)
	}
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o666 {
		t.Fatalf("the socket is %v", fi.Mode().Perm())
	}
}

// Way A of the OAuth login, with the real rclone and a stand-in provider: the address for the
// browser, the paste of the redirect address, the token.
func TestOAuthLoginWayA(t *testing.T) {
	bin := rcloneBin(t)
	if c, err := net.DialTimeout("tcp", accounts.AuthorizeAddr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Skip("something listens on rclone's login port 53682")
	}
	var tokenReq url.Values
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/authorize":
			q := r.URL.Query()
			http.Redirect(w, r, q.Get("redirect_uri")+"?state="+url.QueryEscape(q.Get("state"))+"&code=CODE-1", http.StatusFound)
		case "/token":
			_ = r.ParseForm()
			tokenReq = r.PostForm
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"AT-1","token_type":"Bearer","refresh_token":"RT-1","expires_in":3600}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	home := t.TempDir()
	flows := &accounts.Flows{Bin: bin, Env: append(os.Environ(), "RCLONE_CONFIG="+filepath.Join(home, "authorize.conf"), "HOME="+home),
		Extra: map[string]string{"auth_url": fake.URL + "/authorize", "token_url": fake.URL + "/token"}}
	defer flows.Cancel()
	ctx := context.Background()
	spec := accounts.Spec{Name: "Privat", Kind: accounts.KindDropbox, ClientID: "my-app", ClientSecret: "my-secret"}
	f, err := flows.Start(ctx, spec, "")
	if err != nil {
		t.Fatal(err)
	}
	// the address for the browser is the provider's, with the user's own client and rclone's redirect
	u, _ := url.Parse(f.URL)
	if !strings.HasPrefix(f.URL, fake.URL+"/authorize?") || u.Query().Get("client_id") != "my-app" || u.Query().Get("redirect_uri") != "http://localhost:53682/" {
		t.Fatalf("the login address: %s", f.URL)
	}
	if _, err := flows.Start(ctx, spec, ""); err != accounts.ErrBusy {
		t.Fatalf("a second login: %v", err)
	}
	if p := flows.Pending(); p == nil || p.ID != f.ID {
		t.Fatal("the login is not pending")
	}
	// the browser's part: the provider redirects to rclone's address, which the user copies
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(f.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	address := res.Header.Get("Location")
	// an address that is not rclone's, another login's state, a cut-off address
	for _, bad := range []string{"https://evil.example/?state=x&code=y", "http://localhost:53682/?state=other&code=CODE-1", strings.Split(address, "&code")[0], "http://localhost:8080/?" + u.RawQuery} {
		if _, _, _, err := flows.Finish(ctx, f.ID, bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	token, sp, account, err := flows.Finish(ctx, f.ID, address)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(token, `"access_token":"AT-1"`) || !strings.Contains(token, `"refresh_token":"RT-1"`) || sp.Kind != accounts.KindDropbox || account != "" {
		t.Fatalf("token %q, spec %+v", token, sp)
	}
	if tokenReq.Get("code") != "CODE-1" || tokenReq.Get("redirect_uri") != "http://localhost:53682/" {
		t.Fatalf("the token request: %v", tokenReq)
	}
	if flows.Pending() != nil {
		t.Fatal("the login still waits")
	}
	if _, _, _, err := flows.Finish(ctx, f.ID, address); err != accounts.ErrNoFlow {
		t.Fatalf("a second finish: %v", err)
	}
	// way B: what rclone authorize prints on another computer, pasted with or without its marker lines
	blob := "Paste the following into your remote machine --->\neyJ0b2tlbiI6IntcImFjY2Vzc190b2tlblwiOlwiQVRcIixcInRva2VuX3R5cGVcIjpcIkJlYXJlclwiLFwicmVmcmVzaF90b2tlblwiOlwiUlRcIixcImV4cGlyeVwiOlwiMjAyNi0xMS0xN1QxOToxNzoyOC43NTk4OTUwMjkrMDE6MDBcIixcImV4cGlyZXNfaW5cIjozNjAwfSJ9\n<---End paste"
	if tok := accounts.ParseToken(blob); !strings.Contains(tok, `"refresh_token":"RT"`) {
		t.Fatalf("way B's paste: %q", tok)
	}
	if tok := accounts.ParseToken(`{"access_token":"AT","refresh_token":"RT","expiry":"2026-11-17T19:17:28+01:00"}`); tok == "" {
		t.Fatal("a plain token is not taken")
	}
	for _, bad := range []string{"", "hello", "e30=", `{"x":1}`} {
		if accounts.ParseToken(bad) != "" {
			t.Fatalf("%q read as a token", bad)
		}
	}
}
