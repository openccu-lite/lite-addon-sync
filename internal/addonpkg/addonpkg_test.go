// Package addonpkg tests the package files the addon ships.
package addonpkg

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/hobbyquaker/ccu-addon-kit/go/addonkit/fragment"
)

type manifest struct {
	Format      int               `json:"format"`
	ID          string            `json:"id"`
	Version     string            `json:"version"`
	Description map[string]string `json:"description"`
	Release     struct {
		GitHub string `json:"github"`
		Asset  string `json:"asset"`
	} `json:"release"`
	Requires struct {
		Lite          string   `json:"lite"`
		Architectures []string `json:"architectures"`
	} `json:"requires"`
	UI struct {
		SettingsURL   string `json:"settings_url"`
		SessionHeader bool   `json:"session_header"`
	} `json:"ui"`
	Backup struct {
		Hook    string `json:"hook"`
		Targets *struct {
			Protocol int               `json:"protocol"`
			Label    map[string]string `json:"label"`
			Setup    string            `json:"setup"`
		} `json:"targets"`
	} `json:"backup"`
	Runtime struct {
		Root         bool              `json:"root"`
		Daemon       bool              `json:"daemon"`
		Needs        *[]string         `json:"needs"`
		Capabilities []string          `json:"capabilities"`
		Groups       []string          `json:"groups"`
		Ports        []int             `json:"ports"`
		Paths        []string          `json:"paths"`
		Bin          string            `json:"bin"`
		APIScopes    []string          `json:"api_scopes"`
		Note         map[string]string `json:"note"`
	} `json:"runtime"`
}

func load(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile("../../addon/openccu-lite.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The manifest: the provider declaration as the system's contract wants it, and no privilege.
func TestManifest(t *testing.T) {
	m := load(t)
	if m.Format != 1 || m.ID != "sync" || !m.Runtime.Daemon || !m.UI.SessionHeader || m.UI.SettingsURL != "/addons/sync/" {
		t.Fatalf("%+v", m)
	}
	// the backup targets: protocol 1, the kind's word in both languages, the way to the accounts
	bt := m.Backup.Targets
	if bt == nil || bt.Protocol != 1 || bt.Label["de"] != "Cloud-Speicher" || bt.Label["en"] != "Cloud storage" || bt.Setup != "/addon-settings/sync#accounts" {
		t.Fatalf("backup.targets: %+v", bt)
	}
	if !regexp.MustCompile(`^/(addons|addon-settings)/[A-Za-z0-9._~/?#=&%+-]{1,200}$`).MatchString(bt.Setup) || len(bt.Label["de"]) > 40 {
		t.Error("backup.targets is not of the system's shape")
	}
	if m.Backup.Hook != "" {
		t.Error("the addon declares no snapshot hook: its configuration is small files written atomically")
	}
	// no capability, no root, no group, no port, no extra path: it connects outwards only, reads
	// nothing of the system, and what it gets it gets over the system's socket
	rt := m.Runtime
	if rt.Root || len(rt.Capabilities) != 0 || len(rt.Groups) != 0 || len(rt.Ports) != 0 || len(rt.Paths) != 0 {
		t.Errorf("the addon needs no privilege and no port: %+v", rt)
	}
	if len(rt.APIScopes) != 1 || rt.APIScopes[0] != "system:read" {
		t.Errorf("scopes %v: system:read alone", rt.APIScopes)
	}
	if rt.Needs == nil || len(*rt.Needs) != 0 {
		t.Error("needs: [] - the addon waits for no interface process")
	}
	if rt.Bin != "bin" {
		t.Error("runtime.bin: the daemon and rclone are root's, not the addon user's to replace")
	}
	// an older system refuses a backup block without a hook: the first version that knows targets
	if m.Requires.Lite != "2.0.0-beta.7" {
		t.Errorf("requires.lite %q", m.Requires.Lite)
	}
	if strings.Join(m.Requires.Architectures, ",") != "aarch64,x86_64" {
		t.Errorf("architectures %v", m.Requires.Architectures)
	}
	if m.Release.Asset != "sync-{arch}-{version}.tar.gz" || m.Release.GitHub != "openccu-lite/lite-addon-sync" {
		t.Errorf("release %+v", m.Release)
	}
	if m.Description["de"] == "" || m.Description["en"] == "" || rt.Note["de"] == "" || rt.Note["en"] == "" {
		t.Error("the description and the note in both languages")
	}
	for _, want := range []string{"Contacts nothing until", "encrypted", "system:read", "socket"} {
		if !strings.Contains(rt.Note["en"], want) {
			t.Errorf("the note does not say %q", want)
		}
	}
	for _, text := range []string{m.Description["de"], m.Description["en"], rt.Note["de"], rt.Note["en"]} {
		if regexp.MustCompile(`(?i)\bbox\b`).MatchString(text) {
			t.Errorf("user-facing text says box: %s", text)
		}
	}
}

// The manifest's version is the changelog's newest.
func TestVersionInChangelog(t *testing.T) {
	m := load(t)
	cl, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cl), "## ["+m.Version+"]") {
		t.Errorf("CHANGELOG.md has no entry for %s", m.Version)
	}
}

