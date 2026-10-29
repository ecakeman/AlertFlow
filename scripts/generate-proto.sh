#!/usr/bin/env bash
# Regenerates the committed gRPC stubs. Versions are pinned below.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

PROTOC_GEN_GO_VERSION="${PROTOC_GEN_GO_VERSION:-v1.36.6}"
PROTOC_GEN_GO_GRPC_VERSION="${PROTOC_GEN_GO_GRPC_VERSION:-v1.5.1}"
GRPCIO_TOOLS_VERSION="${GRPCIO_TOOLS_VERSION:-1.67.1}"

export PATH="$(go env GOPATH)/bin:${PATH}"
if ! command -v protoc-gen-go >/dev/null || ! command -v protoc-gen-go-grpc >/dev/null; then
  GOBIN="$(go env GOPATH)/bin" go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"
  GOBIN="$(go env GOPATH)/bin" go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"
fi

py="${PROTO_PYTHON:-python3}"
if ! "$py" -c "import grpc_tools.protoc" >/dev/null 2>&1; then
  echo "grpc_tools is not installed for $py" >&2
  exit 1
fi

out_go="$root/backend/gen"
out_py="$root/agent/src/alertflow_agent/generated"
rm -rf "$out_go/alertflow" "$out_py"
mkdir -p "$out_go" "$out_py/alertflow/agent/v1"
touch "$out_py/__init__.py" "$out_py/alertflow/__init__.py" "$out_py/alertflow/agent/__init__.py" "$out_py/alertflow/agent/v1/__init__.py"

"$py" -m grpc_tools.protoc \
  -I "$root/proto" \
  --go_out="$out_go" --go_opt=module=github.com/ecakeman/alertflow/gen \
  --go-grpc_out="$out_go" --go-grpc_opt=module=github.com/ecakeman/alertflow/gen \
  --python_out="$out_py" --grpc_python_out="$out_py" \
  "$root/proto/alertflow/agent/v1/agent.proto"

# The Python plugin emits an absolute import. The package lives under alertflow_agent.generated.
grpc_py="$(find "$out_py" -name agent_pb2_grpc.py | head -1)"
if [[ -n "$grpc_py" ]]; then
  sed -i 's/^from alertflow\.agent\.v1 import agent_pb2 as/from . import agent_pb2 as/' "$grpc_py"
fi

echo "generated with protoc-gen-go ${PROTOC_GEN_GO_VERSION}, protoc-gen-go-grpc ${PROTOC_GEN_GO_GRPC_VERSION}, grpcio-tools ${GRPCIO_TOOLS_VERSION}"
