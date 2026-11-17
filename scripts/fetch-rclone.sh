#!/bin/sh
#
#   Fetches the pinned rclone for one architecture and prints the path of the binary:
#
#     scripts/fetch-rclone.sh x86_64      # or aarch64
#
#   The zip comes from downloads.rclone.org, is checked against the SHA-256 in scripts/rclone.pin
#   and unpacked into the cache (RCLONE_CACHE, default ~/.cache/lite-addon-sync/rclone). A zip that
#   is already there is checked again and not downloaded twice. Nothing is fetched without a
#   matching pin: a new upstream release changes the pin first.
#
set -e
cd "$(dirname "$0")/.."
. scripts/rclone.pin
arch=$1
case $arch in
    x86_64) rarch=amd64; want=$RCLONE_SHA256_x86_64 ;;
    aarch64) rarch=arm64; want=$RCLONE_SHA256_aarch64 ;;
    *) echo "fetch-rclone: unknown architecture '$arch' (x86_64, aarch64)" >&2; exit 1 ;;
esac
[ -n "$want" ] || { echo "fetch-rclone: no pin for $arch" >&2; exit 1; }
cache=${RCLONE_CACHE:-$HOME/.cache/lite-addon-sync/rclone}
zip=rclone-$RCLONE_VERSION-linux-$rarch.zip
mkdir -p "$cache"
check() { [ -f "$cache/$zip" ] && [ "$(sha256sum "$cache/$zip" | cut -d' ' -f1)" = "$want" ]; }
if ! check; then
    rm -f "$cache/$zip"
    curl -fsSL --retry 3 -o "$cache/$zip.part" "https://downloads.rclone.org/$RCLONE_VERSION/$zip" >&2
    mv "$cache/$zip.part" "$cache/$zip"
    check || { echo "fetch-rclone: $zip does not match the pinned SHA-256 ($want)" >&2; rm -f "$cache/$zip"; exit 1; }
fi
out=$cache/$RCLONE_VERSION-$arch
if [ ! -x "$out/rclone" ]; then
    rm -rf "$out" "$out.tmp"
    mkdir -p "$out.tmp"
    unzip -q -j -o "$cache/$zip" "rclone-$RCLONE_VERSION-linux-$rarch/rclone" -d "$out.tmp"
    chmod 755 "$out.tmp/rclone"
    mv "$out.tmp" "$out"
fi
echo "$out/rclone"
