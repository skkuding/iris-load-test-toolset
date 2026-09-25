#!/usr/bin/env bash
#
# Fake-AWS tests for scripts/aws/bootstrap-deployer.sh.
#
# These tests never contact AWS and never create real credentials. They use the
# stateful fake CLI in tests/fake-aws/aws and assert on simulated IAM state and
# on the recorded invocation log.
#
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../../.." && pwd)"
bootstrap="${repo_root}/scripts/aws/bootstrap-deployer.sh"
fake_bin="${script_dir}/fake-aws"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

export PATH="${fake_bin}:${PATH}"
export FAKE_AWS_STATE_DIR="${tmp}/state"
export FAKE_AWS_LOG="${tmp}/aws.log"
export FAKE_AWS_ACCOUNT="219857217698"
export IRIS_BENCHMARK_VPC_ID="vpc-0123456789abcdef0"
export FAKE_ACCESS_KEY_SECRET="FakeSecretValue/ABC123"
export AWS_SHARED_CREDENTIALS_FILE="${tmp}/credentials"
export AWS_CONFIG_FILE="${tmp}/config"
mkdir -p "${FAKE_AWS_STATE_DIR}"
: >"${FAKE_AWS_LOG}"

chmod +x "${fake_bin}/aws"

account="219857217698"
prefix="codedang-iris-benchmark"
deployer="${prefix}-deployer"
role="${prefix}-terraform"
policy_suffixes=(backend data iam-network)
policy_names=()
policy_arns=()
for suffix in "${policy_suffixes[@]}"; do
  policy_names+=("${prefix}-terraform-permissions-${suffix}")
  policy_arns+=("arn:aws:iam::${account}:policy/${prefix}-terraform-permissions-${suffix}")
done
role_policy_count="${#policy_names[@]}"

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
count_log() { [ -f "${FAKE_AWS_LOG}" ] && grep -c -- "$1" "${FAKE_AWS_LOG}" || true; }

# --- Dry-run makes no AWS calls ----------------------------------------------

: >"${FAKE_AWS_LOG}"
run_out="$("${bootstrap}" --dry-run --account-id "${account}" 2>&1)"
assert_contains "dry-run: no AWS calls" "${run_out}" "dry-run announces no AWS calls"
assert_contains "deployer user  = ${deployer}" "${run_out}" "dry-run resolves the deployer user"
assert_contains "vpc id         = ${IRIS_BENCHMARK_VPC_ID}" "${run_out}" "dry-run resolves the benchmark VPC"
assert_eq "0" "$(count_log .)" "dry-run makes zero AWS calls"

# --- First bootstrap run (no access key) -------------------------------------

: >"${FAKE_AWS_LOG}"
run_out="$("${bootstrap}" --yes --admin-profile bootstrap-admin 2>&1)"
assert_contains "bootstrap complete" "${run_out}" "first run completes"
assert_contains "simulation; no access key created" "${run_out}" "first run verifies by simulation"
assert_eq "1" "$(count_log 'sts get-caller-identity')" "caller identity resolved once"
assert_eq "1" "$(count_log 'iam create-user')" "user created once"
assert_eq "1" "$(count_log 'iam create-role')" "role created once"
assert_eq "${role_policy_count}" "$(count_log 'iam create-policy ')" "customer-managed policies created once each"
assert_eq "1" "$(count_log 'iam put-user-policy')" "user inline policy written once"
assert_eq "${role_policy_count}" "$(count_log 'iam attach-role-policy')" "role policies attached once each"
assert_eq "1" "$(count_log 'iam simulate-principal-policy')" "assume-role simulated"
assert_eq "0" "$(count_log 'iam create-access-key')" "no access key created by default"

# --- State assertions --------------------------------------------------------

if [ -f "${FAKE_AWS_STATE_DIR}/user.json" ]; then
  pass "deployer user exists in fake state"
else
  fail "deployer user missing in fake state"
fi

