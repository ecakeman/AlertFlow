#!/usr/bin/env bash
# Build the four runtime images and write dist/alertflow-images.tar.
# POSTGRES_PULL and KAFKA_PULL select where to pull. The archive is tagged
# with the names docker-compose.yml uses by default.
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$root"

fail() {
  echo "package-images: $1" >&2
  exit 1
}

docker info >/dev/null 2>&1 || fail "docker is not available"
docker compose version >/dev/null 2>&1 || fail "docker compose is not available"

pull_as() {
  local src=$1 dest=$2 step=$3 attempt
  echo "package-images: pull ${step} from ${src}"
  for attempt in 1 2 3; do
    if docker pull "$src"; then
      docker tag "$src" "$dest" || fail "tag failed for ${step}"
      return 0
    fi
    echo "package-images: pull attempt ${attempt} failed for ${step}" >&2
  done
  if docker image inspect "$src" >/dev/null 2>&1; then
    echo "package-images: registry pull failed for ${step}; using local image ${src}" >&2
    docker tag "$src" "$dest" || fail "tag failed for ${step}"
    return 0
  fi
  fail "pull failed for ${step} (${src})"
}

mkdir -p dist
archive=$root/dist/alertflow-images.tar
partial=$root/dist/alertflow-images.tar.partial
rm -f "$archive" "$partial"

postgres_src=${POSTGRES_PULL:-${POSTGRES_IMAGE:-postgres:16-alpine}}
kafka_src=${KAFKA_PULL:-${KAFKA_IMAGE:-apache/kafka:3.9.1}}
pull_as "$postgres_src" postgres:16-alpine postgres
pull_as "$kafka_src" apache/kafka:3.9.1 kafka

runtime_image=${RUNTIME_IMAGE:-gcr.io/distroless/static:latest}
node_image=${NODE_IMAGE:-node:22-alpine}
go_image=${GO_IMAGE:-golang:1.25}
python_image=${PYTHON_IMAGE:-python:3.12-slim}
echo "package-images: build alertflow runtime ${runtime_image}"
POSTGRES_IMAGE=postgres:16-alpine \
KAFKA_IMAGE=apache/kafka:3.9.1 \
RUNTIME_IMAGE="$runtime_image" \
NODE_IMAGE="$node_image" \
GO_IMAGE="$go_image" \
PYTHON_IMAGE="$python_image" \
NPM_REGISTRY="${NPM_REGISTRY:-}" \
GOPROXY="${GOPROXY:-}" \
PIP_INDEX_URL="${PIP_INDEX_URL:-}" \
  docker compose --profile agent build alertflow agent \
  || fail "image build failed"

for tag in postgres:16-alpine apache/kafka:3.9.1 alertflow:local alertflow-agent:local; do
  docker image inspect "$tag" >/dev/null 2>&1 || fail "${tag} is missing before save"
done

echo "package-images: save ${partial}"
docker save \
  postgres:16-alpine \
  apache/kafka:3.9.1 \
  alertflow:local \
  alertflow-agent:local \
  -o "$partial" || fail "docker save failed"

python3 - "$partial" "$archive" << 'PY' || fail "archive check failed"
import hashlib, json, os, pathlib, sys, tarfile
partial, archive = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
required = {
    "postgres:16-alpine",
    "apache/kafka:3.9.1",
    "alertflow:local",
    "alertflow-agent:local",
}
with tarfile.open(partial) as bundle:
    manifest = json.load(bundle.extractfile("manifest.json"))
found = {tag for item in manifest for tag in item.get("RepoTags") or []}
missing = required - found
if missing:
    raise SystemExit(f"package-images: archive tags missing {sorted(missing)}")
digest = hashlib.sha256()
with partial.open("rb") as handle:
    for chunk in iter(lambda: handle.read(1024 * 1024), b""):
        digest.update(chunk)
os.replace(partial, archive)
print("package-images: tags " + " ".join(sorted(found)))
print(f"package-images: bytes {archive.stat().st_size}")
print(f"package-images: sha256 {digest.hexdigest()}")
PY
uname -m
