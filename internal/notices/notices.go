// Package notices tells the user on the system's Status page when a login is about to lapse:
// OneDrive ends a login that was not used for 90 days, and a backup target on such an account
// would then fail one night. The addon posts an addon notice (openccu-lite's own mechanism: the
// Status page shows it with the addon's name in front and a button into the addon) two weeks
// before, and clears it once the account was used again or is gone. It contacts nothing but the
// system for this: an account's last use is what the addon itself saw.
package notices

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
)

// The login's life at OneDrive without use, and how long before its end the notice appears.
const (
	OneDriveIdle = 90 * 24 * time.Hour
	Ahead        = 14 * 24 * time.Hour
)

// ControlTokenDir is where the system writes every addon's control token (the one its notices take).
const ControlTokenDir = "/run/occulite/addon-tokens"

// Notifier posts and clears the notices.
type Notifier struct {
	// Base is the system service's loopback address (http://127.0.0.1:8183).
	Base string
	// TokenFile is the addon's control token.
	TokenFile string
	// AddonID is this addon's id (the notice's link).
	AddonID  string
	Accounts func() []accounts.Account
	HTTP     *http.Client
	Now      func() time.Time
	Log      func(format string, args ...any)

	posted map[string]int
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// Due answers the accounts whose login lapses within Ahead, with the days left (0 = it has lapsed
// by the provider's rule). Only a OneDrive login by OAuth has such a rule the addon can count.
func Due(list []accounts.Account, now time.Time) map[string]int {
	out := map[string]int{}
	for _, a := range list {
		if a.Kind != accounts.KindOneDrive || a.Auth == "service-account" || a.LastOK.IsZero() {
			continue
		}
		left := OneDriveIdle - now.Sub(a.LastOK)
		if left > Ahead {
			continue
		}
		days := int(left / (24 * time.Hour))
		if days < 0 {
			days = 0
		}
		out[a.ID] = days
	}
	return out
}

func noticeID(account string) string { return "login-" + account }

// Run evaluates once now and then every hour until ctx ends.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once posts a notice for every account that is due (again only when the days changed) and clears
// the notices of accounts that are not.
func (n *Notifier) Once(ctx context.Context) {
	list := n.Accounts()
	due := Due(list, n.now())
	if n.posted == nil {
		n.posted = map[string]int{}
		// a notice a previous run of the daemon posted is not known here: an account that is not
		// due gets one clearing call, once
		for _, a := range list {
			if _, ok := due[a.ID]; !ok && a.Kind == accounts.KindOneDrive {
				n.posted[a.ID] = -1
			}
		}
	}
	names := map[string]string{}
	for _, a := range list {
		names[a.ID] = a.Name
	}
	for id, days := range due {
		if last, ok := n.posted[id]; ok && last == days {
			continue
		}
		title := map[string]string{
			"de": fmt.Sprintf("Die Anmeldung für %s läuft in %d Tagen ab", names[id], days),
			"en": fmt.Sprintf("The login for %s expires in %d days", names[id], days),
		}
		if days == 0 {
			title = map[string]string{"de": fmt.Sprintf("Die Anmeldung für %s ist vermutlich abgelaufen", names[id]), "en": fmt.Sprintf("The login for %s has probably expired", names[id])}
		}
		body := map[string]any{"severity": "warning", "title": title, "link": "/addon-settings/" + n.AddonID + "#accounts",
			"text": map[string]string{
				"de": "OneDrive beendet eine Anmeldung, die 90 Tage nicht benutzt wurde. Eine Sicherung in dieses Konto oder sein Test erneuert sie; danach hilft nur „Erneut anmelden“.",
				"en": "OneDrive ends a login that was not used for 90 days. A backup to this account or its test renews it; after that only \"Log in again\" helps.",
			}}
		if err := n.call(ctx, http.MethodPut, noticeID(id), body); err != nil {
			n.logf("the notice for %s could not be posted: %v", names[id], err)
			continue
		}
		n.posted[id] = days
	}
	for id := range n.posted {
		if _, ok := due[id]; ok {
			continue
		}
		if err := n.call(ctx, http.MethodDelete, noticeID(id), nil); err != nil {
			n.logf("a notice could not be cleared: %v", err)
			continue
		}
		delete(n.posted, id)
	}
}

func (n *Notifier) logf(format string, args ...any) {
	if n.Log != nil {
		n.Log(format, args...)
	}
}

func (n *Notifier) call(ctx context.Context, method, id string, body any) error {
	tok, err := os.ReadFile(n.TokenFile)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(n.Base, "/")+"/api/system/v1/addon-notices/"+id, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := n.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	// a notice that is not there is cleared
	if res.StatusCode/100 == 2 || method == http.MethodDelete && res.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("the system answered HTTP %d", res.StatusCode)
}