assert_eq "1" "$(jq -r 'length' "${FAKE_AWS_STATE_DIR}/inline-user.json")" "user has exactly one inline policy"
assert_eq "${prefix}-assume-terraform" \
  "$(jq -r 'keys[0]' "${FAKE_AWS_STATE_DIR}/inline-user.json")" \
  "user inline policy has the expected name"
assert_eq "1" \
  "$(jq -r '.["'"${prefix}"'-assume-terraform"].Statement | length' "${FAKE_AWS_STATE_DIR}/inline-user.json")" \
  "user policy has a single statement"
assert_eq "sts:AssumeRole" \
  "$(jq -r '.["'"${prefix}"'-assume-terraform"].Statement[0].Action[0]' "${FAKE_AWS_STATE_DIR}/inline-user.json")" \
  "user policy grants only sts:AssumeRole"
assert_eq "arn:aws:iam::${account}:role/${role}" \
  "$(jq -r '.["'"${prefix}"'-assume-terraform"].Statement[0].Resource[0]' "${FAKE_AWS_STATE_DIR}/inline-user.json")" \
  "user policy scoped to the terraform role"

assert_eq "arn:aws:iam::${account}:user/${deployer}" \
  "$(jq -r '.AssumeRolePolicyDocument.Statement[0].Principal.AWS' "${FAKE_AWS_STATE_DIR}/role.json")" \
  "role trusts only the deployer user"
assert_eq "sts:AssumeRole" \
  "$(jq -r '.AssumeRolePolicyDocument.Statement[0].Action' "${FAKE_AWS_STATE_DIR}/role.json")" \
  "role trust allows only sts:AssumeRole"

assert_eq "${role_policy_count}" \
  "$(jq -r 'length' "${FAKE_AWS_STATE_DIR}/attached-role.json")" \
  "every customer-managed policy attached to the role"
for policy_arn in "${policy_arns[@]}"; do
  assert_eq "1" \
    "$(jq -r --arg a "${policy_arn}" '[.[] | select(. == $a)] | length' "${FAKE_AWS_STATE_DIR}/attached-role.json")" \
    "attached policy: ${policy_arn##*/}"
done

# The customer-managed policies must be the only place Terraform permissions
# live and must cover the required tight scopes.
role_policy_doc="$(jq -c '[.[] | .Document]' "${FAKE_AWS_STATE_DIR}/policies.json")"
assert_contains '"TerraformBackendStateObject"' "${role_policy_doc}" "state object statement present"
assert_contains "${prefix}" "${role_policy_doc}" "policy is benchmark-prefix scoped"
assert_contains '"s3:GetBucketCORS"' "${role_policy_doc}" "S3 bucket CORS read granted"
assert_contains '"s3:GetBucketObjectLockConfiguration"' "${role_policy_doc}" "S3 object-lock read granted"
assert_contains '"rds:ListTagsForResource"' "${role_policy_doc}" "RDS tag reads granted"
assert_contains '"rds:ResetDBParameterGroup"' "${role_policy_doc}" "RDS parameter reset granted"
assert_contains '"kms:CreateAlias"' "${role_policy_doc}" "KMS alias create granted"
assert_contains '"kms:ListAliases"' "${role_policy_doc}" "KMS alias list granted"
assert_contains '"ec2:CreateSecurityGroup"' "${role_policy_doc}" "EC2 security group create granted"
assert_contains "${IRIS_BENCHMARK_VPC_ID}" "${role_policy_doc}" "EC2 security group is scoped to the configured VPC"
if grep -Fq '"Action":["*"]' <<<"${role_policy_doc}" || grep -Fq '"Action": "*"' <<<"${role_policy_doc}"; then
  fail "role policy contains a wildcard action"
else
  pass "role policy has no wildcard action"
fi
assert_not_contains "__" "${role_policy_doc}" "role policy has no unrendered placeholders"

# --- Second run is idempotent -------------------------------------------------

