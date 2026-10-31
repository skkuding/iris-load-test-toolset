#!/usr/bin/env bash
# Run as root on codedang8 after staging the pinned workload and libjudger.
set -euo pipefail
base=/tmp/iris-cat-20261101
cd "$base"
root=/sys/fs/resctrl
[[ $(cat "$root/info/L3/cbm_mask") == fffff ]]
[[ $(sha256sum "$base/libjudger.so" | cut -d' ' -f1) == 2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103 ]]
[[ $(sha256sum /tmp/iris-rdt-20261031/workload | cut -d' ' -f1) == 71100d7247185c5324c4f1f88ea4a149330cb11ca892dad422936053754d7ba3 ]]
for cpu in {0..7} {16..23}; do [[ $(cat "/sys/devices/system/cpu/cpu$cpu/cache/index3/id") == 0 ]]; done
for cpu in {8..15} {24..31}; do [[ $(cat "/sys/devices/system/cpu/cpu$cpu/cache/index3/id") == 1 ]]; done
socket0=(0 1 2 3 4 5 6 7 16 17 18 19 20 21 22)
socket1=(8 9 10 11 12 13 14 15 24 25 26 27 28 29 30)
current_run= current_workers= current_mode= sampler= active_pids=()

clean_sandboxes() {
  local run=$1 workers=$2 suffix path child
  for ((w=1;w<=workers;w++)); do
    printf -v suffix '%02d' "$w"
    path="/sys/fs/cgroup/sandbox-${run}-worker-${suffix}"
    [[ -d "$path" ]] || continue
    for child in "$path"/box-*; do
      [[ -d "$child" ]] || continue
      [[ -z $(cat "$child/cgroup.procs") ]]
      rmdir "$child"
    done
    [[ -z $(cat "$path/cgroup.procs") ]]
    rmdir "$path"
  done
}
cleanup_current() {
  [[ -n "$current_run" ]] || return 0
  local pid
  for pid in "${active_pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${active_pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  touch "$base/$current_run.stop"
  if [[ -n "$sampler" ]]; then wait "$sampler" 2>/dev/null || true; fi
  clean_sandboxes "$current_run" "$current_workers" || true
  if [[ "$current_mode" == equal ]]; then
    bash "$base/scripts/cat-groups.sh" cleanup "$current_run" "$current_workers" || true
  fi
  current_run=
  active_pids=() sampler=
}
trap cleanup_current EXIT

run_one() {
  local n=$1 mode=$2 rep=$3 run classes iterations load start ready rc=0
  run="iris-cat-20261101-${rep}-${mode}-${n}"
  current_run=$run current_workers=$n current_mode=$mode
  [[ ! -e "$base/$run.start" && ! -e "$base/$run.rdt.tsv" ]]
  classes=$((n / 2))
  ((classes > 0)) || classes=1
  iterations=50
  for attempt in {1..90}; do
    load=$(cut -d' ' -f1 /proc/loadavg)
    if awk -v x="$load" 'BEGIN { exit !(x < 0.7) }'; then break; fi
    if [[ "$attempt" == 90 ]]; then echo "load did not settle: $load" >&2; exit 1; fi
    sleep 10
  done
  echo "Starting $run (load1=$load)"
  if [[ "$mode" == equal ]]; then bash "$base/scripts/cat-groups.sh" setup "$run" "$n"; fi
  bash "$base/scripts/cat-sample.sh" "$run" "$([[ "$mode" == equal ]] && echo "$classes" || echo 0)" > "$base/$run.sampler.log" 2>&1 &
  sampler=$!
  for attempt in {1..30}; do
    [[ -r "$base/$run.ready" ]] && break
    sleep 0.1
  done
  [[ -r "$base/$run.ready" ]]
  start="$base/$run.start"
  active_pids=()
  for ((i=0;i<classes;i++)); do
    for node in 0 1; do
      worker=$((i + 1 + node * classes))
      cpu=${socket0[i]}
      [[ "$node" == 1 ]] && cpu=${socket1[i]}
      ready="$base/$run.worker-$worker.ready"
      bash "$base/scripts/direct-cat-worker.sh" "$run" "$mode" "$((i+1))" "$cpu" "$node" "$worker" "$iterations" "$ready" "$start" > "$base/$run.worker-$worker.log" 2>&1 &
      active_pids+=("$!")
    done
  done
  for ((worker=1;worker<=n;worker++)); do
    for attempt in {1..300}; do
      [[ -r "$base/$run.worker-$worker.ready" ]] && break
      sleep 0.1
    done
    [[ -r "$base/$run.worker-$worker.ready" ]] || { echo "worker $worker not ready" >&2; exit 1; }
  done
  touch "$start"
  for pid in "${active_pids[@]}"; do wait "$pid" || rc=1; done
  active_pids=()
  touch "$base/$run.stop"
  wait "$sampler" || rc=1
  clean_sandboxes "$run" "$n" || rc=1
  if [[ "$mode" == equal ]]; then bash "$base/scripts/cat-groups.sh" cleanup "$run" "$n" || rc=1; fi
  current_run=
  sampler=
  ((rc == 0)) || { echo "failed: $run" >&2; exit 1; }
  echo "Completed $run"
}

for spec in "$@"; do
  IFS=: read -r n mode rep <<< "$spec"
  [[ "$n" =~ ^(2|4|8|16|30)$ && "$mode" =~ ^(base|equal)$ && "$rep" =~ ^[a-z0-9]+$ ]] || { echo "invalid spec $spec" >&2; exit 2; }
  run_one "$n" "$mode" "$rep"
done
