#!/usr/bin/env bash
# Execute inside the run-scoped privileged keeper after resctrl is mounted.
set -euo pipefail
action=${1:?setup or cleanup}
run_id=${2:?run id}
workers=${3:?worker count}
root=/sys/fs/resctrl
[[ "$run_id" =~ ^[a-z0-9-]+$ && "$run_id" == *-equal-"$workers" ]] || { echo 'unsafe or mismatched run id' >&2; exit 1; }

case "$workers" in
  1) classes=1 ;;
  2|4|8|16|30) classes=$((workers / 2)) ;;
  *) echo "unsupported worker count $workers" >&2; exit 1 ;;
esac

if [[ "$action" == setup ]]; then
  [[ $(cat "$root/info/L3/cbm_mask") == fffff ]]
  [[ $(cat "$root/info/L3/min_cbm_bits") == 1 ]]
  [[ $(cat "$root/info/L3/num_closids") == 16 ]]
  [[ $(cat "$root/info/L3/shareable_bits") == c0000 ]]
  [[ $(cat "$root/schemata" | tr -d '[:space:]') == 'L3:0=fffff;1=fffff' ]]
  for path in "$root"/*; do
    [[ -d "$path" ]] || continue
    case "${path##*/}" in info|mon_data|mon_groups) ;; *) echo "pre-existing resctrl group: $path" >&2; exit 1 ;; esac
  done
  width=$((18 / classes))
  ((width >= 1 && classes <= 15))
  for ((i=1;i<=classes;i++)); do
    printf -v suffix '%02d' "$i"
    group="$root/iris-cat-${run_id}-class-${suffix}"
    mask=$(( ((1 << width) - 1) << ((i - 1) * width) ))
    printf -v hex '%x' "$mask"
    mkdir "$group"
    printf 'L3:0=%s;1=%s\n' "$hex" "$hex" > "$group/schemata"
    [[ $(tr -d '[:space:]' < "$group/schemata") == "L3:0=$hex;1=$hex" ]]
    printf '%s %s\n' "${group##*/}" "$hex"
  done
elif [[ "$action" == cleanup ]]; then
  for ((i=1;i<=classes;i++)); do
    printf -v suffix '%02d' "$i"
    group="$root/iris-cat-${run_id}-class-${suffix}"
    [[ -d "$group" ]] || continue
    [[ -z $(cat "$group/tasks") ]] || { echo "CAT group still has tasks: $group" >&2; exit 1; }
    rmdir "$group"
  done
else
  echo "unknown action $action" >&2
  exit 1
fi
