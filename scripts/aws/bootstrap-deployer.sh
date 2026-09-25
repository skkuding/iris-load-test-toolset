#!/usr/bin/env bash
#
# Bootstrap the codedang-iris-benchmark Terraform deployer identity.
#
# Run this once with an existing administrative AWS profile. It is idempotent
# and converges the following dedicated resources:
#
#   * IAM user   codedang-iris-benchmark-deployer
#       - the user is granted ONLY sts:AssumeRole on the Terraform role;
#   * IAM role   codedang-iris-benchmark-terraform
#       - trusts only that user;
#       - carries a set of customer-managed policies with every Terraform
#         permission, split so each stays under the IAM 6144-character limit.
#
# The user never receives direct Terraform permissions. All Terraform access
# lives in the customer-managed policies attached to the role.
#
# Optional access key handling:
#   --create-access-key   Create an access key for the user and import it into a
#                         local named AWS profile. The secret is written only to
#                         the local AWS credentials file (or
#                         $AWS_SHARED_CREDENTIALS_FILE). It is never printed,
#                         never written to this repository, and never stored in
#                         Terraform state. The script fails safely if an active
#                         access key already exists.
#   --rotate-access-key   With --create-access-key, deactivate and delete the
#                         user's existing access keys first (destructive).
#
# The script then verifies role assumption end to end when a key was created,
# or with iam:SimulatePrincipalPolicy against the user otherwise.
#
# Usage:
#   scripts/aws/bootstrap-deployer.sh [options]
#
# Options:
#   --admin-profile NAME      Existing administrative profile used to bootstrap
#                             (default: $IRIS_BENCHMARK_ADMIN_PROFILE, else the
#                             default credential chain).
#   --deployer-user NAME      IAM user to create (default:
#                             ${name_prefix}-deployer).
#   --terraform-role NAME     IAM role to create (default:
#                             ${name_prefix}-terraform).
#   --local-profile NAME      Local AWS profile for the created access key
#   --role-profile NAME       Local profile that assumes the Terraform role
#                             (default: the deployer user name).
#   --region REGION           AWS region (default: $IRIS_BENCHMARK_AWS_REGION or
#                             ap-northeast-2).
#   --name-prefix PREFIX      Resource/tag prefix (default:
#                             codedang-iris-benchmark).
#   --state-bucket NAME       Terraform S3 state bucket (default:
#                             codedang-tf-state).
#   --state-key KEY           Terraform state key (default:
#                             terraform/iris-benchmark.tfstate).
#   --vpc-state-key KEY       VPC remote state key (default:
#                             terraform/vpc.tfstate).
#   --lock-table NAME         Terraform state lock table (default:
#                             terraform-state-lock).
#   --source-snapshot-arn ARN Explicit source RDS snapshot ARN
#                             (default: $IRIS_BENCHMARK_SOURCE_SNAPSHOT_ARN).
#   --create-access-key       Create a key and configure the local profile.
#   --rotate-access-key       With --create-access-key, delete active keys
#                             first. Without this flag an existing active key
#                             aborts the script.
#   --no-enforce-exclusive    Do not detach foreign policies from the dedicated
#                             user/role (default: enforce that they carry only
#                             the bootstrap policies).
#   --account-id ID           Account id used for rendering in --dry-run or
#                             --render-only (otherwise resolved from AWS).
#   --render-only             Render the policy documents to --output-dir and
#                             exit. Makes no AWS calls.
#   --output-dir DIR          Destination directory for --render-only.
#   --dry-run                 Print the resolved plan. Makes no AWS calls and
#                             creates nothing.
#   --yes                     Skip interactive confirmation.
#   -h, --help                Show this help.
#
set -euo pipefail

umask 077

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
# shellcheck source=lib/common.sh
source "${script_dir}/lib/common.sh"

policies_dir="${repo_root}/infra/aws/bootstrap/policies"

