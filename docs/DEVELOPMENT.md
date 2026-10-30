# Development

AlertFlow is a Go backend (`backend/`) that serves a Vue 3 SPA (`web-ui/`). It
is based on upmonitor. In production the built SPA is embedded into the Go
binary; in development you run the two side by side and let Vite proxy API
calls to the backend. Runtime data is PostgreSQL, not a file in the config
directory.

## Prerequisites

- **Go** 1.25+
- **Node** 22+ (or the version in `web-ui/package.json` `engines`)
- **PostgreSQL 16** (`docker compose up -d postgres` from the repo root)

## Run both dev servers

Copy `.env.example` and export `ALERTFLOW_DATABASE_URL` before starting the API.

**Terminal 1 — backend** (serves the API on `:8080`, YAML in `./config`):

```bash
cd backend
export ALERTFLOW_DATABASE_URL='postgres://alertflow:alertflow@127.0.0.1:5432/alertflow?sslmode=disable'
go run ./cmd/alertflow --config-dir ../config --addr :8080
```

**Terminal 2 — frontend** (Vite dev server on `:5173`):

```bash
cd web-ui
npm install
npm run dev
```

Open **http://localhost:5173**. Vite proxies `/api` and `/images` to the backend
(see `web-ui/vite.config.ts`), so cookies and requests work end to end. Edits to
either side hot-reload.

## Optional Compose profiles

`docker compose up` starts the core services only. Optional URLs come from the shell, then the root `.env`, then the empty Compose default. The scripts do not export those URLs. `--profile agent` starts the Python agent. `--profile runbook` starts only the external A2A adapter. `--profile ragflow` starts RAGFlow. The three core specialists are not separate profiles.

Agent dependencies are locked with `uv`. From `agent/`: `uv sync --frozen --extra dev`, then `uv run pytest`. The image and CI use the same `agent/uv.lock`.

```bash
scripts/up-core.sh
scripts/up-agent-observability.sh
scripts/up-agent-ragflow.sh
scripts/up-all.sh
```

Stop a profile with the same `docker compose --profile ... down`. Do not pass `-v` unless you mean to delete volumes. The observability Docker socket is for local development. RAGFlow uses its own volumes and does not share AlertFlow's Postgres, Redis, or Kafka.

## Build the single binary (production layout)

The Go binary embeds `backend/internal/web/dist`. Build the SPA, copy it in, then
build the binary:

```bash
# 1. Build the SPA
cd web-ui && npm ci && npm run build && cd ..

# 2. Embed it into the backend
rm -rf backend/internal/web/dist
cp -r web-ui/dist backend/internal/web/dist

# 3. Build the static binary (pure Go, no CGO)
cd backend
CGO_ENABLED=0 go build -o ../alertflow ./cmd/alertflow

# 4. Run it — serves UI + API on :8080
cd .. && ALERTFLOW_DATABASE_URL='postgres://alertflow:alertflow@127.0.0.1:5432/alertflow?sslmode=disable' ./alertflow --config-dir ./config
```

The `Dockerfile` automates exactly these steps across build stages.

## Quality gates

**Frontend** (from `web-ui/`):

```bash
npm run type-check   # vue-tsc, no emit
npm run lint         # oxlint + eslint (auto-fix)
npm run build        # type-check + production build
```

**Backend** (from `backend/`):

```bash
go vet ./...
go build ./...
go test ./...        # add tests under internal/*
```

## Project layout

```
backend/
  cmd/alertflow/         # main: flags, wiring, graceful shutdown
  internal/
    config/              # config.yaml types, load/save/validate (atomic)
    state/               # bootstrap state.json (active config dir)
    db/                  # PostgreSQL: users, sessions, checks, tls, incidents, integrations
      migrations/        #   goose *.sql migrations (embedded, run on Open)
    auth/                # bcrypt + session tokens
    monitor/             # HTTP checker (+TLS cert) + per-service ticker scheduler
    incident/            # opens/resolves incidents from status transitions
    notify/              # agent webhook sender and test dispatch
    image/               # WebP validate/store/serve
    archive/             # export/import zip (validate → backup → apply)
    logger/              # slog JSON logger setup
    api/                 # Gin server, middleware, all handlers
    web/                 # //go:embed dist (exposes web.FS())
web-ui/
  src/
    api/                 # typed REST client
    components/          # ui (shadcn-vue), layout, dashboard, services,
                         #   incidents, integrations, common
    composables/         # useServicesPolling
    stores/              # pinia: auth, services, incidents, integrations, settings, ui
    views/               # Dashboard, ServiceDetail, Incidents(+Detail), Integrations,
                         #   Resources, Settings, Setup, Login, NotFound
    lib/                 # icon-generator, image (canvas→WebP), prng, format
```

Import direction worth remembering: `incident → {db, notify}`. The check scheduler does not import `incident`. `monitor`, `publish`, `consume`, and `deliver` are separate processes selected by `ALERTFLOW_ROLE`; the server process does not start them. `monitor` is the process that runs periodic checks, diagnostics collection, and, when `ALERTFLOW_K8S_ENABLED` is set, the read-only Kubernetes observer (`k8s.io/client-go` v0.35.9).
`incident` must never import `monitor` (cycle) — see
[ARCHITECTURE.md §4](ARCHITECTURE.md#4-incident-lifecycle).

## Notes

- **PostgreSQL** uses pgx through `database/sql`. The binary stays static
  (`CGO_ENABLED=0`). Tests need `ALERTFLOW_TEST_DATABASE_URL`.
- **Image optimization** happens client-side (Canvas → WebP) so the backend has
  no image dependencies; it only validates and stores the WebP bytes.
- **Config vs DB**: `config.yaml` is the source of truth for services/settings;
  PostgreSQL holds users, sessions and metrics history. Old SQLite files are not imported.
- **HTTP** uses [Gin](https://github.com/gin-gonic/gin); handlers return `fail(code, msg)` for
  errors (rendered centrally as `{ "error": ... }`) and `writeJSON` for success.
- **Migrations** are goose `*.sql` files in `internal/db/migrations`, embedded and
  applied on startup (postgres dialect). Add a new numbered file for schema
  changes; never edit an applied one.
- **Logs** are structured `slog` JSON on stdout; set `ALERTFLOW_LOG_LEVEL`
  (`debug|info|warn|error`) to change verbosity.
