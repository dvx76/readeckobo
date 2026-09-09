# Kobo → Readeck highlight sync (beta)

> End-user guide. For the agent internals, install/troubleshooting and the
> device-validation checklist in full, see [docs/AGENT.md](AGENT.md). This page
> is the "what does it do and how do I use it" version.

## What it does

Your non-archived Readeck articles are generated as **kepubs** (the Kobo epub
flavour) and imported into your Kobo library into a shelf/collection named
**Readeck**, sorted by **date added** (the server's article timestamps drive
the device's "Date added" / Recent sort). Readeck **videos** (its built-in
"Videos" filter) are skipped — there is no readable text to put on an
e-reader.

Once the articles are on the device, **highlights and notes you make while
reading sync wirelessly back into Readeck** — over Wi-Fi, no USB cable, no
manual export. The agent on the device notices the new highlights in
`KoboReader.sqlite` and uploads them to the server, which maps them onto the
article and creates/patches the corresponding Readeck annotations.

The flow:

```
Kobo (on-device agent) ──Wi-Fi──► readeckobo server ──► Readeck
  GET /api/agent/state          article list (add/update/remove)
  GET /api/kepub/{id}           download the .kepub.epub
  POST /api/agent/annotations   upload highlights/notes you made
```

The classic Instapaper-proxy flow ("My Articles" on the device) is unchanged
and keeps working alongside this — it just does not support highlighting.

## Prerequisites

| Thing | Requirement |
|---|---|
| Kobo | A Nickel **4.x** device (firmware 4.38.x targeted; **not** 5.x). Tested target: Libra 2 on 4.38.23697. |
| Readeck | **≥ 0.22** (the annotations API; verified live against 0.23.2). |
| readeckobo | This feature ships in the current build; server needs `server.data_dir` writable (defaults to `data`). |
| On the device | **NickelDBus** installed (ships `qndb`, needed for the library rescan). **NickelMenu** only if you want a manual "Sync Readeck" menu entry. |

## Server configuration

**Nothing is required.** The three agent endpoints are served by the same
binary:

| Endpoint | Purpose |
|---|---|
| `GET /api/agent/state?device=<serial>` | article list the agent syncs from |
| `GET /api/kepub/{id}` | the generated `.kepub.epub` for one article |
| `POST /api/agent/annotations` | highlights/notes upload |

