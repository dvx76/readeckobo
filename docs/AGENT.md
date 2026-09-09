# readeckobo on-device agent

The agent (`cmd/readeckobo-agent`) runs **on the Kobo** (target: Libra 2,
firmware 4.38.x, Nickel 4.x). It downloads kepubs served by the readeckobo
server, imports them into the stock Nickel library into a **Readeck**
collection, and uploads highlights/notes the user makes on-device back to the
server — all over Wi-Fi, no USB.

It is a single static binary (`CGO_ENABLED=0`, `linux/arm` `GOARM=7`) that
needs nothing but the stock firmware, NickelDBus (`qndb`) for the library
rescan, and (optionally) NickelMenu for a manual sync entry.

```
Kobo ──Wi-Fi──► readeckobo server (stream D)
  │  GET /api/agent/state?device=…        → article list (add/update/remove; videos excluded)
  │  GET /api/kepub/{id}                  → the .kepub.epub
  │  POST /api/agent/annotations          → highlights/notes from the device
  │
  ├── Kepubs → /mnt/onboard/.kobo/readeck/<sanitized-title>-<bookmark_id>.kepub.epub
  ├── NickelDBus qndb rescan → Nickel imports them
  ├── KoboReader.sqlite ← highlights read out via a WAL-safe snapshot
  └── .adds/readeckobo/{config,index.json,ledger.json,agent.log,agent.lock}
```

Everything below assumes the research in
[`docs/research/kobo-db-schema.md`](research/kobo-db-schema.md) — read it
first if you need the "why".

---

## 1. Prerequisites on the device

| Requirement | Notes |
|---|---|
| Nickel **4.x** (not 5.x) | 4.38.23697 target. NickelMenu is 4.x-only. |
| **NickelDBus** installed | ships `qndb` at `/usr/bin/qndb`; required for the library rescan. If absent, the agent still downloads files and reports errors on rescan. |
| **NickelMenu** (optional) | only needed for the manual "Sync Readeck" menu entry. |

## 2. Build

```sh
./scripts/build-agent.sh          # OUT=dist/agent by default
# outputs:
#   dist/agent/KoboRoot.tgz                   → firmware-style install
#   dist/agent/readeckobo-agent-manual.tar.gz → same payload + samples
#   dist/agent/nm-readeckobo-sample           → optional NickelMenu entry
```

The build script cross-compiles with `CGO_ENABLED=0 GOOS=linux GOARCH=arm
GOARM=7` (`file` on the result shows `ELF 32-bit … ARM, EABI5 …
statically linked`), assembles the `KoboRoot.tgz` layout from
`scripts/agent-payload/`, and **never** touches an existing NickelMenu
config — the NickelMenu entry ships only as a sample to copy manually.

`KoboRoot.tgz` contains:

```
usr/local/readeckobo-agent/readeckobo-agent
usr/local/readeckobo-agent/start.sh
usr/local/readeckobo-agent/udev_program.sh
etc/udev/rules.d/98-readeckobo.rules
mnt/onboard/.adds/readeckobo/config.sample
```

## 3. Install

1. **Copy** `dist/agent/KoboRoot.tgz` to the device as **`.kobo/KoboRoot.tgz`**
   (hidden folder, root of onboard storage). It must keep the `.tgz`
   extension — some browsers rename it to `.tar`.
2. **Eject/unplug.** The device reboots and extracts the archive over the
   root filesystem (the standard mod install flow, same as NickelDBus /
   NickelMenu / KoboCloud).
3. **Create the config file** (first boot; the agent refuses to run without
   it):

   ```sh
   # on the device (or over USB: /mnt/onboard/.adds/readeckobo/config)
   cp /mnt/onboard/.adds/readeckobo/config.sample \
      /mnt/onboard/.adds/readeckobo/config
   # then edit: SERVER_URL + TOKEN are required
   ```

   Config is a simple `KEY=VALUE` file (`#` comments allowed). Recognized keys
   (uppercase; values are case-insensitive for keys):

   | Key | Default | Meaning |
   |---|---|---|
   | `SERVER_URL` | — (required) | base URL of the readeckobo server (`http(s)://host[:port]`) |
   | `TOKEN` | — (required) | bearer token (the server also accepts `?token=` but the agent sends the header) |
   | `DEVICE_SERIAL` | `unknown` | Nickel serial reported to the server. Reading a serial automatically is not implemented — there is no reliable non-root source on 4.38 (affiliate.conf does not carry it), so until we validate a sysfs/`/proc` source, either set it here or let the server see `"unknown"`. |
   | `COLLECTION` | `Readeck` | shelf name kepubs are filed into |
   | `ARCHIVE_ON_FINISHED` | `false` | parsed for forward compatibility, **unused in v1** (server-side archiving is out of scope) |
   | `QNDB_PATH` | `/usr/bin/qndb` | NickelDBus CLI path |
   | `QNDB_ARGS` | `-t 30000 -s pfmDoneProcessing -m pfmRescanBooksFull` | space-separated qndb arguments |
   | `HTTP_TIMEOUT` | `60s` | per-request timeout (Go duration) |
   | `CA_FILE` | — (optional) | path to a PEM CA bundle, for servers behind a **private CA**. When set, HTTPS requests trust exactly that bundle. Validated at startup (missing/unreadable/unparsable → the agent refuses to start). Leave unset to use the embedded Mozilla bundle (+ system roots when present) — see §5 "TLS certificate errors". |

