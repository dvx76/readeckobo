# Device diagnostics — Readeck-Kobo highlight-sync runbook

> Purpose: collect a **read-only** snapshot of a Kobo attached over USB (device
> DB copy + queries, agent sidecars, optional server probes) into
> `diag-out/`, so a maintainer who does not have the device in hand can reason
> about what the agent is seeing. Nothing on the device is modified.
>
> Run `./scripts/device-diagnostics.sh` and report back the contents of
> **`diag-out/report.txt`** (and, when asked, the rest of the `diag-out/`
> tree).

## Before you start — redeploy both sides first

The fixes in this round only take effect after **both** sides are redeployed.
Do this first, in order:

1. **Rebuild + restart the readeckobo server.** `go build ./...` and restart
   the server process. The kepub cache version bump (`kepubGenVersion` →
   `"v2"`) changes the cache key, so old cached kepubs regenerate on demand on
   the next request — a restart alone suffices, **no config change needed**.
2. **Rebuild + reinstall the device agent.** Rebuild with `make agent`, copy
   the new `dist/agent/KoboRoot.tgz` to the device as `.kobo/KoboRoot.tgz`,
   and reboot the Kobo (it installs on boot).

> The agent currently on the device **predates** the Shelf-UUID and
> diagnostics-logging fixes, so reinstallation is **required** for those fixes
> to take effect — refreshing the config is not enough.

## Recent fixes in this round (2026-09-08)

1. **`rd-annotation` highlights are now rendered in kepubs** — Readeck→Kobo
   direction. Readeck's `rd-*` annotation wrappers are rewritten into styled
   `<span class="rd-annotation">` elements (previously they were dropped, so a
   highlight already made in Readeck was invisible on the device).
2. **kepub cache version bump** (`kepubGenVersion` now `"v2"`) — the server's
   cache key changed, so every cached kepub from before the annotation-render
   fix is regenerated on the next request instead of being served stale.
3. **`Shelf.Id` is now a real UUID** — the agent's modern-shape (DbVersion ≥
   64) shelf insert generates an RFC 4122 v4 UUID (calibre's driver also
   writes a UUID there), instead of reusing the shelf name as the Id.
4. **Fixed a `wrapRuns` traversal bug** in the kepub generator that could
   silently drop text after adjacent inline elements — text is now preserved
   byte-for-byte.
5. **Added diagnostics logging to the agent** — the collection step logs
   `collection "<name>" (DbVersion <N>) has <N> book row(s)` and the highlight
   step logs `highlight scan: <N> candidate row(s) in device DB` plus a
   `no new highlights to upload (<scanned>, <skipped> this pass)` message.

## 1. Prepare the device BEFORE collecting

The report is only useful if it captures a device that was *trying* to sync.
Do this first, in order:

1. **Make a test highlight.** Open one of your imported Readeck articles from
   the library (it should be in the **Readeck** collection). Long-press a
   word, drag the selection handles over a sentence, tap **Highlight**, and
   optionally **Add note**. Then exit the book (back to the library).
2. **Run a manual agent pass** if a NickelMenu **"Sync Readeck"** entry is
   installed (`.adds/nm/readeckobo` → menu item). Otherwise the agent runs on
   its schedule (udev Wi-Fi trigger / periodic). Wait ~30 s after the pass
   starts so the agent's writes and log lines settle.

The point of step 1 is that a *fresh* highlight is the strongest signal: it
must show up in the Bookmark rows **and** either in the ledger or as an
upload attempt.

## 2. Run the script

```sh
# 1. plug the Kobo in over USB, accept "Connect" on the device,
#    wait for it to mount (e.g. /media/$USER/KOBOeReader)
cd <repo>                       # a copy of this repo
./scripts/device-diagnostics.sh --server
```

