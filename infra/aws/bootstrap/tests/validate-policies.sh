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
sample_vpc_id="vpc-0123456789abcdef0"

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
  --vpc-id "${sample_vpc_id}" \
  --source-snapshot-arn "${sample_source_snapshot_arn}" >/dev/null 2>&1; then
  pass "render-only produced the policy documents"
else
  fail "render-only failed"
fi

user_policy="${rendered}/deployer-assume-role-policy.json"
trust_policy="${rendered}/terraform-role-trust-policy.json"

role_policies=()
while IFS= read -r f; do
  [ -n "${f}" ] || continue
  role_policies+=("${f}")
done < <(find "${rendered}" -maxdepth 1 -type f -name 'terraform-role-policy-*.json' | LC_ALL=C sort)

if [ "${#role_policies[@]}" -ge 2 ]; then
  pass "role permissions are split across ${#role_policies[@]} policy documents"
else
  fail "expected at least two rendered terraform-role-policy-*.json documents"
fi

for f in "${user_policy}" "${trust_policy}" "${role_policies[@]}"; do
  if jq -e . "${f}" >/dev/null 2>&1; then
    pass "valid rendered JSON: $(basename "${f}")"
  else
    fail "invalid rendered JSON: $(basename "${f}")"
  fi
done

# --- IAM policy size limit ---------------------------------------------------
# IAM rejects a customer-managed policy whose non-whitespace document exceeds
# 6144 characters. Assert every rendered document stays clear of the limit.

for f in "${role_policies[@]}"; do
  compact_size="$(jq -c . "${f}" | tr -d '[:space:]' | wc -c | tr -d '[:space:]')"
  if [ "${compact_size}" -le 6144 ]; then
    pass "policy size ${compact_size} <= 6144: $(basename "${f}")"
  else
    fail "policy size ${compact_size} exceeds 6144: $(basename "${f}")"
  fi
done

# Combine the split documents for whole-set coverage and scoping checks.
all_role_policies="${tmp}/terraform-role-policies-combined.json"
jq -s '{Version: "2012-10-17", Statement: [.[].Statement[]]}' \
  "${role_policies[@]}" >"${all_role_policies}"

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

# --- Role policies: required coverage and tight scoping ----------------------

assert_jq "${all_role_policies}" '.Version == "2012-10-17"' "role policies use the IAM policy version"
assert_jq "${all_role_policies}" '.Statement | length > 0' "role policies have statements"

role_actions="$(jq -r '[.Statement[] | .Action] | flatten | unique[]' "${all_role_policies}")"
required_actions=(
  "s3:GetObject"
  "s3:PutObject"
  "s3:DeleteObject"
  "dynamodb:GetItem"
  "dynamodb:PutItem"
  "dynamodb:DeleteItem"
  "rds:DescribeDBSnapshots"
  "rds:CopyDBSnapshot"
  "rds:ListTagsForResource"
  "rds:ResetDBParameterGroup"
  "kms:CreateKey"
  "kms:CreateAlias"
  "kms:UpdateAlias"
  "kms:ListAliases"
  "kms:Decrypt"
  "secretsmanager:GetSecretValue"
  "iam:CreateRole"
  "iam:CreatePolicy"
  "iam:AttachRolePolicy"
  "iam:CreateServiceLinkedRole"
  "ec2:DescribeSecurityGroups"
  "ec2:CreateSecurityGroup"
  "s3:GetBucketCORS"
  "s3:GetBucketWebsite"
  "s3:GetBucketObjectLockConfiguration"
  "s3:DeleteBucketLifecycle"
)
for action in "${required_actions[@]}"; do
  if grep -Fxq -- "${action}" <<<"${role_actions}"; then
    pass "role policy grants required action ${action}"
  else
    fail "role policy is missing required action ${action}"
  fi
done

assert_jq "${all_role_policies}" \
  '[.Statement[] | select(.Effect == "Allow") | (.Action | if type == "array" then .[] else . end) | select(. == "*")] | length == 0' \
  "role policies have no wildcard action"

assert_jq "${all_role_policies}" '
  [ .Statement[]
    | select(.Effect == "Allow")
    | select(
        ([.Action] | flatten) as $a
        | ($a | any(. == "s3:PutObject"
                 or . == "rds:CopyDBSnapshot"
                 or . == "secretsmanager:GetSecretValue"
                 or . == "iam:CreateRole"
                 or . == "kms:CreateAlias"
                 or . == "ec2:CreateSecurityGroup"))
      )
    | select(([.Resource] | flatten) | any(. == "*"))
  ] | length == 0
' "sensitive role-policy actions are never scoped to a wildcard resource"

# --- Provider CRUD/read coverage and narrow scoping --------------------------

