// Package accounts keeps the addon's accounts: the places a user connected once - OneDrive, Google
// Drive, Dropbox by OAuth, S3 and WebDAV by keys - and that the system's backup (and later the copy
// jobs) use. An account is a record here (accounts.json, nothing secret in it) and a remote in
// rclone's configuration (rclone.conf, 0600: the tokens and keys, written by rclone itself).
package accounts

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
)

// The kinds of account.
const (
	KindOneDrive = "onedrive"
	KindDrive    = "drive"
	KindDropbox  = "dropbox"
	KindS3       = "s3"
	KindWebDAV   = "webdav"
)

// Kinds are the kinds in the order the page offers them.
var Kinds = []string{KindOneDrive, KindDrive, KindDropbox, KindS3, KindWebDAV}

// OAuth says whether a kind logs in at its provider in a browser.
func OAuth(kind string) bool {
	return kind == KindOneDrive || kind == KindDrive || kind == KindDropbox
}

// Label is a kind's name as its provider writes it.
func Label(kind string) string {
	switch kind {
	case KindOneDrive:
		return "OneDrive"
	case KindDrive:
		return "Google Drive"
	case KindDropbox:
		return "Dropbox"
	case KindS3:
		return "S3"
	case KindWebDAV:
		return "WebDAV"
	}
	return kind
}

// Account is one connected place.
type Account struct {
	// ID is stable and opaque: the backup target on the system names the account by it.
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Detail is one line that says which account it is ("OneDrive · max@example.org").
	Detail string `json:"detail"`
	// Root is the path every folder lies under on the remote: an S3 account's bucket.
	Root string `json:"root,omitempty"`
	// Auth is how an OAuth account logged in: "oauth", or "service-account" (Google Drive).
	Auth string `json:"auth,omitempty"`
	// OwnClient: the account uses the user's own client ID (Google Drive always).
	OwnClient bool      `json:"own_client,omitempty"`
	Created   time.Time `json:"created"`
	// State is the account's state as the last test or transfer left it, Text rclone's line for a
	// failure, Checked when.
	State   string    `json:"state"`
	Text    string    `json:"text,omitempty"`
	Checked time.Time `json:"checked,omitzero"`
	// LastOK is when the account last answered: its login was in use then. OneDrive ends a login
	// that is not used for 90 days.
	LastOK time.Time `json:"last_ok,omitzero"`
	// UploadLimit is the rate limit of uploads in KiB/s, 0 = none.
	UploadLimit int `json:"upload_limit_kbps"`
	// LastUpload is the last backup this account took.
	LastUpload *Upload `json:"last_upload,omitempty"`
	// Free and Total are what the account said about its room at the last test, 0 = it does not say.
	Free  int64 `json:"free_bytes,omitempty"`
	Total int64 `json:"total_bytes,omitempty"`
}

// Upload is the outcome of one upload.
type Upload struct {
	At   time.Time `json:"at"`
	OK   bool      `json:"ok"`
	Name string    `json:"name,omitempty"`
	Size int64     `json:"size,omitempty"`
}

// Remote is the account's remote in rclone's configuration.
func (a Account) Remote() string { return "acc-" + a.ID }

// ValidationError is a field the user must correct.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, a...)} }

// ErrNotFound: no account of that id.
var ErrNotFound = errors.New("no such account")

// File is the accounts' file in the configuration directory.
const File = "accounts.json"

type file struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
}

// Store keeps the accounts.
type Store struct {
	// Dir is the configuration directory (0700).
	Dir    string
	Rclone *rclone.Runner
	Now    func() time.Time

	mu   sync.Mutex
	list []Account
}

// Open reads the accounts; a record whose remote is gone from rclone.conf (a file removed by hand,
// a restore of one without the other) stays and says so in its state.
func Open(dir string, r *rclone.Runner) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, Rclone: r}
	b, err := os.ReadFile(filepath.Join(dir, File))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	s.list = f.Accounts
	return s, nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(file{Version: 1, Accounts: s.list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, File+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, File))
}

// List answers the accounts, oldest first.
func (s *Store) List() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Account{}, s.list...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Get answers one account.
func (s *Store) Get(id string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.list {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// update changes one account under the lock and saves.
func (s *Store) update(id string, f func(*Account)) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.list {
		if s.list[i].ID == id {
			f(&s.list[i])
			return s.list[i], s.save()
		}
	}
	return Account{}, ErrNotFound
}

// NewID makes an account id: "a" and nine random letters and digits.
func NewID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "a" + string(b)
}

