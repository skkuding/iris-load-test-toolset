#!/usr/bin/env bash
#
# Tests for the AWS benchmark workflow scripts using a fake AWS CLI. No AWS
# credentials, network, or real infrastructure are touched.
#
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"
discover="${repo_root}/scripts/aws/discover-rds-snapshot.sh"
upload="${repo_root}/scripts/aws/upload-fixtures.sh"
bootstrap="${repo_root}/scripts/aws/bootstrap-benchmark-db-role.sh"
build_manifest="${repo_root}/scripts/aws/build-fixture-manifest.sh"
fake_bin="${script_dir}/fake-aws"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

export FAKE_AWS_LOG="${tmp}/aws.log"
export FAKE_S3_STATE_DIR="${tmp}/s3state"
: >"${FAKE_AWS_LOG}"
mkdir -p "${FAKE_S3_STATE_DIR}"

export IRIS_BENCHMARK_AWS_REGION="ap-northeast-2"
export IRIS_BENCHMARK_SOURCE_DB_IDENTIFIER="terraform-20250506182211604800000001"
source_identifier="${IRIS_BENCHMARK_SOURCE_DB_IDENTIFIER}"

chmod +x "${fake_bin}/aws" 2>/dev/null || true
export PATH="${fake_bin}:${PATH}"

failures=0
pass() { printf 'ok: %s\n' "$*"; }
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
assert_eq() {
  local expected="$1" actual="$2" msg="$3"
  if [ "${expected}" = "${actual}" ]; then
    pass "${msg}"
  else
    fail "${msg} (expected [${expected}] got [${actual}])"
  fi
}
assert_contains() {
  local needle="$1" haystack="$2" msg="$3"
  if grep -Fq -- "${needle}" <<<"${haystack}"; then
    pass "${msg}"
  else
    fail "${msg} (missing [${needle}])"
  fi
}
assert_not_contains() {
  local needle="$1" haystack="$2" msg="$3"
  if grep -Fq -- "${needle}" <<<"${haystack}"; then
    fail "${msg} (found [${needle}])"
  else
    pass "${msg}"
  fi
}
assert_failure() {
  local msg="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    fail "${msg} (expected non-zero exit)"
  else
    pass "${msg}"
  fi
}
count_log() { [ -f "${FAKE_AWS_LOG}" ] && grep -c "$1" "${FAKE_AWS_LOG}" || true; }

# --- Snapshot discovery ------------------------------------------------------

cat >"${tmp}/snapshots.json" <<'JSON'
{"DBSnapshots":[
 {"DBSnapshotIdentifier":"rds:src-2026-01-01","DBInstanceIdentifier":"terraform-20250506182211604800000001","Status":"available","SnapshotCreateTime":"2026-01-01T00:00:00.000Z","DBSnapshotArn":"arn:old","Encrypted":true},
 {"DBSnapshotIdentifier":"rds:src-2026-06-01","DBInstanceIdentifier":"terraform-20250506182211604800000001","Status":"available","SnapshotCreateTime":"2026-06-01T00:00:00.000Z","DBSnapshotArn":"arn:new","Encrypted":true},
 {"DBSnapshotIdentifier":"rds:src-pending","DBInstanceIdentifier":"terraform-20250506182211604800000001","Status":"creating","SnapshotCreateTime":"2026-07-01T00:00:00.000Z","DBSnapshotArn":"arn:pending","Encrypted":true},
 {"DBSnapshotIdentifier":"rds:other-db","DBInstanceIdentifier":"some-other-db","Status":"available","SnapshotCreateTime":"2026-08-01T00:00:00.000Z","DBSnapshotArn":"arn:other","Encrypted":false}
]}
JSON
export FAKE_SNAPSHOTS_FILE="${tmp}/snapshots.json"

: >"${FAKE_AWS_LOG}"
out="$("${discover}" --format id 2>/dev/null)"
assert_eq "rds:src-2026-06-01" "${out}" "discover selects newest available snapshot for the source"
assert_eq "1" "$(count_log 'rds describe-db-snapshots')" "discover calls only describe-db-snapshots"
assert_eq "1" "$(count_log .)" "discover makes exactly one AWS call"
assert_not_contains "s3api" "$(cat "${FAKE_AWS_LOG}")" "discover never calls a mutating S3 API"

out="$("${discover}" --format tfvars 2>/dev/null)"
assert_contains 'source_snapshot_identifier = "rds:src-2026-06-01"' "${out}" "tfvars output is apply-ready"

out="$("${discover}" --format json 2>/dev/null)"
assert_contains '"DBSnapshotIdentifier": "rds:src-2026-06-01"' "${out}" "json output contains the selected snapshot"

out="$("${discover}" --format env 2>/dev/null)"
assert_contains "IRIS_BENCHMARK_SOURCE_SNAPSHOT_IDENTIFIER='rds:src-2026-06-01'" "${out}" "env output is shell-safe"

cat >"${tmp}/none.json" <<'JSON'
{"DBSnapshots":[
 {"DBSnapshotIdentifier":"rds:src-pending","DBInstanceIdentifier":"terraform-20250506182211604800000001","Status":"creating","SnapshotCreateTime":"2026-07-01T00:00:00.000Z"}
]}
JSON
saved_snapshots_file="${FAKE_SNAPSHOTS_FILE}"
export FAKE_SNAPSHOTS_FILE="${tmp}/none.json"
assert_failure "discover fails when no snapshot is available" "${discover}" --format id
export FAKE_SNAPSHOTS_FILE="${saved_snapshots_file}"

