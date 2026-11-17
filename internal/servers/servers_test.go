package servers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpecRules(t *testing.T) {
	good := Spec{Name: "NAS", Host: "nas.example.org", User: "backup", Path: "/volume1/ccu"}
	sp := good
	if err := sp.clean(); err != nil || sp.Port != 22 {
		t.Fatal(sp, err)
	}
	for what, change := range map[string]func(*Spec){
		"no name":                  func(sp *Spec) { sp.Name = "" },
		"a host that is an option": func(sp *Spec) { sp.Host = "-oProxyCommand=sh" },
		"a host with a space":      func(sp *Spec) { sp.Host = "nas example" },
		"a host with a user":       func(sp *Spec) { sp.Host = "root@nas" },
		"a host with a slash":      func(sp *Spec) { sp.Host = "nas/x" },
		"a port out of range":      func(sp *Spec) { sp.Port = 70000 },
		"a user that is an option": func(sp *Spec) { sp.User = "-l" },
		"a user with a space":      func(sp *Spec) { sp.User = "a b" },
		"a user with a colon":      func(sp *Spec) { sp.User = "a:b" },
		"no path":                  func(sp *Spec) { sp.Path = "" },
		"a path that is an option": func(sp *Spec) { sp.Path = "-e" },
		"a path with ..":           func(sp *Spec) { sp.Path = "/a/../b" },
		"a path with a newline":    func(sp *Spec) { sp.Path = "/a\n/b" },
		"a path with a semicolon":  func(sp *Spec) { sp.Path = "/a;id" },
		"a path with a backtick":   func(sp *Spec) { sp.Path = "/a`id`" },
		"a path with a dollar":     func(sp *Spec) { sp.Path = "/a$(id)" },
	} {
		sp := good
		change(&sp)
		if err := sp.clean(); err == nil {
			t.Errorf("%s: accepted %+v", what, sp)
		}
	}
	for _, host := range []string{"192.0.2.10", "fd00::5", "nas", "a-b.example.org"} {
		sp := good
		sp.Host = host
		if err := sp.clean(); err != nil {
			t.Errorf("host %s: %v", host, err)
		}
	}
}

func TestFingerprintAndClassify(t *testing.T) {
	// ssh-keygen -lf of this key prints SHA256:… of the decoded blob
	fp, err := Fingerprint("AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl")
	if err != nil || !strings.HasPrefix(fp, "SHA256:") || len(fp) != 50 {
		t.Fatalf("%q %v", fp, err)
	}
	if _, err := Fingerprint("not base64!"); err == nil {
		t.Error("a broken key has a fingerprint")
	}
	for text, want := range map[string]string{
		"@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @":         StateHostKeyChanged,
		"No ED25519 host key is known for nas and you have requested strict":  StateHostKeyUnknown,
		"Host key verification failed.":                                       StateHostKeyUnknown,
		"backup@nas: Permission denied (publickey).":                          StateAuthFailed,
		"ssh: connect to host nas port 22: Connection refused":                StateUnreachable,
		"ssh: Could not resolve hostname nas: Name or service not known":      StateUnreachable,
		"bash: line 1: rsync: command not found":                              StateNoRsync,
		`rsync: [Receiver] mkdir "/volume1/x" failed: Permission denied (13)`: StateReadOnly,
		"rsync: [receiver] mkstemp failed: Read-only file system (30)":        StateReadOnly,
		"something nobody knows":                                              StateError,
	} {
		if got := Classify(text, 255); got != want {
			t.Errorf("%q: %s, want %s", text, got, want)
		}
	}
	if Classify("", 0) != StateOK || Classify("", 127) != StateNoRsync {
		t.Error("exit codes")
	}
}

// A server gets a key pair of its own, the line for authorized_keys starts with "restrict", and
// the remote shell names that key and that known_hosts and nothing else.
func TestKeyAndRSH(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen here")
	}
	dir := t.TempDir()
	s, err := Open(dir, Tools{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Create(context.Background(), Spec{Name: "NAS", Host: "nas.example.org", Port: 2222, User: "backup", Path: "/volume1/ccu"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v.PublicKey, "restrict ssh-ed25519 AAAA") || v.State != StateNotTested || len(v.HostKeys) != 0 {
		t.Fatalf("%+v", v)
	}
	key := filepath.Join(KeyDir(dir, v.ID), "id_ed25519")
	st, err := os.Stat(key)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the private key: %v %v", st, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, File))
	if strings.Contains(string(b), "PRIVATE") || !strings.Contains(string(b), "restrict ssh-ed25519") {
		t.Errorf("servers.json:\n%s", b)
	}
	rsh := RSH(Tools{}, dir, v)
	for _, want := range []string{"ssh -p 2222 -i " + key, "BatchMode=yes", "IdentitiesOnly=yes", "StrictHostKeyChecking=yes", "UserKnownHostsFile=" + filepath.Join(KeyDir(dir, v.ID), "known_hosts"), "GlobalKnownHostsFile=/dev/null", "-F /dev/null"} {
		if !strings.Contains(rsh, want) {
			t.Errorf("the remote shell lacks %q: %s", want, rsh)
		}
	}
	// without a confirmed host key nothing is tried
	if v, _ = s.Test(context.Background(), v.ID); v.State != StateHostKeyUnknown {
		t.Errorf("a test without a host key: %+v", v)
	}
	// nothing is trusted that was not read
	if _, err := s.Trust(v.ID, []string{"SHA256:x"}); err == nil {
		t.Error("a host key was trusted without a scan")
	}
	// a new key replaces the old one
	old := v.PublicKey
	if v, err = s.NewKey(context.Background(), v.ID); err != nil || v.PublicKey == old {
		t.Errorf("new key: %v", err)
	}
	// another host forgets the host keys; the file is read again as written
	_, _ = s.update(v.ID, func(v *Server) { v.HostKeys = []HostKey{{Type: "ssh-ed25519", Fingerprint: "SHA256:x"}} })
	if v, _ = s.Update(v.ID, Spec{Name: "NAS", Host: "other.example.org", Port: 2222, User: "backup", Path: "/volume1/ccu"}); len(v.HostKeys) != 0 {
		t.Errorf("another host kept the host keys: %+v", v)
	}
	if list, err := Load(dir); err != nil || len(list) != 1 || list[0].Host != "other.example.org" {
		t.Errorf("load: %+v %v", list, err)
	}
	if err := s.Delete(v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(KeyDir(dir, v.ID)); err == nil {
		t.Error("a deleted server's key stays")
	}
	if s.Delete("../../etc") == nil {
		t.Error("a path as an id")
	}
}