admin_profile="${IRIS_BENCHMARK_ADMIN_PROFILE:-}"
region="${IRIS_BENCHMARK_AWS_REGION:-ap-northeast-2}"
name_prefix="${IRIS_BENCHMARK_NAME_PREFIX:-codedang-iris-benchmark}"
deployer_user=""
terraform_role=""
local_profile=""
role_profile=""
state_bucket="${IRIS_BENCHMARK_STATE_BUCKET:-codedang-tf-state}"
state_key="${IRIS_BENCHMARK_STATE_KEY:-terraform/iris-benchmark.tfstate}"
vpc_state_key="${IRIS_BENCHMARK_VPC_STATE_KEY:-terraform/vpc.tfstate}"
lock_table="${IRIS_BENCHMARK_LOCK_TABLE:-terraform-state-lock}"
source_snapshot_arn="${IRIS_BENCHMARK_SOURCE_SNAPSHOT_ARN:-arn:aws:rds:ap-northeast-2:219857217698:snapshot:rds:terraform-20250506182211604800000001-2026-09-24-16-05}"
account_id=""
create_access_key="false"
rotate_access_key="false"
enforce_exclusive="true"
render_only="false"
output_dir=""
dry_run="false"
assume_yes="false"

work_dir=""
cleanup() {
  if [ -n "${work_dir}" ] && [ -d "${work_dir}" ]; then
    rm -rf "${work_dir}"
  fi
}
trap cleanup EXIT

usage() {
  sed -n '2,75p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --admin-profile) admin_profile="$2"; shift 2 ;;
    --deployer-user) deployer_user="$2"; shift 2 ;;
    --terraform-role) terraform_role="$2"; shift 2 ;;
    --local-profile) local_profile="$2"; shift 2 ;;
    --role-profile) role_profile="$2"; shift 2 ;;
    --region) region="$2"; shift 2 ;;
    --name-prefix) name_prefix="$2"; shift 2 ;;
    --state-bucket) state_bucket="$2"; shift 2 ;;
    --state-key) state_key="$2"; shift 2 ;;
    --vpc-state-key) vpc_state_key="$2"; shift 2 ;;
    --lock-table) lock_table="$2"; shift 2 ;;
    --source-snapshot-arn) source_snapshot_arn="$2"; shift 2 ;;
    --create-access-key) create_access_key="true"; shift ;;
    --rotate-access-key) rotate_access_key="true"; shift ;;
    --no-enforce-exclusive) enforce_exclusive="false"; shift ;;
    --account-id) account_id="$2"; shift 2 ;;
    --render-only) render_only="true"; shift ;;
    --output-dir) output_dir="$2"; shift 2 ;;
    --dry-run) dry_run="true"; shift ;;
    --yes) assume_yes="true"; shift ;;
    -h | --help) usage; exit 0 ;;
    *) die "unknown argument: $1 (use --help)" ;;
  esac
done

[ -n "${deployer_user}" ] || deployer_user="${name_prefix}-deployer"
[ -n "${terraform_role}" ] || terraform_role="${name_prefix}-terraform"
[ -n "${local_profile}" ] || local_profile="${deployer_user}"
[ -n "${role_profile}" ] || role_profile="${terraform_role}"

require_value "--region" "${region}"
require_value "--name-prefix" "${name_prefix}"
require_value "--deployer-user" "${deployer_user}"
require_value "--terraform-role" "${terraform_role}"
require_value "--state-bucket" "${state_bucket}"
require_value "--state-key" "${state_key}"
require_value "--vpc-state-key" "${vpc_state_key}"
require_value "--lock-table" "${lock_table}"
require_value "--source-snapshot-arn" "${source_snapshot_arn}"
require_cmd jq

[ -d "${policies_dir}" ] || die "policy directory not found: ${policies_dir}"
user_policy_src="${policies_dir}/deployer-assume-role-policy.json"
trust_policy_src="${policies_dir}/terraform-role-trust-policy.json"

# Role permissions are split across a set of customer-managed policies so each
# rendered document stays below the IAM 6144-character policy size limit. The
# suffix after "terraform-role-policy-" names the corresponding managed policy.
role_policy_templates=()
while IFS= read -r f; do
  [ -n "${f}" ] || continue
  role_policy_templates+=("${f}")
done < <(find "${policies_dir}" -maxdepth 1 -type f -name 'terraform-role-policy-*.json' | LC_ALL=C sort)