# --- Fixture upload ----------------------------------------------------------

fx="${tmp}/fixtures"
mkdir -p "${fx}/568"
printf '1 2\n' >"${fx}/568/1.in"
printf '3\n' >"${fx}/568/1.out"

"${build_manifest}" --fixtures-dir "${fx}" --problems "568" --output "${fx}/manifest.json" >/dev/null 2>&1
[ -f "${fx}/manifest.json" ] && pass "fixture manifest generated" || fail "fixture manifest generation"

: >"${FAKE_AWS_LOG}"
out="$("${upload}" --dry-run --fixtures-dir "${fx}" --manifest "${fx}/manifest.json" --region ap-northeast-2 2>/dev/null)"
assert_contains '"dry_run": true' "${out}" "dry-run reports dry_run"
assert_contains '"verified_count": 2' "${out}" "dry-run verifies both objects"
assert_eq "0" "$(count_log .)" "dry-run makes no AWS calls"

printf 'tampered\n' >"${fx}/568/1.in"
assert_failure "dry-run fails on hash mismatch" \
  "${upload}" --dry-run --fixtures-dir "${fx}" --manifest "${fx}/manifest.json" --region ap-northeast-2
printf '1 2\n' >"${fx}/568/1.in"

: >"${FAKE_AWS_LOG}"
out="$("${upload}" --yes --fixtures-dir "${fx}" --manifest "${fx}/manifest.json" --region ap-northeast-2 --bucket test-bucket 2>/dev/null)"
assert_contains '"verified_count": 2' "${out}" "upload verifies both objects"
assert_contains '"version_id": "test-version-1"' "${out}" "upload records object version"
assert_eq "2" "$(count_log 's3api put-object')" "upload calls put-object once per object"
assert_eq "2" "$(count_log 's3api head-object')" "upload verifies each object with head-object"
assert_contains "--key 568/1.in" "$(cat "${FAKE_AWS_LOG}")" "upload uses the manifest S3 key"
assert_contains "--body ${fx}/568/1.in" "$(cat "${FAKE_AWS_LOG}")" "upload sends the exact local fixture file"

# --- Read-only DB role bootstrap --------------------------------------------

cat >"${tmp}/master.json" <<'JSON'
{"username":"skkuding","password":"TEST-master-not-a-secret"}
JSON
cat >"${tmp}/ro.json" <<'JSON'
{"username":"benchmark_ro","password":"testreadonlypassword123","host":"bench.example","port":5433,"dbname":"codedang_db"}
JSON
export FAKE_MASTER_SECRET_FILE="${tmp}/master.json"
export FAKE_RO_SECRET_FILE="${tmp}/ro.json"
export IRIS_BENCHMARK_RDS_MASTER_SECRET_ID="benchmark/master"
export IRIS_BENCHMARK_RDS_READONLY_SECRET_ID="benchmark/readonly"

fake_psql="${tmp}/psql"
cat >"${fake_psql}" <<'FAKE_PSQL'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${FAKE_PSQL_ARGS_LOG}"
if [[ "$*" == *"public.problem_testcase"* ]]; then
  printf 'benchmark_ro:on:42\n'
fi
if [ ! -t 0 ]; then
  cat >>"${FAKE_PSQL_STDIN_LOG}"
fi
FAKE_PSQL
chmod +x "${fake_psql}"
export FAKE_PSQL_ARGS_LOG="${tmp}/psql-args.log"
export FAKE_PSQL_STDIN_LOG="${tmp}/psql-stdin.log"
: >"${FAKE_PSQL_ARGS_LOG}"
: >"${FAKE_PSQL_STDIN_LOG}"

: >"${FAKE_AWS_LOG}"
out="$("${bootstrap}" --dry-run --psql "${fake_psql}" 2>&1)"
assert_contains "no AWS calls" "${out}" "bootstrap dry-run states no AWS calls"
assert_eq "0" "$(count_log .)" "bootstrap dry-run makes no AWS calls"

: >"${FAKE_AWS_LOG}"
out="$("${bootstrap}" --yes --psql "${fake_psql}" --region ap-northeast-2 </dev/null 2>&1)"
assert_not_contains "testreadonlypassword123" "${out}" "bootstrap does not print the read-only password"
assert_not_contains "TEST-master-not-a-secret" "${out}" "bootstrap does not print the master password"
assert_eq "2" "$(count_log 'secretsmanager get-secret-value')" "bootstrap reads both secrets from Secrets Manager"
assert_contains "\set ro_user 'benchmark_ro'" "$(cat "${FAKE_PSQL_STDIN_LOG}")" "bootstrap renders the role name"
assert_contains "ALTER ROLE :\"ro_user\"" "$(cat "${FAKE_PSQL_STDIN_LOG}")" "bootstrap applies the SQL on stdin"
assert_not_contains "testreadonlypassword123" "$(cat "${FAKE_PSQL_ARGS_LOG}")" "password is never passed as a psql argument"

# --- Problem fixture export --------------------------------------------------
if ! "${script_dir}/export-problem-fixtures.test.sh"; then
  failures=$((failures + 1))
fi

# -----------------------------------------------------------------------------

if [ "${failures}" -ne 0 ]; then
  printf '\n%d script test(s) failed\n' "${failures}" >&2
  exit 1
fi
printf '\nall script tests passed\n'