- With no mount argument the script auto-detects by scanning
  `/media/$USER`, `/run/media/$USER`, `/mnt` and `/Volumes` for a directory
  containing `.kobo/KoboReader.sqlite`. Pass the mount point explicitly if
  detection misses it:
  `./scripts/device-diagnostics.sh /media/$USER/KOBOeReader --server`.
- `--server` additionally probes the readeckobo server (state feed + kepub
  URL HEADs) using `SERVER_URL`/`TOKEN` from the agent config; it is tolerant
  of failures (10 s curl timeout, exit codes printed).
- All output lands in **`diag-out/`** (fresh each run):
  - `diag-out/device/KoboReader.sqlite` — WAL-safe copy (main first,
    `-wal` last) of the on-device DB; **all queries run against this copy**,
  - `diag-out/sql/*.txt` — raw query outputs (see §3),
  - `diag-out/agent/` — `config.redacted` (TOKEN stripped), `agent.log.tail200`,
    `index.json`, `ledger.json`,
  - `diag-out/report.txt` — the summary + quick triage. **This is the file to
    report back.**

> No device handy? Offline smoke test with the synthetic fixture:
>
> ```sh
> cd tools/kobofixdb && go run .          # writes /tmp/kobofixture/KoboReader.sqlite
> mkdir -p /tmp/fakemount/.kobo && cp /tmp/kobofixture/KoboReader.sqlite /tmp/fakemount/.kobo/
> ./scripts/device-diagnostics.sh /tmp/fakemount
> ```

## 3. What a HEALTHY output looks like

Section by section, expecting roughly:

- **[2] schema & version** — `DbVersion` rows shows a 4.x value (e.g. `126`);
  `PRAGMA user_version` can be `0` (Nickel uses the `DbVersion` table, not
  `user_version`).
- **[3] content rows under `.kobo/readeck/`** — one **book row** per imported
  article (`VolumeIndex = -1`, `MimeType` `application/x-kepub+zip` — on
  sideloads the research records `application/x-kobo-epub+zip` — plus one
  `application/xhtml+xml` chapter row per chapter, ids joined with `!!`).
- **[5] highlight/note rows** — `Type=highlight` / `Type=note` rows whose
  `StartContainerPath` ends in `#kobo.<block>.<run>` (e.g.
  `span#kobo\.5\.2`), `Hidden=false`, with `DateCreated`/`DateModified` set.
  The Type histogram shows e.g. `highlight|2` `note|1`.
- **[6] Shelf/ShelfContent** — one `Shelf` row `Name=Readeck`,
  `Type=UserTag`, `_IsDeleted=false` (modern shape has `Id` = a UUID), and
  `ShelfContent` rows pointing at the **book-row** `ContentID`s.
- **[7] agent sidecars** — `config` present (TOKEN redacted in the copy),
  `agent.log` tail shows the new diagnostic lines:

  ```
  [INFO] collection "Readeck" (DbVersion 126) has 1 book row(s)
  [INFO] highlight scan: 3 candidate row(s) in device DB
  [INFO] uploading 3 highlight(s)/note(s) to device …
  [INFO] annotation <row> (highlight): created
  ```

- **[8] server probes** (with `--server`) — `HTTP 200` on
  `GET /api/agent/state?device=unknown&token=…` with `articles: <N>`, and
  `HTTP 200` on the kepub URL HEADs.

A healthy ledger shows `created`/`updated`/`unchanged` statuses (and
`skipped` only for rows that genuinely cannot be mapped — see triage row 3).

## 4. Triage table