4. **Optional: NickelMenu entry** for a manual "Sync Readeck":

   ```sh
   # copy the sample (NickelMenu reads EVERY file in .adds/nm as config)
   cp /mnt/onboard/.adds/nm/readeckobo ...   # see dist/agent/nm-readeckobo-sample
   # reboot (or run NickelMenu's reload) and use:  Home ▸ ... ▸ Sync Readeck
   ```

## 4. How it runs

Three triggers, all leading to `readeckobo-agent --once`:

1. **udev hook on Wi-Fi** — `etc/udev/rules.d/98-readeckobo.rules` (KoboCloud
   pattern) fires when `wlan0` (or `eth*`) appears. `udev_program.sh` detaches
   into the background (setsid) because udev kills slow scripts; `start.sh`
   then loops: wait for a reachable network (ping `1.1.1.1`, bounded), run one
   sync pass, sleep (`RKB_INTERVAL`, default 1800 s).
2. **NickelMenu entry** — `cmd_spawn` runs `readeckobo-agent --once`.
3. **Manual / troubleshooting** — via telnet/ftp:

   ```sh
   /usr/local/readeckobo-agent/readeckobo-agent --once \
       --config /mnt/onboard/.adds/readeckobo/config
   ```

Flags: `--config`, `--once`, `--interval` (loop mode, default `30m`),
`--kobo-db` (default `/mnt/onboard/.kobo/KoboReader.sqlite`), `--onboard`
(default `/mnt/onboard`), `--no-rescan` (dry run: downloads/removes files but
never invokes qndb). A single-instance flock (`agent.lock`) prevents the udev
hook, the menu entry and loop mode from running concurrently.

### One sync pass does (in order)

1. **State** — `GET /api/agent/state?device=<serial>`. The feed carries the
   user's non-archived bookmarks with `add`/`update`/`remove` actions; Readeck
   **video** bookmarks (`type="video"`, its built-in "Videos" filter) are
   excluded — they have no readable text for an e-reader, and a video synced
   before this exclusion shows up as `remove`.
2. **Files** — for `add`/`update`: download the kepub to a temp file and
   rename into `.kobo/readeck/<sanitized-title>-<bookmark_id>.kepub.epub`
   (skipped when the local index already has the same etag). For `remove`:
   delete the file and tombstone the index entry. **v1 leaves the DB rows of
   removed books alone** — Nickel drops stale content rows on the next
   rescan, or the user can delete the entry manually from the library.
   Finally the agent sweeps managed files whose bookmark is **absent from
   the feed entirely**: the server emits each `remove` only once, so a
   missed emission (offline device, shared device id) would otherwise leave
   the file and its shelf entry behind forever.
3. **Rescan + import poll** — when files changed (or an earlier import is
   still pending) run the NickelDBus rescan (`qndb -t 30000 -s
   pfmDoneProcessing -m pfmRescanBooksFull`), then poll the `content` table
   (read-only) until every pending `ContentID` appears (timeout 180 s, 2 s
   interval). `--no-rescan` skips both.
4. **Collection + date-added** — ensure `content.___SyncTime` +
   `content.DateCreated = article.created` (Readeck date added) for every
   managed book row, so the device's "Date added"/Recent sort follows
   Readeck's order, and ensure the collection shelf exists (`DbVersion ≥ 64`
   shape with `Id`/`Type`) and owns the imported book rows with
   `ShelfContent.DateModified = article.created` (calibre
   `Shelf`/`ShelfContent` pattern). The ensure is idempotent (`created` is
   immutable, unlike `updated`) so devices synced before the date-added fix
   self-heal on the next pass. The agent **never inserts content rows** —
   Nickel creates them; we only update the date columns.
