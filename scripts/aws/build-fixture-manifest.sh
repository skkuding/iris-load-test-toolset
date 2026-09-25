#!/usr/bin/env bash
#
# Build (or check) the deterministic fixture manifest that pins the exact set
# of testcase objects uploaded to the benchmark S3 bucket.
#
# The manifest records each object's relative path, S3 key, byte size, SHA-256,
# content type, and hidden tag. `upload-fixtures.sh` uploads exactly these
# objects and verifies the hashes, so the local sanitized assets and the
# uploaded objects are guaranteed to match.
#
# Usage:
#   scripts/aws/build-fixture-manifest.sh [--write | --check]
#
# Options:
#   --fixtures-dir DIR   Fixture root (default: <repo>/fixtures)
#   --output FILE        Manifest path (default: <fixtures-dir>/manifest.json)
#   --problems "A B C"   Problem IDs to include (default: "568 569 570")
#   --write              Write the manifest (default).
#   --check              Exit non-zero if the committed manifest differs.
#   -h, --help           Show this help.
#
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
# shellcheck source=lib/common.sh
source "${script_dir}/lib/common.sh"

fixtures_dir="${repo_root}/fixtures"
output=""
problems="568 569 570"
mode="write"

usage() {
  sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --fixtures-dir)
      fixtures_dir="$2"
      shift 2
      ;;
    --output)
      output="$2"
      shift 2
      ;;
    --problems)
      problems="$2"
      shift 2
      ;;
    --write)
      mode="write"
      shift
      ;;
    --check)
      mode="check"
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1 (use --help)"
      ;;
  esac
done

if [ -z "${output}" ]; then
  output="${fixtures_dir}/manifest.json"
fi

require_cmd jq
require_value "--fixtures-dir" "${fixtures_dir}"
[ -d "${fixtures_dir}" ] || die "fixtures directory not found: ${fixtures_dir}"

# shellcheck disable=SC2206
problem_array=(${problems})
[ "${#problem_array[@]}" -gt 0 ] || die "--problems must list at least one problem id"

ndjson="$(mktemp)"
trap 'rm -f "${ndjson}"' EXIT
metadata="${fixtures_dir}/metadata.json"
if [ -f "${metadata}" ]; then
  jq -e '.objects | type == "array"' "${metadata}" >/dev/null ||
    die "invalid fixture metadata: ${metadata}"
fi

for problem in "${problem_array[@]}"; do
  [[ "${problem}" =~ ^[0-9]+$ ]] || die "problem id must be numeric: ${problem}"
  problem_dir="${fixtures_dir}/${problem}"
  [ -d "${problem_dir}" ] || die "missing fixture directory for problem ${problem}: ${problem_dir}"

  count=0
  while IFS= read -r file; do
    [ -n "${file}" ] || continue
    rel="${file#"${fixtures_dir}/"}"
    sha="$(sha256_file "${file}")"
    size="$(file_size "${file}")"
    hidden=false
    if [ -f "${metadata}" ]; then
      metadata_object="$(jq -c --arg rel "${rel}" '.objects[] | select(.relative_path == $rel)' "${metadata}")"
      [ -n "${metadata_object}" ] || die "metadata is missing fixture object: ${rel}"
      [ "$(jq -r '.sha256' <<<"${metadata_object}")" = "${sha}" ] ||
        die "metadata sha256 mismatch: ${rel}"
      hidden="$(jq -r '.hidden' <<<"${metadata_object}")"
      [ "${hidden}" = "true" ] || [ "${hidden}" = "false" ] ||
        die "metadata hidden flag is invalid: ${rel}"
    fi
    jq -cn \
      --arg pid "${problem}" \
      --arg key "${rel}" \
      --arg sha "${sha}" \
      --argjson size "${size}" \
      --argjson hidden "${hidden}" \
      '{problem_id: $pid, s3_key: $key, relative_path: $key, sha256: $sha, size: $size, content_type: "text/plain", hidden: $hidden}' \
      >>"${ndjson}"
    count=$((count + 1))
  done < <(find "${problem_dir}" -type f \( -name '*.in' -o -name '*.out' \) | LC_ALL=C sort)

  [ "${count}" -gt 0 ] || die "no .in/.out fixtures found for problem ${problem} in ${problem_dir}"
done

problems_json="$(printf '%s\n' "${problem_array[@]}" | jq -R . | jq -s .)"

render() {
  jq -S -n \
    --argjson schema_version 1 \
    --arg generator "scripts/aws/build-fixture-manifest.sh" \
    --arg source "sanitized benchmark fixtures derived from the codedang-iris-benchmark RDS clone" \
    --argjson problems "${problems_json}" \
    --slurpfile objects "${ndjson}" \
    '{
      schema_version: $schema_version,
      generator: $generator,
      source: $source,
      problems: $problems,
      objects: ($objects | sort_by(.s3_key))
    }'
}

rendered="$(mktemp)"
trap 'rm -f "${ndjson}" "${rendered}"' EXIT
render >"${rendered}"

if [ "${mode}" = "check" ]; then
  [ -f "${output}" ] || die "manifest not found for --check: ${output}"
  if ! diff -u "${output}" "${rendered}" >&2; then
    die "fixture manifest is out of date: regenerate with ${BASH_SOURCE[0]} --write"
  fi
  log "fixture manifest is up to date: ${output}"
else
  mv "${rendered}" "${output}"
  log "wrote fixture manifest: ${output}"
fi
