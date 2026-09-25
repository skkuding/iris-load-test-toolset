#!/usr/bin/env bash
#
# Upload the sanitized fixture objects to the dedicated benchmark S3 bucket.
#
# The upload is object-exact: it iterates the fixture manifest and uploads each
# listed file individually (never `aws s3 sync`), preserving the relative path
# as the S3 key. Every object is verified by SHA-256 before upload and the
# stored checksum is verified with head-object after upload.
#
# Usage:
#   scripts/aws/upload-fixtures.sh [options]
#
# Options:
#   --bucket BUCKET      Target bucket (default: $IRIS_BENCHMARK_S3_BUCKET)
#   --region REGION      AWS region (default: $IRIS_BENCHMARK_AWS_REGION or
#                        ap-northeast-2)
#   --fixtures-dir DIR   Fixture root (default: <repo>/fixtures)
#   --manifest FILE      Manifest path (default: <fixtures-dir>/manifest.json)
#   --receipt FILE       Write an upload receipt JSON to FILE
#                        (default: print receipt JSON to stdout)
#   --dry-run            Verify local hashes and print the plan; no AWS calls.
#   --yes                Skip the interactive confirmation prompt.
#   -h, --help           Show this help.
#
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
# shellcheck source=lib/common.sh
source "${script_dir}/lib/common.sh"

bucket="${IRIS_BENCHMARK_S3_BUCKET:-}"
region="${IRIS_BENCHMARK_AWS_REGION:-ap-northeast-2}"
fixtures_dir="${repo_root}/fixtures"
manifest=""
receipt=""
dry_run="false"
assume_yes="false"

usage() {
  sed -n '2,27p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --bucket)
      bucket="$2"
      shift 2
      ;;
    --region)
      region="$2"
      shift 2
      ;;
    --fixtures-dir)
      fixtures_dir="$2"
      shift 2
      ;;
    --manifest)
      manifest="$2"
      shift 2
      ;;
    --receipt)
      receipt="$2"
      shift 2
      ;;
    --dry-run)
      dry_run="true"
      shift
      ;;
    --yes)
      assume_yes="true"
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

if [ -z "${manifest}" ]; then
  manifest="${fixtures_dir}/manifest.json"
fi

require_cmd jq
require_value "--fixtures-dir" "${fixtures_dir}"
[ -d "${fixtures_dir}" ] || die "fixtures directory not found: ${fixtures_dir}"
[ -f "${manifest}" ] || die "fixture manifest not found: ${manifest} (run build-fixture-manifest.sh --write)"

schema_version="$(jq -r '.schema_version' "${manifest}")"
[ "${schema_version}" = "1" ] || die "unsupported fixture manifest schema_version: ${schema_version}"
object_count="$(jq '.objects | length' "${manifest}")"
[ "${object_count}" -gt 0 ] || die "fixture manifest contains no objects: ${manifest}"

if [ "${dry_run}" != "true" ]; then
  require_cmd aws
  require_value "--bucket (or IRIS_BENCHMARK_S3_BUCKET)" "${bucket}"
else
  [ -n "${bucket}" ] || bucket="<dry-run-bucket>"
fi

require_value "--region" "${region}"

log "upload: bucket=${bucket} region=${region} objects=${object_count} dry_run=${dry_run}"

if [ "${dry_run}" != "true" ] && [ "${assume_yes}" != "true" ]; then
  confirm "Upload ${object_count} fixture objects to s3://${bucket} in ${region}?" ||
    die "aborted by operator"
fi

receipt_ndjson="$(mktemp)"
trap 'rm -f "${receipt_ndjson}"' EXIT

failures=0
verified=0

