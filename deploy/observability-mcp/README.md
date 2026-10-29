# Observability MCP

Optional evidence for the Agent. These processes are not part of the core Compose file. An empty `PROMETHEUS_MCP_URL` or `LOKI_MCP_URL` does not open a connection.

The Agent uses MCP Python SDK 1.30.0 and Streamable HTTP. After `initialize` it lists tools and calls only a tool that both the server and this project allow.

## Locked servers

| Side | Release | Transport | Approved tools |
| --- | --- | --- | --- |
| Prometheus | `ghcr.io/tjhop/prometheus-mcp-server:v0.18.0` | Streamable HTTP `http://<host>:8080/mcp` (`--mcp.transport=http`) | `query`, `range_query`, `metric_metadata`, `label_names`, `label_values` |
| Loki | `grafana/loki-mcp` tag `v0.6.0`, binary `loki-mcp-server` | Streamable HTTP `http://<host>:8080/stream` | `loki_query` |

Do not use the Prometheus image tag `latest`. v0.19.0 renames the binary to `prometheus-mcp`. The Loki tag also serves legacy SSE at `/sse` and `/mcp`; the Agent URL must be the `/stream` endpoint. The Agent never calls `series` or TSDB admin tools. `label_names` and `metric_metadata` are not stored as service evidence.

## Secret and permissions

Set `PROMETHEUS_MCP_TOKEN` and `LOKI_MCP_TOKEN` only in the Agent process environment or a secret store. They are bearer tokens for the MCP endpoints, not `ALERTFLOW_AGENT_GRPC_TOKEN`. The Agent does not write them into evidence, reports, or logs.

`PROMETHEUS_MCP_TOKEN` is forwarded by `prometheus-mcp-server` v0.18.0 to the Prometheus backend. A client that sends `Authorization` does not, by itself, mean the MCP endpoint requires authentication. If that port is reachable beyond the Agent, put it behind a proxy or another network control that actually checks the caller.

The MCP process needs read-only access to Prometheus and Loki. Do not enable TSDB admin on the Prometheus MCP server.

Map each AlertFlow `service_id` before expecting evidence:

```text
PROMETHEUS_SERVICE_SELECTORS={"api":"service=\"api\""}
LOKI_SERVICE_SELECTORS={"api":"{service=\"api\"}"}
```

A service with no entry is not queried. PromQL that the Agent cannot attach to that one matcher, and LogQL that it cannot merge into that stream selector, are not sent.

Per run the Agent also caps the window (`MCP_MAX_WINDOW_SECONDS`, default 3600), samples (100), log lines (100), response bytes (65536), MCP calls (4), and the HTTP timeout (`MCP_TIMEOUT_SECONDS`, default 10).

## Start and stop

Prometheus MCP, pinned tag, Streamable HTTP only:

```text
docker run --rm -p 8080:8080 \
  -e PROMETHEUS_MCP_SERVER_PROMETHEUS_URL=http://prometheus:9090 \
  ghcr.io/tjhop/prometheus-mcp-server:v0.18.0 \
  --mcp.transport=http --web.listen-address=:8080
```

Stop that container with `docker stop` on its id, or Ctrl-C if it is attached. It is not started by `docker compose up`.

Loki MCP from the locked tag:

```text
git checkout v0.6.0
go build -o loki-mcp-server ./cmd/server
LOKI_URL=http://loki:3100 PORT=8080 ./loki-mcp-server
```

The Streamable HTTP endpoint is `http://<host>:8080/stream`. Stop the process with SIGINT.

Point the Agent at those URLs with the selector JSON above. Leave the URLs empty to turn the integration off. Core Compose does not gain a Prometheus, Loki, or MCP service.

## Smoke test

This is an operator check against a server you started. It is not part of the Agent test suite.

1. `POST` `initialize` to the Streamable HTTP URL and confirm the response is an MCP initialize result.
2. List tools. Prometheus must include the five approved names. Loki must include `loki_query`.
3. Call `query` with a PromQL selector that contains the mapped matcher, and `loki_query` with the mapped stream selector, using a window of at most one hour.
4. Call a tool that is not approved, such as `series`, and confirm the Agent does not store it as evidence.
5. Unset the URL and confirm a new investigation still finishes without dialing the MCP port.
