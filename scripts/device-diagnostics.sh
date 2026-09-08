#!/usr/bin/env bash
#
# device-diagnostics.sh — read-only diagnostics collector for the Readeck-Kobo
# highlight-sync agent session (Kobo attached over USB).
#
# It never writes to the device: the KoboReader.sqlite (+ -wal/-shm) is COPIED
# WAL-safely (main file first, -wal last) into ./diag-out/ and every query runs
# against the copy. Sidecars under .adds/readeckobo are copied with the config
# TOKEN redacted. With --server it also probes the readeckobo server found in
# the agent config (read-only endpoints only).
#
# Usage:
#   ./scripts/device-diagnostics.sh [MOUNT_POINT] [--server]
#
#   MOUNT_POINT  directory where the Kobo appears over USB (default: auto-detect
#                by scanning /media/$USER, /run/media/$USER, /mnt, /Volumes for
#                a directory containing .kobo/KoboReader.sqlite)
#   --server     additionally probe the readeckobo server from the agent config
#
# Output: everything lands in <repo>/diag-out/ — raw query outputs under
# diag-out/sql/, the DB copy under diag-out/device/, agent sidecars under
# diag-out/agent/, and a human-readable diag-out/report.txt summarizing the
# findings with a quick triage. See docs/device-diagnostics.md for the runbook
# (how to prepare the device, what healthy looks like, the full triage table).
#
# Dependencies: bash, sqlite3, coreutils. curl + jq are required only for
# --server. No root needed; the device must be mounted (accept "Connect" on the
# Kobo) and the DB must be closed on the device side.
set -u

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT_DIR="$REPO_ROOT/diag-out"
REPORT="$OUT_DIR/report.txt"

SERVER_PROBE=0
MOUNT_ARG=""

usage() {
    sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
}

for arg in "$@"; do
    case "$arg" in
        --server) SERVER_PROBE=1 ;;
        -h | --help) usage; exit 0 ;;
        *)
            if [ -n "$MOUNT_ARG" ]; then
                echo "unexpected extra argument: $arg" >&2
                usage >&2
                exit 2
            fi
            MOUNT_ARG="$arg"
            ;;
    esac
done

# ---------------------------------------------------------------------------
# plumbing: say/show tee everything into the report
# ---------------------------------------------------------------------------

say() { printf '%s\n' "$@" | tee -a "$REPORT"; }
hr() { printf '%s\n' '---------------------------------------------------------------------' | tee -a "$REPORT"; }

# show <file> prints an indented, capped copy of a raw output file inline.
show() {
    local f="$1" total n
    total=$(wc -l < "$f" 2>/dev/null || echo 0)
    n=$((total))
    if [ "$n" -gt 80 ]; then n=80; fi
    head -n "$n" "$f" | sed 's/^/    /' | tee -a "$REPORT"
    if [ "$total" -gt 80 ]; then
        say "    … ($total lines total; full raw output in $(basename "$f"))"
    fi
}

# ---------------------------------------------------------------------------
# 0. locate the device
# ---------------------------------------------------------------------------

die() {
    echo "ERROR: $*" >&2
    exit 1
}

