// lite-sync is the lite-addon-sync daemon: it keeps the accounts a user connected (OneDrive,
// Google Drive, Dropbox by OAuth; S3 and WebDAV by keys) and offers them to openccu-lite as backup
// targets - the system hands each encrypted backup to it over a Unix socket, and it uploads it
// with the rclone it ships. It serves its page and API on the loopback behind lighttpd's proxy of
// /addons/sync/. Nothing is contacted until an account is connected. openccu-lite only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/hobbyquaker/ccu-addon-kit/go/addonkit"

	"github.com/openccu-lite/lite-addon-sync/internal/accounts"
	"github.com/openccu-lite/lite-addon-sync/internal/httpapi"
	"github.com/openccu-lite/lite-addon-sync/internal/jobs"
	"github.com/openccu-lite/lite-addon-sync/internal/notices"
	"github.com/openccu-lite/lite-addon-sync/internal/provider"
	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
	"github.com/openccu-lite/lite-addon-sync/internal/servers"
	"github.com/openccu-lite/lite-addon-sync/internal/sysinfo"
	"github.com/openccu-lite/lite-addon-sync/internal/systimers"
)

// version is set by the build (-ldflags "-X main.version=…").
var version = "dev"

type options struct {
	addonDir, confDir, id, base, basePath, www, rcloneBin, socket, tokenFile, oauthExtra, noticesURL, runDir string
	port                                                                                                     int
	noSession, anyPeer                                                                                       bool
}

// envOr answers an environment variable, or def when it is not set. The variables place a copy
// job's run in a test: on the system the unit's command line is fixed (`lite-sync run <job>`).
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// runJob is `lite-sync run <job>`: one run of a copy job, as the job's unit on the system starts
// it. It exits 0 when the run succeeded or was stopped, so a failed run is a failed unit and a
// stopped one is not.
func runJob(id string) int {
	logf := func(format string, args ...any) { log.Printf("sync: "+format, args...) }
	jobs.Root = envOr("SYNC_ROOT", jobs.Root)
	addonDir := envOr("SYNC_ADDON_DIR", "/usr/local/addons/sync")
	confDir := envOr("SYNC_CONF_DIR", "/usr/local/etc/config/addons/sync")
	home := filepath.Join(addonDir, "var")
	_ = os.MkdirAll(home, 0o700)
	rc := &rclone.Runner{Bin: envOr("SYNC_RCLONE", filepath.Join(addonDir, "bin", "rclone")), Config: filepath.Join(confDir, "rclone.conf"), Home: home, Log: nil}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rec, err := jobs.Run(ctx, jobs.Env{AddonDir: addonDir, ConfDir: confDir, RunDir: envOr("SYNC_RUN_DIR", jobs.DefaultRunDir()), Rclone: rc, Log: logf}, id)
	os.Stdout.Sync()
	// a run somebody stopped did not fail: its unit is not a failed unit (the record says "stopped")
	if rec.Result == jobs.ResultStopped {
		return 0
	}
	if err != nil {
		logf("%v", err)
		return 1
	}
	return 0
}