5. **Highlights** — snapshot `KoboReader.sqlite` (+ `-wal`/`-shm`) to
   `/tmp/readeckobo-agent.sqlite` (WAL-safe read), extract non-hidden
   `highlight`/`note` rows whose `VolumeID` is one of our files, diff against
   `ledger.json` (`bookmark_row_id → device DateModified sent`), and POST
   only new/changed rows to `/api/agent/annotations`. Definitive outcomes
   (`created`/`updated`/`unchanged`/`skipped`) are recorded in the ledger;
   server `error` outcomes are left for the next pass. Skipped-with-reason
   rows are recorded (with the reason) so a broken row is not retried
   forever.
6. **Log rotation** — `agent.log` is truncated when it exceeds 1 MB.

### State files (all under `.adds/readeckobo/`)

| File | Purpose |
|---|---|
| `config` | config (see above) |
| `index.json` | per-kepub state: etag, bookmark id, `downloaded` → `imported` → `removed` |
| `ledger.json` | per device Bookmark row: last server status + `date_modified` sent |
| `agent.log` | rotating log (truncated > 1 MB) |
| `agent.lock` | single-instance flock |

If you delete `index.json`, the agent re-downloads every article (and
re-ensures the date-added columns on the next pass). If you delete
`ledger.json`,
every device highlight is re-sent on the next pass — harmless, the server
dedups by `bookmark_row_id`.

## 5. Troubleshooting

**Run one pass manually and read the log**

```sh
/usr/local/readeckobo-agent/readeckobo-agent --once \
    --config /mnt/onboard/.adds/readeckobo/config
tail -n 100 /mnt/onboard/.adds/readeckobo/agent.log
```

Common symptoms:

| Symptom | Likely cause / fix |
|---|---|
| `cannot read config …` | create the config file from `config.sample` |
| `SERVER_URL is required` / `TOKEN is required` | fill both in config |
| `qndb not found at /usr/bin/qndb` | install NickelDBus, or set `QNDB_PATH` |
| files download but never appear in the library | the rescan/import is failing on-device: check the `qndb` output in the log, run `--once` without `--no-rescan`, and confirm the kepub renders/highlights (see §6.3); the content-row assumption (`.kobo/readeck` is scanned by `pfmRescanBooksFull`) is **unverified** (§6.1) |
| highlights sync but annotations are misplaced/offset | the span/offset mapping is server-side; verify with the kepub span map (see `docs/research/kepub-generation.md`) and §6.2 |
| nothing uploads for a book the user just removed | expected: removed books are excluded from the highlight scan |
| `next_cursor` error | the server paged the state response; v1 does not paginate — fix on the server or wait for a v2 |

**TLS certificate errors (`tls: failed to verify certificate: x509:
certificate signed by unknown authority`).** The Kobo has no system CA store
(firmware 4.38 has no `/etc/ssl/certs`), so any HTTPS verification fails out
of the box. The agent solves this by embedding the Mozilla CA bundle
(`cmd/readeckobo-agent/cabundle.pem`, wired up in `certs.go`) and registering
it via `x509.SetFallbackRoots()`: Go then verifies against that bundle
whenever **no system roots are available** — exactly the device case — and
ignores it when a real system pool exists (desktop, CI). A valid Let's
Encrypt / other public-CA certificate therefore just works; there is nothing
to configure. If your server still fails with this error, its certificate is
signed by a CA that is not in the Mozilla bundle (expired chain, private CA,
or a self-signed test cert). For a **private CA**, set `CA_FILE=/path/to/ca.pem`
in the config to the CA's PEM bundle — HTTPS then trusts exactly that bundle.
`CA_FILE` is validated at startup: a missing, unreadable or unparsable file
is a hard error, never silently ignored. (**Never** work around this with
`InsecureSkipVerify`-style settings — there is no such option in this agent.)

**Refreshing the embedded bundle.** `make refresh-cabundle` re-downloads
<https://curl.se/ca/cacert.pem> (requires `curl`) into
`cmd/readeckobo-agent/cabundle.pem` — commit the updated file afterwards.
The bundle's source and retrieval date are recorded at the top of
`cmd/readeckobo-agent/certs.go`. It is **not** re-fetched as part of `make
agent`: builds stay hermetic with the committed bundle.

**WAL-safety.** The device DB is WAL mode while Nickel runs. The agent copies
`KoboReader.sqlite` + `-wal`/`-shm` to a snapshot and opens the copy read-only
(3 retries on torn copies), and writes (date-added columns, shelf) with a
`busy_timeout` — but writing while Nickel has the DB open is the one area
that still needs device validation (§6.7).

## 6. Needs real-device validation

Everything in this section is **open** — it is the checklist from
[`docs/research/kobo-db-schema.md`](research/kobo-db-schema.md) §11 plus the
items specific to this agent. A real Libra 2 / 4.38.23697 must answer:

