# Stream F — Integration polish, docs, CI & final gates

**Stream:** F (final) · **Date:** 2026-09-08
**Scope:** glue + docs + gates on top of streams A–E (no refactor of their code).
**Sibling docs:** `readeck-annotations-api.md` (A), `kobo-db-schema.md` (B),
`kepub-generation.md` (C), `mapping-and-endpoints.md` (D), `kobo-fixture-db.md`,
plus `docs/AGENT.md` (E).

---

## 1. What changed in this stream

### Build / Makefile
- `Makefile`: added **`agent`** (`./scripts/build-agent.sh`) and **`dist`**
  (alias for `agent`). Added **`fmt`** which runs
  `git ls-files '*.go' ':!vendor/**' | xargs gofmt -l` — the gofmt gate
  **excludes `vendor/`** (vendored deps are not gofmt-clean by design and are
  gitignored). `ci: lint test` unchanged.

### User docs
- **NEW `docs/HIGHLIGHTS.md`** — end-user guide: what it does (kepubs in a
  "Readeck" collection sorted by date added; highlights/notes sync wirelessly
  into Readeck), prerequisites (firmware 4.38.x Kobo, Readeck ≥ 0.22),
  server config (none required; optional `data_dir`), one-time device install
  (KoboRoot.tgz → `.kobo`, reboot, `.adds/readeckobo/config` with
  `SERVER_URL`+`TOKEN`, optional NickelMenu entry), first-run expectations,
  v1 limitations, troubleshooting, and a clearly-marked
  **"✅ Validate on your device"** checklist paraphrased from `docs/AGENT.md`
  §6 (ten actionable items).
- `README.md` — added a feature bullet, a short **"Kobo → Readeck Highlight
  Sync (beta)"** section linking `docs/HIGHLIGHTS.md` and `docs/AGENT.md`, the
  three new routes in the API endpoints table, and the new Makefile targets.
- `docs/CONFIG.md` — documented **`server.data_dir`** (default `"data"`;
  SQLite state store for kepub cache + annotation ledger).
- `config.yaml.example` — added a commented optional `data_dir` line under
  `server:`.

### e2e scripts
- **`scripts/e2e-tests/05-test-kepub.sh`** — `GET /api/kepub/<BOOKMARK_ID>`,
  prints content-type/size, asserts PK zip magic and validates with
  `unzip -t`/`unzip -l` when `unzip` exists.
- **`scripts/e2e-tests/06-test-agent-annotations.sh`** — `POST
  /api/agent/annotations` with a single item using an **unknown** `bookmark_id`
  and asserts the per-item JSON result reports `status: "error"` gracefully
  (HTTP 200, `results[]` populated with `error`).
- Both follow the 01–04 sibling structure (`#!/bin/sh`, usage guard → exit 1,
  `BASE_URL`), and are more defensive via `curl --fail-with-body` (the 01–04
  siblings predate this; noted for anyone who wants to retrofit them).

### CI
- `.woodpecker/build-docker.yml` — added an **`agent`** step (after `test`,
  shared workspace, `golang:1.24.5-alpine`) that runs **`make agent`**, so the
  ARM cross-compile + KoboRoot.tgz installer build is CI-checked on every push
  to `main` / tag (the top-level `when` already scopes the pipeline). Low-risk:
  build-agent.sh uses `-mod=vendor` (workspace `vendor` step runs first), and
  its `file`/`unzip` deps are optional/guarded. Docker build steps untouched.

---

## 2. Consistency pass results

- **Wire contract parity (internal/models/agent.go ↔ cmd/readeckobo-agent/client.go):**
  all 27 JSON keys match exactly (`bookmark_id`, `bookmark_row_id`,
  `start_path`, `start_offset`, `end_path`, `end_offset`, `text`,
  `annotation`, `type`, `date_created`, `date_modified`, `device`, `items`,
  `results`, `status`, `annotation_id`, `error`, plus the state-feed fields).
  Status strings match: server emits `created|updated|unchanged|skipped|error`;
  the client treats anything outside the first four as an error. Only cosmetic
  asymmetry: the server structs use `annotation_id,omitempty`/`error,omitempty`
  while the client's `annotationResult` has no `omitempty` — that struct is
  decode-only, so it is not a wire discrepancy.
- **docs/AGENT.md paths vs scripts/build-agent.sh outputs:** match. Build emits
  `dist/agent/{KoboRoot.tgz,readeckobo-agent-manual.tar.gz,nm-readeckobo-sample,
  config.sample,INSTALL.txt}`; the KoboRoot.tgz layout (`usr/local/readeckobo-agent/
  {readeckobo-agent,start.sh,udev_program.sh}`, `etc/udev/rules.d/98-readeckobo.rules`,
  `mnt/onboard/.adds/readeckobo/config.sample`, no NickelMenu config) is exactly what
  AGENT.md documents. Verified against the actual archive listing.
- **No compiled binaries in the repo tree:** `git status` untracked/ignored
  shows only source/docs/scripts dirs; `dist/` and the root `readeckobo`
  binary are gitignored.
- **go.mod:** `modernc.org/sqlite v1.46.0` is a direct dependency;
  `go mod vendor` reproduces cleanly (go.mod/go.sum byte-identical before/after).

---

## 3. Final gates (exact tails)

```
make build          → EXIT 0
make test           → ok (all packages)
make agent          → EXIT 0; dist/agent/KoboRoot.tgz + manual tarball written;
                      ELF 32-bit LSB executable, ARM, EABI5, statically linked
make fmt            → "All tracked Go files are gofmt-clean"
make -n ci          → golangci-lint run / go test ./...   (lint itself not run:
                      no golangci-lint binary locally)
gofmt (git ls-files '*.go' ':!vendor/**') → CLEAN (empty)
go vet ./...        → EXIT 0
go test -count=1 ./... → all ok (cmd/readeckobo-agent, internal/app, config,
                      kepub, mapper, readeck, store)
go build ./...      → EXIT 0
bash -n scripts/e2e-tests/05-test-kepub.sh / 06-test-agent-annotations.sh → OK
                      (syntax only; cannot run without a live server)
```

---

## 4. What's left — cutting over a real device

The software is built and unit-tested; nothing here can be confirmed without a
Libra 2 / 4.38.23697. To go live:

1. **Build the installer** — `make agent`, then copy
   `dist/agent/KoboRoot.tgz` to the device as `.kobo/KoboRoot.tgz`, eject,
   reboot.
2. **Configure** — copy `.adds/readeckobo/config.sample` → `config`, set
   `SERVER_URL` + `TOKEN` (a `users[].token` from `config.yaml`).
3. **Work through `docs/HIGHLIGHTS.md` → "Validate on your device"** (the
   ten-item checklist from `docs/AGENT.md` §6). The highest-risk items, in
   order: (1) does `pfmRescanBooksFull` import kepubs from `.kobo/readeck/`;
   (2) do highlights work on sideloaded kepubs and do offsets map correctly
   (one end-to-end highlight); (3) are the `DateCreated`/`Shelf` DB writes
   safe while Nickel runs; (4) does the Wi-Fi udev trigger fire.
4. **Per-device state** — find a real serial source and set `DEVICE_SERIAL`
   before relying on per-device state (defaults to `"unknown"`).
5. **Lift the beta label** once the checklist passes.

### Known v1 gaps (tracked in the docs)
- Cross-block highlights skipped (no L3 / split-into-per-block annotations).
- No pagination in the state feed (`next_cursor` must stay null).
- Deletions/progress not synced; colors always `yellow`.
- Kepub store location (`.kobo/readeck`) unverified on-device.
