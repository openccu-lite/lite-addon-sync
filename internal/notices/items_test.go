package notices

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A set of notices follows its source: posted once, posted again when the text changes, cleared
// when it is gone; what an earlier life of the daemon may have posted is cleared at the first look;
// a source that cannot say changes nothing.
func TestItems(t *testing.T) {
	var calls []string
	sys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ctl" {
			w.WriteHeader(401)
			return
		}
		calls = append(calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api/system/v1/addon-notices/"))
		w.WriteHeader(200)
	}))
	defer sys.Close()
	tok := filepath.Join(t.TempDir(), "sync")
	if err := os.WriteFile(tok, []byte("ctl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	due, ok := []Item{{ID: "overdue-a", Title: map[string]string{"en": "A is overdue", "de": "A ist überfällig"}}}, true
	it := &Items{N: &Notifier{Base: sys.URL, TokenFile: tok, AddonID: "sync"},
		Source: func(context.Context) ([]Item, bool) { return due, ok },
		All:    func() []string { return []string{"overdue-a", "overdue-b"} }}
	ctx := context.Background()
	it.Once(ctx)
	it.Once(ctx)
	if got := strings.Join(calls, ","); got != "DELETE overdue-b,PUT overdue-a" {
		t.Fatalf("the first looks: %s", got)
	}
	calls = nil
	ok = false // the system does not answer: nothing is cleared
	it.Once(ctx)
	ok = true
	due[0].Title["en"] = "A is still overdue"
	it.Once(ctx)
	due = nil
	it.Once(ctx)
	it.Once(ctx)
	if got := strings.Join(calls, ","); got != "PUT overdue-a,DELETE overdue-a" {
		t.Fatalf("later looks: %s", got)
	}
}
