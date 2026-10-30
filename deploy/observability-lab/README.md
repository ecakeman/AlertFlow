# Observability lab

The `observability` profile is the local Compose path. A Kind cluster is a separate
operator step. The Agent tests do not start Kind, Prometheus, or Loki.

The P0 audit of the old Compose stack is in [P0_AUDIT.md](P0_AUDIT.md). The
Kubernetes form (Kustomize, the P5 target) lives in
[`deploy/k8s/overlays/observability/`](../k8s/overlays/observability/README.md),
and the three-layer check is [verify-p5.sh](verify-p5.sh).

## Path

1. Create a Kind cluster and install Prometheus and Loki so they scrape it.
   Prometheus must keep a label whose value is the AlertFlow `service_id`
   (`fault-lab`, `alertflow`); Loki must keep the same label on the stream
   selector. In the K8s overlay that label is produced by the Prometheus
   `relabel_configs` and by Alloy's `discovery.relabel` from the Pod label
   `alertflow.io/service-id`. The Compose profile predates that relabel and has
   no `service_id` series; do not treat a Compose run as P5 evidence.
2. Run the locked MCP processes from
   [../observability-mcp/README.md](../observability-mcp/README.md) inside the
   cluster or on the host, with read-only access to those two stores.
3. Set the Agent environment, and nothing else in the core Compose file:

```text
PROMETHEUS_MCP_URL=http://prometheus-mcp:8080/mcp
LOKI_MCP_URL=http://loki-mcp:8080/stream
PROMETHEUS_SERVICE_SELECTORS={"fault-lab":"service_id=\"fault-lab\"","alertflow":"service_id=\"alertflow\""}
LOKI_SERVICE_SELECTORS={"fault-lab":"{service_id=\"fault-lab\"}","alertflow":"{service_id=\"alertflow\"}"}
```

Tokens, when the MCP servers require them, come from a Kubernetes Secret mounted
only into the Agent. Leave the URLs empty when the lab is not in use.

The Agent then opens Streamable HTTP, discovers tools, and stores only a scoped
result in that run's evidence. It does not write `runtime_events`.

## Minimal case

Use one AlertFlow service id, `fault-lab`, and one incident.

1. Confirm Prometheus has a series for that service, for example
   `probe_success{service_id="fault-lab"}`, over the last hour.
2. Confirm Loki has at least one line for `{service_id="fault-lab"}` in that same
   hour.
3. Start an investigation for `fault-lab` with both MCP URLs set.
4. The report evidence for Prometheus must record the sent query, the applied
   start and end, and the sample cap. The Loki evidence must record the merged
   stream selector, the window, and the line cap.
5. Repeat with the selector JSON removed. The run must finish, the coverage gaps
   must say the sources have no evidence, and neither result may be stored under
   `fault-lab`.
6. Stop one MCP process and leave the other running. The live source may still
   produce evidence. The dead source must be a coverage gap, not a failed run.

`verify-p5.sh` automates steps 1–2 plus the MCP round trip and prints
`RESULT: PASS`/`RESULT: FAIL`. It only reads; it never writes to the cluster.

Shut the lab down by deleting the Kind cluster and stopping the two MCP
processes. Core Compose does not need a restart when the URLs are empty.