// Spec is what the user enters for a new account, or for a new login of an existing one.
type Spec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// OAuth kinds: the user's own client (Google Drive: required unless a service account is given).
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// Google Drive without a browser: the service account's JSON key, and the folder shared with it.
	ServiceAccount string `json:"service_account,omitempty"`
	FolderID       string `json:"folder_id,omitempty"`
	// S3
	Provider  string `json:"provider,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	// WebDAV
	URL      string `json:"url,omitempty"`
	Vendor   string `json:"vendor,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

var (
	bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	tokenRe  = regexp.MustCompile(`^[A-Za-z0-9._~+/=:@-]{1,300}$`)
	regionRe = regexp.MustCompile(`^[a-z0-9-]{0,40}$`)
)

// s3Providers are the values of rclone's S3 "provider" the page offers.
var s3Providers = map[string]bool{"AWS": true, "Minio": true, "Wasabi": true, "Cloudflare": true, "DigitalOcean": true, "Scaleway": true, "Hetzner": true, "IONOS": true, "Other": true}

// webdavVendors are the values of rclone's WebDAV "vendor" the page offers.
var webdavVendors = map[string]bool{"nextcloud": true, "owncloud": true, "other": true}

func oneLine(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// checkURL accepts an http(s) URL with a host and nothing that could be read as an option.
func checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || !oneLine(raw, 500) {
		return nil, invalid("an address like https://cloud.example.org/remote.php/dav/files/name")
	}
	return u, nil
}

// Validate checks a spec's shape. withSecrets: the keys a new account or a new login needs.
func (sp *Spec) Validate() error {
	sp.Name = strings.TrimSpace(sp.Name)
	if !oneLine(sp.Name, 64) {
		return invalid("name: 1 to 64 characters")
	}
	if sp.ClientID != "" && !tokenRe.MatchString(sp.ClientID) || sp.ClientSecret != "" && !tokenRe.MatchString(sp.ClientSecret) {
		return invalid("client ID and secret: as the provider shows them, without spaces")
	}
	switch sp.Kind {
	case KindOneDrive, KindDropbox:
		if (sp.ClientID == "") != (sp.ClientSecret == "") {
			return invalid("an own client needs its ID and its secret")
		}
	case KindDrive:
		if sp.ServiceAccount != "" {
			var k struct {
				Type        string `json:"type"`
				ClientEmail string `json:"client_email"`
				PrivateKey  string `json:"private_key"`
			}
			if len(sp.ServiceAccount) > 16<<10 || json.Unmarshal([]byte(sp.ServiceAccount), &k) != nil || k.Type != "service_account" || k.ClientEmail == "" || k.PrivateKey == "" {
				return invalid("the service account's key: the JSON file Google gave you for it")
			}
			if sp.FolderID != "" && !regexp.MustCompile(`^[A-Za-z0-9_-]{5,100}$`).MatchString(sp.FolderID) {
				return invalid("folder ID: the last part of the folder's address in Google Drive")
			}
			if sp.FolderID == "" {
				return invalid("a service account has no storage of its own: share a folder of your Google Drive with its address and enter that folder's ID")
			}
		} else if sp.ClientID == "" || sp.ClientSecret == "" {
			return invalid("Google Drive needs your own client ID and secret (or a service account): rclone's shared ID is being retired")
		}
	case KindS3:
		if sp.Provider == "" {
			sp.Provider = "Other"
			if sp.Endpoint == "" {
				sp.Provider = "AWS"
			}
		}
		if !s3Providers[sp.Provider] {
			return invalid("provider: one of the listed S3 providers")
		}
		if sp.Endpoint != "" {
			if _, err := checkURL(sp.Endpoint); err != nil {
				return invalid("endpoint: an address like https://s3.example.org")
			}
		} else if sp.Provider != "AWS" {
			return invalid("endpoint: the address of the S3 service")
		}
		if !regionRe.MatchString(sp.Region) {
			return invalid("region: lower-case letters, digits and -")
		}
		if !bucketRe.MatchString(sp.Bucket) {
			return invalid("bucket: the name of an existing bucket")
		}
		if !tokenRe.MatchString(sp.AccessKey) || !oneLine(sp.SecretKey, 300) || strings.HasPrefix(sp.SecretKey, "-") {
			return invalid("access key and secret key: as the provider shows them")
		}
	case KindWebDAV:
		if _, err := checkURL(sp.URL); err != nil {
			return err
		}
		if sp.Vendor == "" {
			sp.Vendor = "other"
		}
		if !webdavVendors[sp.Vendor] {
			return invalid("vendor: nextcloud, owncloud or other")
		}
		if !oneLine(sp.User, 200) || !oneLine(sp.Password, 300) {
			return invalid("user and password (Nextcloud: an app password)")
		}
	default:
		return invalid("kind: one of %s", strings.Join(Kinds, ", "))
	}
	return nil
}