detect_mount() {
    local roots r d cand
    roots=()
    if [ -n "${USER:-}" ]; then
        roots+=("/media/$USER" "/run/media/$USER")
    fi
    roots+=("/mnt" "/Volumes")
    for r in "${roots[@]}"; do
        [ -d "$r" ] || continue
        # The root itself plus one level of children (typical automounts:
        # /media/$USER/DEVICE, /run/media/$USER/DEVICE, /mnt/DEVICE, /Volumes/DEVICE).
        cand=("$r")
        for d in "$r"/*/; do
            [ -d "$d" ] && cand+=("$d")
        done
        for d in "${cand[@]}"; do
            if [ -f "$d/.kobo/KoboReader.sqlite" ]; then
                printf '%s\n' "$d"
                return 0
            fi
        done
    done
    return 1
}

if [ -n "$MOUNT_ARG" ]; then
    MOUNT="$MOUNT_ARG"
    if [ ! -f "$MOUNT/.kobo/KoboReader.sqlite" ]; then
        die "no Kobo database at $MOUNT/.kobo/KoboReader.sqlite — is $MOUNT the device mount point?"
    fi
    MOUNT_SOURCE="argument"
else
    MOUNT=$(detect_mount) || {
        echo "ERROR: no Kobo mount found." >&2
        echo "" >&2
        echo "I scanned /media/\$USER, /run/media/\$USER, /mnt and /Volumes (one level deep) for a" >&2
        echo "directory containing .kobo/KoboReader.sqlite. To fix:" >&2
        echo "  1. plug the Kobo in over USB and accept \"Connect\" on the device," >&2
        echo "  2. wait for it to appear as a drive (e.g. /media/\$USER/KOBOeReader)," >&2
        echo "  3. re-run, or pass the mount point explicitly:" >&2
        echo "       $0 /media/\$USER/KOBOeReader [--server]" >&2
        echo "" >&2
        echo "For offline testing against the synthetic fixture (no device):" >&2
        echo "  mkdir -p /tmp/fakemount/.kobo && cp /tmp/kobofixture/KoboReader.sqlite /tmp/fakemount/.kobo/" >&2
        echo "  $0 /tmp/fakemount" >&2
        exit 1
    }
    MOUNT_SOURCE="auto-detect"
fi

DEVICE_DB="$MOUNT/.kobo/KoboReader.sqlite"
[ -r "$DEVICE_DB" ] || die "cannot read $DEVICE_DB (permissions? still syncing?)"

# ---------------------------------------------------------------------------
# 1. fresh output directory + WAL-safe device DB copy
# ---------------------------------------------------------------------------

if [ -e "$OUT_DIR" ]; then
    echo "removing previous run's $OUT_DIR (fresh output per run)"
    rm -rf "$OUT_DIR"
fi
mkdir -p "$OUT_DIR/device" "$OUT_DIR/sql" "$OUT_DIR/agent" || die "cannot create $OUT_DIR"
: > "$REPORT" || die "cannot write $REPORT"

say "Readeck-Kobo device diagnostics — $(date -u '+%Y-%m-%d %H:%M:%SZ')"
say "host: $(uname -srm 2>/dev/null || echo unknown) · script: $0"
say "mount: $MOUNT ($MOUNT_SOURCE)"
hr

# WAL-safe copy: main file first, then -shm, then -wal LAST (a -wal copied
# before the main file would be stale and could describe a DB that does not
# exist yet in the copy).
COPY_DB="$OUT_DIR/device/KoboReader.sqlite"
say "[1] device DB copy (WAL-safe; queries run against the COPY, never the device)"
say "    source: $DEVICE_DB"
if ! cp -p "$DEVICE_DB" "$COPY_DB"; then
    die "copying $DEVICE_DB failed — is the device still connected? (retry after re-connecting)"
fi
for side in -shm -wal; do
    if [ -f "$DEVICE_DB$side" ]; then
        cp -p "$DEVICE_DB$side" "$COPY_DB$side"
        say "    $side present on device -> copied (order: main, -shm, -wal last)"
    fi
done
say "    copy: $COPY_DB ($(wc -c < "$COPY_DB") bytes)"
say ""

# ---------------------------------------------------------------------------
# 2. sqlite3 invocation (read-only, URI form with -readonly fallback)
# ---------------------------------------------------------------------------

command -v sqlite3 >/dev/null 2>&1 || die "sqlite3 CLI not found — install it (e.g. apt-get install sqlite3)"

SQLITE=(sqlite3 -readonly "file:$COPY_DB?mode=ro")
if ! "${SQLITE[@]}" "SELECT 1" >/dev/null 2>&1; then
    # Older builds without URI support: plain readonly open of the copy.
    SQLITE=(sqlite3 -readonly "$COPY_DB")
    "${SQLITE[@]}" "SELECT 1" >/dev/null 2>&1 || die "cannot open the DB copy read-only ($COPY_DB)"
fi

# run_sql <name> <sql> — save raw output to diag-out/sql/<name>.txt; print rc.
run_sql() {
    local name="$1" sql="$2"
    if "${SQLITE[@]}" "$sql" > "$OUT_DIR/sql/$name.txt" 2> "$OUT_DIR/sql/$name.txt.err"; then
        printf 'ok'
    else
        printf 'FAILED (see diag-out/sql/%s.txt.err)' "$name"
    fi
}

# ---------------------------------------------------------------------------
# 3. schema + version
# ---------------------------------------------------------------------------

say "[2] schema & DB version"
r=$(run_sql user_version "PRAGMA user_version;")
uv=$(head -n 1 "$OUT_DIR/sql/user_version.txt" 2>/dev/null || echo '?')
say "    PRAGMA user_version      = $uv  -> diag-out/sql/user_version.txt [$r]"
r=$(run_sql dbversion "SELECT * FROM DbVersion LIMIT 5;")
say "    DbVersion rows           -> diag-out/sql/dbversion.txt [$r]"
if [ -s "$OUT_DIR/sql/dbversion.txt" ]; then
    say "    DbVersion value(s):"
    show "$OUT_DIR/sql/dbversion.txt"
else
    say "    WARNING: DbVersion table empty or missing — the agent's shelf shape "
    say "             detection (>= 64 selects the Id/Type shelf columns) will fail."
fi
say ""

# ---------------------------------------------------------------------------
# 4. our content rows
# ---------------------------------------------------------------------------

say "[3] content rows under .kobo/readeck/ (our imports)"
CONTENT_SQL="SELECT ContentID, ContentType, MimeType, BookTitle, VolumeIndex, DateCreated, DateAdded, IsDownloaded FROM content WHERE ContentID LIKE '%/.kobo/readeck/%' ORDER BY DateCreated;"
r=$(run_sql content_readeck_rows "$CONTENT_SQL")
content_n=$(wc -l < "$OUT_DIR/sql/content_readeck_rows.txt" 2>/dev/null || echo 0)
say "    diag-out/sql/content_readeck_rows.txt [$r] — $content_n row(s):"
show "$OUT_DIR/sql/content_readeck_rows.txt"
if [ "$content_n" -gt 0 ]; then
    say "    book rows (VolumeIndex = -1):"
    grep -E '\|-1\|' "$OUT_DIR/sql/content_readeck_rows.txt" | sed 's/^/      /' | tee -a "$REPORT" || true
fi
say ""

# ---------------------------------------------------------------------------
# 5. Bookmark schema + our highlight/note rows
# ---------------------------------------------------------------------------

say "[4] Bookmark schema"
r=$(run_sql bookmark_schema "PRAGMA table_info(Bookmark);")
say "    diag-out/sql/bookmark_schema.txt [$r]"
say "    columns: $("${SQLITE[@]}" "SELECT group_concat(name, ', ') FROM pragma_table_info('Bookmark');" 2>/dev/null || echo 'n/a')"
say ""

say "[5] highlight/note rows whose VolumeID is one of our volumes"
r=$(run_sql bookmark_readeck_rows "SELECT * FROM Bookmark WHERE VolumeID LIKE '%/.kobo/readeck/%' ORDER BY DateCreated;")
bookmark_n=$(wc -l < "$OUT_DIR/sql/bookmark_readeck_rows.txt" 2>/dev/null || echo 0)
say "    diag-out/sql/bookmark_readeck_rows.txt [$r] — $bookmark_n row(s):"
show "$OUT_DIR/sql/bookmark_readeck_rows.txt"
say ""
r=$(run_sql bookmark_type_histogram "SELECT Type, count(*) FROM Bookmark WHERE VolumeID LIKE '%/.kobo/readeck/%' GROUP BY Type;")
say "    Type histogram -> diag-out/sql/bookmark_type_histogram.txt [$r]:"
show "$OUT_DIR/sql/bookmark_type_histogram.txt"
say ""
say "    full diagnostic breadth (ALL Bookmark columns matching our volumes,"
say "    regardless of Hidden/Text):"
BREADTH_SQL="SELECT BookmarkID, VolumeID, ContentID, Type, Hidden, Text, Annotation, StartContainerPath, StartOffset, EndContainerPath, EndOffset, DateCreated, DateModified FROM Bookmark WHERE VolumeID LIKE '%/.kobo/readeck/%';"
r=$(run_sql bookmark_breadth "$BREADTH_SQL")
say "    diag-out/sql/bookmark_breadth.txt [$r]:"
show "$OUT_DIR/sql/bookmark_breadth.txt"
say ""

# ---------------------------------------------------------------------------
# 6. Shelf / ShelfContent
# ---------------------------------------------------------------------------

say "[6] Shelf / ShelfContent state"
r=$(run_sql shelf "SELECT * FROM Shelf;")
shelf_n=$(wc -l < "$OUT_DIR/sql/shelf.txt" 2>/dev/null || echo 0)
say "    diag-out/sql/shelf.txt [$r] — $shelf_n Shelf row(s):"
show "$OUT_DIR/sql/shelf.txt"
say ""
r=$(run_sql shelfcontent "SELECT * FROM ShelfContent;")
sc_n=$(wc -l < "$OUT_DIR/sql/shelfcontent.txt" 2>/dev/null || echo 0)
say "    diag-out/sql/shelfcontent.txt [$r] — $sc_n ShelfContent row(s):"
show "$OUT_DIR/sql/shelfcontent.txt"
say ""

# ---------------------------------------------------------------------------
# 7. agent sidecars (.adds/readeckobo)
# ---------------------------------------------------------------------------

say "[7] agent sidecars (.adds/readeckobo on the mount)"
AGENT_DIR="$MOUNT/.adds/readeckobo"
CONFIG_PRESENT=0
LOG_PRESENT=0
if [ -d "$AGENT_DIR" ]; then
    if [ -f "$AGENT_DIR/config" ]; then
        CONFIG_PRESENT=1
        sed -E 's/^([[:space:]]*TOKEN[[:space:]]*=).*/\1<redacted>/' "$AGENT_DIR/config" \
            > "$OUT_DIR/agent/config.redacted"
        say "    config: present -> diag-out/agent/config.redacted (TOKEN redacted):"
        show "$OUT_DIR/agent/config.redacted"
    else
        say "    config: MISSING — the agent cannot run without .adds/readeckobo/config"
        say "            (copy it from the KoboRoot.tgz payload's config.sample)"
    fi
    if [ -f "$AGENT_DIR/agent.log" ]; then
        LOG_PRESENT=1
        tail -n 200 "$AGENT_DIR/agent.log" > "$OUT_DIR/agent/agent.log.tail200"
        say "    agent.log: present -> diag-out/agent/agent.log.tail200 (last 200 lines)"
    else
        say "    agent.log: missing (agent has never run? look at .adds/nm for the NickelMenu entry)"
    fi
    for f in index.json ledger.json; do
        if [ -f "$AGENT_DIR/$f" ]; then
            cp -p "$AGENT_DIR/$f" "$OUT_DIR/agent/$f"
            say "    $f: present -> diag-out/agent/$f"
        else
            say "    $f: missing (normal before the first successful pass)"
        fi
    done
else
    say "    .adds/readeckobo not found on the mount — the agent payload is not installed"
    say "    (KoboRoot.tgz from 'make agent', copied to .kobo/ + reboot) or the device"
    say "    shows a different mount than this one."
fi

if [ "$LOG_PRESENT" = 1 ]; then
    say ""
    say "    agent.log patterns (last pass lines):"
    grep -E "collection|shelf|DbVersion|highlight scan|uploading|rejected|skipping|maintenance failed" \
        "$OUT_DIR/agent/agent.log.tail200" 2>/dev/null | tail -n 10 | sed 's/^/      /' | tee -a "$REPORT" || true
fi

if [ -f "$OUT_DIR/agent/ledger.json" ]; then
    say ""
    if command -v jq >/dev/null 2>&1; then
        say "    ledger.json status histogram (status -> count):"
        jq -r '.entries[].status' "$OUT_DIR/agent/ledger.json" 2>/dev/null | sort | uniq -c \
            | sed 's/^/      /' | tee -a "$REPORT" || say "    (ledger.json not parseable as expected)"
        say "    skipped entries carry their reason in .last_error — grep it:"
        jq -r '.entries[] | select(.status == "skipped") | [.bookmark_row_id, (.last_error // "-")] | @tsv' \
            "$OUT_DIR/agent/ledger.json" 2>/dev/null | sed 's/^/      /' | tee -a "$REPORT" || true
    else
        say "    ledger.json copied (jq not installed — skip histogram; statuses + reasons are in the file)"
    fi
fi
say ""

# ---------------------------------------------------------------------------
# 8. server probes (only with --server)
# ---------------------------------------------------------------------------

if [ "$SERVER_PROBE" = 1 ]; then
    say "[8] readeckobo server probes (read-only; tolerant of failures)"
    if [ "$CONFIG_PRESENT" = 1 ]; then
        SERVER_URL=$(sed -nE 's/^[[:space:]]*SERVER_URL[[:space:]]*=[[:space:]]*//p' "$AGENT_DIR/config" | tail -n 1 | sed 's/[[:space:]]*$//')
        TOKEN=$(sed -nE 's/^[[:space:]]*TOKEN[[:space:]]*=[[:space:]]*//p' "$AGENT_DIR/config" | tail -n 1 | sed 's/[[:space:]]*$//')
        if [ -z "$SERVER_URL" ] || [ -z "$TOKEN" ]; then
            say "    cannot probe: config lacks SERVER_URL or TOKEN"
        else
            mkdir -p "$OUT_DIR/server" || die "cannot create $OUT_DIR/server"
            if ! command -v curl >/dev/null 2>&1; then
                say "    curl not found — skipping all server probes"
            else
                say "    server: $SERVER_URL (config redacted copy in diag-out/agent/config.redacted)"
                # state feed
                STATE_URL="$SERVER_URL/api/agent/state?device=unknown&token=$TOKEN"
                state_code=$(curl -sS -m 10 -o "$OUT_DIR/server/state.json" -w '%{http_code}' "$STATE_URL" 2> "$OUT_DIR/server/state.err")
                state_rc=$?
                printf '%s\n' "$state_code" > "$OUT_DIR/server/state.http"
                say "    GET /api/agent/state?device=unknown&token=… -> HTTP $state_code (curl exit $state_rc)"
                if command -v jq >/dev/null 2>&1 && [ "$(cat "$OUT_DIR/server/state.http")" = "200" ]; then
                    say "      articles: $(jq '.articles | length' "$OUT_DIR/server/state.json" 2>/dev/null || echo '?')"
                    jq -r '.articles[:20][] | "      " + .action + " " + .bookmark_id + " \"" + .title + "\""' \
                        "$OUT_DIR/server/state.json" 2>/dev/null | head -n 25 | tee -a "$REPORT"
                    if [ "$(jq '.articles | length' "$OUT_DIR/server/state.json" 2>/dev/null || echo 0)" -gt 20 ]; then
                        say "      … list truncated at 20 (full body in diag-out/server/state.json)"
                    fi
                    # HEAD the kepub url of up to 2 articles (kepub urls carry ?token=)
                    i=0
                    while IFS= read -r kepub_url; do
                        [ -z "$kepub_url" ] && continue
                        i=$((i + 1))
                        [ "$i" -gt 2 ] && break
                        code=$(curl -sS -I -m 10 -o /dev/null -w '%{http_code}' "$kepub_url" 2> "$OUT_DIR/server/kepub-head-$i.err")
                        rc=$?
                        say "      HEAD kepub url (article $i) -> HTTP $code (curl exit $rc)"
                    done < <(jq -r '.articles[] | select(.url != null) | .url' "$OUT_DIR/server/state.json" 2>/dev/null | head -n 2)
                else
                    say "      (body + curl diagnostics in diag-out/server/state.json / state.err)"
                fi
                say "    note: the per-article annotation listing is done on the Readeck side"
                say "          (GET /api/bookmarks/{id}/annotations with the Readeck token from the"
                say "          server config); it is not probed here because the agent config only"
                say "          carries the readeckobo device token."
            fi
        fi
    else
        say "    cannot probe the server: no agent config on the mount (see section [7])"
    fi
    hr
fi

# ---------------------------------------------------------------------------
# 9. quick triage (observation -> likely meaning)
# ---------------------------------------------------------------------------

say "[9] quick triage"
triage() {
    local c="$1"
    say "  - $c"
    say "      ok/good meaning:  $2"
    say "      if bad, check:    $3"
}
if [ "$content_n" -gt 0 ]; then
    triage "content rows under .kobo/readeck/: $content_n" \
        "imported kepubs have content rows (Nickel scanned and indexed them)" \
        "MimeType column: book rows should be a kepub mime type, chapters application/xhtml+xml"
else
    triage "content rows under .kobo/readeck/: 0" \
        "nothing imported yet (or the DB predates the first import)" \
        "agent.log 'downloading' vs 'up-to-date' lines; run a manual pass; if rows stay 0 the rescan/import path is broken (Nickel not scanning .kobo/readeck?)"
fi
if [ "$bookmark_n" -gt 0 ]; then
    triage "highlight/note rows for our volumes: $bookmark_n" \
        "device-side highlights exist and are candidates for upload" \
        "Type histogram: 'highlight' and 'note' expected; StartContainerPath should end in #kobo.<block>.<run>"
else
    triage "highlight/note rows for our volumes: 0" \
        "nothing highlighted on device yet, or highlights keyed to a different VolumeID" \
        "make a test highlight first (open a kepub from the library, long-press, drag handles, Highlight); then re-run; if still 0, compare VolumeID strings in the content rows vs Bookmark rows (both should contain /.kobo/readeck/)"
fi
if [ "$shelf_n" -gt 0 ]; then
    triage "Shelf rows: $shelf_n" \
        "collection shelf row exists (Name=Readeck, Type=UserTag, _IsDeleted=false is healthy)" \
        "_IsDeleted=true means the shelf was deleted; the agent revives it on the next pass"
else
    triage "Shelf rows: 0" \
        "no collection shelf in the DB copy" \
        "agent.log 'collection maintenance failed' / 'ensure shelf' errors; DbVersion >= 64 needed for the Id/Type insert shape"
fi
if [ "$CONFIG_PRESENT" = 1 ]; then
    triage "agent config: present" \
        "the agent is installed and configured (SERVER_URL/TOKEN set)" \
        "TOKEN value is only in the unredacted on-device file; keep diag-out/agent/config.redacted as the shareable copy"
else
    triage "agent config: missing" \
        "agent not installed or not configured yet" \
        "install the KoboRoot.tgz payload and create .adds/readeckobo/config from config.sample"
fi
say ""
say "Full runbook + triage table: docs/device-diagnostics.md"
say "Report complete. Share diag-out/report.txt (and, if asked, the diag-out/ tree)."
