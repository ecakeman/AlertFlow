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

To turn this off, leave `RAGFLOW_BASE_URL` empty and do not pass `--profile ragflow`. Stopping the profile with `docker compose --profile ragflow down` keeps the volumes. Health is `GET http://127.0.0.1:9380/api/v1/system/healthz`. Create the dataset `AlertFlow Operations Docs` and the native agent `AlertFlow Runbook Specialist` in that UI after a model is configured.

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

Empty `RAGFLOW_BASE_URL` does not open a connection. This phase does not add an investigation tool or an A2A adapter.
