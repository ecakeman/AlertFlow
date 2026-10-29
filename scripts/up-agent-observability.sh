#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -z "${PROMETHEUS_MCP_URL+set}" ]; then
  export PROMETHEUS_MCP_URL=http://prometheus-mcp:8080/mcp
fi
if [ -z "${LOKI_MCP_URL+set}" ]; then
  export LOKI_MCP_URL=http://loki-mcp:8080/stream
fi
docker compose --profile agent --profile observability up -d