// params are rclone's parameters for a spec; token is an OAuth login's token ("" for the others).
func (sp Spec) params(token string) (typ string, p map[string]string, root, detail string) {
	p = map[string]string{}
	switch sp.Kind {
	case KindOneDrive, KindDropbox, KindDrive:
		typ = sp.Kind
		p["client_id"], p["client_secret"] = sp.ClientID, sp.ClientSecret
		if sp.Kind == KindDrive {
			p["scope"] = "drive.file"
			if sp.ServiceAccount != "" {
				// the account sees what is shared with it: the one folder
				p["scope"], p["service_account_credentials"], p["root_folder_id"] = "drive", sp.ServiceAccount, sp.FolderID
				p["client_id"], p["client_secret"] = "", ""
				var k struct {
					ClientEmail string `json:"client_email"`
				}
				_ = json.Unmarshal([]byte(sp.ServiceAccount), &k)
				detail = Label(sp.Kind) + " · " + k.ClientEmail
			}
		}
		if token != "" {
			p["token"] = token
		}
	case KindS3:
		typ = "s3"
		p["provider"], p["env_auth"], p["access_key_id"], p["secret_access_key"] = sp.Provider, "false", sp.AccessKey, sp.SecretKey
		p["region"], p["endpoint"] = sp.Region, sp.Endpoint
		root = sp.Bucket
		host := "AWS"
		if u, err := url.Parse(sp.Endpoint); err == nil && u.Host != "" {
			host = u.Host
		}
		detail = "S3 · " + sp.Bucket + " · " + host
	case KindWebDAV:
		typ = "webdav"
		p["url"], p["vendor"], p["user"], p["pass"] = sp.URL, sp.Vendor, sp.User, sp.Password
		host := sp.URL
		if u, err := url.Parse(sp.URL); err == nil {
			host = u.Host
		}
		detail = "WebDAV · " + sp.User + "@" + host
	}
	if detail == "" {
		detail = Label(sp.Kind)
	}
	return typ, p, root, detail
}