[ "${#role_policy_templates[@]}" -ge 2 ] ||
  die "expected at least two terraform-role-policy-*.json templates in ${policies_dir}"

for f in "${user_policy_src}" "${trust_policy_src}" "${role_policy_templates[@]}"; do
  [ -f "${f}" ] || die "policy file not found: ${f}"
done

# Render the JSON templates with the resolved values. Values are account-scoped
# identifiers and names; no secret ever passes through this path.
render_policy() {
  local src="$1" dest="$2" content
  content="$(cat "${src}")"
  content="${content//__ACCOUNT_ID__/${account_id}}"
  content="${content//__REGION__/${region}}"
  content="${content//__NAME_PREFIX__/${name_prefix}}"
  content="${content//__STATE_BUCKET__/${state_bucket}}"
  content="${content//__STATE_KEY__/${state_key}}"
  content="${content//__VPC_STATE_KEY__/${vpc_state_key}}"
  content="${content//__LOCK_TABLE__/${lock_table}}"
  content="${content//__SOURCE_SNAPSHOT_ARN__/${source_snapshot_arn}}"
  content="${content//__DEPLOYER_USER__/${deployer_user}}"
  content="${content//__TERRAFORM_ROLE__/${terraform_role}}"
  printf '%s\n' "${content}" >"${dest}"
  jq -e . "${dest}" >/dev/null || die "rendered policy is not valid JSON: ${src}"
}

render_all() {
  local dir="$1" tpl
  mkdir -p "${dir}"
  render_policy "${user_policy_src}" "${dir}/deployer-assume-role-policy.json"
  render_policy "${trust_policy_src}" "${dir}/terraform-role-trust-policy.json"
  for tpl in "${role_policy_templates[@]}"; do
    render_policy "${tpl}" "${dir}/$(basename "${tpl}")"
  done
}

canonical_hash() {
  local file="$1" canon
  canon="$(jq -cS . "${file}")"
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "${canon}" | sha256sum | awk '{print $1}'
  else
    printf '%s' "${canon}" | shasum -a 256 | awk '{print $1}'
  fi
}

if [ "${render_only}" = "true" ]; then
  [ -n "${output_dir}" ] || die "--render-only requires --output-dir"
  require_value "--account-id (required for --render-only)" "${account_id}"
  render_all "${output_dir}"
  log "rendered policies to ${output_dir} (no AWS calls)"
  exit 0
fi

if [ "${dry_run}" = "true" ]; then
  [ -n "${account_id}" ] || account_id="000000000000"
  work_dir="$(mktemp -d)"
  render_all "${work_dir}" 2>/dev/null || die "dry-run policy rendering failed"
  log "dry-run: no AWS calls, no IAM changes, no access keys"
  log "dry-run: account        = ${account_id}"
  log "dry-run: region         = ${region}"
  log "dry-run: deployer user  = ${deployer_user}"
  log "dry-run: terraform role = ${terraform_role}"
  log "dry-run: role policies  = ${#role_policy_templates[@]} (${name_prefix}-terraform-permissions-*)"
  log "dry-run: local profile  = ${local_profile}"
  log "dry-run: role profile   = ${role_profile}"
  log "dry-run: state bucket   = ${state_bucket}"
  log "dry-run: state key      = ${state_key}"
  log "dry-run: vpc state key  = ${vpc_state_key}"
  log "dry-run: lock table     = ${lock_table}"
  log "dry-run: source snapshot= ${source_snapshot_arn}"
  log "dry-run: create key     = ${create_access_key}"
  log "dry-run: rotate keys    = ${rotate_access_key}"
  log "dry-run: enforce exclusive policies = ${enforce_exclusive}"
  exit 0
fi

require_cmd aws

# All AWS calls print JSON; jq performs extraction so the fake AWS CLI used by
# tests only needs to emit structured responses.
aws_admin() {
  local -a extra=()
  [ -n "${admin_profile}" ] && extra+=(--profile "${admin_profile}")
  aws "$@" --region "${region}" "${extra[@]}"
}

aws_deployer() {
  aws "$@" --region "${region}" --profile "${local_profile}"
}