# Iterate objects from the pinned manifest. `base64 -d` of the key is not used;
# keys are plain relative paths, which we re-validate here.
while IFS= read -r object; do
  s3_key="$(jq -r '.s3_key' <<<"${object}")"
  expected_sha="$(jq -r '.sha256' <<<"${object}")"
  expected_size="$(jq -r '.size' <<<"${object}")"
  content_type="$(jq -r '.content_type' <<<"${object}")"
  hidden="$(jq -r '.hidden' <<<"${object}")"

  case "${s3_key}" in
    *..* | /* | "") die "refusing unsafe s3_key in manifest: ${s3_key}" ;;
  esac
  case "${expected_sha}" in
    [0-9a-f][0-9a-f]*) ;;
    *) die "refusing non-hex sha256 in manifest for ${s3_key}" ;;
  esac

  local_path="${fixtures_dir}/${s3_key}"
  [ -f "${local_path}" ] || {
    printf 'FAIL %s: local fixture missing: %s\n' "${s3_key}" "${local_path}" >&2
    failures=$((failures + 1))
    continue
  }

  actual_sha="$(sha256_file "${local_path}")"
  actual_size="$(file_size "${local_path}")"

  if [ "${actual_sha}" != "${expected_sha}" ]; then
    printf 'FAIL %s: sha256 mismatch local=%s manifest=%s\n' \
      "${s3_key}" "${actual_sha}" "${expected_sha}" >&2
    failures=$((failures + 1))
    continue
  fi
  if [ "${actual_size}" != "${expected_size}" ]; then
    printf 'FAIL %s: size mismatch local=%s manifest=%s\n' \
      "${s3_key}" "${actual_size}" "${expected_size}" >&2
    failures=$((failures + 1))
    continue
  fi

  if [ "${dry_run}" = "true" ]; then
    log "plan ${s3_key} -> s3://${bucket}/${s3_key} sha256=${actual_sha} size=${actual_size}"
    jq -cn \
      --arg key "${s3_key}" --arg sha "${actual_sha}" --argjson size "${actual_size}" \
      '{s3_key: $key, sha256: $sha, size: $size, verified: true, dry_run: true}' \
      >>"${receipt_ndjson}"
    verified=$((verified + 1))
    continue
  fi

  put_response="$(
    aws s3api put-object \
      --region "${region}" \
      --bucket "${bucket}" \
      --key "${s3_key}" \
      --body "${local_path}" \
      --content-type "${content_type}" \
      --checksum-algorithm SHA256 \
      --metadata "sha256=${actual_sha}" \
      --tagging "hidden=${hidden}" \
      --output json
  )"

  head_response="$(
    aws s3api head-object \
      --region "${region}" \
      --bucket "${bucket}" \
      --key "${s3_key}" \
      --checksum-mode ENABLED \
      --output json
  )"

  version_id="$(jq -r '.VersionId // empty' <<<"${put_response}")"
  stored_checksum="$(jq -r '.ChecksumSHA256 // empty' <<<"${head_response}")"
  stored_size="$(jq -r '.ContentLength // empty' <<<"${head_response}")"
  expected_checksum_b64="$(hex_to_base64 "${actual_sha}")"

  if [ "${stored_size}" != "${actual_size}" ]; then
    printf 'FAIL %s: stored size %s != %s\n' "${s3_key}" "${stored_size}" "${actual_size}" >&2
    failures=$((failures + 1))
    continue
  fi
  if [ "${stored_checksum}" != "${expected_checksum_b64}" ]; then
    printf 'FAIL %s: stored checksum mismatch (reported=%s expected=%s)\n' \
      "${s3_key}" "${stored_checksum:-<none>}" "${expected_checksum_b64}" >&2
    failures=$((failures + 1))
    continue
  fi

  jq -cn \
    --arg key "${s3_key}" \
    --arg sha "${actual_sha}" \
    --argjson size "${actual_size}" \
    --arg checksum "${stored_checksum}" \
    --arg version "${version_id}" \
    '{s3_key: $key, sha256: $sha, size: $size, checksum_sha256: $checksum, version_id: $version, verified: true}' \
    >>"${receipt_ndjson}"

  log "uploaded ${s3_key} version=${version_id:-<none>}"
  verified=$((verified + 1))
done < <(jq -c '.objects[]' "${manifest}")

receipt_json="$(
  jq -S -n \
    --arg bucket "${bucket}" \
    --arg region "${region}" \
    --argjson dry_run "${dry_run}" \
    --argjson failures "${failures}" \
    --argjson verified "${verified}" \
    --slurpfile objects "${receipt_ndjson}" \
    '{
      schema_version: 1,
      bucket: $bucket,
      region: $region,
      dry_run: $dry_run,
      verified_count: $verified,
      failure_count: $failures,
      objects: ($objects | sort_by(.s3_key))
    }'
)"

if [ -n "${receipt}" ]; then
  printf '%s\n' "${receipt_json}" >"${receipt}"
  log "wrote upload receipt: ${receipt}"
else
  printf '%s\n' "${receipt_json}"
fi

if [ "${failures}" -ne 0 ]; then
  die "${failures} fixture object(s) failed verification"
fi

log "all ${verified} fixture object(s) verified"
