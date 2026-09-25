#!/usr/bin/env bash
#
# Tests for scripts/aws/export-problem-fixtures.sh using the fake AWS CLI and a
# fake psql. No AWS credentials, network, database, or real infrastructure are
# touched. Testcase content is synthetic.
#
# Invoked standalone or appended from scripts/aws/tests/run-tests.sh.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"
export_script="${repo_root}/scripts/aws/export-problem-fixtures.sh"
sql_file="${repo_root}/scripts/aws/sql/export-problem-fixtures.sql"
fake_aws_dir="${script_dir}/fake-aws"
fake_psql="${script_dir}/fake-psql/psql"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

mkdir -p "${tmp}/s3state"
: >"${tmp}/aws.log"
: >"${tmp}/psql-args.log"
: >"${tmp}/psql-pass.log"
: >"${tmp}/psql-stdin.log"

export PATH="${fake_aws_dir}:${script_dir}/fake-psql:${PATH}"
export FAKE_AWS_LOG="${tmp}/aws.log"
export FAKE_S3_STATE_DIR="${tmp}/s3state"
export FAKE_MASTER_SECRET_FILE="${tmp}/master.json"
export FAKE_RO_SECRET_FILE="${tmp}/ro.json"
export FAKE_PSQL_ARGS_LOG="${tmp}/psql-args.log"
export FAKE_PSQL_PASSWORD_LOG="${tmp}/psql-pass.log"
export FAKE_PSQL_STDIN_LOG="${tmp}/psql-stdin.log"
export FAKE_PSQL_OUTPUT_FILE="${tmp}/rows.ndjson"
export IRIS_BENCHMARK_AWS_REGION="ap-northeast-2"
export IRIS_BENCHMARK_RDS_READONLY_SECRET_ID="codedang-iris-benchmark/readonly-db"
export IRIS_BENCHMARK_RDS_IDENTIFIER="codedang-iris-benchmark"

readonly_password="testreadonlypassword123"
benchmark_host="codedang-iris-benchmark.abc123.ap-northeast-2.rds.amazonaws.com"

cat >"${FAKE_MASTER_SECRET_FILE}" <<'JSON'
{"username":"skkudingpostgres","password":"TEST-master-not-a-secret"}
JSON
cat >"${FAKE_RO_SECRET_FILE}" <<JSON
{"username":"benchmark_ro","password":"${readonly_password}","host":"${benchmark_host}","port":5433,"dbname":"codedang_db"}
JSON

write_rows() {
  cat >"${FAKE_PSQL_OUTPUT_FILE}" <<'ROWS'
{"kind":"problem","id":568,"title":"A+B","time_limit_ms":1000,"memory_limit_mb":256,"difficulty":"Bronze","source":"synthetic"}
{"kind":"problem","id":569,"title":"A-B","time_limit_ms":1000,"memory_limit_mb":256,"difficulty":"Bronze","source":"synthetic"}
{"kind":"problem","id":570,"title":"A*B","time_limit_ms":2000,"memory_limit_mb":512,"difficulty":"Silver","source":"synthetic"}
{"kind":"testcase","problem_id":568,"testcase_id":1001,"order":1,"score_weight":1,"is_hidden":false,"is_outdated":false,"input":"ALPHA-IN\n","output":"ALPHA-OUT\n"}
{"kind":"testcase","problem_id":568,"testcase_id":1002,"order":2,"score_weight":1,"is_hidden":true,"is_outdated":false,"input":"BETA-IN\r\n","output":"BETA-OUT\r\n"}
{"kind":"testcase","problem_id":569,"testcase_id":1003,"order":1,"score_weight":1,"is_hidden":false,"is_outdated":false,"input":"GAMMA-IN\n","output":"GAMMA-OUT\n"}
{"kind":"testcase","problem_id":570,"testcase_id":1004,"order":1,"score_weight":1,"is_hidden":true,"is_outdated":false,"input":"DELTA-IN\n","output":""}
{"kind":"testcase","problem_id":568,"testcase_id":1005,"order":3,"score_weight":1,"is_hidden":false,"is_outdated":true,"input":"1\n","output":"1\n"}
{"kind":"testcase","problem_id":569,"testcase_id":1006,"order":2,"score_weight":1,"is_hidden":false,"is_outdated":false,"input":null,"output":"1\n"}
ROWS
}
write_rows