aws_role() {
  aws "$@" --region "${region}" --profile "${role_profile}"
}

caller="$(aws_admin sts get-caller-identity)"
account_id="$(jq -er '.Account' <<<"${caller}")"
caller_arn="$(jq -er '.Arn' <<<"${caller}")"

case "${caller_arn}" in
  *":user/${deployer_user}" | *":role/${terraform_role}")
    die "refusing to bootstrap with the deployer identity itself; use an administrative profile"
    ;;
esac

work_dir="$(mktemp -d)"
render_all "${work_dir}"

user_arn="arn:aws:iam::${account_id}:user/${deployer_user}"
role_arn="arn:aws:iam::${account_id}:role/${terraform_role}"
user_inline_policy="${name_prefix}-assume-terraform"

# Resolve the set of customer-managed role policies from the template names. The
# rendered file basename (minus the template prefix) is the policy suffix.
role_policy_names=()
role_policy_arns=()
role_policy_files=()
for tpl in "${role_policy_templates[@]}"; do
  base="$(basename "${tpl}" .json)"
  suffix="${base#terraform-role-policy-}"
  role_policy_names+=("${name_prefix}-terraform-permissions-${suffix}")
  role_policy_arns+=("arn:aws:iam::${account_id}:policy/${name_prefix}-terraform-permissions-${suffix}")
  role_policy_files+=("${base}.json")
done

log "bootstrap: account=${account_id} region=${region}"
log "bootstrap: user=${deployer_user} role=${terraform_role}"
log "bootstrap: role policies=${#role_policy_names[@]} (${name_prefix}-terraform-permissions-*)"

if [ "${assume_yes}" != "true" ]; then
  confirm "Bootstrap IAM user ${deployer_user} and role ${terraform_role} in ${account_id}?" ||
    die "aborted by operator"
fi

# --- IAM user -----------------------------------------------------------------

if aws_admin iam get-user --user-name "${deployer_user}" >/dev/null 2>&1; then
  log "iam user exists: ${deployer_user}"
else
  aws_admin iam create-user --user-name "${deployer_user}" \
    --tags "Key=Project,Value=${name_prefix}" \
    "Key=Environment,Value=benchmark" \
    "Key=ManagedBy,Value=bootstrap-deployer" >/dev/null
  log "iam user created: ${deployer_user}"
fi

aws_admin iam tag-user --user-name "${deployer_user}" \
  --tags "Key=Project,Value=${name_prefix}" \
  "Key=Environment,Value=benchmark" \
  "Key=ManagedBy,Value=bootstrap-deployer" >/dev/null

# The only permission this user may hold: sts:AssumeRole on the role.
aws_admin iam put-user-policy --user-name "${deployer_user}" \
  --policy-name "${user_inline_policy}" \
  --policy-document "file://${work_dir}/deployer-assume-role-policy.json" >/dev/null

if [ "${enforce_exclusive}" = "true" ]; then
  while IFS= read -r pname; do
    [ -n "${pname}" ] || continue
    [ "${pname}" = "${user_inline_policy}" ] && continue
    log "removing unexpected inline user policy: ${pname}"
    aws_admin iam delete-user-policy --user-name "${deployer_user}" --policy-name "${pname}" >/dev/null
  done < <(aws_admin iam list-user-policies --user-name "${deployer_user}" | jq -r '.PolicyNames[]?')
  while IFS= read -r attached_arn; do
    [ -n "${attached_arn}" ] || continue
    log "detaching unexpected managed user policy: ${attached_arn}"
    aws_admin iam detach-user-policy --user-name "${deployer_user}" --policy-arn "${attached_arn}" >/dev/null
  done < <(aws_admin iam list-attached-user-policies --user-name "${deployer_user}" | jq -r '.AttachedPolicies[]?.PolicyArn')
fi

# --- IAM role -----------------------------------------------------------------

if aws_admin iam get-role --role-name "${terraform_role}" >/dev/null 2>&1; then
  log "iam role exists: ${terraform_role}"
