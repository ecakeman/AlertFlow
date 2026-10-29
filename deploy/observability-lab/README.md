# Observability lab

The `observability` profile is the local path. A Kind cluster is a separate operator step. The Agent tests do not start Kind, Prometheus, or Loki.

## Path

1. Create a Kind cluster and install Prometheus and Loki so they scrape the cluster. Prometheus must keep a label whose value is the AlertFlow `service_id` (the examples below use `service`). Loki must keep the same label on the stream selector.
2. Run the locked MCP processes from [../observability-mcp/README.md](../observability-mcp/README.md) inside the cluster or on the host, with read-only access to those two stores.
3. Set the Agent environment, and nothing else in the core Compose file:

```text
PROMETHEUS_MCP_URL=http://prometheus-mcp:8080/mcp
LOKI_MCP_URL=http://loki-mcp:8080/stream
PROMETHEUS_SERVICE_SELECTORS={"checkout":"service=\"checkout\""}
LOKI_SERVICE_SELECTORS={"checkout":"{service=\"checkout\"}"}
```

Tokens, when the MCP servers require them, come from a Kubernetes Secret mounted only into the Agent. Leave the URLs empty when the lab is not in use.

The Agent then opens Streamable HTTP, discovers tools, and stores only a scoped result in that run's evidence. It does not write `runtime_events`.

## Minimal case

Use one AlertFlow service id, `checkout`, and one incident.

1. Confirm Prometheus has a series for that service, for example `up{service="checkout"}`, over the last hour.
2. Confirm Loki has at least one line for `{service="checkout"}` in that same hour.
3. Start an investigation for `checkout` with both MCP URLs set.
4. The report evidence for Prometheus must record the sent query, the applied start and end, and the sample cap. The Loki evidence must record the merged stream selector, the window, and the line cap.
5. Repeat with the selector JSON removed. The run must finish, the coverage gaps must say the sources have no evidence, and neither result may be stored under `checkout`.
6. Stop one MCP process and leave the other running. The live source may still produce evidence. The dead source must be a coverage gap, not a failed run.

Shut the lab down by deleting the Kind cluster and stopping the two MCP processes. Core Compose does not need a restart when the URLs are empty.