Optional: `server.data_dir` (default `"data"`) points at the directory holding
the server-side state store — the **kepub cache** (generated kepubs + span
maps, so articles aren't re-generated every sync) and the **annotation ledger**
(so re-uploaded highlights are deduped). See [docs/CONFIG.md](CONFIG.md).

Auth: the agent authenticates with `Authorization: Bearer <token>` where
`<token>` is the same token you already put in `users[].token` in
`config.yaml`. (The server also accepts `?token=` for download URLs.)

## Device install (one-time)

1. **Build or download** the installer: `make agent` produces
   `dist/agent/KoboRoot.tgz` (see [docs/AGENT.md](AGENT.md) §2).
2. **Copy** `dist/agent/KoboRoot.tgz` to the device as
   **`.kobo/KoboRoot.tgz`** (hidden folder, root of onboard storage). Keep the
   `.tgz` extension.
3. **Eject / unplug.** The device reboots and installs the agent (the standard
   mod flow, same as NickelDBus/NickelMenu/KoboCloud).
4. **Create the config file** on first boot:

   ```sh
   # on the device, or over USB:
   cp /mnt/onboard/.adds/readeckobo/config.sample \
      /mnt/onboard/.adds/readeckobo/config
   ```

   Then edit it. Two keys are **required**:

   ```
   SERVER_URL=https://your-readeckobo.example   # base URL of the server
   TOKEN=your-device-token                       # users[].token from config.yaml
   ```

   Optional keys: `DEVICE_SERIAL` (reported to the server; defaults to
   `"unknown"`), `COLLECTION` (shelf name, default `Readeck`),
   `HTTP_TIMEOUT`. Full table in [docs/AGENT.md](AGENT.md) §3.

5. **Optional: NickelMenu entry** for a manual sync. Copy
   `dist/agent/nm-readeckobo-sample` to `/mnt/onboard/.adds/nm/readeckobo` and
   reboot. You then get **Home ▸ … ▸ Sync Readeck**. (This never overwrites an
   existing NickelMenu config.)

## First run — what to expect

1. The agent runs automatically when the device joins a Wi-Fi network (and/or
   when you hit "Sync Readeck" in NickelMenu).
2. First pass: articles download as kepubs into a **Readeck** collection and
   appear in your library, sorted by date added. It can take a minute or two
   (download + library rescan + import).
3. Syncs repeat every 30 minutes by default while on Wi-Fi (and on every Wi-Fi
   (re)association).
4. Make a highlight or note while reading. On the next sync pass the agent
   reads it out of `KoboReader.sqlite` and uploads it; it shows up in Readeck
   shortly after. New highlights create annotations; edits update them;
   unchanged rows are left alone (the ledger keeps track).
5. Check the agent's log for what a pass did:

   ```sh
   tail -n 100 /mnt/onboard/.adds/readeckobo/agent.log
   ```

## Limitations (v1)

- **Cross-block highlights are skipped.** A single Readeck annotation cannot
  span two block elements, so a highlight whose start and end land in
  different paragraphs is dropped (unless the text happens to sit entirely
  inside one block). Same-paragraph highlights work.
- **Deletions and reading progress are not synced** in v1. Removing a
  highlight on the device does not delete the Readeck annotation; progress /
  read-status stays one-way (Readeck → Kobo).
- **Highlight color is always yellow.** Kobo has no color field, and v1 does
  not map note/color precedence; every annotation is created with color
  `yellow`.
- **The classic "My Articles" flow is unchanged** and does **not** support
  highlighting. Highlighting only works on the kepubs the agent imports into
  the Readeck collection.
- Articles you archive/delete in Readeck are removed from the device on the
  next sync (the file is deleted; stale library rows clear on the next
  rescan).
- Device-side DB writes (`DateCreated`, shelf) happen while Nickel is running —
  safe in the agent's design, but still to be confirmed on real hardware
  (see the checklist below).

## Troubleshooting

**Run one pass manually and read the log:**

```sh
/usr/local/readeckobo-agent/readeckobo-agent --once \
    --config /mnt/onboard/.adds/readeckobo/config
tail -n 100 /mnt/onboard/.adds/readeckobo/agent.log
```

| Symptom | Likely cause / fix |
|---|---|
| `cannot read config …` | create the config file from `config.sample` |
| `SERVER_URL is required` / `TOKEN is required` | fill both in `.adds/readeckobo/config` |
| `qndb not found at /usr/bin/qndb` | install NickelDBus, or set `QNDB_PATH` in config |
| files download but never appear in the library | the on-device rescan/import is failing: check the `qndb` output in the log, run `--once` **without** `--no-rescan`, confirm the kepub renders/highlights (see checklist item 1) |
| highlights sync but are placed at the wrong spot | the span/offset mapping is server-side; verify with the kepub span map and checklist item 3 |
| nothing uploads for a book you just removed | expected: removed books are excluded from the highlight scan |
| `next_cursor` error | the server paged the state response; v1 does not paginate — fix on the server or wait for a v2 |

**Server-side:** the agent state, kepub cache and annotation ledger live in
`<data_dir>/readeckobo.sqlite`. If you delete it, the server re-generates
kepubs and re-deduplicates highlights (harmless).

---

## ✅ Validate on your device

> This feature is **beta**. Everything below is the checklist from
> [docs/AGENT.md](AGENT.md) §6 — the open items that only a real device can
> answer. Work through them in order; most are single commands or a quick
> visual check.

1. **Kepub import from `.kobo/readeck/`** — install the agent, let one sync
   run, and confirm the articles actually appear in the library under the
   **Readeck** collection. If Nickel does not scan that folder, the kepub
   store moves to a visible directory (e.g. `.adds/readeckobo/Library`) — a
   small config/script change.
2. **Highlight support on sideloaded kepubs** — open a synced kepub and
   highlight a sentence. Confirm the highlight persists (restart the book).
3. **One end-to-end highlight** — make a highlight on a paragraph, run
   `readeckobo-agent --once`, and confirm it appears in Readeck at the correct
   spot. This validates the whole offset-mapping pipeline on real firmware.
4. **Date added sort** — set an old timestamp on a fresh import and confirm
   the device's Recent/"Date added" sort follows it. Also confirm an edited
   article (etag change) updates the existing library row instead of adding a
   duplicate.
5. **Collection/shelf shape** — after a sync, confirm the Readeck shelf shows
   up in the library UI (and note the firmware's `DbVersion` if you can see
   it, so the agent's shelf-write code can be pinned to it).
6. **DB writes while Nickel runs** — highlight, then immediately force a sync
   (`--once`) a few times in a row; confirm no lock errors in the log and the
   library stays stable.
7. **Wi-Fi trigger** — toggle Wi-Fi off/on and confirm a sync starts
   automatically (the udev rule + backgrounded script), and that a manual
   NickelMenu "Sync Readeck" also works.
8. **Filenames / filesystem** — confirm long titles with spaces sync and show
   up correctly on the device's filesystem; confirm the device can reach
   `SERVER_URL`.
9. **Per-device state** — set `DEVICE_SERIAL` to a unique value and confirm
   the server's state feed is per-device (each device gets its own
   add/update/remove actions). Without a real serial source this key defaults
   to `"unknown"`.
10. **Rescan signal** — if imports are slow, try switching `QNDB_ARGS` to the
    targeted `n3fssSyncOnboard` variant and confirm the library still updates
    correctly.

When all ten pass, the "beta" can be lifted. Until then, treat synced
highlights as an experiment, not your only copy.
