package accounts

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The OAuth login on a system without a browser. rclone's redirect address is always
// http://localhost:53682/ - the machine rclone runs on, which the user's browser cannot reach
// under that name. Two ways, both rclone's own:
//
// A. In this browser. The addon starts `rclone authorize` on the system; rclone listens on
// 127.0.0.1:53682 and names a link there that redirects to the provider. The addon follows that
// one redirect itself and hands the provider's address to the page, which opens it in a new tab.
// After the login the provider sends the tab to http://localhost:53682/?state=…&code=…, which
// does not load on the user's computer; the user copies that address and pastes it into the page,
// and the addon hands its query to the rclone that waits on the system's loopback. rclone
// exchanges the code and prints the token.
//
// B. With rclone on another computer. The page shows the command `rclone authorize "<kind>"`;
// the user runs it where a browser is and pastes what it prints.
//
// One login at a time (the port is rclone's and fixed), ten minutes long.

// AuthorizeAddr is where `rclone authorize` listens.
const AuthorizeAddr = "127.0.0.1:53682"

// FlowTTL is how long a started login waits for its paste.
const FlowTTL = 10 * time.Minute

// Flow is a started login.
type Flow struct {
	ID string `json:"id"`
	// URL is the provider's login address for the user's browser.
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"`

	spec    Spec
	account string // the account a new login is for, "" = a new account
	state   string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	token   chan string
	failed  chan error
}

// Flows runs the logins.
type Flows struct {
	// Bin is rclone; Env its environment (the Runner's).
	Bin string
	Env []string
	// Addr replaces AuthorizeAddr (tests); Extra adds rclone options to the login (tests: the
	// provider's addresses).
	Addr  string
	Extra map[string]string
	Log   func(format string, args ...any)

	mu  sync.Mutex
	cur *Flow
}

func (f *Flows) addr() string {
	if f.Addr != "" {
		return f.Addr
	}
	return AuthorizeAddr
}

var linkRe = regexp.MustCompile(`http://127\.0\.0\.1:\d+/auth\?state=([A-Za-z0-9_-]+)`)

// ErrBusy: another login is under way.
var ErrBusy = errors.New("another login is under way: finish or cancel it first")

// ErrNoFlow: the login is not there any more (it took longer than ten minutes, or was cancelled).
var ErrNoFlow = errors.New("this login is not waiting any more: start it again")

// blob is the argument `rclone authorize` takes for a backend's options: their JSON in standard
// base64 without padding (rclone refuses the padded form).
func blob(opts map[string]string) string {
	b, _ := json.Marshal(opts)
	return base64.RawStdEncoding.EncodeToString(b)
}

// options are the login's options for a spec: the user's own client, the narrow scope.
func (f *Flows) options(sp Spec) map[string]string {
	o := map[string]string{}
	if sp.ClientID != "" {
		o["client_id"], o["client_secret"] = sp.ClientID, sp.ClientSecret
	}
	if sp.Kind == KindDrive {
		o["scope"] = "drive.file"
	}
	for k, v := range f.Extra {
		o[k] = v
	}
	return o
}

// Command is way B's command line for the user's own computer.
func Command(sp Spec) string {
	if sp.ClientID != "" {
		return fmt.Sprintf("rclone authorize %q %q %q", sp.Kind, sp.ClientID, sp.ClientSecret)
	}
	return fmt.Sprintf("rclone authorize %q", sp.Kind)
}

// Start begins way A for a spec (account != "": a new login of that account).
func (f *Flows) Start(ctx context.Context, sp Spec, account string) (*Flow, error) {
	if !OAuth(sp.Kind) || sp.ServiceAccount != "" {
		return nil, invalid("this kind of account has no login in a browser")
	}
	if err := sp.Validate(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cur != nil {
		if time.Now().Before(f.cur.Expires) {
			return nil, ErrBusy
		}
		f.stopLocked()
	}
	cctx, cancel := context.WithTimeout(context.Background(), FlowTTL)
	args := []string{"authorize", sp.Kind}
	if o := f.options(sp); len(o) > 0 {
		args = append(args, blob(o))
	}
	args = append(args, "--auth-no-open-browser", "--log-level", "NOTICE")
	cmd := exec.CommandContext(cctx, f.Bin, args...)
	cmd.Env = f.Env
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	fl := &Flow{ID: NewID(), Expires: time.Now().Add(FlowTTL), spec: sp, account: account, cmd: cmd, cancel: cancel, token: make(chan string, 1), failed: make(chan error, 1)}
	link := make(chan string, 1)
	go func() {
		// rclone names its link on stderr; anything else there is its own report of a failure
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		var last string
		for sc.Scan() {
			l := sc.Text()
			if m := linkRe.FindStringSubmatch(l); m != nil {
				select {
				case link <- m[0]:
				default:
				}
				continue
			}
			if strings.Contains(l, "ERROR") || strings.Contains(l, "CRITICAL") || strings.Contains(l, "Failed") {
				last = l
			}
		}
		if last != "" {
			select {
			case fl.failed <- errors.New(lastWords(last)):
			default:
			}
		}
	}()
	go func() {
		// the token comes on stdout, between rclone's two marker lines
		b, _ := io.ReadAll(io.LimitReader(stdout, 1<<20))
		err := cmd.Wait()
		if tok := pasted(string(b)); tok != "" {
			fl.token <- tok
			return
		}
		if err == nil {
			err = errors.New("rclone ended without a token")
		}
		select {
		case fl.failed <- err:
		default:
		}
	}()
	select {
	case l := <-link:
		m := linkRe.FindStringSubmatch(l)
		fl.state = m[1]
		// the port is the one rclone listens on; the request goes to it on this system
		u, err := f.providerURL(ctx, l)
		if err != nil {
			cancel()
			return nil, err
		}
		fl.URL = u
	case err := <-fl.failed:
		cancel()
		return nil, fmt.Errorf("rclone could not start the login: %w", err)
	case <-time.After(20 * time.Second):
		cancel()
		return nil, errors.New("rclone did not name its login address")
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	f.cur = fl
	go func() {
		<-cctx.Done()
		f.mu.Lock()
		if f.cur == fl {
			f.cur = nil
		}
		f.mu.Unlock()
	}()
	return fl, nil
}

func lastWords(l string) string {
	if i := strings.Index(l, ": "); i >= 0 && i < 40 {
		l = l[i+2:]
	}
	if len(l) > 300 {
		l = l[:300]
	}
	return l
}

var noRedirect = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// providerURL follows rclone's own link once: it redirects to the provider's login address.
func (f *Flows) providerURL(ctx context.Context, link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return "", err
	}
	u.Host = f.addr()
	var last error
	for i := 0; i < 20; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		res, err := noRedirect.Do(req)
		if err != nil {
			last = err
			time.Sleep(150 * time.Millisecond) // rclone names the link a moment before it listens
			continue
		}
		res.Body.Close()
		loc := res.Header.Get("Location")
		lu, err := url.Parse(loc)
		if res.StatusCode/100 != 3 || err != nil || (lu.Scheme != "https" && lu.Scheme != "http") || lu.Host == "" {
			return "", errors.New("rclone's login link did not lead to the provider")
		}
		return loc, nil
	}
	return "", fmt.Errorf("rclone's login link could not be read: %w", last)
}

