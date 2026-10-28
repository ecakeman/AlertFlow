<div align="center">

# AlertFlow

### Service monitoring and alerts, based on [upmonitor](https://github.com/antlko/upmonitor).

AlertFlow keeps the upmonitor dashboard and adds a PostgreSQL runtime. The MIT
license and copyright of Anatol Kozhukhar stay in [`LICENSE`](LICENSE). This
version does **not** import an existing `upmonitor.db` on startup; back that
file up before upgrading. Service definitions still live in `config.yaml`.

A modern, minimal dashboard for keeping an eye on your web services. Add a URL,
watch its status, response time and uptime in real time, and rearrange
everything with drag‑and‑drop. The app is one Go binary; a full deployment also
runs PostgreSQL 16.

[![Repository](https://img.shields.io/badge/github-ecakeman%2FAlertFlow-181717?logo=github)](https://github.com/ecakeman/AlertFlow)
[![Made with Go + Vue](https://img.shields.io/badge/stack-Go%20%2B%20Vue%203-00ADD8?logo=go&logoColor=white)](#how-it-works)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

<!-- Add a screenshot at docs/screenshot.png to show it off here. -->
<!-- ![upmonitor dashboard](docs/screenshot.png) -->

</div>

<img width="1511" height="749" alt="image" src="https://github.com/user-attachments/assets/fa6343a1-17c3-4a71-be6c-8d1fee758814" />
<img width="1510" height="746" alt="image" src="https://github.com/user-attachments/assets/fedab591-5e5d-405a-a936-5baecdb0aee6" />


---

## Why AlertFlow?

AlertFlow is the upmonitor dashboard with a PostgreSQL runtime. The MIT license
and Anatol Kozhukhar's copyright stay in [`LICENSE`](LICENSE). It is small and
good‑looking — think Linear/Vercel, not a grafana clone — while still doing the
real work:

- 🟢 **Live status** — HTTP health checks with online / offline / warning /
  unknown states, colored borders and a pulsing indicator.
- 🔁 **No false alarms** — a failed check is retried before anything is declared
  down. A check that recovers on a retry is a **warning**, not an outage: it
  still counts as uptime and opens no incident. Tune the attempts and delays
  globally or per service.
- 🧩 **Drag‑and‑drop dashboard** — resize widgets and rearrange them; positions
  are saved to `config.yaml`. The **Warning** and **Avg uptime** summary tiles
  are configurable — hover for a pencil to set the warning look‑back period or
  pick which services count toward the average.
- 🎛️ **Three widget modes** — icon only, icon + name, or a mini dashboard with
  response time, uptime and a sparkline. Switch mode in one click from the card.
- 🔔 **Incidents that log themselves** — when the consumer sees a service go
  down it opens an incident and closes it on recovery, with start/end times and a
  duration; a check that recovers on a retry logs a warning event alongside
  them. Add your own for planned work, comment to keep the team in sync, and
  page through the full history.
- 📣 **Get told about it** — channels are **Telegram, Slack, email or any
  webhook**. Add a channel, hit *Send test*, done. An outage queues one task per
  enabled channel, and the delivery process sends it. A crash after the receiver
  accepts the request can send it again. Warnings are opt‑in per channel.
- 🔍 **Per‑service detail** — uptime over 7/30/365 days, a response‑time chart,
  recent incidents (outages **and** warnings), a paginated **ping console**
  that logs every check, 10 rows a page (successful runs collapsed, failures
  spelled out with the attempt and the reason), and **SSL certificate issuer
  and expiry** with a warning as the date approaches.
- 📈 **Metrics that matter** — uptime %, response time, error count and last
  success, kept for 30 days (configurable) in PostgreSQL and trimmed automatically.
- 🎨 **Instant icons** — generate a crisp, unique icon for any service on‑device
  (no external AI), or upload your own — auto‑optimized to WebP in your browser.
- 👥 **Share it** — invite friends as **admin** or **read‑only**.
- 💾 **Portable** — one YAML file plus images, incidents and integrations;
  export/import as a `.zip` with an automatic backup on restore.
- 🌗 **Dark mode first**, fully responsive, and one small binary that serves both
  the API and the UI.

---

## Quick start

Use Docker Compose. PostgreSQL 16 stores users, sessions, checks, incidents and
integrations. Kafka (`apache/kafka:3.9.1`) carries check events. The `alertflow`
container serves the API and the embedded UI and runs checks. `publisher`,
`consumer`, and `delivery` are the same binary with `ALERTFLOW_ROLE` set; run
one publisher. The app connects with `ALERTFLOW_DATABASE_URL` from `.env`. No
published image is required — Compose builds `alertflow:local` from this
repository. The consumer records incidents and queues notification tasks. The
delivery process sends those tasks. Sending is at-least-once.

```bash
cp .env.example .env
docker compose up -d --build
```

Open **http://localhost:8080** and create your admin account.

The upstream `ghcr.io/antlko/upmonitor` image is a SQLite app. It is not a
deployment of this repository.

### Portainer

1. **Stacks → Add stack**, and give it a name (for example `alertflow`).
2. Paste the contents of this repository's [`docker-compose.yml`](docker-compose.yml) into the **Web editor**, and set `POSTGRES_PASSWORD` the same way `.env.example` does.
3. Click **Deploy the stack**. Portainer builds the app image; it does not pull a published AlertFlow image.
4. Browse to `http://<your-server>:8080` and finish setup.

---

## First run

On first launch you'll be asked to **create an administrator password** — it's
hashed with bcrypt and stored in PostgreSQL; it never leaves your server.
After that:

1. **Add a service** — name + URL (e.g. `https://grafana.home.lab`).
2. Watch it turn green as the first health check runs.
3. Switch its widget mode, drag it around, generate an icon.
4. Invite people or enable the public dashboard from **Settings**.

---

## Configuration

Everything lives in one **config directory** (mounted at `/config` in Docker):

```
/config
├── config.yaml       # services + settings (managed by the UI, editable by hand)
├── images/           # service icons (<id>.webp)
└── backups/          # automatic snapshots created before an import
```

Users, sessions, check history, incidents and notification settings are in
PostgreSQL (`ALERTFLOW_DATABASE_URL`), not in this directory. Goose applies the
schema on startup. An old SQLite `upmonitor.db` is left untouched and is not
imported.

### Ports & volumes

| What      | Value                     | Notes                                  |
| --------- | ------------------------- | -------------------------------------- |
| HTTP port | `8080`                    | host side of `8080:8080` in `docker-compose.yml` |
| Config    | volume mounted at `/config` | holds YAML, images and backups |
| Database  | PostgreSQL service          | users, history, incidents, integrations |
| Kafka     | `apache/kafka:3.9.1`        | topic `alert-events`, three partitions; host port `9092` |

### Environment variables

| Variable                | Default   | Description                                  |
| ----------------------- | --------- | -------------------------------------------- |
| `ALERTFLOW_CONFIG_DIR`   | `/config` | YAML, images and backups.                    |
| `ALERTFLOW_ADDR`         | `:8080`   | Listen address (`host:port`).                |
| `ALERTFLOW_LOG_LEVEL`    | `info`    | Log verbosity: `debug`, `info`, `warn`, `error`. |
| `ALERTFLOW_DATABASE_URL` | —         | PostgreSQL DSN. Required.                    |
| `ALERTFLOW_ROLE`         | `server`  | `server`, `publisher`, `consumer`, or `delivery`. |
| `ALERTFLOW_KAFKA_BROKERS`| —        | Broker list for publisher and consumer.      |
| `ALERTFLOW_AGENT_CALLBACK_TOKEN` | — | Bearer token for `POST /webhooks/agent` (the agent pushes its diagnosis back). Empty disables that endpoint. |
| `REDIS_URL`              | —         | Optional read cache. Empty serves everything from PostgreSQL. |
| `ALERTFLOW_AGENT_TOKEN`  | —         | Bearer token for `GET /internal/agent/v1/...`. Empty disables those routes. |
| `ALERTFLOW_DB_MAX_OPEN`  | `10`      | Maximum open database connections.           |
| `ALERTFLOW_DB_MAX_IDLE`  | `5`       | Maximum idle database connections.           |
| `ALERTFLOW_DB_CONN_MAX_LIFETIME` | `30m` | Maximum lifetime of a connection.       |
| `ALERTFLOW_DB_CONN_MAX_IDLE_TIME` | `5m` | Maximum time a connection may stay idle. |

`docker compose --profile agent up --build` starts the notification path and the agent. `docker compose --profile agent up --no-deps --build agent` starts only the agent.

`make package-images` writes `dist/alertflow-images.tar` for this machine's CPU architecture. It contains `alertflow:local`, `alertflow-agent:local`, `postgres:16-alpine`, and `apache/kafka:3.9.1`. On another machine with the same architecture and Docker installed:

```bash
docker load -i dist/alertflow-images.tar
docker compose --profile agent up -d
```

Prepare `.env` on that machine. The archive does not include tokens or model settings. Leave `POSTGRES_IMAGE` and `KAFKA_IMAGE` unset so Compose uses the loaded tags and does not pull. `POSTGRES_PULL` and `KAFKA_PULL` only affect `make package-images`.

The agent reads
`AGENT_WEBHOOK_TOKEN` on `POST /webhooks/alertflow` and `AGENT_API_TOKEN` on
`/api/v1`. Both require the `Bearer` scheme. `LLM_API_KEY` also requires
`LLM_BASE_URL` and `LLM_MODEL`. `DIAGNOSTICS_FILE` is the only diagnostics URL
source. Names match [`.env.example`](.env.example) and
[`agent/.env.example`](agent/.env.example).

Command‑line flags mirror these: `--config-dir` and `--addr`.

A commented reference config lives at
[`config/config.example.yaml`](config/config.example.yaml); full field docs in
[`docs/CONFIGURATION.md`](docs/CONFIGURATION.md).

### Backup & restore

**Settings → Configuration → Export** downloads a `.zip` containing
`config.yaml` and your images. **Import** validates the archive, snapshots your
current config into `backups/`, then applies the new one — so a restore is
always safe.

### Roles

- **Admin** — full control (services, users, settings).
- **Read‑only** — can view the dashboard and metrics, nothing else.

---

## How it works

One static Go binary embeds the built Vue app and serves both the API and the UI.

- **Backend** — Go with [Gin](https://github.com/gin-gonic/gin) and PostgreSQL
  ([pgx](https://github.com/jackc/pgx), no CGO) with
  [goose](https://github.com/pressly/goose) migrations, a goroutine‑per‑service
  scheduler for checks, bcrypt + cookie sessions, and structured `slog` logging.
- **Frontend** — Vue 3 + TypeScript, Pinia, Tailwind CSS v4 and shadcn‑vue
  components, `grid-layout-plus` for the draggable grid.
- **Config** — `config.yaml` is the source of truth for services and settings;
  users, sessions and metrics live in PostgreSQL.

The application itself is one static binary. A running stack also needs
PostgreSQL, one Kafka broker, one publisher, one consumer, and one delivery
worker. Publishing and consuming are not one atomic commit with the database. The image-publish
workflow is set up to build `linux/amd64` and `linux/arm64`, but this repository
has not published an image, and the local `docker build` in phase 1 did not finish.

## Local development

See [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) to run the Vue dev server and Go
backend together, [`docs/API.md`](docs/API.md) for the REST API, and
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for how it behaves under the hood
(incidents, notifications, the dashboard grid, roles).

## License

[MIT](LICENSE)