// Create connects a new account: the remote in rclone's configuration, a first look at it (the
// login's account name where the provider tells it, a listing as the test), the record. token is
// the OAuth login's token for the kinds that have one. An account whose first test fails is not
// kept, so a typo does not leave a dead card.
func (s *Store) Create(ctx context.Context, sp Spec, token string) (Account, error) {
	if err := sp.Validate(); err != nil {
		return Account{}, err
	}
	if OAuth(sp.Kind) && sp.ServiceAccount == "" && token == "" {
		return Account{}, invalid("this kind of account logs in at its provider first")
	}
	a := Account{ID: NewID(), Name: sp.Name, Kind: sp.Kind, Created: s.now().UTC(), OwnClient: sp.ClientID != ""}
	if OAuth(sp.Kind) {
		a.Auth = "oauth"
		if sp.ServiceAccount != "" {
			a.Auth = "service-account"
		}
	}
	if err := s.configure(ctx, &a, sp, token); err != nil {
		return Account{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) >= 32 {
		_ = s.Rclone.ConfigDelete(ctx, a.Remote())
		return Account{}, invalid("at most 32 accounts")
	}
	s.list = append(s.list, a)
	return a, s.save()
}

// configure writes the remote and looks at it once.
func (s *Store) configure(ctx context.Context, a *Account, sp Spec, token string) error {
	typ, params, root, detail := sp.params(token)
	if err := s.Rclone.ConfigCreate(ctx, a.Remote(), typ, params); err != nil {
		return err
	}
	a.Root, a.Detail = root, detail
	if OAuth(sp.Kind) && sp.ServiceAccount == "" {
		if who := s.Rclone.UserInfo(ctx, a.Remote()); who != "" {
			a.Detail = Label(sp.Kind) + " · " + who
		}
	}
	space, err := s.look(ctx, *a)
	if err != nil {
		_ = s.Rclone.ConfigDelete(ctx, a.Remote())
		return err
	}
	a.State, a.Text, a.Checked, a.Free, a.Total = rclone.StateWritable, "", s.now().UTC(), space.Free, space.Total
	a.LastOK = a.Checked
	return nil
}

// look is the light test: the account answers a listing of its root (the bucket for S3), and says
// its room where it can.
func (s *Store) look(ctx context.Context, a Account) (rclone.Space, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if _, err := s.Rclone.ListDirs(ctx, rclone.Path(a.Remote(), a.Root)); err != nil {
		return rclone.Space{}, err
	}
	space, _ := s.Rclone.About(ctx, a.Remote())
	return space, nil
}

// Relogin replaces an account's credentials: a new OAuth login, new keys. The id, and with it
// every backup target that names the account, stays.
func (s *Store) Relogin(ctx context.Context, id string, sp Spec, token string) (Account, error) {
	old, ok := s.Get(id)
	if !ok {
		return Account{}, ErrNotFound
	}
	sp.Kind, sp.Name = old.Kind, old.Name
	if err := sp.Validate(); err != nil {
		return Account{}, err
	}
	if OAuth(sp.Kind) && sp.ServiceAccount == "" && token == "" {
		return Account{}, invalid("this kind of account logs in at its provider first")
	}
	// the new remote is made under another name first: a login that fails leaves the old one
	next := old
	next.ID = old.ID + "new"
	if err := s.configure(ctx, &next, sp, token); err != nil {
		return Account{}, err
	}
	if err := s.Rclone.ConfigRename(ctx, next.Remote(), old.Remote()); err != nil {
		_ = s.Rclone.ConfigDelete(ctx, next.Remote())
		return Account{}, err
	}
	return s.update(id, func(a *Account) {
		a.Detail, a.Root, a.State, a.Text, a.Checked, a.Free, a.Total = next.Detail, next.Root, next.State, "", next.Checked, next.Free, next.Total
		a.LastOK = next.Checked
		a.OwnClient = sp.ClientID != ""
		a.Auth = next.Auth
	})
}

// Rename changes an account's name and upload limit.
func (s *Store) Rename(id, name string, uploadLimit int) (Account, error) {
	name = strings.TrimSpace(name)
	if !oneLine(name, 64) {
		return Account{}, invalid("name: 1 to 64 characters")
	}
	if uploadLimit < 0 || uploadLimit > 1<<20 {
		return Account{}, invalid("upload limit: 0 (none) to 1048576 KiB/s")
	}
	return s.update(id, func(a *Account) { a.Name, a.UploadLimit = name, uploadLimit })
}

// Delete removes an account and its remote. Nothing on the account's storage is touched.
func (s *Store) Delete(ctx context.Context, id string) error {
	a, ok := s.Get(id)
	if !ok {
		return ErrNotFound
	}
	if err := s.Rclone.ConfigDelete(ctx, a.Remote()); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.list[:0]
	for _, x := range s.list {
		if x.ID != id {
			kept = append(kept, x)
		}
	}
	s.list = kept
	return s.save()
}

// Test looks at the account and records what it found.
func (s *Store) Test(ctx context.Context, id string) (Account, error) {
	a, ok := s.Get(id)
	if !ok {
		return Account{}, ErrNotFound
	}
	space, err := s.look(ctx, a)
	return s.Note(id, err, &space)
}

// Note records the outcome of a test or a transfer on the account: its state, rclone's line, and
// its room when the caller knows it.
func (s *Store) Note(id string, err error, space *rclone.Space) (Account, error) {
	return s.update(id, func(a *Account) {
		a.State, a.Text, a.Checked = rclone.StateOf(err), "", s.now().UTC()
		if err != nil {
			a.Text = err.Error()
		} else {
			a.LastOK = a.Checked
		}
		if space != nil && err == nil {
			a.Free, a.Total = space.Free, space.Total
		}
	})
}

// NoteUpload records the last upload an account took.
func (s *Store) NoteUpload(id string, u Upload) {
	_, _ = s.update(id, func(a *Account) { a.LastUpload = &u })
}
