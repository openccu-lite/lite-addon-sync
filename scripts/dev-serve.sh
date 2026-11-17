#!/bin/sh
#
#   Runs the daemon against the fake system, for the page's development and the Playwright tests:
#   the fake system on 127.0.0.1:18081, the page on http://127.0.0.1:18292/addons/sync/ (no
#   session check), and a WebDAV server on 127.0.0.1:18293 (rclone's own, over a temporary
#   directory; user "backup", password "dev-pass") as an account to connect without a cloud. A
#   login in a browser goes to the fake system's stand-in provider. A fresh configuration
#   directory each run unless SYNC_CONF_DIR names one. The page is served from ui/dist (npm run
#   build first); rclone is the pinned one (scripts/fetch-rclone.sh).
#
set -e
cd "$(dirname "$0")/.."
DIR=${SYNC_CONF_DIR:-$(mktemp -d)}
RCLONE=${RCLONE:-$(scripts/fetch-rclone.sh x86_64)}
mkdir -p build "$DIR/addon" "$DIR/remote" "$DIR/conf" "$DIR/run"
# a storage of its own for the copy jobs' folder picker (SYNC_ROOT; /usr/local on a system)
mkdir -p "$DIR/storage/export/sub" "$DIR/storage/etc/config" "$DIR/storage/tmp" "$DIR/storage/sdcard/backup" "$DIR/storage/addons/tools/bin"
echo alpha > "$DIR/storage/export/a.txt"; echo beta > "$DIR/storage/export/sub/b.txt"; echo KEY > "$DIR/storage/export/server.pem"
echo x > "$DIR/storage/sdcard/backup/ccu-2026-11-17.sbk.age"; : > "$DIR/storage/addons/tools/bin/.nobackup"
export SYNC_ROOT="$DIR/storage" SYNC_ADDON_DIR="$DIR/addon" SYNC_CONF_DIR="$DIR/conf" SYNC_RUN_DIR="$DIR/run" SYNC_RCLONE="$RCLONE"
go build -o build/fakesystem ./cmd/fakesystem
go build -o build/lite-sync-dev ./cmd/lite-sync
echo dev > "$DIR/token"
# a timer's "run" starts the addon's own runner, as the job's unit on a system does
build/fakesystem -addr 127.0.0.1:18081 -runner "$PWD/build/lite-sync-dev" &
FAKE=$!
"$RCLONE" serve webdav "$DIR/remote" --addr 127.0.0.1:18293 --user backup --pass dev-pass --config "$DIR/none.conf" 2>/dev/null &
DAV=$!
sleep 0.5
build/lite-sync-dev -system http://127.0.0.1:18081 -no-session-check -any-peer -port 18292 -conf-dir "$DIR/conf" -addon-dir "$DIR/addon" -www ui/dist \
    -rclone "$RCLONE" -token-file "$DIR/token" -socket "$DIR/target.sock" -run-dir "$DIR/run" \
    -oauth-extra '{"auth_url":"http://127.0.0.1:18081/oauth/authorize","token_url":"http://127.0.0.1:18081/oauth/token"}' &
DAEMON=$!
trap 'kill $DAEMON $FAKE $DAV 2>/dev/null' EXIT INT TERM
wait $DAEMON
