#!/usr/bin/env bash
#
# Static and configuration validation for the iris-benchmark Terraform module.
#
# Always runs safety/regression checks that need no AWS access. Unless
# --static-only is passed, it also runs `terraform init -backend=false` and
# `terraform validate`.
#
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
module_dir="$(cd "${script_dir}/.." && pwd)"
static_only="false"

if [ "${1:-}" = "--static-only" ]; then
  static_only="true"
fi

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
pass() {
  printf 'ok: %s\n' "$*"
}

# --- Static safety checks ----------------------------------------------------

if grep -RIn --include='*.tf' -E 'data[[:space:]]+"aws_db_snapshot"' "${module_dir}"; then
  fail "nondeterministic data \"aws_db_snapshot\" found; snapshot must be an input"
else
  pass "no aws_db_snapshot data source"
fi

if grep -RIn --include='*.tf' -E 'most_recent[[:space:]]*=[[:space:]]*true' "${module_dir}"; then
  fail "most_recent = true found; snapshot selection must be deterministic"
else
  pass "no most_recent selection"
fi

if grep -RIn --include='*.tf' -E 'source_db_snapshot_identifier[[:space:]]*=[[:space:]]*var\.source_snapshot_arn' "${module_dir}" >/dev/null &&
  grep -RIn --include='*.tf' -E 'snapshot_identifier[[:space:]]*=[[:space:]]*aws_db_snapshot_copy\.benchmark\.id' "${module_dir}" >/dev/null; then
  pass "RDS restore is wired through the explicit durable snapshot copy"
else
  fail "RDS restore is not wired through var.source_snapshot_arn and aws_db_snapshot_copy.benchmark"
fi

if grep -RIn --include='*.tf' -E 'deletion_protection[[:space:]]*=[[:space:]]*var\.deletion_protection' "${module_dir}" >/dev/null; then
  pass "deletion protection is variable-driven"
else
  fail "deletion_protection is not wired to var.deletion_protection"
fi

if grep -RIn -E 'AKIA[0-9A-Z]{16}|aws_secret_access_key|BEGIN [A-Z ]*PRIVATE KEY' \
  "${module_dir}" --include='*.tf' --include='*.example' --include='*.md' --include='*.hcl'; then
  fail "possible committed credential material"
else
  pass "no obvious committed credentials"
fi

if grep -RIn -E '(postgres|postgresql)://[^"[:space:]]+:[^"@[:space:]]+@' \
  "${module_dir}" --include='*.tf' --include='*.example' --include='*.md'; then
  fail "resolved secret connection string found"
else
  pass "no resolved secret connection strings"
fi

if grep -RIn --include='*.tf' -E 'testcase_problem_ids' "${module_dir}" >/dev/null; then
  pass "testcase prefixes are variable-driven"
else
  fail "testcase_problem_ids is not referenced"
fi

# --- KMS key policy ----------------------------------------------------------

kms_tf="${module_dir}/kms.tf"
rds_tf="${module_dir}/rds.tf"

if grep -Eq 'policy[[:space:]]*=[[:space:]]*data\.aws_iam_policy_document\.benchmark_kms\.json' "${kms_tf}"; then
  pass "benchmark key has an explicit key policy"
else
  fail "benchmark key policy is not wired to data.aws_iam_policy_document.benchmark_kms"
fi

if grep -Fq 'EnableIamUserPermissions' "${kms_tf}" &&
  grep -Fq 'kms:*' "${kms_tf}" &&
  grep -Fq ':root' "${kms_tf}"; then
  pass "key policy preserves account-root IAM delegation"
else
  fail "key policy must keep the account-root IAM delegation statement"
fi

for via_service in rds secretsmanager s3; do
  if grep -Fq 'kms:ViaService' "${kms_tf}" &&
    grep -Fq "${via_service}.\${var.region}.amazonaws.com" "${kms_tf}"; then
    pass "key policy scopes ${via_service} use with kms:ViaService"
  else
    fail "key policy is missing the ${via_service} kms:ViaService condition"
  fi
done

if grep -Fq 'kms:GrantIsForAWSResource' "${kms_tf}"; then
  pass "key policy limits service grants with kms:GrantIsForAWSResource"
else
  fail "key policy must condition kms:CreateGrant with kms:GrantIsForAWSResource"
fi

if grep -Eq 'type[[:space:]]*=[[:space:]]*"Service"' "${kms_tf}"; then
  fail "key policy uses broad service-principal grants unsupported by AWS docs"
else
  pass "key policy uses no service-principal grants"
fi

if grep -Fq 'master_user_secret_kms_key_id = aws_kms_key.benchmark.arn' "${rds_tf}" &&
  grep -Fq 'performance_insights_kms_key_id = var.performance_insights_enabled ? aws_kms_key.benchmark.arn : null' "${rds_tf}"; then
  pass "RDS managed secret and Performance Insights use the benchmark key"
else
  fail "RDS must encrypt the managed master secret and Performance Insights with the benchmark key"
fi

# --- Terraform CLI checks ----------------------------------------------------

if command -v terraform >/dev/null 2>&1; then
  if (cd "${module_dir}" && terraform fmt -check -recursive); then
    pass "terraform fmt -check"
  else
    fail "terraform fmt -check"
  fi

  if [ "${static_only}" = "true" ]; then
    printf 'skip: terraform init/validate (--static-only)\n'
  else
    if (cd "${module_dir}" && terraform init -backend=false -input=false -no-color >/dev/null); then
      pass "terraform init -backend=false"
      if (cd "${module_dir}" && terraform validate -no-color); then
        pass "terraform validate"
      else
        fail "terraform validate"
      fi
    else
      fail "terraform init -backend=false (network or provider access required)"
    fi
  fi
else
  printf 'skip: terraform not on PATH\n'
fi

if [ "${failures}" -ne 0 ]; then
  printf '\n%d check(s) failed\n' "${failures}" >&2
  exit 1
fi
printf '\nall terraform checks passed\n'