: >"${FAKE_AWS_LOG}"
run_out="$("${bootstrap}" --yes --admin-profile bootstrap-admin 2>&1)"
assert_contains "customer-managed policy is current" "${run_out}" "second run reports the policies are current"
assert_eq "0" "$(count_log 'iam create-user')" "second run does not recreate the user"
assert_eq "0" "$(count_log 'iam create-role')" "second run does not recreate the role"
assert_eq "0" "$(count_log 'iam create-policy ')" "second run does not recreate the policies"
assert_eq "0" "$(count_log 'iam create-policy-version')" "second run does not add a policy version"
assert_eq "0" "$(count_log 'iam attach-role-policy')" "second run does not re-attach the policies"

# --- Exclusivity enforcement -------------------------------------------------

[ -f "${FAKE_AWS_STATE_DIR}/attached-user.json" ] || echo '[]' >"${FAKE_AWS_STATE_DIR}/attached-user.json"
jq '. + ["arn:aws:iam::'"${account}"':policy/ForeignAdmin"]' \
  "${FAKE_AWS_STATE_DIR}/attached-user.json" >"${tmp}/au.json"
mv "${tmp}/au.json" "${FAKE_AWS_STATE_DIR}/attached-user.json"
jq '. + {"ForeignInline":{"Version":"2012-10-17","Statement":[]}}' \
  "${FAKE_AWS_STATE_DIR}/inline-user.json" >"${tmp}/iu.json"
mv "${tmp}/iu.json" "${FAKE_AWS_STATE_DIR}/inline-user.json"
[ -f "${FAKE_AWS_STATE_DIR}/attached-role.json" ] || echo '[]' >"${FAKE_AWS_STATE_DIR}/attached-role.json"
jq '. + ["arn:aws:iam::'"${account}"':policy/ForeignRolePolicy"]' \
  "${FAKE_AWS_STATE_DIR}/attached-role.json" >"${tmp}/ar.json"
mv "${tmp}/ar.json" "${FAKE_AWS_STATE_DIR}/attached-role.json"
[ -f "${FAKE_AWS_STATE_DIR}/inline-role.json" ] || echo '{}' >"${FAKE_AWS_STATE_DIR}/inline-role.json"
jq '. + {"ForeignRoleInline":{"Version":"2012-10-17","Statement":[]}}' \
  "${FAKE_AWS_STATE_DIR}/inline-role.json" >"${tmp}/ir.json"
mv "${tmp}/ir.json" "${FAKE_AWS_STATE_DIR}/inline-role.json"

: >"${FAKE_AWS_LOG}"
run_out="$("${bootstrap}" --yes --admin-profile bootstrap-admin 2>&1)"
assert_eq "1" "$(count_log 'iam detach-user-policy')" "foreign managed user policy detached"
assert_eq "1" "$(count_log 'iam delete-user-policy')" "foreign inline user policy removed"
assert_eq "1" "$(count_log 'iam detach-role-policy')" "foreign managed role policy detached"
assert_eq "1" "$(count_log 'iam delete-role-policy')" "foreign inline role policy removed"
assert_eq "0" \
  "$(jq -r 'map(select(test("Foreign")))|length' "${FAKE_AWS_STATE_DIR}/attached-user.json")" \
  "no foreign managed user policy remains"
assert_eq "0" \
  "$(jq -r 'map(select(test("Foreign")))|length' "${FAKE_AWS_STATE_DIR}/attached-role.json")" \
  "no foreign managed role policy remains"
for policy_arn in "${policy_arns[@]}"; do
  assert_eq "1" \
    "$(jq -r --arg a "${policy_arn}" '[.[] | select(. == $a)] | length' "${FAKE_AWS_STATE_DIR}/attached-role.json")" \
    "wanted policy retained: ${policy_arn##*/}"
done
assert_eq "0" \
  "$(jq -r 'length' "${FAKE_AWS_STATE_DIR}/inline-role.json")" \
  "no foreign inline role policy remains"

# --- Recovery from a partially completed bootstrap ---------------------------
# Simulate the failure this refactor fixes: the user, role, and trust policy
# already exist but the role has no customer-managed policies because the single
# oversized CreatePolicy call failed. A retry must converge without recreating
# the user or role.