// pasted reads a token out of what `rclone authorize` prints, or out of what a user pasted from
// it: the text between the marker lines when they are there, then either the token's JSON itself
// or rclone's base64 blob that carries it.
func pasted(text string) string {
	if i := strings.Index(text, "--->"); i >= 0 {
		text = text[i+4:]
		if j := strings.Index(text, "<---"); j >= 0 {
			text = text[:j]
		}
	}
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 64<<10 {
		return ""
	}
	if strings.HasPrefix(text, "{") {
		return validToken(text)
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(strings.Join(strings.Fields(text), ""), "="))
	if err != nil {
		return ""
	}
	var b struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return ""
	}
	return validToken(b.Token)
}

func validToken(s string) string {
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal([]byte(s), &t) != nil || t.AccessToken == "" && t.RefreshToken == "" {
		return ""
	}
	return s
}

// ParseToken is pasted for way B: what the user copied from `rclone authorize` on another
// computer; "" when it holds no token.
func ParseToken(text string) string { return pasted(text) }

// Pending answers the login that waits, nil when none does.
func (f *Flows) Pending() *Flow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cur != nil && time.Now().After(f.cur.Expires) {
		f.stopLocked()
	}
	return f.cur
}

// Finish ends way A: the address the provider sent the user's tab to. It must be rclone's redirect
// address and carry this login's state; its query is handed to the rclone that waits, and the
// token rclone then prints is the answer, with the spec the login was started for.
func (f *Flows) Finish(ctx context.Context, id, address string) (token string, sp Spec, account string, err error) {
	f.mu.Lock()
	fl := f.cur
	f.mu.Unlock()
	if fl == nil || fl.ID != id || time.Now().After(fl.Expires) {
		return "", Spec{}, "", ErrNoFlow
	}
	u, perr := url.Parse(strings.TrimSpace(address))
	if perr != nil || u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") || u.Port() != "53682" {
		return "", Spec{}, "", invalid("that is not the address the login ended on: it begins with http://localhost:53682/")
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		f.Cancel()
		return "", Spec{}, "", invalid("the provider refused the login: %s", strings.TrimSpace(e+" "+q.Get("error_description")))
	}
	if q.Get("state") != fl.state || q.Get("code") == "" {
		return "", Spec{}, "", invalid("the address belongs to another login, or is cut off: copy all of it")
	}
	defer f.Cancel()
	// a browser that could reach rclone's address itself (it runs on this system, or through a
	// tunnel) has handed the code over already: the token is there
	select {
	case tok := <-fl.token:
		return tok, fl.spec, fl.account, nil
	default:
	}
	cb := url.URL{Scheme: "http", Host: f.addr(), Path: "/", RawQuery: url.Values{"state": {fl.state}, "code": {q.Get("code")}}.Encode()}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cb.String(), nil)
	res, derr := noRedirect.Do(req)
	if derr != nil {
		select {
		case tok := <-fl.token:
			return tok, fl.spec, fl.account, nil
		case <-time.After(2 * time.Second):
		}
		return "", Spec{}, "", fmt.Errorf("the waiting rclone could not be reached: %w", derr)
	}
	res.Body.Close()
	select {
	case tok := <-fl.token:
		return tok, fl.spec, fl.account, nil
	case e := <-fl.failed:
		return "", Spec{}, "", fmt.Errorf("the provider did not accept the login: %w", e)
	case <-time.After(60 * time.Second):
		return "", Spec{}, "", errors.New("the provider did not answer rclone in time")
	case <-ctx.Done():
		return "", Spec{}, "", ctx.Err()
	}
}

// Cancel ends the login that waits.
func (f *Flows) Cancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopLocked()
}

func (f *Flows) stopLocked() {
	if f.cur != nil {
		f.cur.cancel()
		f.cur = nil
	}
}
