#!/bin/sh
# build-agent.sh — cross-compile the readeckobo on-device agent and assemble
# the install payloads.
#
# Outputs (into $OUT, default <repo>/dist/agent):
#   readeckobo-agent            the raw armv7 static binary
#   KoboRoot.tgz                firmware-style install: copy to .kobo/ and reboot
#   readeckobo-agent-manual.tar.gz   same payload + config/NickelMenu samples,
#                               for installing over ftp/telnet without a reboot
#
# KoboRoot.tgz layout follows docs/research/kobo-db-schema.md §7.3:
#   usr/local/readeckobo-agent/{readeckobo-agent,start.sh,udev_program.sh}
#   etc/udev/rules.d/98-readeckobo.rules
#   mnt/onboard/.adds/readeckobo/config.sample
#
# It deliberately does NOT write any NickelMenu config into KoboRoot.tgz —
# that would risk overwriting the user's existing config. The optional
# NickelMenu entry (mnt/onboard/.adds/nm/readeckobo) ships only in the manual
# tarball; the script prints instructions for installing it.
#
# Requirements: Go toolchain on PATH (or GOROOT set), tar. `file` is used for
# verification when available.

set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT=${OUT:-"$REPO_ROOT/dist/agent"}
STAGE="$OUT/stage"
PAYLOAD="$REPO_ROOT/scripts/agent-payload"
AGENT_PKG=./cmd/readeckobo-agent

GO=${GO:-go}
GOOS=linux GOARCH=arm GOARM=7

echo "==> readeckobo-agent installer build"
echo "    repo:   $REPO_ROOT"
echo "    output: $OUT"

rm -rf "$STAGE"
mkdir -p "$STAGE" "$OUT"

echo "==> cross-compiling (CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH GOARM=$GOARM)"
# -ldflags "-s -w" keeps the static binary small (no debug info needed on-device).
CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH GOARM=$GOARM \
    "$GO" build -mod=vendor -trimpath -ldflags="-s -w" \
    -o "$STAGE/usr/local/readeckobo-agent/readeckobo-agent" "$AGENT_PKG"

# Stage the payload tree (payload files are the source of truth; the compiled
# binary above overlays the placeholder path).
cp -a "$PAYLOAD/." "$STAGE/"
chmod +x "$STAGE/usr/local/readeckobo-agent/readeckobo-agent" \
         "$STAGE/usr/local/readeckobo-agent/start.sh" \
         "$STAGE/usr/local/readeckobo-agent/udev_program.sh"

# ---------------------------------------------------------------- KoboRoot.tgz
# Contents: agent binaries + udev rule + config.sample. Deliberately NOT the
# NickelMenu entry (would risk clobbering an existing .adds/nm config), so it
# is excluded from this archive.
KOBOROOT="$OUT/KoboRoot.tgz"
rm -f "$KOBOROOT"
tar -czf "$KOBOROOT" --owner=0 --group=0 \
    --exclude='mnt/onboard/.adds/nm' \
    -C "$STAGE" usr etc mnt
if tar -tzf "$KOBOROOT" | grep -q '\.adds/nm'; then
    echo "error: KoboRoot.tgz must not contain NickelMenu config files" >&2
    exit 1
fi
echo "==> wrote $KOBOROOT"

# ------------------------------------------------------------- manual tarball
MANUAL="$OUT/readeckobo-agent-manual.tar.gz"
rm -f "$MANUAL"
tar -czf "$MANUAL" --owner=0 --group=0 \
    -C "$STAGE" usr etc mnt
echo "==> wrote $MANUAL (with config.sample + NickelMenu sample; see INSTALL.txt)"

cp "$STAGE/mnt/onboard/.adds/readeckobo/config.sample" "$OUT/config.sample"
cp "$STAGE/mnt/onboard/.adds/nm/readeckobo" "$OUT/nm-readeckobo-sample"
cp "$STAGE/INSTALL.txt" "$OUT/INSTALL.txt"

echo
echo "==> verification"
tar -tzf "$KOBOROOT" | sort | sed 's/^/    KoboRoot: /'
if command -v file >/dev/null 2>&1; then
    file "$STAGE/usr/local/readeckobo-agent/readeckobo-agent"
fi

echo
echo "==> install (KoboRoot.tgz):"
echo "    1. copy $KOBOROOT to the device as .kobo/KoboRoot.tgz"
echo "    2. eject/unplug — the device reboots and extracts the archive"
echo "    3. on first boot, copy .adds/readeckobo/config.sample -> config"
echo "       and set SERVER_URL + TOKEN, then run a sync (see docs/AGENT.md)"
echo
echo "==> optional NickelMenu entry (NOT included in KoboRoot.tgz):"
echo "    copy $OUT/nm-readeckobo-sample to /mnt/onboard/.adds/nm/readeckobo"
echo "    on the device (NickelMenu reads every file in .adds/nm) and reboot."
echo "    This never overwrites an existing NickelMenu config."