failures=0
pass() { printf 'ok: %s\n' "$*"; }
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
assert_eq() {
  if [ "$1" = "$2" ]; then pass "$3"; else fail "$3 (expected [$1] got [$2])"; fi
}
assert_contains() {
  if grep -Fq -- "$1" <<<"$2"; then pass "$3"; else fail "$3 (missing [$1])"; fi
}
assert_not_contains() {
  if grep -Fq -- "$1" <<<"$2"; then fail "$3 (found [$1])"; else pass "$3"; fi
}
assert_status() {
  if [ "${STATUS}" -eq "$1" ]; then pass "$2"; else fail "$2 (status ${STATUS}, expected $1)"; fi
}

OUT=""
ERR=""
STATUS=0
capture() {
  OUT="$("$@" 2>"${tmp}/err.txt")"
  STATUS=$?
  ERR="$(cat "${tmp}/err.txt")"
}

# --- SQL guardrail -----------------------------------------------------------

if grep -Fq 'SET default_transaction_read_only = on;' "${sql_file}" &&
  grep -Fq 'is_outdated = false' "${sql_file}" &&
  grep -Fq 'IN (568, 569, 570)' "${sql_file}"; then
  pass "export SQL forces read-only and selects only the approved problems"
else
  fail "export SQL is missing the read-only or problem guardrails"
fi

# --- Extract happy path ------------------------------------------------------

: >"${FAKE_AWS_LOG}"
: >"${FAKE_PSQL_ARGS_LOG}"
: >"${FAKE_PSQL_PASSWORD_LOG}"
out="${tmp}/out"
capture "${export_script}" --out "${out}" --psql "${fake_psql}" --region ap-northeast-2
assert_status 0 "extract succeeds"
assert_eq "extract" "$(jq -r '.mode' <<<"${OUT}")" "receipt reports extract mode"
assert_eq "4" "$(jq -r '.testcase_count' <<<"${OUT}")" "receipt counts four exported testcases"
assert_eq "8" "$(jq -r '.object_count' <<<"${OUT}")" "receipt counts eight objects"
assert_eq "2" "$(jq -r '.skipped_count' <<<"${OUT}")" "receipt counts skipped testcases"

assert_eq "1" "$(grep -c 'secretsmanager get-secret-value' "${FAKE_AWS_LOG}")" \
  "extract reads exactly one secret from Secrets Manager"
assert_not_contains "master-db" "$(cat "${FAKE_AWS_LOG}")" "extract never reads the master secret"
assert_eq "${readonly_password}" "$(cat "${FAKE_PSQL_PASSWORD_LOG}")" \
  "password is delivered through PGPASSWORD"
assert_not_contains "${readonly_password}" "$(cat "${FAKE_PSQL_ARGS_LOG}")" \
  "password is never a psql argument"
assert_contains "-h ${benchmark_host}" "$(cat "${FAKE_PSQL_ARGS_LOG}")" \
  "psql targets the benchmark host"
assert_contains "export-problem-fixtures.sql" "$(cat "${FAKE_PSQL_ARGS_LOG}")" \
  "psql runs the read-only export SQL file"

assert_eq "ALPHA-IN" "$(cat "${out}/568/1001.in")" "input uses the database testcase id as filename"
assert_eq "BETA-IN" "$(cat "${out}/568/1002.in")" "CR characters are removed deterministically"
assert_eq "BETA-OUT" "$(cat "${out}/568/1002.out")" "CRLF output is normalized"
if ! grep -q $'\r' "${out}/568/1002.in"; then pass "sanitized input has no CR"; else fail "sanitized input contains CR"; fi
assert_eq "GAMMA-IN" "$(cat "${out}/569/1003.in")" "second problem fixture is written"
if [ ! -s "${out}/570/1004.out" ]; then pass "empty output is preserved"; else fail "empty output was altered"; fi
if [ ! -e "${out}/568/1005.in" ]; then pass "outdated testcase is not exported"; else fail "outdated testcase was exported"; fi
if [ ! -e "${out}/569/1006.in" ]; then pass "null inline testcase is skipped"; else fail "null inline testcase was exported"; fi

assert_eq "true" "$(jq -r '.objects[] | select(.s3_key == "568/1002.in") | .hidden' "${out}/metadata.json")" \
  "hidden flag is preserved for a hidden testcase"
