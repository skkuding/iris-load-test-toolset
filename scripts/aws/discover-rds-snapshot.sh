#!/usr/bin/env bash
#
# Discover the snapshot identifier to restore the codedang-iris-benchmark RDS
# instance from.
#
# This script is READ-ONLY. It only calls `aws rds describe-db-snapshots` and
# never mutates AWS. Terraform consumes the printed identifier as an explicit
# input so that snapshot selection is deterministic and reviewable instead of a
# nondeterministic "latest" lookup happening at apply time.
#
# Usage:
#   scripts/aws/discover-rds-snapshot.sh [options]
#
# Options:
#   --region REGION             AWS region (default: $IRIS_BENCHMARK_AWS_REGION
#                               or ap-northeast-2)
#   --source-identifier ID      Source RDS instance identifier
#                               (default: $IRIS_BENCHMARK_SOURCE_DB_IDENTIFIER
#                               or terraform-20250506182211604800000001)
#   --snapshot-type TYPE        automated | manual | shared | public |
#                               awsbackup | all (default: automated)
#   --format FORMAT             id | json | env | tfvars (default: id)
#   --snapshot-id ID            Verify that a specific snapshot is available
#                               for the source instance and exit 0/1.
#   -h, --help                  Show this help.
#
# Exit status:
#   0  a snapshot was found/verified
#   1  usage, dependency, AWS, or "no snapshot" error
#
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${script_dir}/lib/common.sh"

region="${IRIS_BENCHMARK_AWS_REGION:-ap-northeast-2}"
source_identifier="${IRIS_BENCHMARK_SOURCE_DB_IDENTIFIER:-terraform-20250506182211604800000001}"
snapshot_type="automated"
format="id"
verify_snapshot_id=""

usage() {
  sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --region)
      region="$2"
      shift 2
      ;;
    --source-identifier)
      source_identifier="$2"
      shift 2
      ;;
    --snapshot-type)
      snapshot_type="$2"
      shift 2
      ;;
    --format)
      format="$2"
      shift 2
      ;;
    --snapshot-id)
      verify_snapshot_id="$2"
      shift 2
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

case "${snapshot_type}" in
  automated | manual | shared | public | awsbackup | all) ;;
  *) die "invalid --snapshot-type: ${snapshot_type}" ;;
esac
case "${format}" in
  id | json | env | tfvars) ;;
  *) die "invalid --format: ${format}" ;;
esac

require_value "--region" "${region}"
require_value "--source-identifier" "${source_identifier}"
require_cmd aws
require_cmd jq

type_args=()
if [ "${snapshot_type}" != "all" ]; then
  type_args=(--snapshot-type "${snapshot_type}")
fi

log "discover: region=${region} source=${source_identifier} type=${snapshot_type}"

# The only AWS API call in this script. Keep it read-only.
raw="$(
  aws rds describe-db-snapshots \
    --region "${region}" \
    --db-instance-identifier "${source_identifier}" \
    "${type_args[@]}" \
    --output json
)"

# Keep only available snapshots for this source and pick the newest. Creation
# times are normalized to whole-second UTC strings, which sort chronologically,
# avoiding locale/format-dependent date parsing.
selected="$(
  jq -c --arg src "${source_identifier}" '
    [
      .DBSnapshots[]
      | select(.Status == "available")
      | select(.DBInstanceIdentifier == $src)
    ]
    | sort_by(.SnapshotCreateTime | sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z"))
    | reverse
    | .[0]
  ' <<<"${raw}"
)"

if [ "${selected}" = "null" ] || [ -z "${selected}" ]; then
  die "no available ${snapshot_type} snapshot found for ${source_identifier} in ${region}"
fi

snapshot_id="$(jq -r '.DBSnapshotIdentifier' <<<"${selected}")"
snapshot_created="$(jq -r '.SnapshotCreateTime' <<<"${selected}")"
snapshot_arn="$(jq -r '.DBSnapshotArn' <<<"${selected}")"
snapshot_encrypted="$(jq -r '.Encrypted' <<<"${selected}")"

if [ -n "${verify_snapshot_id}" ]; then
  verified="$(jq -c --arg id "${verify_snapshot_id}" \
    '.DBSnapshots[] | select(.DBSnapshotIdentifier == $id and .Status == "available")' \
    <<<"${raw}")"
  if [ -z "${verified}" ]; then
    die "snapshot ${verify_snapshot_id} is not available for ${source_identifier} in ${region}"
  fi
  selected="${verified}"
  snapshot_id="$(jq -r '.DBSnapshotIdentifier' <<<"${selected}")"
  snapshot_created="$(jq -r '.SnapshotCreateTime' <<<"${selected}")"
  snapshot_arn="$(jq -r '.DBSnapshotArn' <<<"${selected}")"
  snapshot_encrypted="$(jq -r '.Encrypted' <<<"${selected}")"
  log "verified snapshot: ${verify_snapshot_id}"
fi

case "${format}" in
  id)
    printf '%s\n' "${snapshot_id}"
    ;;
  json)
    jq . <<<"${selected}"
    ;;
  env)
    printf "IRIS_BENCHMARK_SOURCE_DB_IDENTIFIER='%s'\n" "${source_identifier}"
    printf "IRIS_BENCHMARK_SOURCE_SNAPSHOT_IDENTIFIER='%s'\n" "${snapshot_id}"
    printf "IRIS_BENCHMARK_SOURCE_SNAPSHOT_CREATED_AT='%s'\n" "${snapshot_created}"
    printf "IRIS_BENCHMARK_SOURCE_SNAPSHOT_ARN='%s'\n" "${snapshot_arn}"
    printf "IRIS_BENCHMARK_SOURCE_SNAPSHOT_ENCRYPTED='%s'\n" "${snapshot_encrypted}"
    ;;
  tfvars)
    printf 'source_snapshot_identifier = "%s"\n' "${snapshot_id}"
    printf 'source_snapshot_arn = "%s"\n' "${snapshot_arn}"
    ;;
esac