func main() {
	log.SetFlags(0) // the journal adds the time
	if len(os.Args) == 3 && os.Args[1] == "run" {
		os.Exit(runJob(os.Args[2]))
	}
	var o options
	jobs.Root = envOr("SYNC_ROOT", jobs.Root) // tests only: another storage than /usr/local
	flag.StringVar(&o.addonDir, "addon-dir", "/usr/local/addons/sync", "the addon's directory (app/, bin/, var/)")
	flag.StringVar(&o.confDir, "conf-dir", "/usr/local/etc/config/addons/sync", "the configuration's directory (accounts.json, rclone.conf; mode 0700)")
	flag.StringVar(&o.id, "id", "sync", "the addon id (token file, base path)")
	flag.IntVar(&o.port, "port", 8292, "the loopback port lighttpd proxies to")
	flag.StringVar(&o.base, "system", "http://127.0.0.1", "openccu-lite's URL")
	flag.StringVar(&o.basePath, "base-path", "", "the path the page is served at (/addons/<id> by default)")
	flag.StringVar(&o.www, "www", "", "the built page (<addon-dir>/app by default)")
	flag.StringVar(&o.rcloneBin, "rclone", "", "the rclone binary (<addon-dir>/bin/rclone by default)")
	flag.StringVar(&o.socket, "socket", provider.DefaultSocket, "the backup provider's Unix socket (the system makes its directory)")
	flag.StringVar(&o.tokenFile, "token-file", "", "a token file for the system's API (the addon's own token by default)")
	flag.BoolVar(&o.noSession, "no-session-check", false, "serve the page without the session check (development only)")
	flag.BoolVar(&o.anyPeer, "any-peer", false, "serve the provider's socket to this process's own user too (tests only)")
	flag.StringVar(&o.oauthExtra, "oauth-extra", "", "extra rclone options for a login, as JSON (tests only: the provider's addresses)")
	flag.StringVar(&o.runDir, "run-dir", jobs.DefaultRunDir(), "the runtime directory: a running copy job's progress, the run lock")
	flag.StringVar(&o.noticesURL, "notices", "http://127.0.0.1:8183", "the system service's loopback address for the addon's notices on the Status page (empty: none)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if o.basePath == "" {
		o.basePath = "/addons/" + o.id
	}
	if o.www == "" {
		o.www = filepath.Join(o.addonDir, "app")
	}
	if o.rcloneBin == "" {
		o.rcloneBin = filepath.Join(o.addonDir, "bin", "rclone")
	}
	if err := run(o); err != nil {
		log.Fatalf("sync: %v", err)
	}
}

