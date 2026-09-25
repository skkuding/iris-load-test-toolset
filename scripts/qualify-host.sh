#!/usr/bin/env bash
# Read-only qualification of an approved Iris benchmark host.
#
# Usage:
#   scripts/qualify-host.sh                 # defaults to codedang8
#   scripts/qualify-host.sh codedang8
#   scripts/qualify-host.sh codedang8 --ask-become-pass
#   IRIS_BENCH_REPORT_DIR=~/iris-bench-reports scripts/qualify-host.sh codedang8
#
# This never mutates the target. It gathers effective host state, evaluates
# config/compatibility.yaml, and writes a JSON report under
# IRIS_BENCH_REPORT_DIR (default /tmp/iris-bench-qualification).
#
# Production nodes are read-only and must never be passed here; the Ansible
# guard refuses hosts that are not on the benchmark allowlist.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ANSIBLE_DIR="${REPO_ROOT}/ansible"
INVENTORY="${ANSIBLE_DIR}/inventory/loadtest/hosts"

HOST="codedang8"
if [[ $# -gt 0 && "$1" != -* ]]; then
  HOST="$1"
  shift
fi

if [[ ! -f "${INVENTORY}" ]]; then
  echo "error: load-test inventory not found at ${INVENTORY}" >&2
  exit 1
fi

export ANSIBLE_CONFIG="${ANSIBLE_DIR}/ansible.cfg"
export ANSIBLE_ROLES_PATH="${ANSIBLE_DIR}/roles"
export ANSIBLE_COLLECTIONS_PATH="${ANSIBLE_DIR}/collections:${HOME}/.ansible/collections:/usr/share/ansible/collections"
export IRIS_BENCH_REPORT_DIR="${IRIS_BENCH_REPORT_DIR:-/tmp/iris-bench-qualification}"

echo "qualify: host=${HOST} report_dir=${IRIS_BENCH_REPORT_DIR}" >&2

exec ansible-playbook \
  "${ANSIBLE_DIR}/playbooks/qualify_host.yml" \
  -i "${INVENTORY}" \
  --limit "${HOST}" \
  "$@"
