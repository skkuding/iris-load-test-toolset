#!/usr/bin/env bash
#
# Export sanitized problem fixtures from the dedicated codedang-iris-benchmark
# RDS clone, and validate an extracted fixture directory.
#
# The script runs only against the cloned benchmark database. It reads the
# read-only credential from AWS Secrets Manager by secret id, refuses any host
# or identifier that is not codedang-iris-benchmark, forces
# default_transaction_read_only, and queries problem specifications and active
# (is_outdated = false) problem_testcase rows for exactly problems 568, 569,
# and 570. Credentials and testcase input/output are never printed: query rows
# are written to private temporary files and content is only ever written to
# the staged fixture directory.
#
# Modes:
#   extract (default): read the database and write a staged fixture directory
#     using the database testcase id as the file name, plus metadata.json that
#     preserves the database hidden flag.
#   validate (--validate): read an extracted directory and its metadata.json,
#     check its safety and consistency, and rewrite canonical metadata for the
#     existing manifest builder.
#
# Output layout (extract):
#   <out>/<problemId>/<testcaseId>.in
#   <out>/<problemId>/<testcaseId>.out
#   <out>/metadata.json
#
# The metadata `objects[]` array uses the exact shape the existing
# scripts/aws/build-fixture-manifest.sh pins (problem_id, s3_key,
# relative_path, sha256, size, content_type, hidden), so a builder reading this
# metadata preserves the database hidden flags instead of hard-coding false.
#
# Safety properties:
#   - The database session is read-only (SET default_transaction_read_only).
#   - The secret id, resolved host, and identifier must all be
#     codedang-iris-benchmark resources; anything else is refused before any
#     connection is attempted.
#   - The password is passed only through PGPASSWORD and is never an argument.
#   - Extraction is atomic: files are written to a sibling staging directory
#     created by mktemp and activated with a single rename.
#   - Symlinked path components, destination symlinks, and ".." traversal are
#     refused.
#
# Usage:
#   scripts/aws/export-problem-fixtures.sh --out DIR [options]
#   scripts/aws/export-problem-fixtures.sh --validate --fixtures-dir DIR [options]
#
# Options:
#   --out DIR                 Extraction destination (extract mode; required).
#   --validate                Validate mode instead of extract.
#   --extract                 Extract mode (default).
#   --fixtures-dir DIR        Extracted directory to validate (validate mode).
#   --metadata FILE           Input metadata (default: <fixtures-dir>/metadata.json).
#   --metadata-out FILE       Canonical metadata output
#                             (default: the input --metadata path).
#   --secret-id ID            Read-only DB secret
#                             (default: $IRIS_BENCHMARK_RDS_READONLY_SECRET_ID).
#   --region REGION           AWS region (default: $IRIS_BENCHMARK_AWS_REGION or
#                             ap-northeast-2).
#   --identifier ID           Expected RDS identifier
#                             (default: $IRIS_BENCHMARK_RDS_IDENTIFIER or
#                             codedang-iris-benchmark).
#   --host HOST               Override the secret host (must still match).
#   --port PORT               Override the secret port.
#   --connect-host HOST       Loopback transport endpoint after validating the
#                             secret host (for an SSH tunnel).
#   --connect-port PORT       Port for --connect-host.
#   --dbname NAME             Override the secret database name.
#   --problems "A B C"        Problem ids; must be exactly 568 569 570.
#   --max-bytes N             Maximum bytes per fixture file (default: 8388608).
#   --psql PATH               psql binary (default: psql).
#   --sql-file FILE           Export SQL file.
#   --force                   Replace an existing --out directory.
#   --dry-run                 Validate prerequisites and print the plan. No AWS
#                             calls, no secrets read, no database access, and no
#                             writes.
#   -h, --help                Show this help.
#
set -euo pipefail

umask 022

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${script_dir}/lib/common.sh"

required_prefix="codedang-iris-benchmark"
allowed_problems="568 569 570"

