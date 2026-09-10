# AGENTS.md

## Project Context

**readeckobo** bridges **Kobo e-readers** with a self-hosted **Readeck**
instance: it is a proxy server written in Go that emulates the
**Instapaper** API, which Kobo devices support natively, so the devices
sync Readeck articles without custom firmware on the device.

## Architecture Overview

Single Go module (`go 1.24.5`, vendored deps — `vendor/` is gitignored and
regenerated with `make vendor`; build and test with `-mod=vendor`).
Standard Go project layout:

- **`cmd/readeckobo/`**: entry point (`main.go`) — config, logger, web server.
- **`internal/app/`**: business logic — Kobo (Instapaper) handlers
  (`/api/1/oauth/access_token`, `/api/1/bookmarks/list`, …), and image
  conversion.
- **`internal/readeck/`**: Readeck API client — bookmarks (pagination via
  `Link` headers, limit/offset), content sync, image/archive actions.
- **`internal/webserver/`**: HTTP server setup, routing, and middleware.
- **`internal/config/`**: configuration management using `koanf`.
- **`internal/logger/`**: leveled logger.
- **`internal/models/`**: shared data structures for Kobo and Readeck API
  payloads.

## Deployment & Infrastructure

- **Proxy Interception**: the system relies on a reverse proxy (nginx —
  `nginx.conf.snippet` — or Caddy) to intercept requests from the Kobo
  device intended for `www.instapaper.com` and redirect them to this
  service. Docker supported (`Dockerfile`, `docker-compose.yml`).
- **Configuration**: server config is `config.yaml` (see
  `config.yaml.example` and `docs/CONFIG.md`).

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
  external HTTP (Readeck, Kobo).
- **E2E**: `scripts/e2e-tests/` (01–04) for manual human verification;
  maintain their validity when endpoint signatures change.
- `go build ./...`, `go test ./...`, `make lint` must pass; validate with
  the vendored tree (`-mod=vendor`).

### 3. Key Dependencies & Logic

- **Instapaper API emulation**: do not change the JSON structure of
  responses sent to Kobo — device firmware is brittle.
- **Image conversion**: JPEG/grayscale requirements for Kobo live in
  `internal/app`.

### 4. Refactoring & Maintenance

- Keep strict separation between `webserver` (transport) and `app`
  (business logic).
- Config changes must be reflected in `config.yaml.example`.

## Reference Docs

- **`docs/CONFIG.md`** — configuration reference.