else
  aws_admin iam create-role --role-name "${terraform_role}" \
    --description "Terraform apply identity for codedang-iris-benchmark." \
    --assume-role-policy-document "file://${work_dir}/terraform-role-trust-policy.json" \
    --tags "Key=Project,Value=${name_prefix}" \
    "Key=Environment,Value=benchmark" \
    "Key=ManagedBy,Value=bootstrap-deployer" >/dev/null
  log "iam role created: ${terraform_role}"
fi

aws_admin iam update-assume-role-policy --role-name "${terraform_role}" \
  --policy-document "file://${work_dir}/terraform-role-trust-policy.json" >/dev/null

aws_admin iam tag-role --role-name "${terraform_role}" \
  --tags "Key=Project,Value=${name_prefix}" \
  "Key=Environment,Value=benchmark" \
  "Key=ManagedBy,Value=bootstrap-deployer" >/dev/null

# --- Customer-managed role policies ------------------------------------------

# Each policy in the set is created or versioned independently. The
# PolicyDocumentHash tag records the canonical rendered document so an unchanged
# policy is not versioned again.
for i in "${!role_policy_names[@]}"; do
  policy_name="${role_policy_names[$i]}"
  policy_arn="${role_policy_arns[$i]}"
  rendered_policy="${work_dir}/${role_policy_files[$i]}"
  desired_hash="$(canonical_hash "${rendered_policy}")"

  if aws_admin iam get-policy --policy-arn "${policy_arn}" >/dev/null 2>&1; then
    current_hash="$(aws_admin iam list-policy-tags --policy-arn "${policy_arn}" |
      jq -r '[.Tags[]? | select(.Key == "PolicyDocumentHash") | .Value][0] // ""')"
    if [ "${current_hash}" != "${desired_hash}" ]; then
      log "updating customer-managed policy: ${policy_name}"
      aws_admin iam create-policy-version --policy-arn "${policy_arn}" \
        --policy-document "file://${rendered_policy}" \
        --set-as-default >/dev/null
      aws_admin iam tag-policy --policy-arn "${policy_arn}" \
        --tags "Key=PolicyDocumentHash,Value=${desired_hash}" >/dev/null
    else
      log "customer-managed policy is current: ${policy_name}"
    fi
  else
    log "creating customer-managed policy: ${policy_name}"
    aws_admin iam create-policy --policy-name "${policy_name}" \
      --description "Terraform permissions (${role_policy_files[$i]}) for the codedang-iris-benchmark deployer role." \
      --policy-document "file://${rendered_policy}" \
      --tags "Key=Project,Value=${name_prefix}" \
      "Key=Environment,Value=benchmark" \
      "Key=ManagedBy,Value=bootstrap-deployer" \
      "Key=PolicyDocumentHash,Value=${desired_hash}" >/dev/null
  fi
done

if [ "${enforce_exclusive}" = "true" ]; then
  declare -A wanted_role_policy_arns=()
  for policy_arn in "${role_policy_arns[@]}"; do
    wanted_role_policy_arns["${policy_arn}"]=1
  done
  while IFS= read -r attached_arn; do
    [ -n "${attached_arn}" ] || continue
    [ -n "${wanted_role_policy_arns[${attached_arn}]:-}" ] && continue
    log "detaching unexpected role policy: ${attached_arn}"
    aws_admin iam detach-role-policy --role-name "${terraform_role}" --policy-arn "${attached_arn}" >/dev/null
  done < <(aws_admin iam list-attached-role-policies --role-name "${terraform_role}" | jq -r '.AttachedPolicies[]?.PolicyArn')
  while IFS= read -r pname; do
    [ -n "${pname}" ] || continue
    log "removing unexpected inline role policy: ${pname}"
    aws_admin iam delete-role-policy --role-name "${terraform_role}" --policy-name "${pname}" >/dev/null
  done < <(aws_admin iam list-role-policies --role-name "${terraform_role}" | jq -r '.PolicyNames[]?')
fi

attached_role_policy_arns="$(aws_admin iam list-attached-role-policies --role-name "${terraform_role}" |
  jq -r '.AttachedPolicies[]?.PolicyArn')"
