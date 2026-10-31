#!/usr/bin/env bash
# Sample root and all run-scoped CAT groups once per second into TSV.
set -euo pipefail
run_id=${1:?}
classes=${2:?}
output="/tmp/iris-cat-20261101/${run_id}.rdt.tsv"
ready="/tmp/iris-cat-20261101/${run_id}.ready"
stop="/tmp/iris-cat-20261101/${run_id}.stop"
root=/sys/fs/resctrl
[[ ! -e "$output" && ! -e "$ready" && ! -e "$stop" ]]
printf 'observedAtNs\tgroup\tdomain\tllcOccupancyBytes\tmbmTotalBytes\tmbmLocalBytes\ttasks\n' > "$output"
printf 'ready\n' > "$ready"
while [[ ! -e "$stop" ]]; do
  at=$(date +%s%N)
  for ((i=0;i<=classes;i++)); do
    if ((i==0)); then group=root; path="$root"
    else
      printf -v suffix '%02d' "$i"
      group="iris-cat-${run_id}-class-${suffix}"
      path="$root/$group"
    fi
    for domain in 00 01; do
      data="$path/mon_data/mon_L3_$domain"
      [[ -d "$data" ]] || { echo "missing monitor domain $data" >&2; exit 1; }
      if [[ "$group" == root ]]; then tasks=0
      else tasks=$(wc -l < "$path/tasks")
      fi
      printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$at" "$group" "$domain" \
        "$(cat "$data/llc_occupancy")" "$(cat "$data/mbm_total_bytes")" \
        "$(cat "$data/mbm_local_bytes")" "$tasks" >> "$output"
    done
  done
  sleep 1
done