// The manifest's images, the proxy rule and the licences are tracked where the build and the
// catalogue read them.
func TestFilesTracked(t *testing.T) {
	for _, p := range []string{"addon/sync/www/icon.svg", "addon/sync/www/icon-dark.svg", "addon/sync/etc/lighttpd.conf", "addon/sync/www/lighttpd-error-503.html", "licences/rclone-COPYING", "scripts/rclone.pin"} {
		if out, err := exec.Command("git", "-C", "../..", "ls-files", "--error-unmatch", "--", p).CombinedOutput(); err != nil {
			t.Errorf("%s not tracked: %v %s", p, err, out)
		}
	}
}

// The proxy rule is what the kit's renderer writes (apart from the comment lines).
func TestFragment(t *testing.T) {
	want, err := fragment.Render(fragment.Fragment{ID: "sync", Port: 8292, ErrorfilePrefix: "/usr/local/addons/sync/www/lighttpd-error-"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile("../../addon/sync/etc/lighttpd.conf")
	strip := func(s string) string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if !strings.HasPrefix(l, "#") && strings.TrimSpace(l) != "" {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	if strip(string(got)) != strip(want) {
		t.Errorf("etc/lighttpd.conf:\n%s\nwant:\n%s", got, want)
	}
}

// The 503 page names the addon, in the tracked copy and the one the build copies.
func TestErrorPage(t *testing.T) {
	for _, p := range []string{"../../addon/static/lighttpd-error-503.html", "../../addon/sync/www/lighttpd-error-503.html"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "<title>Sync</title>") {
			t.Errorf("%s does not name the addon", p)
		}
	}
}

// The rc.d script and the update_script name no other addon, are sh scripts, and keep the data.
func TestScripts(t *testing.T) {
	for _, p := range []string{"../../addon/sync/rc.d", "../../addon/update_script"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, other := range []string{"tailscale", "Tailscale", "TS_", "snmp", "forwarder", "upsd", "wireguard"} {
			if strings.Contains(string(b), other) {
				t.Errorf("%s names another addon: %s", p, other)
			}
		}
		if !strings.HasPrefix(string(b), "#!/bin/sh\n") {
			t.Errorf("%s is not a sh script", p)
		}
		// neither an update nor an uninstall removes the accounts
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "rm ") && strings.Contains(l, "$CONF_DIR/addons/$ADDON") && !strings.Contains(l, "www") {
				t.Errorf("%s removes the configuration: %s", p, l)
			}
		}
	}
	rc, _ := os.ReadFile("../../addon/sync/rc.d")
	if !strings.Contains(string(rc), "ADDON=sync\n") || !strings.Contains(string(rc), "DAEMON=$ADDON_DIR/bin/lite-sync\n") || !strings.Contains(string(rc), `DAEMON_ARGS="-addon-dir $ADDON_DIR"`) {
		t.Error("rc.d: the addon name, the daemon or its arguments")
	}
	us, _ := os.ReadFile("../../addon/update_script")
	for _, tagged := range []string{"$ADDON_DIR/bin/.nobackup", "$ADDON_DIR/var/.nobackup"} {
		if !strings.Contains(string(us), tagged) {
			t.Errorf("update_script: %s keeps rclone (80 MB) or its cache out of every backup", tagged)
		}
	}
}

// The pin names a version and a SHA-256 per architecture, and the build refuses anything else.
func TestRclonePin(t *testing.T) {
	b, err := os.ReadFile("../../scripts/rclone.pin")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{`(?m)^RCLONE_VERSION=v\d+\.\d+\.\d+$`, `(?m)^RCLONE_SHA256_x86_64=[0-9a-f]{64}$`, `(?m)^RCLONE_SHA256_aarch64=[0-9a-f]{64}$`} {
		if !regexp.MustCompile(re).Match(b) {
			t.Errorf("scripts/rclone.pin: no line %s", re)
		}
	}
	fetch, _ := os.ReadFile("../../scripts/fetch-rclone.sh")
	if !strings.Contains(string(fetch), "sha256sum") || !strings.Contains(string(fetch), "does not match the pinned SHA-256") {
		t.Error("fetch-rclone.sh must check the download against the pin")
	}
}