for i in "${!role_policy_names[@]}"; do
  policy_name="${role_policy_names[$i]}"
  policy_arn="${role_policy_arns[$i]}"
  if grep -Fxq "${policy_arn}" <<<"${attached_role_policy_arns}"; then
    continue
  fi
  log "attaching policy ${policy_name} to ${terraform_role}"
  aws_admin iam attach-role-policy --role-name "${terraform_role}" --policy-arn "${policy_arn}" >/dev/null
done

# --- Optional access key and verification -------------------------------------

key_created="false"
if [ "${create_access_key}" = "true" ]; then
  active_keys="$(aws_admin iam list-access-keys --user-name "${deployer_user}" |
    jq -r '.AccessKeyMetadata[]? | select(.Status == "Active") | .AccessKeyId')"
  if [ -n "${active_keys}" ]; then
    if [ "${rotate_access_key}" != "true" ]; then
      die "active access key(s) already exist for ${deployer_user}; refusing to create another. Rotate explicitly with --rotate-access-key."
    fi
    while IFS= read -r key_id; do
      [ -n "${key_id}" ] || continue
      log "rotating access key: ${key_id}"
      aws_admin iam update-access-key --user-name "${deployer_user}" \
        --access-key-id "${key_id}" --status Inactive >/dev/null
      aws_admin iam delete-access-key --user-name "${deployer_user}" \
        --access-key-id "${key_id}" >/dev/null
    done <<<"${active_keys}"
  fi

  key_json="$(aws_admin iam create-access-key --user-name "${deployer_user}")"
  access_key_id="$(jq -er '.AccessKey.AccessKeyId' <<<"${key_json}")"
  secret_access_key="$(jq -er '.AccessKey.SecretAccessKey' <<<"${key_json}")"

  # The secret is written only to a 0600 file under the private work directory
  # and imported into the local AWS credentials file. It is never echoed and
  # never placed on the command line, in this repository, or in Terraform state.
  csv_file="${work_dir}/access-key.csv"
  {
    printf 'User name,Access key ID,Secret access key\n'
    printf '%s,%s,%s\n' "${local_profile}" "${access_key_id}" "${secret_access_key}"
  } >"${csv_file}"
  chmod 600 "${csv_file}"
  aws configure import --csv "file://${csv_file}" >/dev/null
  rm -f "${csv_file}"
  unset secret_access_key key_json

  aws configure set region "${region}" --profile "${local_profile}" >/dev/null
  aws configure set output json --profile "${local_profile}" >/dev/null

  # Terraform and all benchmark mutations use this role profile. The base user
  # profile remains limited to sts:AssumeRole.
  aws configure set role_arn "${role_arn}" --profile "${role_profile}" >/dev/null
  aws configure set source_profile "${local_profile}" --profile "${role_profile}" >/dev/null
  aws configure set role_session_name iris-benchmark-terraform --profile "${role_profile}" >/dev/null
  aws configure set region "${region}" --profile "${role_profile}" >/dev/null
  aws configure set output json --profile "${role_profile}" >/dev/null

  assumed_arn="$(aws_deployer sts assume-role \
    --role-arn "${role_arn}" \
    --role-session-name iris-benchmark-verify |
    jq -er '.AssumedRoleUser.Arn')"
  case "${assumed_arn}" in
    *":assumed-role/${terraform_role}/"*) ;;
    *) die "role assumption verification returned an unexpected identity: ${assumed_arn}" ;;
  esac
  key_created="true"
  log "role assumption verified using role profile '${role_profile}'"
else
  decision="$(aws_admin iam simulate-principal-policy \
    --policy-source-arn "${user_arn}" \
    --action-names sts:AssumeRole \
    --resource-arns "${role_arn}" |
    jq -er '.EvaluationResults[0].EvalDecision')"
  if [ "${decision}" = "allowed" ]; then
    log "assume-role allowed for ${deployer_user} (simulation; no access key created)"
  else
    die "assume-role simulation returned '${decision}' for ${deployer_user}"
  fi
fi

log "bootstrap complete"
log "deployer user: ${deployer_user} (only sts:AssumeRole on ${terraform_role})"
if [ "${key_created}" = "true" ]; then
  log "base profile '${local_profile}' and role profile '${role_profile}' configured; no credential was printed or committed"
fi
