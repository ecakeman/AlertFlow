#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -z "${RAGFLOW_BASE_URL+set}" ]; then
  export RAGFLOW_BASE_URL=http://ragflow:9380
fi
if [ -z "${A2A_ADAPTER_URL+set}" ]; then
  export A2A_ADAPTER_URL=http://a2a-adapter:8000
fi
docker compose --profile agent --profile ragflow --profile multi-agent up -d