assert_eq "true" "$(jq -r '.objects[] | select(.s3_key == "570/1004.out") | .hidden' "${out}/metadata.json")" \
  "hidden flag is preserved on the output object"
assert_eq "false" "$(jq -r '.objects[] | select(.s3_key == "568/1001.in") | .hidden' "${out}/metadata.json")" \
  "non-hidden flag is preserved"
assert_eq "8" "$(jq '.objects | length' "${out}/metadata.json")" "metadata lists all objects"
assert_eq "1002" "$(jq -r '.testcases[] | select(.problem_id == "568" and .hidden == true) | .testcase_id' "${out}/metadata.json")" \
  "metadata records the database testcase id and hidden flag"
assert_eq "7" "$(jq -r '.objects[] | select(.s3_key == "568/1002.in") | keys | length' "${out}/metadata.json")" \
  "metadata objects match the manifest builder object shape"

assert_not_contains "testreadonlypassword123" "${OUT}${ERR}" "no credential is printed"
assert_not_contains "ALPHA-IN" "${OUT}${ERR}" "testcase input is not printed"
assert_not_contains "BETA-OUT" "${OUT}${ERR}" "sanitized content is not printed"
assert_not_contains "GAMMA-IN" "${OUT}${ERR}" "testcase input is not printed from the stream"
assert_not_contains "ALPHA-OUT" "${OUT}${ERR}" "testcase output is not printed"

# --- Validate mode -----------------------------------------------------------

capture "${export_script}" --validate --fixtures-dir "${out}" --metadata "${out}/metadata.json"
assert_status 0 "validate succeeds on extracted data"
assert_eq "8" "$(jq -r '.object_count' <<<"${OUT}")" "validate counts eight objects"
assert_eq "true" "$(jq -r '.objects[] | select(.s3_key == "568/1002.in") | .hidden' "${out}/metadata.json")" \
  "validate rewrites metadata preserving hidden flags"

cp -r "${out}" "${tmp}/out_tampered"
printf 'tampered\n' >>"${tmp}/out_tampered/568/1001.in"
capture "${export_script}" --validate --fixtures-dir "${tmp}/out_tampered" --metadata "${tmp}/out_tampered/metadata.json"
if [ "${STATUS}" -ne 0 ]; then pass "validate rejects a tampered fixture"; else fail "validate accepted a tampered fixture"; fi

cp -r "${out}" "${tmp}/out_meta_missing"
jq 'del(.objects[0])' "${tmp}/out_meta_missing/metadata.json" >"${tmp}/meta.tmp"
mv "${tmp}/meta.tmp" "${tmp}/out_meta_missing/metadata.json"
capture "${export_script}" --validate --fixtures-dir "${tmp}/out_meta_missing" --metadata "${tmp}/out_meta_missing/metadata.json"
if [ "${STATUS}" -ne 0 ]; then pass "validate rejects a fixture missing from metadata"; else fail "validate accepted unlisted fixture"; fi

cp -r "${out}" "${tmp}/out_symlink"
ln -s "${tmp}/out_symlink/568/1001.in" "${tmp}/out_symlink/568/9999.in"
capture "${export_script}" --validate --fixtures-dir "${tmp}/out_symlink" --metadata "${tmp}/out_symlink/metadata.json"
if [ "${STATUS}" -ne 0 ]; then pass "validate refuses a symlink in the fixture tree"; else fail "validate accepted a symlink"; fi

# --- Refusals ----------------------------------------------------------------

: >"${FAKE_AWS_LOG}"
: >"${FAKE_PSQL_ARGS_LOG}"
capture "${export_script}" --out "${tmp}/refuse_secret" --secret-id "prod/readonly-db" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract refuses a non-benchmark secret id"; else fail "extract accepted a non-benchmark secret id"; fi
assert_eq "0" "$(grep -c . "${FAKE_AWS_LOG}" || true)" "refused secret id makes no AWS call"
assert_eq "0" "$(grep -c . "${FAKE_PSQL_ARGS_LOG}" || true)" "refused secret id makes no psql call"