# KMS CreateAlias and UpdateAlias authorize against both the alias and the
# target key, so both resource types must be granted.
assert_jq "${all_role_policies}" '
  [ .Statement[]
    | select(.Effect == "Allow")
    | select(([.Action] | flatten) | index("kms:CreateAlias"))
    | ([.Resource] | flatten)[]
  ] as $r
  | (($r | map(select(test(":alias/"))) | length > 0)
     and ($r | map(select(test(":key/"))) | length > 0))
' "kms:CreateAlias covers both the alias and the key"

# The key-side CreateAlias/UpdateAlias grant stays constrained to the
# benchmark Project tag.
assert_jq "${all_role_policies}" '
  [ .Statement[]
    | select(.Effect == "Allow")
    | select(([.Action] | flatten) | any(. == "kms:CreateAlias" or . == "kms:UpdateAlias"))
    | select(([.Resource] | flatten) | any(test(":key/")))
    | select(((.Condition // {})["StringEquals"]["aws:ResourceTag/Project"]) == "'"${sample_prefix}"'")
  ] | length > 0
' "kms alias writes on the key require the benchmark Project tag"

# aws_kms_alias reads call ListAliases without a filter, which cannot be
# resource-scoped.
assert_jq "${all_role_policies}" '
  [ .Statement[]
    | select(.Effect == "Allow")
    | select(([.Action] | flatten) | index("kms:ListAliases"))
    | select(([.Resource] | flatten) | index("*"))
  ] | length > 0
' "kms:ListAliases is granted on the account-wide resource"

# RDS tag reads are needed for the instance, parameter group, subnet group,
# and snapshots.
assert_jq "${all_role_policies}" '
  [ .Statement[]
    | select(.Effect == "Allow")
    | select(([.Action] | flatten) | index("rds:ListTagsForResource"))
    | ([.Resource] | flatten)[]
  ] as $r
  | (($r | map(select(test(":db:"))) | length > 0)
     and ($r | map(select(test(":pg:"))) | length > 0)
     and ($r | map(select(test(":subgrp:"))) | length > 0)
     and ($r | map(select(test(":snapshot:"))) | length > 0))
' "rds:ListTagsForResource covers instances, parameter groups, subnet groups, and snapshots"

# S3 bucket read/config actions stay on the dedicated bucket only.
assert_jq "${all_role_policies}" '
  [ .Statement[]
    | select(.Effect == "Allow")
    | select(([.Action] | flatten) | any(. == "s3:GetBucketCORS"
                                        or . == "s3:GetBucketWebsite"
                                        or . == "s3:GetBucketObjectLockConfiguration"
                                        or . == "s3:DeleteBucketLifecycle"
                                        or . == "s3:GetReplicationConfiguration"
                                        or . == "s3:GetAccelerateConfiguration"))
    | ([.Resource] | flatten)[]
    | select(test("^arn:aws:s3:::'"${sample_prefix}"'-testcases$") | not)
  ] | length == 0
' "S3 bucket read/config actions are scoped to the benchmark bucket"

# ec2:CreateSecurityGroup authorizes against the VPC as well as the security
# group, so the benchmark VPC ARN must be granted explicitly and not widened.
assert_jq "${all_role_policies}" '
  [ .Statement[]
    | select(.Effect == "Allow")
    | select(([.Action] | flatten) | index("ec2:CreateSecurityGroup"))
    | ([.Resource] | flatten)[]
  ] | index("arn:aws:ec2:'"${sample_region}"':'"${sample_account}"':vpc/'"${sample_vpc_id}"'") != null
' "ec2:CreateSecurityGroup is scoped to the benchmark VPC ARN"

assert_jq "${all_role_policies}" '
  [ .Statement[]
    | select(.Effect == "Allow")
    | select(([.Action] | flatten) | index("ec2:CreateSecurityGroup"))
    | ([.Resource] | flatten)[]
    | select(. == "*" or test(":vpc/\\*$"))
  ] | length == 0
' "ec2:CreateSecurityGroup is not scoped to a wildcard VPC"

rendered_text="$(cat "${all_role_policies}")"
for needle in \
  "${sample_state_key}" \
  "${sample_lock_table}" \
  "${sample_source_snapshot_arn}" \
  "${sample_vpc_id}" \
  "${sample_prefix}"; do
  if grep -Fq -- "${needle}" <<<"${rendered_text}"; then
    pass "role policies reference ${needle}"
  else
    fail "role policies are missing ${needle}"
  fi
done

if grep -Eq '__[A-Z_]+__' "${role_policies[@]}"; then
  fail "role policies have unrendered placeholders"
else
  pass "role policies have no unrendered placeholders"
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
