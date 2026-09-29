#!/usr/bin/env bash
# Build a short-lived Docker env-file for the external Iris benchmark runtime.
# The file contains only read-only database and S3 credentials.
set -euo pipefail
umask 077

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${script_dir}/lib/common.sh"

required_prefix="codedang-iris-benchmark"
region="${IRIS_BENCHMARK_AWS_REGION:-ap-northeast-2}"
secret_id="${IRIS_BENCHMARK_RDS_READONLY_SECRET_ID:-}"
bucket="${IRIS_BENCHMARK_S3_BUCKET:-}"
role_arn="${IRIS_BENCHMARK_S3_READ_ROLE_ARN:-${IRIS_BENCHMARK_TESTCASE_READ_ROLE_ARN:-}}"
profile=""
secret_profile=""
s3_profile=""
out=""
duration=3600
dry_run="false"
assume_yes="false"

usage() {
  cat <<'USAGE'
Usage: build-iris-runtime-env.sh --out FILE [options]

Options:
  --out FILE             Required mode-0600 Docker env-file destination.
  --profile NAME         AWS CLI profile for the read and assume-role calls.
  --secret-profile NAME  AWS profile allowed to read the benchmark DB secret.
  --s3-profile NAME      AWS profile that already assumes the benchmark S3 read role.
  --duration SECONDS     STS session duration (default: 3600, maximum: 3600).
  --dry-run              Validate local inputs; make no AWS calls or writes.
  --yes                  Required for a real AWS call and env-file write.
  -h, --help             Show this help.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --out) [ $# -ge 2 ] || die "--out requires a value"; out="$2"; shift 2 ;;
    --profile) [ $# -ge 2 ] || die "--profile requires a value"; profile="$2"; shift 2 ;;
    --secret-profile) [ $# -ge 2 ] || die "--secret-profile requires a value"; secret_profile="$2"; shift 2 ;;
    --s3-profile) [ $# -ge 2 ] || die "--s3-profile requires a value"; s3_profile="$2"; shift 2 ;;
    --duration) [ $# -ge 2 ] || die "--duration requires a value"; duration="$2"; shift 2 ;;
    --dry-run) dry_run="true"; shift ;;
    --yes) assume_yes="true"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1 (use --help)" ;;
  esac
done

require_value "--out" "${out}"
require_value "--region" "${region}"
[[ "${duration}" =~ ^[0-9]+$ ]] || die "--duration must be numeric"
[ "${duration}" -ge 900 ] && [ "${duration}" -le 3600 ] || die "--duration must be between 900 and 3600 seconds"

# These checks happen before any AWS call. Prefix matching permits generation
# suffixes while refusing similarly named production resources.
case "${secret_id}" in
  "${required_prefix}"/*|"${required_prefix}"|arn:aws:secretsmanager:*:*:secret:${required_prefix}/*|arn:aws:secretsmanager:*:*:secret:${required_prefix}-*) ;;
  *) die "read-only secret id must be a codedang-iris-benchmark resource" ;;
esac
case "${bucket}" in
  "${required_prefix}"-*|"${required_prefix}") ;;
  *) die "S3 bucket must be a codedang-iris-benchmark resource" ;;
esac
case "${role_arn}" in
  arn:aws:iam::*:role/${required_prefix}-*) ;;
  *) die "S3 read role ARN must be a codedang-iris-benchmark role" ;;
esac

if [ "${dry_run}" = "true" ]; then
  printf '%s\n' "dry-run: no AWS calls and no env-file written" >&2
  exit 0
fi

[ "${assume_yes}" = "true" ] || die "real AWS access requires --yes"
require_cmd aws
require_cmd jq
[ -d "$(dirname -- "${out}")" ] || die "output directory does not exist: $(dirname -- "${out}")"

aws_args=()
[ -n "${profile}" ] && aws_args+=(--profile "${profile}")
secret_aws_args=("${aws_args[@]}")
[ -n "${secret_profile}" ] && secret_aws_args=(--profile "${secret_profile}")
readonly_json="$(aws "${secret_aws_args[@]}" secretsmanager get-secret-value \
  --region "${region}" --secret-id "${secret_id}" --query SecretString --output text)"
username="$(jq -er '.username' <<<"${readonly_json}")"
password="$(jq -er '.password' <<<"${readonly_json}")"
host="$(jq -er '.host' <<<"${readonly_json}")"
port="$(jq -er '.port' <<<"${readonly_json}")"
dbname="$(jq -er '.dbname' <<<"${readonly_json}")"
case "${host}" in
  ${required_prefix}.*|${required_prefix}-*) ;;
  *) die "database host is not a codedang-iris-benchmark host" ;;
esac

url_encode() { printf '%s' "$1" | jq -sRr @uri; }
database_url="postgresql://$(url_encode "${username}"):$(url_encode "${password}")@${host}:${port}/$(url_encode "${dbname}")?sslmode=require"

if [ -n "${s3_profile}" ]; then
  caller_arn="$(aws --profile "${s3_profile}" sts get-caller-identity --query Arn --output text)"
  case "${caller_arn}" in
    arn:aws:sts::*:assumed-role/${required_prefix}-testcase-read/*) ;;
    *) die "S3 profile is not authenticated as the benchmark testcase-read role" ;;
  esac
  credentials="$(aws --profile "${s3_profile}" configure export-credentials --format process)"
  access_key="$(jq -er '.AccessKeyId' <<<"${credentials}")"
  secret_key="$(jq -er '.SecretAccessKey' <<<"${credentials}")"
  session_token="$(jq -er '.SessionToken' <<<"${credentials}")"
else
  credentials="$(aws "${aws_args[@]}" sts assume-role \
    --role-arn "${role_arn}" \
    --role-session-name iris-external-runtime \
    --duration-seconds "${duration}" --query Credentials --output json)"
  access_key="$(jq -er '.AccessKeyId' <<<"${credentials}")"
  secret_key="$(jq -er '.SecretAccessKey' <<<"${credentials}")"
  session_token="$(jq -er '.SessionToken' <<<"${credentials}")"
fi

if [ "${assume_yes}" != "true" ]; then
  confirm "Write short-lived benchmark credentials to ${out}?" || die "aborted by operator"
fi
tmp="$(mktemp "$(dirname -- "${out}")/.iris-runtime-env.XXXXXX")"
cleanup() { rm -f -- "${tmp:-}"; }
trap cleanup EXIT
chmod 600 "${tmp}"
{
  printf 'DATABASE_URL=%s\n' "${database_url}"
  printf 'AWS_ACCESS_KEY_ID=%s\n' "${access_key}"
  printf 'AWS_SECRET_ACCESS_KEY=%s\n' "${secret_key}"
  printf 'AWS_SESSION_TOKEN=%s\n' "${session_token}"
  printf 'AWS_REGION=%s\n' "${region}"
  printf 'AWS_DEFAULT_REGION=%s\n' "${region}"
} >"${tmp}"
mv -f -- "${tmp}" "${out}"
trap - EXIT
log "wrote mode-0600 Iris runtime env-file: ${out}"