rm -f "${FAKE_AWS_STATE_DIR}/policies.json"
echo '[]' >"${FAKE_AWS_STATE_DIR}/attached-role.json"
echo '{}' >"${FAKE_AWS_STATE_DIR}/inline-role.json"
: >"${FAKE_AWS_LOG}"
run_out="$("${bootstrap}" --yes --admin-profile bootstrap-admin 2>&1)"
assert_contains "bootstrap complete" "${run_out}" "retry after partial bootstrap completes"
assert_eq "0" "$(count_log 'iam create-user')" "retry does not recreate the existing user"
assert_eq "0" "$(count_log 'iam create-role')" "retry does not recreate the existing role"
assert_eq "${role_policy_count}" "$(count_log 'iam create-policy ')" "retry creates every missing policy"
assert_eq "${role_policy_count}" "$(count_log 'iam attach-role-policy')" "retry attaches every missing policy"
assert_eq "${role_policy_count}" \
  "$(jq -r 'length' "${FAKE_AWS_STATE_DIR}/attached-role.json")" \
  "retry leaves all policies attached"
assert_eq "0" \
  "$(jq -r 'map(select(test("Foreign")))|length' "${FAKE_AWS_STATE_DIR}/attached-role.json")" \
  "retry attaches no foreign policy"

# --- Access key creation, no-print, fail-safe, rotation ----------------------

: >"${FAKE_AWS_LOG}"
: >"${AWS_SHARED_CREDENTIALS_FILE}"
run_out="$("${bootstrap}" --yes --admin-profile bootstrap-admin --create-access-key 2>&1)"
assert_contains "role assumption verified" "${run_out}" "access key run verifies role assumption"
assert_eq "1" "$(count_log 'iam create-access-key')" "access key created once"
assert_eq "1" "$(count_log 'configure import')" "credentials imported into the local profile"
assert_eq "1" "$(count_log 'sts assume-role')" "role assumption exercised end to end"
assert_not_contains "${FAKE_ACCESS_KEY_SECRET}" "${run_out}" "secret is never printed to the terminal"
assert_not_contains "${FAKE_ACCESS_KEY_SECRET}" "$(cat "${FAKE_AWS_LOG}")" "secret never appears in an AWS CLI invocation"
if grep -Fq "aws_secret_access_key = ${FAKE_ACCESS_KEY_SECRET}" "${AWS_SHARED_CREDENTIALS_FILE}"; then
  pass "secret is stored only in the local credentials file"
else
  fail "expected secret in the local credentials file"
fi

: >"${FAKE_AWS_LOG}"
if run_out="$("${bootstrap}" --yes --admin-profile bootstrap-admin --create-access-key 2>&1)"; then
  fail "second access-key run should fail when an active key exists"
else
  pass "second access-key run fails safely"
fi
assert_contains "active access key" "${run_out}" "failure explains the existing active key"
assert_eq "0" "$(count_log 'iam create-access-key')" "no new key created while an active key exists"
assert_eq "0" "$(count_log 'iam delete-access-key')" "fail-safe path deletes nothing"

: >"${FAKE_AWS_LOG}"
run_out="$("${bootstrap}" --yes --admin-profile bootstrap-admin --create-access-key --rotate-access-key 2>&1)"
assert_contains "role assumption verified" "${run_out}" "rotation verifies role assumption"
assert_eq "1" "$(count_log 'iam update-access-key')" "old key deactivated during rotation"
assert_eq "1" "$(count_log 'iam delete-access-key')" "old key deleted during rotation"
assert_eq "1" "$(count_log 'iam create-access-key')" "replacement key created during rotation"
assert_not_contains "${FAKE_ACCESS_KEY_SECRET}" "${run_out}" "rotated secret is never printed"

# --- Option handling ---------------------------------------------------------

if "${bootstrap}" --definitely-not-an-option >/dev/null 2>&1; then
  fail "unknown option should fail"
else
  pass "unknown option is rejected"
fi

# -----------------------------------------------------------------------------

if [ "${failures}" -ne 0 ]; then
  printf '\n%d bootstrap test(s) failed\n' "${failures}" >&2
  exit 1
fi
printf '\nall bootstrap tests passed\n'
