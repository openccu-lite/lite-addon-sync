#!/bin/sh
#
#   Builds the addon packages: dist/sync-<arch>-<version>.tar.gz and .sha256 for aarch64 and
#   x86_64 (the asset names openccu-lite's catalogue resolves by uname -m).
#
#     VERSION=0.1.0 scripts/build.sh            # both
#     ARCHES=x86_64 scripts/build.sh            # one
#     SKIP_UI=1 scripts/build.sh                # keep addon/sync/app as it is
#     REQUIRES_LITE=2.0.0-beta.6 scripts/build.sh   # a lab build (see below)
#
#   The page is built with npm in ui/ and copied to addon/sync/app (the daemon serves it at
#   /addons/sync/); the manifest's icons, the 503 page and the proxy rule live in addon/sync/www
#   and addon/sync/etc, tracked, where the catalogue reads the icons from the repository at the
#   release tag; the daemon is a static Go binary (CGO_ENABLED=0) per architecture; rclone is
#   upstream's release binary for the architecture, fetched and checked against the pinned
#   SHA-256 (scripts/fetch-rclone.sh, scripts/rclone.pin) - it is never built here and never part
#   of the system image.
#
set -e
cd "$(dirname "$0")/.."
VERSION=${VERSION:-0.0.0-dev}
ARCHES=${ARCHES:-aarch64 x86_64}
GO=${GO:-go}
. scripts/rclone.pin

if [ -z "$SKIP_UI" ]; then
    (cd ui && npm ci --no-audit --no-fund && npm run build)
    rm -rf addon/sync/app
    mkdir -p addon/sync/app
    cp -a ui/dist/. addon/sync/app/
fi
cp -a addon/static/. addon/sync/www/
printf 'VERSION_ADDON=%s\nVERSION_RCLONE=%s\n' "$VERSION" "$RCLONE_VERSION" > addon/sync/versions
rm -rf addon/sync/licences
mkdir -p addon/sync/licences
cp LICENSE addon/sync/licences/lite-addon-sync-LICENSE
cp licences/rclone-COPYING addon/sync/licences/

mkdir -p dist
for arch in $ARCHES; do
    case $arch in
        aarch64) goarch=arm64 ;;
        x86_64) goarch=amd64 ;;
        *) echo "unknown architecture $arch" >&2; exit 1 ;;
    esac
    rclone=$(scripts/fetch-rclone.sh "$arch")
    rm -rf addon/sync/bin
    mkdir -p addon/sync/bin
    CGO_ENABLED=0 GOOS=linux GOARCH=$goarch $GO build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o addon/sync/bin/lite-sync ./cmd/lite-sync
    cp "$rclone" addon/sync/bin/rclone

    out=dist/sync-$arch-$VERSION.tar.gz
    stage=$(mktemp -d)
    cp -a addon/update_script addon/openccu-lite.json addon/sync "$stage/"
    rm -rf "$stage/sync/var"
    # a lab build for a system whose image is older than requires.lite but whose system service
    # was replaced by hand: REQUIRES_LITE=2.0.0-beta.6 rewrites the package's copy of the manifest
    if [ -n "$REQUIRES_LITE" ]; then
        sed -i "s/\"lite\": \"[^\"]*\"/\"lite\": \"$REQUIRES_LITE\"/" "$stage/openccu-lite.json"
        grep -q "\"lite\": \"$REQUIRES_LITE\"" "$stage/openccu-lite.json"
    fi
    find "$stage" \( -name '._*' -o -name '.DS_Store' \) -exec rm -f {} +
    chmod 755 "$stage/update_script" "$stage/sync/rc.d" "$stage/sync/bin/lite-sync" "$stage/sync/bin/rclone"
    test -f "$stage/sync/etc/lighttpd.conf"
    COPYFILE_DISABLE=1 tar --owner=root --group=root --sort=name --mtime='2026-01-01 00:00Z' -czf "$out" -C "$stage" .
    rm -rf "$stage"
    (cd dist && sha256sum "$(basename "$out")" > "$(basename "$out").sha256")
    echo "built $out"
done
