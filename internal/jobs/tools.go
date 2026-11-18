package jobs

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openccu-lite/lite-addon-sync/internal/rclone"
	"github.com/openccu-lite/lite-addon-sync/internal/servers"
)

// The arguments of the two tools, built as lists from a job that was checked when it was saved.
// There is no free flags field: rsync's -e and rclone's --config or --password-command run
// commands or move the credentials. Paths come after "--" where the tool has it; a source is an
// absolute path, a remote's name is the addon's own, and a folder never starts with a dash.

// RsyncArgs are rsync's arguments for a run; the remote shell is in RSYNC_RSH.
func RsyncArgs(j Job, source string, srv servers.Server, excludeFile, stamp string, dryRun bool) []string {
	args := []string{"-rlt", "-s", "--out-format=%i %n", "--info=progress2", "--stats", "--timeout=120", "--exclude-from=" + excludeFile}
	if j.Mode == ModeMirror {
		args = append(args, "--delete")
		if j.MaxDelete > 0 {
			args = append(args, "--max-delete="+strconv.Itoa(j.MaxDelete))
		}
	}
	if j.BwLimit > 0 {
		args = append(args, "--bwlimit="+strconv.Itoa(j.BwLimit))
	}
	if j.Versions {
		// relative to the destination: what is replaced or deleted goes there instead
		args = append(args, "--backup", "--backup-dir="+VersionsDir+"/"+stamp)
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	return append(args, "--", strings.TrimRight(source, "/")+"/", servers.Dest(srv, j.Target.Folder))
}

// RsyncMkdirArgs are rsync's arguments for making the job's folder below the server's base path
// before a run: tree is a local directory that holds the folder's path as empty directories, and
// copying it creates what is missing on the server and changes nothing else. rsync itself makes
// only the last part of a destination, and --mkpath is unknown to the rsync of many a NAS.
func RsyncMkdirArgs(tree string, srv servers.Server) []string {
	return []string{"-r", "-s", "--timeout=60", "--", strings.TrimRight(tree, "/") + "/", servers.Dest(srv, "")}
}

// RsyncListArgs lists the folders of runs in the versions' folder on a server.
func RsyncListArgs(j Job, srv servers.Server) []string {
	return []string{"--list-only", "-s", "--timeout=60", "--", servers.Dest(srv, j.Target.Folder+"/"+VersionsDir)}
}

// RsyncPruneArgs removes one run's folder from the versions' folder on a server, with rsync alone
// (a server that allows nothing but rsync takes it): an empty local directory is mirrored onto the
// versions' folder with only that run's folder inside the transfer.
func RsyncPruneArgs(j Job, srv servers.Server, empty, stamp string) []string {
	return []string{"-r", "-s", "--timeout=120", "--delete", "--include=/" + stamp + "/***", "--exclude=*", "--", strings.TrimRight(empty, "/") + "/", servers.Dest(srv, j.Target.Folder+"/"+VersionsDir)}
}

// OldVersions picks, out of the names in a versions' folder, the runs older than days: names of
// the runs' time format alone, so nothing else that lies there is ever removed.
func OldVersions(names []string, days int, now time.Time) []string {
	if days <= 0 {
		return nil
	}
	limit := now.UTC().AddDate(0, 0, -days)
	var out []string
	for _, n := range names {
		n = strings.TrimSuffix(n, "/")
		t, err := time.Parse(runTimeLayout, n)
		if err == nil && t.Before(limit) {
			out = append(out, n)
		}
	}
	return out
}

// RcloneArgs are rclone's arguments for a run (after Runner.JobCommand's own).
func RcloneArgs(j Job, source, dest, excludeFile, stamp string, dryRun bool) []string {
	verb := "copy"
	if j.Mode == ModeMirror {
		verb = "sync"
	}
	args := []string{verb, "--exclude-from", excludeFile, "--skip-links"}
	if j.Mode == ModeMirror && j.MaxDelete > 0 {
		args = append(args, "--max-delete", strconv.Itoa(j.MaxDelete))
	}
	if j.BwLimit > 0 {
		args = append(args, "--bwlimit", strconv.Itoa(j.BwLimit)+"k")
	}
	if j.Versions {
		args = append(args, "--backup-dir", RcloneJoin(dest, VersionsDir, stamp))
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	return append(args, source, dest)
}

// RcloneDest is where a job's files go on its account: the job's folder below the account's root,
// or - for an encrypted job - the job's own crypt remote, which lies over that folder.
func RcloneDest(j Job, remote, root string) string {
	if j.Encrypt {
		return CryptRemote(j.ID) + ":"
	}
	return rclone.Path(remote, root, j.Target.Folder)
}

// RcloneJoin appends path parts to a destination ("remote:" or "remote:path").
func RcloneJoin(dest string, parts ...string) string {
	p := strings.Join(parts, "/")
	if strings.HasSuffix(dest, ":") {
		return dest + p
	}
	return dest + "/" + p
}

var (
	rsyncProgressRe = regexp.MustCompile(`^\s*([\d,.]+)\s+(\d+)%\s+(\S+)\s+(\d+:\d+:\d+)`)
	rsyncItemRe     = regexp.MustCompile(`^([<>ch.*][fdLDS.][+.?A-Za-z ]{9}|\*deleting  ) (.+)$`)
	rsyncSizeRe     = regexp.MustCompile(`^Total transferred file size: ([\d,.]+) bytes`)
)

func digits(s string) int64 {
	n, _ := strconv.ParseInt(strings.NewReplacer(",", "", ".", "").Replace(s), 10, 64)
	return n
}

// parseRsync reads one line of rsync's output: an itemized change, the progress line, a figure of
// the summary, or (on stderr) an error.
func parseRsync(line string, stderr bool, rl *runLog, rec *Record, prog *Progress) {
	if stderr {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "rsync error:") {
			rl.add("E", t)
		}
		return
	}
	if m := rsyncProgressRe.FindStringSubmatch(line); m != nil {
		rl.mu.Lock()
		prog.Bytes = digits(m[1])
		prog.Percent, _ = strconv.Atoi(m[2])
		prog.Speed, prog.ETA = m[3], m[4]
		rl.mu.Unlock()
		return
	}
	if m := rsyncSizeRe.FindStringSubmatch(line); m != nil {
		rl.mu.Lock()
		rec.Bytes = digits(m[1])
		rl.mu.Unlock()
		return
	}
	m := rsyncItemRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	item, name := m[1], m[2]
	switch {
	case strings.HasPrefix(item, "*deleting"):
		rl.add("D", name)
	case (item[0] == '>' || item[0] == '<' || item[0] == 'c') && (item[1] == 'f' || item[1] == 'L'):
		if strings.Trim(item[2:], "+") == "" {
			rl.add("C", name)
		} else {
			rl.add("R", name)
		}
	}
}

// rsyncResult maps rsync's exit code and what it said onto a run's result.
func rsyncResult(exit int, stderr string) string {
	switch exit {
	case 0:
		return ResultOK
	case 23, 24:
		return ResultWarnings
	case 25:
		return ResultTooManyDeletes
	case 20:
		return ResultStopped
	}
	low := strings.ToLower(stderr)
	if strings.Contains(low, "no space left") || strings.Contains(low, "quota exceeded") || strings.Contains(low, "disk quota") {
		return ResultFull
	}
	switch servers.Classify(stderr, exit) {
	case servers.StateHostKeyChanged, servers.StateHostKeyUnknown:
		return ResultHostKey
	case servers.StateAuthFailed:
		return ResultLoginRefused
	case servers.StateUnreachable:
		return ResultUnreachable
	}
	if exit == 10 || exit == 30 || exit == 35 {
		return ResultUnreachable
	}
	return ResultFailed
}

// rcloneLine is one JSON line of rclone's log.
type rcloneLine struct {
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Object string `json:"object"`
	Stats  *struct {
		Bytes      int64   `json:"bytes"`
		TotalBytes int64   `json:"totalBytes"`
		Speed      float64 `json:"speed"`
		ETA        *int64  `json:"eta"`
	} `json:"stats"`
}

// parseRclone reads one line of rclone's JSON log (stderr): a file that was copied, replaced or
// deleted (or would be, in a dry run), the figures, or an error.
func parseRclone(line string, stderr bool, rl *runLog, rec *Record, prog *Progress) {
	if !stderr {
		return
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var l rcloneLine
	if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &l) != nil {
		rl.add("E", line)
		return
	}
	if l.Stats != nil {
		rl.mu.Lock()
		rec.Bytes, prog.Bytes = l.Stats.Bytes, l.Stats.Bytes
		if l.Stats.TotalBytes > 0 {
			prog.Percent = int(l.Stats.Bytes * 100 / l.Stats.TotalBytes)
		}
		if l.Stats.Speed > 0 {
			prog.Speed = strconv.FormatFloat(l.Stats.Speed/1024, 'f', 0, 64) + " KiB/s"
		}
		if l.Stats.ETA != nil {
			prog.ETA = strconv.FormatInt(*l.Stats.ETA, 10) + " s"
		}
		rl.mu.Unlock()
		return
	}
	msg := strings.ToLower(l.Msg)
	// with a backup directory (versions) a file that is replaced is first moved away and then
	// copied as if it were new, and a deleted one is "moved into backup dir"
	if l.Object != "" && (strings.HasPrefix(msg, "moved into backup dir") || strings.HasPrefix(msg, "skipped move into backup dir")) {
		rl.add("D", l.Object)
		return
	}
	if l.Object != "" && (strings.HasPrefix(msg, "moved") || strings.HasPrefix(msg, "skipped move")) {
		rl.mu.Lock()
		if rl.moved == nil {
			rl.moved = map[string]bool{}
		}
		rl.moved[l.Object] = true
		rl.mu.Unlock()
		return
	}
	rl.mu.Lock()
	wasMoved := rl.moved[l.Object]
	rl.mu.Unlock()
	switch {
	case l.Level == "error" || l.Level == "critical" || l.Level == "fatal":
		// "Attempt 1/1 failed with 2 errors and: …" repeats the errors above it
		if strings.HasPrefix(l.Msg, "Attempt ") && strings.Contains(l.Msg, " failed with ") {
			return
		}
		text := l.Msg
		if l.Object != "" {
			text = l.Object + ": " + text
		}
		rl.add("E", text)
	case l.Object == "":
	case strings.HasPrefix(msg, "copied (replaced"), wasMoved && (strings.HasPrefix(msg, "copied") || strings.HasPrefix(msg, "skipped copy as --dry-run")):
		rl.add("R", l.Object)
	case strings.HasPrefix(msg, "copied"), strings.HasPrefix(msg, "skipped copy as --dry-run"), strings.HasPrefix(msg, "multi-thread copied"):
		rl.add("C", l.Object)
	case strings.HasPrefix(msg, "deleted"), strings.HasPrefix(msg, "skipped delete as --dry-run"):
		rl.add("D", l.Object)
	}
}

// rcloneResult maps rclone's exit code and what it said onto a run's result.
func rcloneResult(exit int, stderr string) string {
	if exit == 0 {
		return ResultOK
	}
	low := strings.ToLower(stderr)
	if strings.Contains(low, "max-delete") || strings.Contains(low, "max delete") {
		return ResultTooManyDeletes
	}
	switch rclone.Classify(stderr, exit) {
	case rclone.StateAuthFailed:
		return ResultLoginExpired
	case rclone.StateFull:
		return ResultFull
	case rclone.StateUnreachable:
		return ResultUnreachable
	}
	if exit == 9 {
		return ResultOK // nothing to transfer
	}
	return ResultFailed
}