func run(o options) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logf := func(format string, args ...any) { log.Printf("sync: "+format, args...) }

	home := filepath.Join(o.addonDir, "var")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(o.confDir, 0o700); err != nil {
		return err
	}
	rc := &rclone.Runner{Bin: o.rcloneBin, Config: filepath.Join(o.confDir, "rclone.conf"), Home: home, Log: logf}
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	rcloneVersion := rc.Version(vctx)
	cancel()
	if rcloneVersion == "" {
		logf("rclone does not run (%s): no account can be reached until the addon is installed again", o.rcloneBin)
	}
	store, err := accounts.Open(o.confDir, rc)
	if err != nil {
		return err
	}
	flows := &accounts.Flows{Bin: o.rcloneBin, Env: append(os.Environ(), "RCLONE_CONFIG="+filepath.Join(home, "authorize.conf"), "HOME="+home), Log: logf}
	if o.oauthExtra != "" {
		if err := json.Unmarshal([]byte(o.oauthExtra), &flows.Extra); err != nil {
			return fmt.Errorf("-oauth-extra: %w", err)
		}
	}
	defer flows.Cancel()

	// the backup provider's socket: the system makes the directory before the unit starts; on a
	// system that does not know the extension yet there is none, and the page says so
	var pmu sync.Mutex
	providerOK, providerProblem := false, ""
	setProvider := func(ok bool, problem string) {
		pmu.Lock()
		providerOK, providerProblem = ok, problem
		pmu.Unlock()
	}
	peers := map[uint32]bool{0: true}
	if u, err := user.Lookup("occulite"); err == nil {
		if n, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
			peers[uint32(n)] = true
		}
	}
	if o.anyPeer {
		peers[uint32(os.Getuid())] = true
	}
	psrv := &provider.Server{Accounts: store, Remote: rc, SetupHref: "/addon-settings/" + o.id + "#accounts", PeerOK: func(uid uint32) bool { return peers[uid] }, Log: logf}
	perr := make(chan error, 1)
	if l, err := provider.Listen(o.socket); err != nil {
		setProvider(false, err.Error())
		logf("the backup provider's socket could not be opened (%v): this system does not offer the addon's accounts as backup targets (it needs openccu-lite 2.0.0-beta.7 or newer)", err)
	} else {
		setProvider(true, "")
		go func() { perr <- psrv.Serve(ctx, l) }()
		defer os.Remove(o.socket)
	}

	// a login that is about to lapse is said on the system's Status page (an addon notice)
	var nt *notices.Notifier
	if o.noticesURL != "" && !o.noSession {
		nt = &notices.Notifier{Base: o.noticesURL, TokenFile: filepath.Join(notices.ControlTokenDir, o.id), AddonID: o.id, Accounts: store.List, Log: logf}
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute): // the system mints the token at its own start
			}
			nt.Run(ctx)
		}()
	}

	var token addonkit.TokenSource
	if o.tokenFile != "" {
		token = &addonkit.FileToken{Path: o.tokenFile}
	} else {
		token = addonkit.AddonToken(o.id)
	}
	facts := &sysinfo.Client{Base: o.base, HTTP: &http.Client{Transport: &addonkit.AuthTransport{Source: token}, Timeout: 30 * time.Second}, AddonID: o.id}

	// the copy jobs: their records, the SSH servers, and the system's timers that run them
	srvStore, err := servers.Open(o.confDir, servers.Tools{}, home)
	if err != nil {
		return err
	}
	srvStore.Log = logf
	jobStore, err := jobs.Open(o.confDir)
	if err != nil {
		return err
	}
	_ = os.MkdirAll(o.runDir, 0o700)
	timers := &systimers.Client{Base: o.base, HTTP: &http.Client{Transport: &addonkit.AuthTransport{Source: token}, Timeout: 90 * time.Second}}
	// a copy job whose last good run is too long ago is said on the Status page as well
	if nt != nil {
		overdue := &notices.Items{N: nt, Source: func(ctx context.Context) ([]notices.Item, bool) {
			list, _, err := timers.List(ctx)
			if err != nil {
				return nil, false
			}
			byName := map[string]systimers.Timer{}
			for _, t := range list {
				byName[t.Name] = t
			}
			var items []notices.Item
			for _, j := range jobStore.List() {
				t, ok := byName[j.ID]
				if !ok {
					continue
				}
				since, late := jobs.Overdue(j, jobs.ReadTimer(j.ID, t.TimerFile, t.Enabled), jobs.History(o.addonDir, j.ID), time.Now())
				if !late {
					continue
				}
				day := since.Local().Format("2006-01-02")
				items = append(items, notices.Item{ID: "overdue-" + j.ID, Link: "/addon-settings/" + o.id + "#jobs",
					Title: map[string]string{"de": fmt.Sprintf("Der Kopierauftrag „%s“ ist überfällig", j.Name), "en": fmt.Sprintf("The copy job \"%s\" is overdue", j.Name)},
					Text: map[string]string{
						"de": fmt.Sprintf("Sein letzter erfolgreicher Lauf war am %s. Der Verlauf des Addons zeigt, woran die Läufe seitdem gescheitert sind.", day),
						"en": fmt.Sprintf("Its last run that worked was on %s. The addon's history shows why the runs since then failed.", day),
					}})
			}
			return items, true
		}, All: func() []string {
			var ids []string
			for _, j := range jobStore.List() {
				ids = append(ids, "overdue-"+j.ID)
			}
			return ids
		}}
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Minute):
			}
			overdue.Run(ctx, 30*time.Minute)
		}()
	}

	wrap := func(h http.Handler) http.Handler { return h }
	if !o.noSession {
		c := addonkit.NewChecker(addonkit.SessionOptions{Base: o.base, Need: addonkit.LevelAdmin, AddonID: o.id, OnRefused: func(r *http.Request, reason string) {
			logf("refused %s %s: %s", r.Method, r.URL.Path, reason)
		}})
		wrap = c.Handler
	}
	h := httpapi.New(httpapi.Options{BasePath: o.basePath, WWW: o.www, Accounts: store, Flows: flows, Facts: facts, Wrap: wrap, Version: version, Rclone: rcloneVersion, Started: time.Now(),
		Servers: srvStore, Jobs: jobStore, Timers: timers, AddonID: o.id, AddonDir: o.addonDir, RunDir: o.runDir, Log: logf,
		Provider: func() (bool, string) {
			pmu.Lock()
			defer pmu.Unlock()
			return providerOK, providerProblem
		}})
	l, err := addonkit.Listen(o.port, "")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logf("%s (rclone %s): serving %s on %s; %d accounts, %d SSH servers, %d copy jobs; the backup provider on %s: %v", version, rcloneVersion, o.basePath, l.Addr(), len(store.List()), len(srvStore.List()), len(jobStore.List()), o.socket, providerOK)
	serr := make(chan error, 1)
	go func() { serr <- srv.Serve(l) }()
	select {
	case err := <-serr:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	case err := <-perr:
		if err != nil {
			setProvider(false, err.Error())
			logf("the backup provider's socket stopped: %v", err)
		}
		if err := <-serr; err != nil && err != http.ErrServerClosed {
			return err
		}
	}
	logf("stopping")
	os.Stdout.Sync()
	return nil
}