cat >"${tmp}/prod.json" <<'JSON'
{"username":"benchmark_ro","password":"testreadonlypassword123","host":"prod-cluster.example.internal","port":5433,"dbname":"codedang_db"}
JSON
saved_ro="${FAKE_RO_SECRET_FILE}"
export FAKE_RO_SECRET_FILE="${tmp}/prod.json"
: >"${FAKE_PSQL_ARGS_LOG}"
capture "${export_script}" --out "${tmp}/refuse_host" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract refuses a non-benchmark host"; else fail "extract accepted a non-benchmark host"; fi
assert_eq "0" "$(grep -c . "${FAKE_PSQL_ARGS_LOG}" || true)" "refused host makes no psql call"
export FAKE_RO_SECRET_FILE="${saved_ro}"

: >"${FAKE_PSQL_ARGS_LOG}"
capture "${export_script}" --out "${tmp}/refuse_host_override" --host "prod-cluster.example.internal" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract refuses a non-benchmark host override"; else fail "extract accepted a non-benchmark host override"; fi
assert_eq "0" "$(grep -c . "${FAKE_PSQL_ARGS_LOG}" || true)" "refused host override makes no psql call"

saved_identifier="${IRIS_BENCHMARK_RDS_IDENTIFIER}"
export IRIS_BENCHMARK_RDS_IDENTIFIER="prod-db"
capture "${export_script}" --out "${tmp}/refuse_identifier" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract refuses a non-benchmark identifier"; else fail "extract accepted a non-benchmark identifier"; fi
export IRIS_BENCHMARK_RDS_IDENTIFIER="${saved_identifier}"

capture "${export_script}" --out "${tmp}/../escape" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract refuses path traversal"; else fail "extract accepted path traversal"; fi

ln -s "${out}" "${tmp}/out_link"
capture "${export_script}" --out "${tmp}/out_link" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract refuses a symlinked destination"; else fail "extract accepted a symlinked destination"; fi

capture "${export_script}" --out "${out}" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract refuses to overwrite without --force"; else fail "extract overwrote without --force"; fi
capture "${export_script}" --out "${out}" --force --psql "${fake_psql}"
assert_status 0 "extract replaces an existing destination with --force"

capture "${export_script}" --out "${tmp}/refuse_problems" --problems "568 569" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract refuses an incomplete problem set"; else fail "extract accepted an incomplete problem set"; fi

# --- Refuse malformed data ---------------------------------------------------

cat >"${FAKE_PSQL_OUTPUT_FILE}" <<'ROWS'
{"kind":"problem","id":568,"title":"A+B"}
{"kind":"testcase","problem_id":568,"testcase_id":2001,"is_hidden":false,"is_outdated":false,"input":"1\n","output":"1\n"}
{"kind":"testcase","problem_id":568,"testcase_id":2001,"is_hidden":false,"is_outdated":false,"input":"2\n","output":"2\n"}
ROWS
capture "${export_script}" --out "${tmp}/dup" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract rejects duplicate testcase ids"; else fail "extract accepted duplicate testcase ids"; fi

cat >"${FAKE_PSQL_OUTPUT_FILE}" <<'ROWS'
{"kind":"problem","id":568,"title":"A+B"}
{"kind":"testcase","problem_id":568,"testcase_id":2002,"is_hidden":false,"is_outdated":false,"input":"1\n","output":"1\n"}
ROWS
capture "${export_script}" --out "${tmp}/missing_problem" --psql "${fake_psql}"
if [ "${STATUS}" -ne 0 ]; then pass "extract requires every approved problem specification"; else fail "extract accepted a missing problem specification"; fi

# --- Dry run -----------------------------------------------------------------

write_rows
: >"${FAKE_AWS_LOG}"
: >"${FAKE_PSQL_ARGS_LOG}"
capture "${export_script}" --dry-run --out "${tmp}/dry" --psql "${fake_psql}"
assert_status 0 "extract dry-run succeeds"
assert_eq "0" "$(grep -c . "${FAKE_AWS_LOG}" || true)" "dry-run makes no AWS calls"
assert_eq "0" "$(grep -c . "${FAKE_PSQL_ARGS_LOG}" || true)" "dry-run makes no psql calls"
if [ ! -e "${tmp}/dry" ]; then pass "dry-run writes nothing"; else fail "dry-run created output"; fi

# -----------------------------------------------------------------------------

if [ "${failures}" -ne 0 ]; then
  printf '\n%d export fixture test(s) failed\n' "${failures}" >&2
  exit 1
fi
printf '\nall export fixture tests passed\n'
