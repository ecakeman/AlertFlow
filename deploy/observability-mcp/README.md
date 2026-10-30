# Observability MCP

Optional evidence for the Agent. The `observability` profile starts Prometheus `v3.15.1`, Blackbox `v0.29.1`, Loki `3.5.12`, Alloy `v1.21.3`, `ghcr.io/tjhop/prometheus-mcp-server:v0.18.0`, and a local image of `grafana/loki-mcp` tag `v0.6.0`. A default `docker compose up` does not start them. An empty `PROMETHEUS_MCP_URL` or `LOKI_MCP_URL` does not open a connection.

Blackbox probes `http://alertflow:8080/api/setup/status`. Prometheus scrapes that probe. Alloy reads the local Docker socket and pushes container logs to Loki. That socket is for local development only. This profile is not a production logging pipeline and it is not highly available. Host ports bind `127.0.0.1`. There is no Alertmanager and no Grafana.

`scripts/up-agent-observability.sh` sets the in-network MCP URLs when those variables are unset. Stop with `docker compose --profile agent --profile observability down`. Do not add `-v` unless the volumes should be deleted.

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

Stop that container with `docker stop` on its id, or Ctrl-C if it is attached. A default `docker compose up` does not start it. `scripts/up-agent-observability.sh` does.

Loki MCP from the locked tag:

```text
git checkout v0.6.0
go build -o loki-mcp-server ./cmd/server
LOKI_URL=http://loki:3100 PORT=8080 ./loki-mcp-server
```

The Streamable HTTP endpoint is `http://<host>:8080/stream`. Stop the process with SIGINT.

Point the Agent at those URLs with the selector JSON above. Leave the URLs empty to turn the integration off. The services are in the `observability` profile, not the default project.

## Smoke test

This is an operator check against a server you started. It is not part of the Agent test suite.

1. `POST` `initialize` to the Streamable HTTP URL and confirm the response is an MCP initialize result.
2. List tools. Prometheus must include the five approved names. Loki must include `loki_query`.
3. Call `query` with a PromQL selector that contains the mapped matcher, and `loki_query` with the mapped stream selector, using a window of at most one hour.
4. Call a tool that is not approved, such as `series`, and confirm the Agent does not store it as evidence.
5. Unset the URL and confirm a new investigation still finishes without dialing the MCP port.

## Kubernetes overlay

`deploy/k8s/overlays/observability/` is the K8s form of this stack. It is a
self-contained Kustomize overlay (no Helm, no Operator) and it keeps the same
pinned versions:

| Workload | Image | Endpoint |
| --- | --- | --- |
| Prometheus | `prom/prometheus:v3.15.1` (StatefulSet, 1Gi PVC, 72h retention) | `prometheus.observability.svc:9090` |
| Blackbox | `prom/blackbox-exporter:v0.29.1` (Deployment) | `blackbox-exporter.observability.svc:9115` |
| Loki | `grafana/loki:3.5.12` (StatefulSet, 2Gi PVC, 72h retention + compactor) | `loki.observability.svc:3100` |
| Alloy | `grafana/alloy:v1.21.3` (DaemonSet, `/var/log/pods` read-only hostPath) | pushes to Loki |
| Prometheus MCP | `ghcr.io/tjhop/prometheus-mcp-server:v0.18.0` | `http://prometheus-mcp.observability.svc:8080/mcp` |
| Loki MCP | `alertflow-loki-mcp:v0.6.0` (built from `deploy/observability/loki-mcp/Dockerfile`) | `http://loki-mcp.observability.svc:8080/stream` |

All services are `ClusterIP`; there is no NodePort and no Ingress. The
Prometheus MCP runs with `--mcp.tools=core`, which keeps the five approved tools
and removes the TSDB admin tools (`delete_series`, `clean_tombstones`, `snapshot`,
`quit`, ...) from the server side. Service-account tokens are not mounted into
any MCP container or Prometheus.

Alloy runs as uid 0 because the node's `/var/log/pods` directory is `0750
root:root` and the log files are `0640 root:root` (measured on
`kindest/node:v1.33.0`). It is not privileged: no `hostPID`, no `hostNetwork`,
no added capabilities, and `/var/log/pods` is mounted read-only. The Alloy
ConfigMap only needs `pods` `get/list/watch` in `alertflow`, `fault-lab`, and
`observability`.

Image versions are centralized in `deploy/k8s/overlays/observability/kustomization.yaml`
(`images:`). `deploy/k8s/versions.lock.yaml` is the authoritative lock file and
must match.

### CI job: build Loki MCP

Add this job to the repository workflow (`.github/workflows/`) before the kind
end-to-end job. It builds the locked image, checks that the binary serves the
`/stream` MCP endpoint, and keeps the tag the overlay expects. The `kind load
docker-image alertflow-loki-mcp:v0.6.0` step of the e2e job consumes it.

```yaml
  build-loki-mcp:
    name: build Loki MCP image
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: docker/setup-buildx-action@v3

      - name: Build (no push)
        uses: docker/build-push-action@v6
        with:
          context: deploy/observability/loki-mcp
          file: deploy/observability/loki-mcp/Dockerfile
          platforms: linux/amd64
          push: false
          load: true
          tags: |
            alertflow-loki-mcp:v0.6.0
            alertflow-loki-mcp:${{ github.sha }}
          cache-from: type=gha,scope=loki-mcp
          cache-to: type=gha,mode=max,scope=loki-mcp

      - name: Smoke test /stream initialize
        run: |
          docker run -d --name loki-mcp -p 18089:8080 \
            -e LOKI_URL=http://127.0.0.1:3100 alertflow-loki-mcp:v0.6.0
          for _ in $(seq 1 30); do
            code=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:18089/stream \
              -H 'Content-Type: application/json' \
              -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"ci","version":"0"}}}' || true)
            if [ "$code" = "200" ]; then
              echo "loki-mcp /stream answered initialize"
              exit 0
            fi
            sleep 1
          done
          echo "loki-mcp /stream did not answer initialize" >&2
          docker logs loki-mcp
          exit 1
```

`kind load docker-image` fails on multi-platform images pulled by Docker with
`content digest ... not found`. In that case load through the node's containerd
directly:

```text
docker save alertflow-loki-mcp:v0.6.0 |
  docker exec -i <kind-node> ctr -n k8s.io images import \
    --platform linux/amd64 --snapshotter=overlayfs -
```

The kind node cannot pull images itself when containerd is configured with an
unreachable proxy (`crictl pull` fails with `proxyconnect tcp: dial tcp
127.0.0.1:7897: connect: connection refused`); pre-load every observability
image before applying the overlay.