mode="extract"
secret_id="${IRIS_BENCHMARK_RDS_READONLY_SECRET_ID:-}"
region="${IRIS_BENCHMARK_AWS_REGION:-ap-northeast-2}"
identifier="${IRIS_BENCHMARK_RDS_IDENTIFIER:-${required_prefix}}"
host_override=""
port_override=""
connect_host=""
connect_port=""
dbname_override=""
psql_bin="psql"
sql_file="${script_dir}/sql/export-problem-fixtures.sql"
out_dir=""
fixtures_dir=""
metadata_file=""
metadata_out=""
problems="${allowed_problems}"
max_bytes=8388608
force="false"
dry_run="false"

usage() {
  sed -n '2,78p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

# Scratch directories created with mktemp that must be removed on any exit.
scratch_dirs=()
scratch_cleanup() {
  local dir
  for dir in "${scratch_dirs[@]}"; do
    [ -n "${dir}" ] || continue
    rm -rf -- "${dir}"
  done
}
trap scratch_cleanup EXIT
register_scratch() {
  scratch_dirs+=("$1")
}

while [ $# -gt 0 ]; do
  case "$1" in
    --out)
      out_dir="$2"
      shift 2
      ;;
    --validate)
      mode="validate"
      shift
      ;;
    --extract)
      mode="extract"
      shift
      ;;
    --fixtures-dir | --in)
      fixtures_dir="$2"
      shift 2
      ;;
    --metadata)
      metadata_file="$2"
      shift 2
      ;;
    --metadata-out)
      metadata_out="$2"
      shift 2
      ;;
    --secret-id)
      secret_id="$2"
      shift 2
      ;;
    --region)
      region="$2"
      shift 2
      ;;
    --identifier)
      identifier="$2"
      shift 2
      ;;
    --host)
      host_override="$2"
      shift 2
      ;;
    --port)
      port_override="$2"
      shift 2
      ;;
    --connect-host)
      connect_host="$2"
      shift 2
      ;;
    --connect-port)
      connect_port="$2"
      shift 2
      ;;
    --dbname)
      dbname_override="$2"
      shift 2
      ;;
    --problems)
      problems="$2"
      shift 2
      ;;
    --max-bytes)
      max_bytes="$2"
      shift 2
      ;;
    --psql)
      psql_bin="$2"
      shift 2
      ;;
    --sql-file)
      sql_file="$2"
      shift 2
      ;;
    --force)
      force="true"
      shift
      ;;
    --dry-run)
      dry_run="true"
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

require_cmd jq

# --- argument validation -----------------------------------------------------

normalize_problem_list() {
  printf '%s\n' "$1" | tr ' ' '\n' | sed '/^[[:space:]]*$/d' |
    LC_ALL=C sort -n | tr '\n' ' ' | sed 's/ $//'
}

if [ "$(normalize_problem_list "${problems}")" != "${allowed_problems}" ]; then
  die "--problems must be exactly '${allowed_problems}'"
fi

[[ "${max_bytes}" =~ ^[0-9]+$ ]] && [ "${max_bytes}" -gt 0 ] ||
  die "--max-bytes must be a positive integer"

if [ "${mode}" = "extract" ]; then
  require_value "--out" "${out_dir}"
else
  require_value "--fixtures-dir" "${fixtures_dir}"
  if [ -z "${metadata_file}" ]; then
    metadata_file="${fixtures_dir}/metadata.json"
  fi
  if [ -z "${metadata_out}" ]; then
    metadata_out="${metadata_file}"
  fi
fi

# --- safety helpers ----------------------------------------------------------

