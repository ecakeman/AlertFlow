# RAGFlow

The `ragflow` Compose profile starts the stable tag `infiniflow/ragflow:v0.27.3` with the dependency images from that tag's `docker/docker-compose-base.yml`: Elasticsearch `8.11.3`, MySQL `8.0.40`, `pgsty/silo:RELEASE.2026-08-06T00-00-00Z`, and `valkey/valkey:8`. Do not use `latest`, `nightly`, or call `v1.0.0-rc1` a stable release. A default `docker compose up` does not start this profile. Its volumes are `ragflow-mysql` and `ragflow-minio`. They are not AlertFlow's Postgres, Redis, or Kafka.

This note records the HTTP API from that tag. It is not a record of a live completion call.

## Run the official Compose

Use the `v0.27.3` tree, not another checkout:

```text
git clone https://github.com/infiniflow/ragflow.git
cd ragflow
git checkout v0.27.3
cd docker
docker compose -f docker-compose.yml up -d
```

`docker/.env` sets `RAGFLOW_IMAGE=infiniflow/ragflow:v0.27.3`. `docker-compose.yml` starts RAGFlow. `docker-compose-base.yml` starts its MySQL, Redis, MinIO, and document engine. Their data stays in that Compose project's volumes. Stop it with `docker compose -f docker-compose.yml down` from `docker/`. Do not add `-v` unless you intend to delete those volumes.

To turn this off, leave `RAGFLOW_BASE_URL` empty and do not pass `--profile ragflow`. Stopping the profile with `docker compose --profile ragflow down` keeps the volumes. The profile sets `API_PROXY_SCHEME=python`, the same value as that tag's `docker/.env`, so the Python API listens on 9380. RAGFlow and its MySQL, Elasticsearch, MinIO, and Valkey sit on the `ragflow` network. That Valkey is the only host named `redis` there. AlertFlow's own Redis stays on the default network, so the two passwords do not get mixed. `a2a-adapter` joins both the default network and the `ragflow` network. The main `agent` service stays on the default network and reaches RAGFlow only through `A2A_ADAPTER_URL`. Elasticsearch answers 401 when security is on; its healthcheck is `curl http://localhost:9200`, matching the tag, because `curl -f` treats that 401 as failure and the API never starts.

These states are not the same: the container process is up; Elasticsearch answers on 9200; `GET /api/v1/system/healthz` returns ok; a chat model, dataset, and native agent exist; `POST /api/v1/agents/chat/completions` returns `code` 0. Health is `GET http://127.0.0.1:9380/api/v1/system/healthz`. Create the dataset `AlertFlow Operations Docs` and the native agent `AlertFlow Runbook Specialist` in that UI after a model is configured.

## API

Health, no API key: `GET /api/v1/system/healthz`.

Agent completion: `POST /api/v1/agents/chat/completions` with `Authorization: Bearer <API_KEY>`. The non-streaming standard body is `agent_id`, `query`, and `"stream": false`. Create the API key in the RAGFlow UI. `RAGFLOW_API_KEY` is that key. It is not an AlertFlow gRPC token.

A chunk is a verifiable document citation only when it has `document_id` (or `doc_id`) and `id` (or `chunk_id`). `data.data.content` is generated prose. An empty `reference` is not a citation.

## Agent and dataset

In that RAGFlow instance:

1. Configure a chat model before creating the agent.
2. Create a dataset named `AlertFlow Operations Docs` and upload the runbooks there.
3. Create a native agent named `AlertFlow Runbook Specialist` and attach that dataset.
4. Constrain its output to a short summary, the runbook conclusion, document sources, the next check, and unknowns.

Point AlertFlow at it only when you want that specialist available:

```text
RAGFLOW_BASE_URL=http://<ragflow-host>:9380
RAGFLOW_API_KEY=<api-key>
RAGFLOW_AGENT_ID=<agent-id>
```

Empty `RAGFLOW_BASE_URL`, `RAGFLOW_API_KEY`, or `RAGFLOW_AGENT_ID` does not open a connection. The optional Runbook tool is `consult_runbook_agent`. It calls the `runbook` Compose profile, which runs `python -m alertflow_agent.a2a_adapter`. That profile is not the core investigation. The core agent does not need RAGFlow.
