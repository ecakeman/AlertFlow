#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -z "${PROMETHEUS_MCP_URL+set}" ]; then
  export PROMETHEUS_MCP_URL=http://prometheus-mcp:8080/mcp
fi
if [ -z "${LOKI_MCP_URL+set}" ]; then
  export LOKI_MCP_URL=http://loki-mcp:8080/stream
fi
if [ -z "${RAGFLOW_BASE_URL+set}" ]; then
  export RAGFLOW_BASE_URL=http://ragflow:9380
fi
if [ -z "${A2A_ADAPTER_URL+set}" ]; then
  export A2A_ADAPTER_URL=http://a2a-adapter:8000
fi
docker compose --profile agent --profile observability --profile ragflow --profile multi-agent up -d
