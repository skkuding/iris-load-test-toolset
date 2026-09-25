#!/usr/bin/env bash
set -euo pipefail

runtime="${IRIS_BENCHMARK_CONTAINER_RUNTIME:-podman}"
image="${IRIS_BENCHMARK_POSTGRES_IMAGE:-docker.io/library/postgres:18}"

command -v "${runtime}" >/dev/null 2>&1 || {
  printf 'required container runtime not found: %s\n' "${runtime}" >&2
  exit 1
}

exec "${runtime}" run --rm --network host -i \
  -e PGPASSWORD \
  "${image}" psql "$@"
