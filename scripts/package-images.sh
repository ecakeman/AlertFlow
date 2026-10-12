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
  fail "pull failed for ${step} (${src})"
}

postgres_src=${POSTGRES_PULL:-${POSTGRES_IMAGE:-postgres:16-alpine}}
kafka_src=${KAFKA_PULL:-${KAFKA_IMAGE:-apache/kafka:3.9.1}}
pull_as "$postgres_src" postgres:16-alpine postgres
pull_as "$kafka_src" apache/kafka:3.9.1 kafka

echo "package-images: build alertflow and agent"
POSTGRES_IMAGE=postgres:16-alpine KAFKA_IMAGE=apache/kafka:3.9.1 \
  docker compose --profile agent build alertflow agent \
  || fail "image build failed"

docker image inspect alertflow:local >/dev/null 2>&1 || fail "alertflow:local was not built"
docker image inspect alertflow-agent:local >/dev/null 2>&1 || fail "alertflow-agent:local was not built"

mkdir -p dist
archive=$root/dist/alertflow-images.tar
echo "package-images: save ${archive}"
docker save \
  postgres:16-alpine \
  apache/kafka:3.9.1 \
  alertflow:local \
  alertflow-agent:local \
  -o "$archive" || fail "docker save failed"

python3 - "$archive" << 'PY'
import hashlib, pathlib, sys
path = pathlib.Path(sys.argv[1])
digest = hashlib.sha256()
with path.open("rb") as handle:
    for chunk in iter(lambda: handle.read(1024 * 1024), b""):
        digest.update(chunk)
print(f"package-images: bytes {path.stat().st_size}")
print(f"package-images: sha256 {digest.hexdigest()}")
PY
echo "package-images: tags postgres:16-alpine apache/kafka:3.9.1 alertflow:local alertflow-agent:local"
uname -m
