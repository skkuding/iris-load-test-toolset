#!/usr/bin/env bash
#
# Bootstrap the read-only benchmark database role on the restored benchmark RDS.
#
# Credentials are read from AWS Secrets Manager at runtime. Nothing is written
# to disk, echoed, or passed as command-line arguments (the role password is
# fed to psql over stdin, and the master password only through PGPASSWORD).
# The SQL is idempotent and grants CONNECT/USAGE/SELECT only, with
# default_transaction_read_only forced on.
#
# Usage:
#   scripts/aws/bootstrap-benchmark-db-role.sh [options]
#
# Options:
#   --master-secret-id ID     Master credential secret
#                             (default: $IRIS_BENCHMARK_RDS_MASTER_SECRET_ID)
#   --readonly-secret-id ID   Read-only credential secret
#                             (default: $IRIS_BENCHMARK_RDS_READONLY_SECRET_ID)
#   --region REGION           AWS region (default: $IRIS_BENCHMARK_AWS_REGION or
#                             ap-northeast-2)
#   --host HOST               Override host (default: readonly secret .host)
#   --port PORT               Override port (default: readonly secret .port)
#   --dbname NAME             Override database (default: readonly secret .dbname)
#   --sql-file FILE           SQL bootstrap file
#                             (default: scripts/aws/sql/benchmark-readonly-role.sql)
#   --psql PATH               psql binary (default: psql)
#   --dry-run                 Validate prerequisites and print the plan. Does not
#                             contact AWS, read secrets, or mutate the database.
#   --yes                     Skip the interactive confirmation prompt.
#   -h, --help                Show this help.
#
set -euo pipefail

umask 077

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${script_dir}/lib/common.sh"

master_secret_id="${IRIS_BENCHMARK_RDS_MASTER_SECRET_ID:-}"
readonly_secret_id="${IRIS_BENCHMARK_RDS_READONLY_SECRET_ID:-}"
region="${IRIS_BENCHMARK_AWS_REGION:-ap-northeast-2}"
sql_file="${script_dir}/sql/benchmark-readonly-role.sql"
psql_bin="psql"
host_override=""
port_override=""
dbname_override=""
dry_run="false"
assume_yes="false"

usage() {
  sed -n '2,33p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --master-secret-id)
      master_secret_id="$2"
      shift 2
      ;;
    --readonly-secret-id)
      readonly_secret_id="$2"
      shift 2
      ;;
    --region)
      region="$2"
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
    --dbname)
      dbname_override="$2"
      shift 2
      ;;
    --sql-file)
      sql_file="$2"
      shift 2
      ;;
    --psql)
      psql_bin="$2"
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

require_value "--region" "${region}"
require_cmd jq
require_cmd "${psql_bin}"
[ -f "${sql_file}" ] || die "SQL bootstrap file not found: ${sql_file}"

if [ "${dry_run}" = "true" ]; then
  log "dry-run: would bootstrap read-only role on the benchmark RDS"
  log "dry-run: master secret = ${master_secret_id:-<unset>}"
  log "dry-run: readonly secret = ${readonly_secret_id:-<unset>}"
  log "dry-run: region = ${region}"
  log "dry-run: sql file = ${sql_file}"
  log "dry-run: no AWS calls, no secrets read, no database mutation"
  exit 0
fi

require_cmd aws
require_value "--master-secret-id (or IRIS_BENCHMARK_RDS_MASTER_SECRET_ID)" "${master_secret_id}"
require_value "--readonly-secret-id (or IRIS_BENCHMARK_RDS_READONLY_SECRET_ID)" "${readonly_secret_id}"

master_json="$(aws secretsmanager get-secret-value \
  --region "${region}" --secret-id "${master_secret_id}" \
  --query SecretString --output text)"
readonly_json="$(aws secretsmanager get-secret-value \
  --region "${region}" --secret-id "${readonly_secret_id}" \
  --query SecretString --output text)"

master_user="$(jq -er '.username' <<<"${master_json}")"
master_pass="$(jq -er '.password' <<<"${master_json}")"
ro_user="$(jq -er '.username' <<<"${readonly_json}")"
ro_pass="$(jq -er '.password' <<<"${readonly_json}")"

host="$(jq -r '.host // empty' <<<"${readonly_json}")"
port="$(jq -r '.port // empty' <<<"${readonly_json}")"
dbname="$(jq -r '.dbname // empty' <<<"${readonly_json}")"
[ -n "${host_override}" ] && host="${host_override}"
[ -n "${port_override}" ] && port="${port_override}"
[ -n "${dbname_override}" ] && dbname="${dbname_override}"

require_value "database host" "${host}"
require_value "database port" "${port}"
require_value "database name" "${dbname}"

# Validate identifiers and the role password charset before interpolating them
# into the psql session. Terraform generates the role password as alphanumerics;
# reject anything else rather than risk malformed quoting.
[[ "${master_user}" =~ ^[A-Za-z_][A-Za-z0-9_]{0,62}$ ]] || die "unsafe master username from secret"
[[ "${ro_user}" =~ ^[A-Za-z_][A-Za-z0-9_]{0,62}$ ]] || die "unsafe read-only username from secret"
[[ "${dbname}" =~ ^[A-Za-z_][A-Za-z0-9_]{0,62}$ ]] || die "unsafe database name"
[[ "${ro_pass}" =~ ^[A-Za-z0-9]+$ ]] || die "unexpected read-only password charset; refusing to bootstrap"

run_psql() {
  PGPASSWORD="${master_pass}" "${psql_bin}" \
    -X \
    -v ON_ERROR_STOP=1 \
    -h "${host}" \
    -p "${port}" \
    -U "${master_user}" \
    -d "${dbname}" \
    "$@"
}

run_readonly_psql() {
  PGPASSWORD="${ro_pass}" "${psql_bin}" \
    -X \
    -v ON_ERROR_STOP=1 \
    -h "${host}" \
    -p "${port}" \
    -U "${ro_user}" \
    -d "${dbname}" \
    "$@"
}

log "bootstrap: host=${host} port=${port} db=${dbname} role=${ro_user}"

if [ "${assume_yes}" != "true" ]; then
  confirm "Apply the read-only role bootstrap to ${dbname} as ${master_user}?" ||
    die "aborted by operator"
fi

# Variables are delivered over stdin so the role password never appears in the
# process table. ON_ERROR_STOP comes from the psql invocation.
{
  printf "\\set ro_user '%s'\n" "${ro_user}"
  printf "\\set ro_password '%s'\n" "${ro_pass}"
  printf "\\set db_name '%s'\n" "${dbname}"
  cat "${sql_file}"
} | run_psql

log "bootstrap applied; verifying role state"

run_psql -tA -c \
  "SELECT 'role=' || rolname || ' superuser=' || rolsuper || ' canlogin=' || rolcanlogin
     FROM pg_roles WHERE rolname = '${ro_user}'"

run_psql -tA -c \
  "SELECT 'readonly_guc=' || COALESCE(array_to_string(rolconfig, ','), 'unset')
     FROM pg_roles WHERE rolname = '${ro_user}'"

# Verify the credential actually reaches the restored application database and
# can read the table needed by the fixture exporter. Do not print row contents.
verification="$(run_readonly_psql -tA -c \
  "SELECT current_user || ':' || current_setting('default_transaction_read_only') || ':' || count(*)
     FROM public.problem_testcase")"
[[ "${verification}" == "${ro_user}:on:"* ]] ||
  die "read-only role verification failed for public.problem_testcase"

log "read-only benchmark role bootstrap complete"
