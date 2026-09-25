#!/usr/bin/env bash
#
# Verify the sanitized fixture set and its pinned manifest.
#
# Checks:
#   - the manifest matches the local files (delegated to build-fixture-manifest --check)
#   - exactly problems 568, 569, 570 are present
#   - every manifest object exists with the recorded size and SHA-256
#   - no .in/.out file is missing from the manifest
#   - assets are non-empty text, LF-terminated, and free of secret patterns
#
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
fixtures_dir="${repo_root}/fixtures"
manifest="${fixtures_dir}/manifest.json"
expected_problems="568 569 570"

# shellcheck source=../../scripts/aws/lib/common.sh
source "${repo_root}/scripts/aws/lib/common.sh"

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
pass() {
  printf 'ok: %s\n' "$*"
}

require_cmd jq
[ -f "${manifest}" ] || die "manifest not found: ${manifest}"

if [ "${1:-}" != "--no-manifest-check" ]; then
  if "${repo_root}/scripts/aws/build-fixture-manifest.sh" --check; then
    pass "manifest matches local assets"
  else
    fail "manifest is out of date (run build-fixture-manifest.sh --write)"
  fi
fi

actual_problems="$(jq -r '.problems | sort | join(" ")' "${manifest}")"
if [ "${actual_problems}" = "${expected_problems}" ]; then
  pass "problems are exactly: ${expected_problems}"
else
  fail "expected problems '${expected_problems}', found '${actual_problems}'"
fi

object_count="$(jq '.objects | length' "${manifest}")"
[ "${object_count}" -gt 0 ] || fail "manifest has no objects"
pass "manifest lists ${object_count} object(s)"

listed_keys="$(mktemp)"
trap 'rm -f "${listed_keys}"' EXIT

while IFS= read -r object; do
  s3_key="$(jq -r '.s3_key' <<<"${object}")"
  expected_sha="$(jq -r '.sha256' <<<"${object}")"
  expected_size="$(jq -r '.size' <<<"${object}")"
  local_path="${fixtures_dir}/${s3_key}"
  printf '%s\n' "${s3_key}" >>"${listed_keys}"

  if [ ! -f "${local_path}" ]; then
    fail "missing file for ${s3_key}"
    continue
  fi

  actual_sha="$(sha256_file "${local_path}")"
  actual_size="$(file_size "${local_path}")"

  [ "${actual_sha}" = "${expected_sha}" ] || fail "sha256 mismatch for ${s3_key}"
  [ "${actual_size}" = "${expected_size}" ] || fail "size mismatch for ${s3_key}"
  [ "${actual_size}" -gt 0 ] || fail "${s3_key} is empty"

  if [ "$(tail -c1 "${local_path}" | od -An -t u1 | tr -d '[:space:]')" != "10" ]; then
    fail "${s3_key} is not LF-terminated"
  fi

  if LC_ALL=C grep -q $'\r' "${local_path}"; then
    fail "${s3_key} contains CR characters"
  fi

  if LC_ALL=C grep -qP '\x00' "${local_path}" 2>/dev/null; then
    fail "${s3_key} contains NUL bytes"
  fi

  if LC_ALL=C grep -qE 'AKIA[0-9A-Z]{16}|BEGIN [A-Z ]*PRIVATE KEY|(postgres|postgresql)://[^ ]+:[^ ]+@' "${local_path}"; then
    fail "${s3_key} contains a secret-like pattern"
  fi
done < <(jq -c '.objects[]' "${manifest}")

# Every .in/.out under a problem directory must be pinned in the manifest.
while IFS= read -r file; do
  rel="${file#"${fixtures_dir}/"}"
  if ! grep -Fxq "${rel}" "${listed_keys}"; then
    fail "unmanifested fixture file: ${rel}"
  fi
done < <(find "${fixtures_dir}/568" "${fixtures_dir}/569" "${fixtures_dir}/570" \
  -type f \( -name '*.in' -o -name '*.out' \) | LC_ALL=C sort)

if [ "${failures}" -ne 0 ]; then
  printf '\n%d fixture check(s) failed\n' "${failures}" >&2
  exit 1
fi
printf '\nall fixture checks passed\n'
