package notices

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
)

func TestDue(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ago := func(days int) time.Time { return now.Add(-time.Duration(days) * 24 * time.Hour) }
	list := []accounts.Account{
		{ID: "fresh", Kind: accounts.KindOneDrive, LastOK: ago(3)},
		{ID: "soon", Kind: accounts.KindOneDrive, LastOK: ago(80)},
		{ID: "edge", Kind: accounts.KindOneDrive, LastOK: ago(76)},
		{ID: "before", Kind: accounts.KindOneDrive, LastOK: ago(75)},
		{ID: "gone", Kind: accounts.KindOneDrive, LastOK: ago(120)},
		{ID: "never", Kind: accounts.KindOneDrive},
		{ID: "dropbox", Kind: accounts.KindDropbox, LastOK: ago(200)},
		{ID: "dav", Kind: accounts.KindWebDAV, LastOK: ago(200)},
	}
	due := Due(list, now)
	if len(due) != 3 || due["soon"] != 10 || due["edge"] != 14 || due["gone"] != 0 {
		t.Fatalf("%v", due)
	}
}

func TestNotifierPostsAndClears(t *testing.T) {
	type call struct{ method, path, auth, body string }
	var calls []call
	sys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "login-never") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sys.Close()
	tok := filepath.Join(t.TempDir(), "sync")
	_ = os.WriteFile(tok, []byte("control-token\n"), 0o600)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	list := []accounts.Account{
		{ID: "soon", Name: "Privat", Kind: accounts.KindOneDrive, LastOK: now.Add(-80 * 24 * time.Hour)},
		{ID: "never", Name: "Neu", Kind: accounts.KindOneDrive, LastOK: now},
		{ID: "dav", Name: "NAS", Kind: accounts.KindWebDAV, LastOK: now.Add(-300 * 24 * time.Hour)},
	}
	n := &Notifier{Base: sys.URL, TokenFile: tok, AddonID: "sync", Accounts: func() []accounts.Account { return list }, Now: func() time.Time { return now }}
	ctx := context.Background()
	n.Once(ctx)
	// one notice for the account that is due; one clearing call for the OneDrive account that is not
	// (a previous run may have left one); nothing for a kind without such a rule
	if len(calls) != 2 {
		t.Fatalf("%+v", calls)
	}
	var put call
	for _, c := range calls {
		if c.auth != "Bearer control-token" {
			t.Fatalf("the control token is not sent: %+v", c)
		}
		if c.method == http.MethodPut {
			put = c
		}
	}
	var body struct {
		Severity string
		Title    map[string]string
		Text     map[string]string
		Link     string
	}
	if err := json.Unmarshal([]byte(put.body), &body); err != nil || put.path != "/api/system/v1/addon-notices/login-soon" {
		t.Fatalf("%+v", put)
	}
	if body.Severity != "warning" || body.Title["en"] != "The login for Privat expires in 10 days" || body.Title["de"] != "Die Anmeldung für Privat läuft in 10 Tagen ab" || body.Link != "/addon-settings/sync#accounts" || body.Text["de"] == "" || len(body.Title["de"]) > 160 {
		t.Fatalf("%+v", body)
	}
	// the same day again: nothing is sent
	calls = nil
	n.Once(ctx)
	if len(calls) != 0 {
		t.Fatalf("posted again: %+v", calls)
	}
	// a day later the count changes; then the account is used and the notice goes
	now = now.Add(24 * time.Hour)
	n.Once(ctx)
	if len(calls) != 1 || !strings.Contains(calls[0].body, "in 9 days") {
		t.Fatalf("%+v", calls)
	}
	calls = nil
	list[0].LastOK = now
	n.Once(ctx)
	if len(calls) != 1 || calls[0].method != http.MethodDelete || calls[0].path != "/api/system/v1/addon-notices/login-soon" {
		t.Fatalf("%+v", calls)
	}
	// an account that was removed while its notice stood
	list[0].LastOK = now.Add(-85 * 24 * time.Hour)
	n.Once(ctx)
	calls = nil
	list = list[1:]
	n.Once(ctx)
	if len(calls) != 1 || calls[0].method != http.MethodDelete {
		t.Fatalf("%+v", calls)
	}
}
