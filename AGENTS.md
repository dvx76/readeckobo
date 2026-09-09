# AGENTS.md

## Project Context

**readeckobo** bridges **Kobo e-readers** with a self-hosted **Readeck**
instance. It has two subsystems:

1. **Instapaper-proxy server** (original flow): Kobo devices have built-in
   Instapaper support; readeckobo emulates the Instapaper API so the device
   syncs Readeck articles without custom firmware on the device.
2. **Highlight-sync agent** (beta): a small on-device binary that imports
   Readeck articles as **kepubs** into a "Readeck" collection and syncs
   highlights/notes made while reading **back into Readeck** over Wi-Fi.

## Architecture Overview

Single Go module (`go 1.24.5`, vendored deps — `vendor/` is gitignored and
regenerated with `make vendor`). Standard Go project layout:

Server:

- **`cmd/readeckobo/`**: entry point — config, logger, state store, web server.
- **`internal/app/`**: business logic — Kobo (Instapaper) handlers, image
  conversion, and the agent routes (`/api/agent/state`, `/api/kepub/{id}`,
  `/api/agent/annotations`).
- **`internal/kepub/`**: kepub generator — Readeck article HTML → Kobo EPUB;
  rewrites Readeck `rd-*` annotation wrappers into
  `<span class="rd-annotation">` (styled yellow); `wrapRuns` keeps adjacent
  inline text byte-for-byte.
- **`internal/mapper/`**: maps device `Bookmark` rows ↔ Readeck annotations
  (start/end paths, offsets).
- **`internal/readeck/`**: Readeck API client — bookmarks (pagination via
  `Link` headers), annotations API, image/archive actions.
- **`internal/store/`**: per-user/device "seen" ledger on SQLite
  (`modernc.org/sqlite`, WAL) under `server.data_dir`.
- **`internal/webserver/`**, **`internal/config/`** (koanf),
  **`internal/logger/`**, **`internal/models/`** (shared types).

On-device agent:

- **`cmd/readeckobo-agent/`**: ARM cross-compiled binary
  (`CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7`) — state fetch, kepub
  download into `.kobo/readeck/`, NickelDBus rescan, Readeck shelf
  maintenance (modern shape with a real UUID `Id`), highlight scan/upload
  with a persistent ledger. TLS: embedded Mozilla bundle registered via
  `x509.SetFallbackRoots()` (the Kobo has **no** Go-readable system CA
  store), plus optional `CA_FILE` for private CAs.

Supporting:

- **`scripts/build-agent.sh`** + **`scripts/agent-payload/`**: agent build
  and the `KoboRoot.tgz` installer payload.
- **`scripts/device-diagnostics.sh`**: read-only diagnostics runbook for a
  Kobo attached over USB (device DB copy + queries, agent sidecars, server
  probes) — see `docs/device-diagnostics.md`.
- **`tools/kobofixdb/`**: synthetic Kobo device-DB fixture (offline smoke
  tests without a device).
- **`scripts/e2e-tests/`**: manual shell checks for the API endpoints.

## Deployment & Infrastructure

- **Server**: reverse proxy (nginx — `nginx.conf.snippet` — or Caddy)
  intercepts the Kobo's Instapaper/store API calls; Docker supported
  (`Dockerfile`, `docker-compose.yml`). The agent API (`/api/agent/*`,
  `/api/kepub/*`) must be routed to the server too.
- **Device agent**: `make agent` → `dist/agent/KoboRoot.tgz` → copy to
  `.kobo/` on the device → reboot installs it. Config:
  `/mnt/onboard/.adds/readeckobo/config`
  (`SERVER_URL`, `TOKEN`, `COLLECTION`, `CA_FILE`, `HTTP_TIMEOUT`, …).
  **NickelDBus** (`/usr/bin/qndb`) is required for wireless library
  rescans; an optional NickelMenu "Sync Readeck" entry runs
  `readeckobo-agent --once`.

## Development Guidelines for AI Agents

### 1. Code Style & Conventions

- Follow standard Go idioms (Effective Go).
- **No external mock libraries**: use manual mocking (e.g.,
  `MockRoundTripper` in `internal/app/app_test.go`); no `mockgen`/`testify`
  unless explicitly requested.
- Use **Table-Driven Tests** for unit logic.
- `make fmt` checks gofmt on tracked Go files (excludes `vendor/`).

### 2. Testing Protocol

- **Unit tests**: required for all logic changes, next to the code. Mock
  external HTTP (Readeck, Kobo). The agent package
  (`cmd/readeckobo-agent`) is fully self-tested against the real device-DB
  schema; note the pagination regression test
  (`TestGetBookmarksPaginationMultiLineLink`) — Readeck sends the `Link`
  header as **multiple header lines**, so `Header.Get` is not enough.
- **E2E**: `scripts/e2e-tests/` for human verification; maintain them when
  endpoint signatures change. `scripts/device-diagnostics.sh` is the
  read-only on-device verification path.
- `go build ./...`, `go test ./...`, `make lint` must pass; validate with
  the vendored tree (`-mod=vendor`).

### 3. Key Dependencies & Logic

- **Pure-Go SQLite** (`modernc.org/sqlite`) is mandatory: the agent
  cross-compiles for ARM with `CGO_ENABLED=0`. Do **not** add cgo
  dependencies.
- **Instapaper API emulation**: do not change the JSON structure of
  responses sent to Kobo — device firmware is brittle.
- **Image conversion**: JPEG/grayscale requirements for Kobo live in
  `internal/app`.
- **Kobo device DB** (`KoboReader.sqlite`): the agent writes it directly
  *on-device* (openReadWrite, busy handling, WAL). From the PC side only
  ever touch a *copy* (the diagnostics script does this); never write the
  live DB over the USB mount unless you know the device side is idle.

### 4. Refactoring & Maintenance

- Keep strict separation between `webserver` (transport) and `app`
  (business logic).
- Config changes must be reflected in **both** `config.yaml.example`
  (server) and `scripts/agent-payload/…/.adds/readeckobo/config.sample`
  (agent) — plus `docs/CONFIG.md` / `docs/AGENT.md` when user-facing.
- `KoboRoot.tgz` must stay minimal: it ships `usr/`, `etc/`, `mnt/` only and
  must **not** contain `.adds/nm` files (would clobber user NickelMenu
  config); the build script enforces this.
- Agent TLS bundle: `cmd/readeckobo-agent/cabundle.pem` is committed;
  refresh deliberately with `make refresh-cabundle` and re-commit.

## Reference Docs

- **`docs/AGENT.md`** — agent reference: wire contract, config keys, build,
  open questions.
- **`docs/HIGHLIGHTS.md`** — highlight-sync user guide + real-device
  validation checklist.
- **`docs/device-diagnostics.md`** — read-only diagnostics runbook.
- **`docs/research/`** — Kobo DB schema, kepub generation, API mapping,
  stream summaries (background for this feature).