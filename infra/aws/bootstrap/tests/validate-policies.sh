#!/usr/bin/env bash
#
# Static validation for the bootstrap IAM policy documents.
#
# No AWS calls. The templates are rendered with sample values through
# scripts/aws/bootstrap-deployer.sh --render-only, then checked for JSON
# validity, required actions, tight scoping, and absence of wildcard actions.
#
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../../.." && pwd)"
bootstrap="${repo_root}/scripts/aws/bootstrap-deployer.sh"
policies_dir="${repo_root}/infra/aws/bootstrap/policies"

sample_account="219857217698"
sample_region="ap-northeast-2"
sample_prefix="codedang-iris-benchmark"
sample_state_bucket="codedang-tf-state"
sample_state_key="terraform/iris-benchmark.tfstate"
sample_lock_table="terraform-state-lock"
sample_source_snapshot_arn="arn:aws:rds:ap-northeast-2:219857217698:snapshot:rds:example-source-2026"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
rendered="${tmp}/rendered"

failures=0
pass() { printf 'ok: %s\n' "$*"; }
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
assert_jq() {
  local file="$1" expr="$2" msg="$3"
  if jq -e "${expr}" "${file}" >/dev/null; then
    pass "${msg}"
  else
    fail "${msg}"
  fi
}

# --- Template syntax ---------------------------------------------------------

for f in "${policies_dir}"/*.json; do
  if jq -e . "${f}" >/dev/null 2>&1; then
    pass "valid JSON template: $(basename "${f}")"
  else
    fail "invalid JSON template: $(basename "${f}")"
  fi
done

# --- Rendering ---------------------------------------------------------------

if "${bootstrap}" --render-only --output-dir "${rendered}" \
  --account-id "${sample_account}" --region "${sample_region}" \
  --name-prefix "${sample_prefix}" \
  --state-bucket "${sample_state_bucket}" \
  --state-key "${sample_state_key}" \
  --lock-table "${sample_lock_table}" \
  --source-snapshot-arn "${sample_source_snapshot_arn}" >/dev/null 2>&1; then
  pass "render-only produced the policy documents"
else
  fail "render-only failed"
fi

user_policy="${rendered}/deployer-assume-role-policy.json"
role_policy="${rendered}/terraform-role-policy.json"
trust_policy="${rendered}/terraform-role-trust-policy.json"

for f in "${user_policy}" "${role_policy}" "${trust_policy}"; do
  if jq -e . "${f}" >/dev/null 2>&1; then
    pass "valid rendered JSON: $(basename "${f}")"
  else
    fail "invalid rendered JSON: $(basename "${f}")"
  fi
done

# --- Deployer user policy: only sts:AssumeRole on the role -------------------

assert_jq "${user_policy}" '.Statement | length == 1' "user policy has exactly one statement"
assert_jq "${user_policy}" '[.Statement[].Action] | flatten | unique == ["sts:AssumeRole"]' \
  "user policy grants only sts:AssumeRole"
assert_jq "${user_policy}" \
  '[.Statement[].Resource] | flatten | all(test(":role/'"${sample_prefix}"'-terraform$"))' \
  "user policy is scoped to the terraform role"
assert_jq "${user_policy}" \
  '[.Statement[] | select(.Effect == "Allow") | (.Action | if type == "array" then .[] else . end) | select(. == "*")] | length == 0' \
  "user policy has no wildcard action"

# --- Trust policy: only the deployer user ------------------------------------

assert_jq "${trust_policy}" \
  '.Statement | length == 1 and .[0].Action == "sts:AssumeRole"' \
  "trust policy has one sts:AssumeRole statement"
assert_jq "${trust_policy}" \
  '.Statement[0].Principal.AWS == "arn:aws:iam::'"${sample_account}"':user/'"${sample_prefix}"'-deployer"' \
  "trust policy names only the deployer user"

# --- Role policy: required coverage and tight scoping ------------------------

assert_jq "${role_policy}" '.Version == "2012-10-17"' "role policy has the IAM policy version"
assert_jq "${role_policy}" '.Statement | length > 0' "role policy has statements"

role_actions="$(jq -r '[.Statement[] | .Action] | flatten | unique[]' "${role_policy}")"
required_actions=(
  "s3:GetObject"
  "s3:PutObject"
  "s3:DeleteObject"
  "dynamodb:GetItem"
  "dynamodb:PutItem"
  "dynamodb:DeleteItem"
  "rds:DescribeDBSnapshots"
  "rds:CopyDBSnapshot"
  "kms:CreateKey"
  "kms:Decrypt"
  "secretsmanager:GetSecretValue"
  "iam:CreateRole"
  "iam:CreatePolicy"
  "iam:AttachRolePolicy"
  "iam:CreateServiceLinkedRole"
  "ec2:DescribeSecurityGroups"
  "ec2:CreateSecurityGroup"
)
for action in "${required_actions[@]}"; do
  if grep -Fxq -- "${action}" <<<"${role_actions}"; then
    pass "role policy grants required action ${action}"
  else
    fail "role policy is missing required action ${action}"
  fi
done

assert_jq "${role_policy}" \
  '[.Statement[] | select(.Effect == "Allow") | (.Action | if type == "array" then .[] else . end) | select(. == "*")] | length == 0' \
  "role policy has no wildcard action"

assert_jq "${role_policy}" '
  [ .Statement[]
    | select(.Effect == "Allow")
    | select(
        ([.Action] | flatten) as $a
        | ($a | any(. == "s3:PutObject"
                 or . == "rds:CopyDBSnapshot"
                 or . == "secretsmanager:GetSecretValue"
                 or . == "iam:CreateRole"
                 or . == "ec2:CreateSecurityGroup"))
      )
    | select(([.Resource] | flatten) | any(. == "*"))
  ] | length == 0
' "sensitive role-policy actions are never scoped to a wildcard resource"

rendered_text="$(cat "${role_policy}")"
for needle in \
  "${sample_state_key}" \
  "${sample_lock_table}" \
  "${sample_source_snapshot_arn}" \
  "${sample_prefix}"; do
  if grep -Fq -- "${needle}" <<<"${rendered_text}"; then
    pass "role policy references ${needle}"
  else
    fail "role policy is missing ${needle}"
  fi
done

if grep -Eq '__[A-Z_]+__' "${rendered}"/terraform-role-policy.json; then
  fail "role policy has unrendered placeholders"
else
  pass "role policy has no unrendered placeholders"
fi

if grep -RIn -E 'AKIA[0-9A-Z]{16}|aws_secret_access_key|BEGIN [A-Z ]*PRIVATE KEY' \
  "${policies_dir}" >/dev/null 2>&1; then
  fail "policy directory contains credential-like material"
else
  pass "policy directory contains no credential material"
fi

# -----------------------------------------------------------------------------

if [ "${failures}" -ne 0 ]; then
  printf '\n%d policy check(s) failed\n' "${failures}" >&2
  exit 1
fi
printf '\nall policy checks passed\n'