1. **Does `pfmRescanBooksFull` import kepubs from `.kobo/readeck/`?** The
   agent stores kepubs under the hidden `.kobo` directory (per this project's
   contract). Calibre and KoboCloud import from visible folders; the
   `.kobo/readeck` path is unverified. If Nickel does not scan it, move the
   store to a visible directory (e.g. `.adds/readeckobo/Library`).
2. **`Bookmark.ContentID` chapter separator** for sideloaded kepubs: `!!`
   (this implementation assumes `!!`, per october) vs `!ops!` (boehs.org).
   Also whether highlights work at all on sideloaded `.kepub.epub` on 4.38.
3. **Highlight offsets on-device**: code points vs UTF-16 units; whether
   offsets are relative to the start/end span. The agent passes device values
   through verbatim — the server maps them, but the whole pipeline needs one
   manual end-to-end highlight test.
4. **Date-added sort key is `___SyncTime` (not `DateCreated`)** on modern
   firmware (Libra 2, 4.38.x): "Date added" sorts by `content.___SyncTime`,
   "Recent" by `MAX(___SyncTime, DateLastRead)` (davidfor, MobileRead
   t=347000; Kobo Utilities' "Update metadata → Date added" writes
   `___SyncTime`, while `DateCreated` is the publishing date). The agent sets
   `___SyncTime` + `DateCreated` + `ShelfContent.DateModified` to
   `article.created` every pass (idempotent). Confirm on-device that the
   Readeck collection sorted by "Date added" now follows Readeck's order, and
   that a re-import (etag change, Nickel re-uses the row) keeps the
   converged timestamp.
5. **`DbVersion` value on 4.38.23697** and the resulting `Shelf`
   columns (≥ 64 → `Id`/`Type`). The agent reads `DbVersion` and picks the
   shape; verify the shelf appears in the UI (and whether `Activity` rows are
   also required).
6. **Writing the DB while Nickel runs**: date-added columns +
   `Shelf`/`ShelfContent`
   writes from a second connection with `busy_timeout` must be safe on-device
   (Calibre only writes when USB-connected/Nickel idle). Fallback if not:
   perform writes immediately after the rescan's `pfmDoneProcessing`.
7. **The udev rule + `setsid` detach**: whether Wi-Fi association reliably
   fires `ACTION=="add"` for `wlan*` on 4.38 and whether the backgrounded
   `start.sh` survives (KoboCloud precedent, but unverified here). Also
   whether `ping` exists in busybox on 4.38 (start.sh uses it for the
   network wait).
8. **Busybox applet set / filesystem**: onboard is assumed to accept long
   UTF-8 file names with spaces; if it is vfat, the sanitized names (ASCII +
   spaces + dashes, ≤ 90 runes) are chosen to be safe, but this needs a
   check. Confirm `RKB_PING_HOST` is reachable from the device's network.
9. **Serial source**: none implemented — `DEVICE_SERIAL` is config-only and
   defaults to `"unknown"`. Find a reliable serial source (sysfs block
   `serial` for `/dev/mmcblk0`, or a Nickel query) before enabling per-device
   state on the server.
10. **`n3fssSyncOnboard` vs `pfmRescanBooksFull`**: the research prefers the
    targeted `n3fssSyncOnboard` (faster import) but the agent defaults to the
    broadly-documented `pfmRescanBooksFull` + `pfmDoneProcessing`; the right
    signal for 4.38 still needs a device test (`QNDB_ARGS` makes this a
    config change, not a rebuild).

## 7. Wire contract (shared, do not change)

Auth: `Authorization: Bearer <token>` on every request.

```
GET  {server}/api/agent/state?device=<serial>
     → {"articles":[{bookmark_id,title,author,url,etag,action:"add|update|remove",updated,created}],
        "next_cursor":null}
     url = absolute kepub download URL.
     updated = last-modified (etag/change detection); created = date added to
     Readeck (Kobo ___SyncTime/DateCreated/shelf sort).

GET  {server}/api/kepub/{id}   → the .kepub.epub bytes.

POST {server}/api/agent/annotations
     {"device":"...","items":[{bookmark_id,bookmark_row_id,start_path,start_offset,
                                end_path,end_offset,text,annotation,type,
                                date_created,date_modified}]}
     → {"results":[{bookmark_row_id,status:"created|updated|unchanged|skipped",
                    annotation_id,error}]}
```

v1 constraints: the agent refuses `next_cursor != null` (no pagination yet);
offsets are sent as JSON integers (device rows store them as text — the agent
converts); an `error`/unknown per-row status is retried on the next pass.
