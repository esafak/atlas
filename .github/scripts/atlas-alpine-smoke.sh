#!/usr/bin/env bash
set -euo pipefail

: "${SOURCE_SHA:?SOURCE_SHA must be set}"
TIDB_VERSION="${TIDB_VERSION:-v8.5.3}"
TIDB_IMAGE="ghcr.io/esafak/atlas/tidb:${TIDB_VERSION}"

docker run -d --name atlas-tidb --platform linux/amd64 \
  "${TIDB_IMAGE}"
trap 'docker rm -f atlas-tidb >/dev/null 2>&1 || true' EXIT

for arch in amd64 arm64; do
  docker buildx build \
    --platform "linux/${arch}" \
    --load \
    --tag "atlas-smoke:${arch}" \
    --file .github/ops/atlas/Dockerfile \
    --build-arg "VERSION=dev-${SOURCE_SHA}" \
    --build-arg "REVISION=${SOURCE_SHA}" \
    --build-arg "SOURCE=https://github.com/${GITHUB_REPOSITORY}/tree/${SOURCE_SHA}" \
    .github/ops/atlas

  for attempt in $(seq 1 60); do
    if docker run --rm --platform "linux/${arch}" --network container:atlas-tidb \
      "atlas-smoke:${arch}" schema inspect --url mysql://root@127.0.0.1:4000/test >/dev/null 2>&1; then
      break
    fi
    if [ "${attempt}" = 60 ]; then
      echo "TiDB did not become ready for Atlas ${arch} schema inspection" >&2
      exit 1
    fi
    sleep 2
  done

  version="$(docker run --rm --platform "linux/${arch}" --network container:atlas-tidb "atlas-smoke:${arch}" version)"
  printf '%s\n' "${version}" | grep -F "dev-${SOURCE_SHA}"
  docker run --rm --platform "linux/${arch}" --network container:atlas-tidb \
    "atlas-smoke:${arch}" schema inspect --url mysql://root@127.0.0.1:4000/test
  revision="$(docker image inspect "atlas-smoke:${arch}" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')"
  test "${revision}" = "${SOURCE_SHA}"
done
