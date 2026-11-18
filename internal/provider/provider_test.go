package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
)

// memRemote is rclone in memory: files by path, and a knob that fails the next call.
type memRemote struct {
	mu    sync.Mutex
	files map[string][]byte
	fail  error
	calls []string
}

func (m *memRemote) call(name, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, name+" "+path)
	return m.fail
}

func (m *memRemote) Rcat(_ context.Context, path string, size int64, in io.Reader, _ int) error {
	b, _ := io.ReadAll(in)
	if err := m.call("rcat", path); err != nil {
		return err
	}
	m.mu.Lock()
	m.files[path] = b
	m.mu.Unlock()
	return nil
}
func (m *memRemote) Moveto(_ context.Context, src, dst string) error {
	if err := m.call("moveto", src); err != nil {
		return err
	}
	m.mu.Lock()
	m.files[dst] = m.files[src]
	delete(m.files, src)
	m.mu.Unlock()
	return nil
}
func (m *memRemote) DeleteFile(_ context.Context, path string) error {
	_ = m.call("delete", path)
	m.mu.Lock()
	delete(m.files, path)
	m.mu.Unlock()
	return nil
}
func (m *memRemote) Mkdir(_ context.Context, path string) error { return m.call("mkdir", path) }
func (m *memRemote) List(_ context.Context, path string) ([]rclone.File, error) {
	if err := m.call("list", path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []rclone.File{}
	for p, b := range m.files {
		if strings.HasPrefix(p, path+"/") && !strings.Contains(p[len(path)+1:], "/") {
			out = append(out, rclone.File{Name: p[len(path)+1:], Size: int64(len(b)), ModTime: time.Unix(1794900000, 0)})
		}
	}
	return out, nil
}
func (m *memRemote) Cat(_ context.Context, path string) (io.ReadCloser, error) {
	if err := m.call("cat", path); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return io.NopCloser(bytes.NewReader(m.files[path])), nil
}
func (m *memRemote) About(context.Context, string) (rclone.Space, error) {
	return rclone.Space{Free: 4_000_000_000, Total: 5_000_000_000}, nil
}
func (m *memRemote) HasHashes(context.Context, string) bool { return true }

func rig(t *testing.T) (*httptest.Server, *memRemote, accounts.Account, *accounts.Store) {
	t.Helper()
	dir := t.TempDir()
	// two accounts as the store keeps them: one by OAuth, one by keys in a bucket
	rec := `{"version":1,"accounts":[
	 {"id":"aone000001","name":"Privat","kind":"onedrive","auth":"oauth","detail":"OneDrive · max@example.org","created":"2026-11-01T10:00:00Z","state":"writable","upload_limit_kbps":0},
	 {"id":"as30000002","name":"Bucket","kind":"s3","detail":"S3 · backups · s3.example.org","root":"backups","created":"2026-11-02T10:00:00Z","state":"writable","upload_limit_kbps":0}]}`
	if err := os.WriteFile(filepath.Join(dir, accounts.File), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := accounts.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := &memRemote{files: map[string][]byte{}}
	srv := &Server{Accounts: store, Remote: mem, SetupHref: "/addon-settings/sync#accounts"}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	a, _ := store.Get("aone000001")
	return ts, mem, a, store
}

func do(t *testing.T, method, url string, body []byte, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestPutVerifiesAndRenames(t *testing.T) {
	ts, mem, a, store := rig(t)
	body := bytes.Repeat([]byte("backup "), 4000)
	name := "ccu-2.0.0-2026-11-17-0007.sbk.age"
	url := ts.URL + "/v1/destinations/" + a.ID + "/files/" + name + "?folder=openccu-lite%2Fccu"
	code, out := do(t, "PUT", url, body, map[string]string{"X-SHA256": sum(body)})
	var put struct {
		State, Verified string
		Size            int64
	}
	if _ = json.Unmarshal([]byte(out), &put); code != 200 || put.State != "writable" || put.Size != int64(len(body)) || put.Verified != "hash" {
		t.Fatalf("%d %s", code, out)
	}
	final := "acc-" + a.ID + ":openccu-lite/ccu/" + name
	if !bytes.Equal(mem.files[final], body) || len(mem.files) != 1 {
		t.Fatalf("on the remote: %v", mem.calls)
	}
	if mem.calls[0] != "rcat "+final+".partial" || mem.calls[1] != "moveto "+final+".partial" {
		t.Fatalf("not uploaded under .partial first: %v", mem.calls)
	}
	if got, _ := store.Get(a.ID); got.LastUpload == nil || !got.LastUpload.OK || got.LastUpload.Size != int64(len(body)) {
		t.Fatalf("%+v", got.LastUpload)
	}
	// a body that does not match its checksum: refused, the .partial removed, the old file stays
	code, out = do(t, "PUT", url, []byte("damaged"), map[string]string{"X-SHA256": sum(body)})
	if code != 200 || !strings.Contains(out, `"state":"error"`) || !bytes.Equal(mem.files[final], body) || len(mem.files) != 1 {
		t.Fatalf("%d %s %d files", code, out, len(mem.files))
	}
	// no checksum, a name that is no encrypted backup, a folder that leads out, an unknown account
	for _, c := range []struct {
		url string
		hdr map[string]string
	}{
		{url, nil},
		{ts.URL + "/v1/destinations/" + a.ID + "/files/passwd?folder=f", map[string]string{"X-SHA256": sum(body)}},
		{ts.URL + "/v1/destinations/" + a.ID + "/files/x.sbk?folder=f", map[string]string{"X-SHA256": sum(body)}},
		{ts.URL + "/v1/destinations/" + a.ID + "/files/" + name + "?folder=..%2F..", map[string]string{"X-SHA256": sum(body)}},
		{ts.URL + "/v1/destinations/" + a.ID + "/files/" + name + "?folder=.hidden", map[string]string{"X-SHA256": sum(body)}},
		{ts.URL + "/v1/destinations/nobody/files/" + name + "?folder=f", map[string]string{"X-SHA256": sum(body)}},
	} {
		if code, out = do(t, "PUT", c.url, body, c.hdr); code < 400 || !strings.Contains(out, `"state":"error"`) {
			t.Errorf("PUT %s: %d %s", c.url, code, out)
		}
	}
	if len(mem.files) != 1 {
		t.Fatalf("a refused put wrote: %d files", len(mem.files))
	}
}

func TestAnS3AccountWritesBelowItsBucket(t *testing.T) {
	ts, mem, _, _ := rig(t)
	body := []byte("x")
	name := "ccu-2.0.0-2026-11-17-0007.sbk.age"
	if code, out := do(t, "PUT", ts.URL+"/v1/destinations/as30000002/files/"+name+"?folder=ccu", body, map[string]string{"X-SHA256": sum(body)}); code != 200 || !strings.Contains(out, "writable") {
		t.Fatalf("%d %s", code, out)
	}
	if _, ok := mem.files["acc-as30000002:backups/ccu/"+name]; !ok {
		t.Fatalf("not below the bucket: %v", mem.calls)
	}
}

func TestStatesMessagesAndTheAction(t *testing.T) {
	ts, mem, a, store := rig(t)
	// a refused login: the state, the sentence in both languages, the way to the addon's page
	mem.fail = &rclone.Error{State: rclone.StateAuthFailed, Text: "invalid_grant"}
	code, out := do(t, "POST", ts.URL+"/v1/destinations/"+a.ID+"/test", []byte(`{"folder":"openccu-lite/ccu"}`), nil)
	var ans struct {
		State   string
		Message map[string]string
		Action  *Action
	}
	_ = json.Unmarshal([]byte(out), &ans)
	if code != 200 || ans.State != "auth-failed" || !strings.Contains(ans.Message["en"], "OneDrive no longer accepts") || !strings.Contains(ans.Message["de"], "OneDrive nimmt") || ans.Action == nil || ans.Action.Href != "/addon-settings/sync#accounts" || ans.Action.Label["en"] != "Log in again" {
		t.Fatalf("%d %s", code, out)
	}
	// the account remembers it, and info says so without contacting anything
	if got, _ := store.Get(a.ID); got.State != rclone.StateAuthFailed {
		t.Fatalf("%+v", got)
	}
	mem.calls = nil
	_, out = do(t, "GET", ts.URL+"/v1/info", nil, nil)
	if !strings.Contains(out, `"protocol":1`) || !strings.Contains(out, `"state":"auth-failed"`) || !strings.Contains(out, `"detail":"OneDrive · max@example.org"`) || len(mem.calls) != 0 {
		t.Fatalf("%s %v", out, mem.calls)
	}
	// an unreachable network last night is not the account's state now
	_, _ = store.Note(a.ID, &rclone.Error{State: rclone.StateUnreachable, Text: "dial tcp"}, nil)
	if _, out = do(t, "GET", ts.URL+"/v1/info", nil, nil); strings.Contains(out, "unreachable") {
		t.Fatalf("%s", out)
	}
	// full, unreachable: sentences without an action; a list that fails is not a 2xx
	for state, word := range map[string]string{rclone.StateFull: "full", rclone.StateUnreachable: "cannot be reached", rclone.StateReadOnly: "does not allow writing"} {
		mem.fail = &rclone.Error{State: state, Text: "x"}
		_, out = do(t, "POST", ts.URL+"/v1/destinations/"+a.ID+"/test", []byte(`{"folder":"f"}`), nil)
		if !strings.Contains(out, `"state":"`+state+`"`) || !strings.Contains(out, word) || strings.Contains(out, "action") {
			t.Errorf("%s: %s", state, out)
		}
		if code, _ = do(t, "GET", ts.URL+"/v1/destinations/"+a.ID+"/files?folder=f", nil, nil); code/100 == 2 {
			t.Errorf("%s: a failed list answered %d", state, code)
		}
	}
	// the test passes again: writable, with the account's room
	mem.fail = nil
	if _, out = do(t, "POST", ts.URL+"/v1/destinations/"+a.ID+"/test", []byte(`{"folder":"f"}`), nil); !strings.Contains(out, `"state":"writable"`) || !strings.Contains(out, `"free_bytes":4000000000`) {
		t.Fatalf("%s", out)
	}
	if len(mem.files) != 0 {
		t.Fatalf("the test left files: %d", len(mem.files))
	}
	// an unknown call
	if code, _ = do(t, "GET", ts.URL+"/v2/info", nil, nil); code != 404 {
		t.Fatalf("%d", code)
	}
}

func TestValidFolder(t *testing.T) {
	for f, want := range map[string]bool{"openccu-lite/ccu-keller": true, "Backups 2026/ccu": true, "a/b/c/d/e/f": true, "a/b/c/d/e/f/g": false, "": false, "/abs": false, "a//b": false, "../x": false, "a/..": false, ".x": false, "a\\b": false, "x ": false, "a/b:c": false} {
		if ValidFolder(f) != want {
			t.Errorf("%q: want %v", f, want)
		}
	}
}

// brokenCat is a remote whose download stops with an error after `after` bytes.
type brokenCat struct {
	*memRemote
	after int
}

func (b brokenCat) Cat(ctx context.Context, path string) (io.ReadCloser, error) {
	rc, err := b.memRemote.Cat(ctx, path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(io.MultiReader(io.LimitReader(rc, int64(b.after)), iotestErr{})), nil
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// The system reads only an encrypted backup's first bytes (its header names the key) and hangs up:
// that is no failed download and says nothing in the log. A download that the remote breaks off is
// still logged.
func TestGetALeftReadIsNoFailure(t *testing.T) {
	_, mem, a, store := rig(t)
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	name := "ccu-2.0.0-2026-11-18-0126.sbk.age"
	body := bytes.Repeat([]byte("age-encryption.org/v1 "), 1<<20) // 22 MB: more than any buffer on the way
	mem.files["acc-"+a.ID+":f/"+name] = body
	url := "/v1/destinations/" + a.ID + "/files/" + name + "?folder=f"

	ts := httptest.NewServer((&Server{Accounts: store, Remote: mem, Log: logf}).Handler())
	defer ts.Close()
	res, err := http.Get(ts.URL + url)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 4096)
	if _, err := io.ReadFull(res.Body, head); err != nil || !bytes.HasPrefix(head, []byte("age-encryption.org/v1")) {
		t.Fatalf("the header: %v %q", err, head[:30])
	}
	res.Body.Close()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	if len(lines) != 0 {
		t.Errorf("a read the system ended was logged: %q", lines)
	}
	mu.Unlock()

	ts2 := httptest.NewServer((&Server{Accounts: store, Remote: brokenCat{mem, 1000}, Log: logf}).Handler())
	defer ts2.Close()
	res, err = http.Get(ts2.URL + url)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, res.Body)
	res.Body.Close()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n == int64(len(body)) || len(lines) != 1 || !strings.Contains(lines[0], "ended after 1000 of") || !strings.Contains(lines[0], "connection reset by peer") {
		t.Errorf("a download the remote broke off: %d bytes, log %q", n, lines)
	}
}
