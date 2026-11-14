#!/usr/bin/env bash
# One bare-metal execution flow (arms B0 and B1). Started by kvmexp-run.py as root.
# usage: host-flow.sh <run-dir> <run-id> <flow NN> <workload> <cpu-list> <warmup> [resctrl-group]
set -euo pipefail
dir=${1:?} run=${2:?} nn=${3:?} workload=${4:?} cpus=${5:?} warmup=${6:?} group=${7:-}
stage=${KVMEXP_STAGE:-/var/tmp/iris-kvm}
wl=$stage/workloads/$workload
if [[ -n $group ]]; then
  # Children inherit the resctrl group, so join it before any work starts.
  echo 0 > "/sys/fs/resctrl/$group/tasks"
fi
common=(--mode execute --run-id "$run" --block-id bare --worker "flow$nn"
  --fixture "$workload" --binary "$wl/bin" --input "$wl/input.txt"
  --expected-output "$wl/output.txt" --judger "$stage/assets/libjudger.so"
  --container-id "$run-flow$nn"
  --expected-cgroup-parent /sys/fs/cgroup/kvmexp.slice
  --production-compat --uid 0 --gid 0
  --max-cpu-time 10000 --max-real-time 30000)
if [[ -f $wl/args ]]; then
  while read -r a; do common+=(--run-args "$a"); done < "$wl/args"
fi
pin=(numactl "--physcpubind=$cpus" --membind=0)
# Scratch I/O on tmpfs, like the guest's /tmp.
work=/dev/shm/kvmexp-$run/flow-$nn
mkdir -p "$work/warm" "$work/run"
"${pin[@]}" "$stage/assets/judger-bench" "${common[@]}" --work-dir "$work/warm" \
  --iterations "$warmup" --output "$dir/warm-$nn.ndjson" > "$dir/warm-$nn.log" 2>&1
: > "$dir/ready-$nn"
while [[ ! -e $dir/start ]]; do sleep 0.02; done
exec "${pin[@]}" "$stage/assets/judger-bench" "${common[@]}" --work-dir "$work/run" \
  --iterations 1000000 --stop-file "$dir/stop" --stream-output "$dir/flow-$nn.ndjson" \
  --output "$dir/final-$nn.ndjson" > "$dir/flow-$nn.log" 2>&1