contains_dotdot() {
  case "/$1/" in
    */../*) return 0 ;;
  esac
  return 1
}

check_no_symlink_components() {
  local path="$1"
  local cur part
  local IFS=/
  local rel="${path#/}"
  if [ "${path#/}" != "${path}" ]; then
    cur="/"
  else
    cur="."
  fi
  for part in ${rel}; do
    [ -n "${part}" ] || continue
    if [ "${cur}" = "/" ]; then
      cur="/${part}"
    else
      cur="${cur%/}/${part}"
    fi
    if [ -L "${cur}" ]; then
      die "refusing symlink in path: ${cur}"
    fi
  done
  return 0
}

# Resolve a directory argument to an absolute path, refusing traversal and
# symlinked components. The parent must already exist.
resolve_dir() {
  local label="$1" dir="$2"
  require_value "${label}" "${dir}"
  if contains_dotdot "${dir}"; then
    die "refusing ${label} containing '..': ${dir}"
  fi
  local parent base
  parent="$(dirname -- "${dir}")"
  base="$(basename -- "${dir}")"
  if [ -z "${base}" ] || [ "${base}" = "." ] || [ "${base}" = "/" ]; then
    die "invalid ${label}: ${dir}"
  fi
  [ -d "${parent}" ] || die "parent directory does not exist for ${label}: ${parent}"
  check_no_symlink_components "${parent}"
  if [ -e "${dir}" ] && [ -L "${dir}" ]; then
    die "refusing symlinked ${label}: ${dir}"
  fi
  printf '%s/%s\n' "$(cd -- "${parent}" && pwd -P)" "${base}"
}

require_benchmark_secret() {
  local value="$1" name="$1"
  require_value "--secret-id (or IRIS_BENCHMARK_RDS_READONLY_SECRET_ID)" "${value}"
  # Strip a Secrets Manager ARN prefix so only the resource name is compared.
  case "${value}" in
    *:secret:*) name="${value##*:secret:}" ;;
  esac
  case "${name}" in
    "${required_prefix}" | "${required_prefix}"/*) ;;
    *) die "refusing non-benchmark secret id: ${value}" ;;
  esac
}

require_benchmark_identifier() {
  local value="$1"
  require_value "--identifier" "${value}"
  case "${value}" in
    "${required_prefix}" | "${required_prefix}"-*) ;;
    *) die "refusing non-benchmark identifier: ${value}" ;;
  esac
}

require_benchmark_host() {
  local value="$1"
  require_value "database host" "${value}"
  case "${value}" in
    "${required_prefix}" | "${required_prefix}".*) ;;
    *) die "refusing non-benchmark database host: ${value}" ;;
  esac
}

# --- fixture content helpers -------------------------------------------------

# Sanitize a raw testcase stream into the destination path. Normalization is
# deterministic: CR characters are removed and a missing trailing LF is added.
# NUL bytes, oversized content, and secret-like patterns cause a hard failure
# rather than being written.
sanitize_into() {
  local raw="$1" dest="$2"
  local size
  size="$(file_size "${raw}")"
  if [ "${size}" -gt "${max_bytes}" ]; then
    die "testcase content exceeds --max-bytes (${size} > ${max_bytes})"
  fi
  if LC_ALL=C grep -qP '\x00' "${raw}" 2>/dev/null; then
    die "refusing testcase content containing NUL bytes"
  fi
  if LC_ALL=C grep -qE 'AKIA[0-9A-Z]{16}|BEGIN [A-Z ]*PRIVATE KEY|(postgres|postgresql)://[^ ]+:[^ ]+@' "${raw}"; then
    die "refusing testcase content containing a secret-like pattern"
  fi
  tr -d '\r' <"${raw}" >"${dest}.tmp"
  if [ -s "${dest}.tmp" ]; then
    if [ "$(tail -c1 "${dest}.tmp" | od -An -t u1 | tr -d '[:space:]')" != "10" ]; then
      printf '\n' >>"${dest}.tmp"
    fi
  fi
  mv -- "${dest}.tmp" "${dest}"
}

# Validate an already-extracted fixture file without modifying it.
check_fixture_file() {
  local file="$1" rel="$2"
  local size
  size="$(file_size "${file}")"
  if [ "${size}" -gt "${max_bytes}" ]; then
    die "${rel} exceeds --max-bytes (${size} > ${max_bytes})"
  fi
  if LC_ALL=C grep -q $'\r' "${file}"; then
    die "${rel} contains CR characters; re-run extract"
  fi
  if LC_ALL=C grep -qP '\x00' "${file}" 2>/dev/null; then
    die "${rel} contains NUL bytes"
  fi
  if [ -s "${file}" ]; then
    if [ "$(tail -c1 "${file}" | od -An -t u1 | tr -d '[:space:]')" != "10" ]; then
      die "${rel} is not LF-terminated; re-run extract"
    fi
  fi
  if LC_ALL=C grep -qE 'AKIA[0-9A-Z]{16}|BEGIN [A-Z ]*PRIVATE KEY|(postgres|postgresql)://[^ ]+:[^ ]+@' "${file}"; then
    die "${rel} contains a secret-like pattern"
  fi
}

# --- extract mode ------------------------------------------------------------

run_extract() {
  local out_abs
  out_abs="$(resolve_dir "--out" "${out_dir}")"

  require_benchmark_secret "${secret_id}"
  require_benchmark_identifier "${identifier}"

  if [ "${dry_run}" = "true" ]; then
    log "dry-run: would export problems '${allowed_problems}' to ${out_abs}"
    log "dry-run: secret = ${secret_id} region = ${region} identifier = ${identifier}"
    log "dry-run: sql file = ${sql_file}"
    log "dry-run: no AWS calls, no secrets read, no database access, no writes"
    return 0
  fi

  require_cmd aws
  require_cmd "${psql_bin}"
  [ -f "${sql_file}" ] || die "SQL export file not found: ${sql_file}"
  require_value "--region" "${region}"

  local secret_json db_user db_pass host port dbname
  secret_json="$(aws secretsmanager get-secret-value \
    --region "${region}" --secret-id "${secret_id}" \
    --query SecretString --output text)"
  db_user="$(jq -er '.username' <<<"${secret_json}")"
  db_pass="$(jq -er '.password' <<<"${secret_json}")"
  host="$(jq -r '.host // empty' <<<"${secret_json}")"
  port="$(jq -r '.port // empty' <<<"${secret_json}")"
  dbname="$(jq -r '.dbname // empty' <<<"${secret_json}")"
  [ -n "${host_override}" ] && host="${host_override}"
  [ -n "${port_override}" ] && port="${port_override}"
  [ -n "${dbname_override}" ] && dbname="${dbname_override}"

  require_benchmark_host "${host}"
  if [ -n "${connect_host}" ]; then
    case "${connect_host}" in
      127.0.0.1 | localhost | ::1) ;;
      *) die "--connect-host must be loopback" ;;
    esac
    host="${connect_host}"
    [ -n "${connect_port}" ] && port="${connect_port}"
  elif [ -n "${connect_port}" ]; then
    die "--connect-port requires --connect-host"
  fi
  require_value "database port" "${port}"
  require_value "database name" "${dbname}"

  local parent_dir stage
  parent_dir="$(dirname -- "${out_abs}")"
  stage="$(mktemp -d "${parent_dir}/.export-problem-fixtures.XXXXXX")"
  register_scratch "${stage}"

  local tmp_rows raw_rows psql_err
  tmp_rows="${stage}/.rows.ndjson"
  raw_rows="${stage}/.psql.out"
  psql_err="${stage}/.psql.err"
  : >"${tmp_rows}"

  log "export: host=${host} port=${port} db=${dbname} problems=${allowed_problems}"

  # The password travels only through the environment, never as an argument or
  # on disk. Query rows and diagnostics stay inside the private staging dir.
  if ! PGPASSWORD="${db_pass}" "${psql_bin}" \
    -X -q -v ON_ERROR_STOP=1 -A -t \
    -h "${host}" -p "${port}" -U "${db_user}" -d "${dbname}" \
    -f "${sql_file}" >"${raw_rows}" 2>"${psql_err}"; then
    if [ -s "${psql_err}" ]; then
      log "psql reported:"
      cat "${psql_err}" >&2
    fi
    die "read-only export query failed"
  fi
  unset db_pass

  # Keep only the JSON rows; ignore psql command tags or blank lines.
  LC_ALL=C grep -E '^\{' "${raw_rows}" >"${tmp_rows}" || true

  local problems_ndjson testcases_ndjson objects_ndjson skipped_ndjson
  problems_ndjson="${stage}/.problems.ndjson"
  testcases_ndjson="${stage}/.testcases.ndjson"
  objects_ndjson="${stage}/.objects.ndjson"
  skipped_ndjson="${stage}/.skipped.ndjson"
  : >"${problems_ndjson}"
  : >"${testcases_ndjson}"
  : >"${objects_ndjson}"
  : >"${skipped_ndjson}"

  local row kind pid tcid
  local seen_global=" "
  local exported_problems=" "

  while IFS= read -r row; do
    [ -n "${row}" ] || continue
    kind="$(jq -r '.kind // empty' <<<"${row}")"
    case "${kind}" in
      problem)
        pid="$(jq -r '.id | tostring' <<<"${row}")"
        case " ${allowed_problems} " in
          *" ${pid} "*) ;;
          *) die "unexpected problem id returned by the database: ${pid}" ;;
        esac
        jq -c 'del(.kind)' <<<"${row}" >>"${problems_ndjson}"
        ;;
      testcase)
        pid="$(jq -r '.problem_id | tostring' <<<"${row}")"
        tcid="$(jq -r '.testcase_id | tostring' <<<"${row}")"
        case " ${allowed_problems} " in
          *" ${pid} "*) ;;
          *) die "unexpected testcase problem id returned by the database: ${pid}" ;;
        esac
        [[ "${tcid}" =~ ^[1-9][0-9]*$ ]] ||
          die "non-numeric testcase id returned by the database: ${tcid}"
        if [ "$(jq -r '.is_outdated' <<<"${row}")" = "true" ]; then
          jq -cn --arg pid "${pid}" --argjson tcid "${tcid}" \
            '{problem_id: $pid, testcase_id: $tcid, reason: "outdated"}' \
            >>"${skipped_ndjson}"
          continue
        fi
        case "${seen_global}" in
          *" ${pid}/${tcid} "*)
            die "duplicate testcase id returned by the database: ${pid}/${tcid}"
            ;;
        esac
        seen_global="${seen_global}${pid}/${tcid} "

        local has_input has_output
        has_input="$(jq -r '.input != null' <<<"${row}")"
        has_output="$(jq -r '.output != null' <<<"${row}")"
        if [ "${has_input}" != "true" ] || [ "${has_output}" != "true" ]; then
          jq -cn --arg pid "${pid}" --argjson tcid "${tcid}" \
            '{problem_id: $pid, testcase_id: $tcid, reason: "null-inline-content"}' \
            >>"${skipped_ndjson}"
          continue
        fi

        local problem_stage
        problem_stage="${stage}/${pid}"
        mkdir -p -- "${problem_stage}"
        if [ -e "${problem_stage}/${tcid}.in" ] || [ -e "${problem_stage}/${tcid}.out" ]; then
          die "duplicate testcase id returned by the database: ${pid}/${tcid}"
        fi

        local raw_in raw_out
        raw_in="${problem_stage}/.${tcid}.in.raw"
        raw_out="${problem_stage}/.${tcid}.out.raw"
        jq -j '.input' <<<"${row}" >"${raw_in}"
        jq -j '.output' <<<"${row}" >"${raw_out}"
        sanitize_into "${raw_in}" "${problem_stage}/${tcid}.in"
        sanitize_into "${raw_out}" "${problem_stage}/${tcid}.out"
        rm -f -- "${raw_in}" "${raw_out}"

        local in_sha in_size out_sha out_size hidden
        in_sha="$(sha256_file "${problem_stage}/${tcid}.in")"
        in_size="$(file_size "${problem_stage}/${tcid}.in")"
        out_sha="$(sha256_file "${problem_stage}/${tcid}.out")"
        out_size="$(file_size "${problem_stage}/${tcid}.out")"
        hidden="$(jq -r '.is_hidden' <<<"${row}")"

        jq -cn \
          --arg pid "${pid}" \
          --arg key "${pid}/${tcid}.in" \
          --arg sha "${in_sha}" --argjson size "${in_size}" \
          --argjson hidden "${hidden}" \
          '{problem_id: $pid, s3_key: $key,
            relative_path: $key, sha256: $sha, size: $size,
            content_type: "text/plain", hidden: $hidden}' \
          >>"${objects_ndjson}"
        jq -cn \
          --arg pid "${pid}" \
          --arg key "${pid}/${tcid}.out" \
          --arg sha "${out_sha}" --argjson size "${out_size}" \
          --argjson hidden "${hidden}" \
          '{problem_id: $pid, s3_key: $key,
            relative_path: $key, sha256: $sha, size: $size,
            content_type: "text/plain", hidden: $hidden}' \
          >>"${objects_ndjson}"

        jq -c \
          --arg pid "${pid}" --argjson tcid "${tcid}" \
          --argjson hidden "${hidden}" \
          '{problem_id: $pid, testcase_id: $tcid, order: .order,
            score_weight: .score_weight, is_outdated: .is_outdated,
            hidden: $hidden}' \
          <<<"${row}" >>"${testcases_ndjson}"

        exported_problems="${exported_problems}${pid} "
        ;;
      "")
        continue
        ;;
      *)
        die "unexpected row kind returned by the database: ${kind}"
        ;;
    esac
  done <"${tmp_rows}"

  # Require every approved problem to be present with at least one testcase.
  local p
  for p in ${allowed_problems}; do
    if ! jq -e -s --arg p "${p}" 'any(.[]; .id == ($p | tonumber))' \
      "${problems_ndjson}" >/dev/null 2>&1; then
      die "database returned no problem specification for problem ${p}"
    fi
    case "${exported_problems}" in
      *" ${p} "*) ;;
      *) die "database returned no usable testcases for problem ${p}" ;;
    esac
  done

  local problems_json="$(
    printf '%s\n' ${allowed_problems} | jq -R . | jq -s .
  )"

  jq -S -n \
    --arg generator "scripts/aws/export-problem-fixtures.sh" \
    --arg source "codedang-iris-benchmark RDS clone (read-only export)" \
    --argjson problems "${problems_json}" \
    --slurpfile specs "${problems_ndjson}" \
    --slurpfile tcs "${testcases_ndjson}" \
    --slurpfile skipped "${skipped_ndjson}" \
    --slurpfile objs "${objects_ndjson}" \
    '{
      schema_version: 1,
      generator: $generator,
      source: $source,
      problems: $problems,
      problem_specs: ($specs | sort_by(.id)),
      testcases: ($tcs | sort_by(.problem_id, .testcase_id)),
      skipped_testcases: ($skipped | sort_by(.problem_id, .testcase_id)),
      objects: ($objs | sort_by(.s3_key))
    }' >"${stage}/metadata.json"

  # Remove staging bookkeeping before activation.
  rm -f -- "${tmp_rows}" "${raw_rows}" "${psql_err}" \
    "${problems_ndjson}" "${testcases_ndjson}" "${objects_ndjson}" "${skipped_ndjson}"

  activate_stage "${out_abs}" "${stage}"
  stage=""

  local testcase_count object_count skipped_count
  testcase_count="$(jq '.testcases | length' "${out_abs}/metadata.json")"
  object_count="$(jq '.objects | length' "${out_abs}/metadata.json")"
  skipped_count="$(jq '.skipped_testcases | length' "${out_abs}/metadata.json")"

  log "exported ${testcase_count} testcase(s), ${object_count} object(s) to ${out_abs}"
  jq -S -n \
    --arg mode "extract" \
    --argjson problems "${problems_json}" \
    --argjson testcases "${testcase_count}" \
    --argjson objects "${object_count}" \
    --argjson skipped "${skipped_count}" \
    --arg output "${out_abs}" \
    --arg metadata "${out_abs}/metadata.json" \
    '{schema_version: 1, mode: $mode, problems: $problems,
      testcase_count: $testcases, object_count: $objects,
      skipped_count: $skipped, output_dir: $output, metadata: $metadata}'
}

# Activate a staged directory atomically. Without --force an existing
# destination is refused, so the single rename cannot expose partial data.
activate_stage() {
  local dest="$1" stage_dir="$2"
  if [ -e "${dest}" ] || [ -L "${dest}" ]; then
    [ "${force}" = "true" ] || die "output already exists: ${dest} (use --force to replace)"
    local backup="${dest}.bak.$$"
    if [ -e "${backup}" ] || [ -L "${backup}" ]; then
      die "backup path already exists: ${backup}"
    fi
    mv -- "${dest}" "${backup}"
    if mv -T -- "${stage_dir}" "${dest}"; then
      rm -rf -- "${backup}"
    else
      mv -- "${backup}" "${dest}" || true
      die "failed to activate staged extraction at ${dest}"
    fi
  else
    mv -T -- "${stage_dir}" "${dest}"
  fi
}

# --- validate mode -----------------------------------------------------------

run_validate() {
  local dir_abs
  dir_abs="$(resolve_dir "--fixtures-dir" "${fixtures_dir}")"
  [ -d "${dir_abs}" ] || die "fixtures directory not found: ${dir_abs}"

  require_value "--metadata" "${metadata_file}"
  if contains_dotdot "${metadata_file}"; then
    die "refusing --metadata containing '..': ${metadata_file}"
  fi
  [ -f "${metadata_file}" ] || die "metadata not found: ${metadata_file}"
  if [ -L "${metadata_file}" ]; then
    die "refusing symlinked metadata file: ${metadata_file}"
  fi
  require_value "--metadata-out" "${metadata_out}"
  if contains_dotdot "${metadata_out}"; then
    die "refusing --metadata-out containing '..': ${metadata_out}"
  fi
  if [ -e "${metadata_out}" ] && [ -L "${metadata_out}" ]; then
    die "refusing symlinked metadata output: ${metadata_out}"
  fi
  check_no_symlink_components "$(dirname -- "${metadata_out}")"

  local schema_version
  schema_version="$(jq -r '.schema_version // empty' "${metadata_file}")"
  [ "${schema_version}" = "1" ] || die "unsupported metadata schema_version: ${schema_version:-<none>}"

  if [ "$(normalize_problem_list "$(jq -r '.problems | join(" ")' "${metadata_file}")")" != "${allowed_problems}" ]; then
    die "metadata problems are not exactly '${allowed_problems}'"
  fi

  if find "${dir_abs}" -type l -print -quit | grep -q .; then
    die "refusing symlink inside fixtures directory: ${dir_abs}"
  fi

  # Reject any unexpected top-level entry.
  local entry base
  while IFS= read -r entry; do
    base="${entry##*/}"
    if [ -d "${entry}" ] && [ ! -L "${entry}" ]; then
      case " ${allowed_problems} " in
        *" ${base} "*) continue ;;
      esac
    fi
    if [ -f "${entry}" ] && { [ "${entry}" = "${metadata_file}" ] || [ "${base}" = "metadata.json" ]; }; then
      continue
    fi
    die "unexpected entry in fixtures directory: ${entry}"
  done < <(find "${dir_abs}" -mindepth 1 -maxdepth 1 | LC_ALL=C sort)

  # Map metadata objects by relative path.
  declare -A meta_sha meta_size meta_hidden meta_type
  while IFS=$'\t' read -r rel sha size hidden ctype; do
    [ -n "${rel}" ] || continue
    meta_sha["${rel}"]="${sha}"
    meta_size["${rel}"]="${size}"
    meta_hidden["${rel}"]="${hidden}"
    meta_type["${rel}"]="${ctype}"
  done < <(jq -r '.objects[] | [.relative_path, .sha256, (.size | tostring), (.hidden | tostring), .content_type] | @tsv' "${metadata_file}")

  declare -A seen_objects=()
  local p file base rel tcid
  local file_count=0
  for p in ${allowed_problems}; do
    local problem_dir="${dir_abs}/${p}"
    [ -d "${problem_dir}" ] || die "missing problem directory: ${problem_dir}"

    while IFS= read -r file; do
      base="${file##*/}"
      case "${base}" in
        *.in | *.out) ;;
        *) die "unexpected fixture file name: ${file}" ;;
      esac
      rel="${p}/${base}"
      tcid="${base%.*}"
      [[ "${tcid}" =~ ^[1-9][0-9]*$ ]] ||
        die "fixture name is not a numeric testcase id: ${rel}"

      [ -n "${meta_sha[${rel}]:-}" ] || die "fixture missing from metadata: ${rel}"
      [ "${meta_type[${rel}]:-}" = "text/plain" ] || die "unsupported content_type for ${rel}"
      [ "${meta_hidden[${rel}]:-}" = "true" ] || [ "${meta_hidden[${rel}]:-}" = "false" ] ||
        die "metadata hidden flag is not a boolean for ${rel}"

      check_fixture_file "${file}" "${rel}"

      local actual_sha actual_size
      actual_sha="$(sha256_file "${file}")"
      actual_size="$(file_size "${file}")"
      if [ "${actual_sha}" != "${meta_sha[${rel}]}" ]; then
        die "sha256 mismatch for ${rel}: file=${actual_sha} metadata=${meta_sha[${rel}]}"
      fi
      if [ "${actual_size}" != "${meta_size[${rel}]}" ]; then
        die "size mismatch for ${rel}: file=${actual_size} metadata=${meta_size[${rel}]}"
      fi
      seen_objects["${rel}"]="1"
      file_count=$((file_count + 1))
    done < <(find "${problem_dir}" -mindepth 1 -maxdepth 1 -type f \( -name '*.in' -o -name '*.out' \) | LC_ALL=C sort)
  done

  # Every metadata object must have a corresponding file.
  local rel_key
  for rel_key in "${!meta_sha[@]}"; do
    [ -n "${seen_objects[${rel_key}]:-}" ] || die "metadata object has no fixture file: ${rel_key}"
  done

  # Every metadata testcase must have both halves present.
  local tcid_key
  while IFS=$'\t' read -r p tcid_key; do
    [ -n "${p}" ] || continue
    [ -n "${seen_objects[${p}/${tcid_key}.in]:-}" ] ||
      die "metadata testcase missing input object: ${p}/${tcid_key}"
    [ -n "${seen_objects[${p}/${tcid_key}.out]:-}" ] ||
      die "metadata testcase missing output object: ${p}/${tcid_key}"
    local hidden_in hidden_out
    hidden_in="${meta_hidden[${p}/${tcid_key}.in]}"
    hidden_out="${meta_hidden[${p}/${tcid_key}.out]}"
    [ "${hidden_in}" = "${hidden_out}" ] ||
      die "inconsistent hidden flags for testcase ${p}/${tcid_key}"
  done < <(jq -r '.testcases[] | [.problem_id, (.testcase_id | tostring)] | @tsv' "${metadata_file}")

  # Every approved problem must have a specification.
  for p in ${allowed_problems}; do
    jq -e --arg p "${p}" 'any(.problem_specs[]; (.id | tostring) == $p)' \
      "${metadata_file}" >/dev/null || die "metadata missing problem specification for ${p}"
  done

  local canonical
  canonical="$(jq -S \
    '.objects = (.objects | sort_by(.s3_key))
     | .testcases = (.testcases | sort_by(.problem_id, .testcase_id))
     | .skipped_testcases = ((.skipped_testcases // []) | sort_by(.problem_id, .testcase_id))
     | .problem_specs = (.problem_specs | sort_by(.id))' "${metadata_file}")"

  if [ "${dry_run}" = "true" ]; then
    log "dry-run: validated ${file_count} fixture file(s); metadata not written"
  else
    check_no_symlink_components "$(dirname -- "${metadata_out}")"
    local meta_dir meta_tmp
    meta_dir="$(dirname -- "${metadata_out}")"
    [ -d "${meta_dir}" ] || die "metadata output directory does not exist: ${meta_dir}"
    meta_tmp="$(mktemp "${meta_dir}/.metadata.XXXXXX")"
    register_scratch "${meta_tmp}"
    printf '%s\n' "${canonical}" >"${meta_tmp}"
    mv -T -- "${meta_tmp}" "${metadata_out}"
    log "validated ${file_count} fixture file(s); wrote metadata ${metadata_out}"
  fi

  jq -S -n \
    --arg mode "validate" \
    --arg fixtures "${dir_abs}" \
    --argjson objects "${file_count}" \
    --arg metadata "${metadata_out}" \
    --argjson dry_run "${dry_run}" \
    '{schema_version: 1, mode: $mode, fixtures_dir: $fixtures,
      object_count: $objects, metadata: $metadata, dry_run: $dry_run}'
}

# --- dispatch ----------------------------------------------------------------

if [ "${mode}" = "extract" ]; then
  [ -f "${sql_file}" ] || die "SQL export file not found: ${sql_file}"
  run_extract
else
  run_validate
fi