| # | Symptom | Likely cause | Next step |
|---|---|---|---|
| 1 | **No "Readeck" collection in the UI** | (a) Shelf row exists but Nickel hasn't refreshed, or (b) the shelf was never created in the DB | Check `diag-out/sql/shelf.txt` for a `Name=Readeck` row. **Exists?** → reboot the device and re-check (Nickel reloads collections at boot; DB writes from a second connection do not refresh the running UI). **Missing?** → grep the agent log: `grep -E "collection|DbVersion|maintenance failed" .adds/readeckobo/agent.log`. `openReadWrite` / `busy`-lock errors → run the NickelMenu **Sync Readeck** manually while the device is awake on the home screen (don't sync while a book is open). No `DbVersion` value in `diag-out/sql/dbversion.txt` → the version query itself fails (that fix is version-specific). |
| 2 | **No Bookmark rows for our volumes at all** | Highlighting not possible on the kepub, or the rows are keyed to a different VolumeID/ContentID | Compare the exact `ContentID`/`VolumeID` strings in `diag-out/sql/content_readeck_rows.txt` vs `bookmark_breadth.txt` (both must contain `/.kobo/readeck/`). Check the imported book rows' `MimeType`. As a control, make a highlight in a **store-bought or Calibre-sideloaded** book and confirm rows appear with a `StartContainerPath` — that isolates a device/highlighting problem from a our-kepub-specific problem. |
| 3 | **Rows exist but the log says "no new highlights to upload"** | The ledger already recorded them | Check `diag-out/agent/ledger.json` `status` values. If they are `created`/`updated`/`unchanged` the rows were already sent — good. If they are `skipped`, each carries its reason in `last_error` (`unparseable offsets`, no usable `DateModified`/`DateCreated`, …) — report that reason. |
| 4 | **Rows uploaded but not visible in Readeck** | Server-side mapping/creation issue | Check the **readeckobo server logs** for `POST /api/agent/annotations` (per-row statuses `created`/`skipped`/`error` and mapper reasons). Verify against Readeck directly: `GET /api/bookmarks/{id}/annotations` with the **Readeck** token from the server config (`users[].readeck_access_token`). Confirm Readeck **≥ 0.22** (notes need it). |
| 5 | **Readeck→Kobo highlights not visible on device** | Stale cached kepub (generated before the annotation-render fix) | The server was updated to `kepubGenVersion v2` — after the server restart, old cached kepubs regenerate on demand. Force a re-download by **deleting the `.kepub.epub`** file on the device (the agent re-downloads when a file is missing) and re-syncing. Then inspect the kepub chapter for `span.rd-annotation` and the `.rd-annotation` CSS rule in the stylesheet; confirm the rendered result on the device. |

## 5. Manual checks (exact SQL / grep)

All SQL below runs **against the copy** (read-only, device untouched):

```sh
DB=diag-out/device/KoboReader.sqlite
sqlite3 -readonly "file:$DB?mode=ro" "SELECT * FROM DbVersion LIMIT 5;"
sqlite3 -readonly "file:$DB?mode=ro" \
  "SELECT ContentID, ContentType, MimeType, BookTitle, VolumeIndex, IsDownloaded \
   FROM content WHERE ContentID LIKE '%/.kobo/readeck/%' ORDER BY DateCreated;"
sqlite3 -readonly "file:$DB?mode=ro" "PRAGMA table_info(Bookmark);"
sqlite3 -readonly "file:$DB?mode=ro" \
  "SELECT BookmarkID, VolumeID, Type, Hidden, Text, Annotation, StartContainerPath, StartOffset, EndContainerPath, EndOffset, DateCreated, DateModified \
   FROM Bookmark WHERE VolumeID LIKE '%/.kobo/readeck/%';"
sqlite3 -readonly "file:$DB?mode=ro" "SELECT Type, count(*) FROM Bookmark WHERE VolumeID LIKE '%/.kobo/readeck/%' GROUP BY Type;"
sqlite3 -readonly "file:$DB?mode=ro" "SELECT * FROM Shelf;"
sqlite3 -readonly "file:$DB?mode=ro" "SELECT * FROM ShelfContent;"
```

Agent log greps (run on the device, or against `diag-out/agent/agent.log.tail200`):

```sh
grep -E "collection|shelf|DbVersion|highlight scan|uploading|rejected|skipping" .adds/readeckobo/agent.log
grep -E "maintenance failed|busy|unparseable|no DateModified" .adds/readeckobo/agent.log
```
