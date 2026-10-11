# AlertFlow Agent

The agent accepts AlertFlow's default webhook and reads `GET /internal/agent/v1`. It does not change incidents or delivery. A run is `queued` in Postgres until a worker slot is free (`MAX_CONCURRENT_RUNS`). On startup, runs left `running` are queued again and resumed from the LangGraph checkpoint. Resume can repeat the last read-only tool call and can append the same event twice.

## Start

Create the database `alertflow_agent` if the application role cannot `CREATE DATABASE`. The file `schema.sql` is idempotent and does not migrate the AlertFlow database.

```bash
cd agent
python -m venv .venv && . .venv/bin/activate
pip install -e ".[dev]"
export AGENT_WEBHOOK_TOKEN=hook AGENT_API_TOKEN=api ALERTFLOW_AGENT_TOKEN=agent
export ALERTFLOW_BASE_URL=http://127.0.0.1:8080
export AGENT_DATABASE_URL=postgres://alertflow:alertflow@127.0.0.1:5432/alertflow_agent?sslmode=disable
alertflow-agent
```

Compose. The profile command starts Postgres, Kafka, the AlertFlow server, and the publisher, consumer, and delivery processes, then the agent:

```bash
docker compose --profile agent up --build
```

To start only the agent against an already running database and AlertFlow:

```bash
docker compose --profile agent up --no-deps --build agent
```

`POSTGRES_IMAGE`, `KAFKA_IMAGE`, `NODE_IMAGE`, `GO_IMAGE`, `RUNTIME_IMAGE`, `PYTHON_IMAGE`, `PIP_INDEX_URL`, `NPM_REGISTRY`, and `GOPROXY` override the official defaults. Leave them unset unless a pull or a package download fails.

## Configuration

See `.env.example`. `AGENT_WEBHOOK_TOKEN` is the inbound webhook. `AGENT_API_TOKEN` is the query API. `ALERTFLOW_AGENT_TOKEN` is sent to AlertFlow and is a different value from `ALERTFLOW_WEBHOOK_TOKEN`. Empty `LLM_API_KEY` uses a scripted model so the graph can run; that output is not a model-quality result.

## Smoke

```bash
python demo/make_fixtures.py
python demo/stub_alertflow.py   # optional stand-in on :8098
AGENT_URL=http://127.0.0.1:8000/webhooks/alertflow AGENT_WEBHOOK_TOKEN=hook python demo/send_event.py
```

Then poll `GET /api/v1/runs/{id}` with `Authorization: Bearer $AGENT_API_TOKEN` until `status` is `completed` or `partial`, and read `GET /api/v1/runs/{id}/report`.

`python -m pytest -q` uses the scripted model. `python evals/run_evals.py` prints `NOT RUN` and exits non-zero when `LLM_API_KEY` is missing. It does not invent scores.

## What an external simulator calls

| Purpose | Value |
| --- | --- |
| AlertFlow inbound alerts | `POST http://alertflow:8080/api/alerts/webhook` with `ALERTFLOW_WEBHOOK_TOKEN` |
| Agent notification | `POST http://agent:8000/webhooks/alertflow` with `AGENT_WEBHOOK_TOKEN` |
| Agent reads AlertFlow | `GET http://alertflow:8080/internal/agent/v1/...` with `ALERTFLOW_AGENT_TOKEN` |
| Run query | `GET http://agent:8000/api/v1/runs/{id}` and `/report` with `AGENT_API_TOKEN` |

The default webhook keeps `event`, `service`, `url`, `status` (`down`, `recovered`, `warning`), and `message`. Added fields: `schema_version`, `event_id`, `incident_id`, optional `check_id`, `service_id`, `service_name`, `occurred_at`, `started_at`, optional `resolved_at`, `severity`, `attempts`, `error`. `event_id` is `<event>:<incident_id>` when an incident id exists.
