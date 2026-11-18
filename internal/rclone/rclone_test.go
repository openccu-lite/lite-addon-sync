package rclone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		stderr string
		exit   int
		want   string
	}{
		{"2026/11/17 18:26:28 ERROR : error listing: couldn't list files: 401 Unauthorized: 401 Unauthorized", 1, StateAuthFailed},
		{`couldn't fetch token: invalid_grant: maybe token expired? - try refreshing with "rclone config reconnect acc-x:"`, 1, StateAuthFailed},
		{"Failed to create file system: empty token found - please run \"rclone config reconnect\"", 1, StateAuthFailed},
		{"InvalidAccessKeyId: The AWS Access Key Id you provided does not exist in our records", 1, StateAuthFailed},
		{"AADSTS700082: The refresh token has expired due to inactivity", 1, StateAuthFailed},
		{"Failed to rcat: googleapi: Error 403: The user's Drive storage quota has been exceeded., storageQuotaExceeded", 1, StateFull},
		{"507 Insufficient Storage", 1, StateFull},
		{"path/insufficient_space/..", 1, StateFull},
		{"dial tcp 192.0.2.1:443: connect: connection refused", 1, StateUnreachable},
		{"dial tcp: lookup cloud.example.org: no such host", 1, StateUnreachable},
		{"Post \"https://x\": net/http: TLS handshake timeout", 1, StateUnreachable},
		{"429 Too Many Requests", 5, StateUnreachable},
		{"403 Forbidden", 1, StateReadOnly},
		{"mkdir /x: permission denied", 1, StateReadOnly},
		{"error listing: directory not found", 3, "not-found"},
		{"acc-a:f/x.tmp is a directory or doesn't exist: object not found", 4, "not-found"},
		{"something nobody has seen", 1, StateError},
		{"", 3, "not-found"},
	}
	for _, c := range cases {
		if got := Classify(c.stderr, c.exit); got != c.want {
			t.Errorf("%q (exit %d): %s, want %s", c.stderr, c.exit, got, c.want)
		}
	}
}

func TestLastLineAndPath(t *testing.T) {
	stderr := "2026/11/17 18:15:09 ERROR : error listing: directory not found\n2026/11/17 18:15:09 NOTICE: Failed to lsjson with 2 errors: last error was: error in ListJSON: directory not found\n"
	if got := lastLine(stderr); got != "error listing: directory not found" {
		t.Errorf("%q", got)
	}
	if got := lastLine("2026/11/17 18:15:09 NOTICE: Failed to copy: boom"); got != "Failed to copy: boom" {
		t.Errorf("a single line: %q", got)
	}
	if lastLine("") != "" {
		t.Error("nothing said")
	}
	for want, parts := range map[string][]string{"acc-a:": {}, "acc-a:bucket/f/x": {"bucket", "/f/", "x"}, "acc-a:f": {"", "f"}} {
		if got := Path("acc-a", parts...); got != want {
			t.Errorf("%v: %q, want %q", parts, got, want)
		}
	}
}

func TestStateOf(t *testing.T) {
	if StateOf(nil) != StateWritable || StateOf(errors.New("x")) != StateError || StateOf(&Error{State: StateFull}) != StateFull {
		t.Fatal("StateOf")
	}
}

// rclone stopped by its context: a deadline is a limit that was hit and logged; a cancelled
// context is a caller that went away - wanted, and not logged.
func TestAStoppedCallIsLoggedOnlyForADeadline(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "rclone")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	r := &Runner{Bin: bin, Config: filepath.Join(dir, "rclone.conf"), Home: dir, Log: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err := r.run(ctx, nil, "lsjson", "x:")
	cancel()
	if StateOf(err) != StateUnreachable || !strings.Contains(err.Error(), "longer than its limit") || len(lines) != 1 {
		t.Fatalf("a deadline: %v, log %q", err, lines)
	}

	lines = nil
	ctx, cancel = context.WithCancel(context.Background())
	rc, err := r.Cat(ctx, "x:f/a.sbk.age")
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err = io.Copy(io.Discard, rc)
	_ = rc.Close()
	if err == nil || !strings.Contains(err.Error(), "caller went away") || len(lines) != 0 {
		t.Fatalf("a caller that went away: %v, log %q", err, lines)
	}
}
