#!/usr/bin/env bash
set -euo pipefail
run_id=${1:?} mode=${2:?} class=${3:?} cpu=${4:?} node=${5:?} worker=${6:?}
iterations=${7:?} ready=${8:?} start=${9:?}
base=/tmp/iris-cat-20261101
printf -v suffix '%02d' "$worker"
if [[ "$mode" == equal ]]; then
  printf -v class_suffix '%02d' "$class"
  group="iris-cat-${run_id}-class-${class_suffix}"
  [[ -f "/sys/fs/resctrl/$group/tasks" ]]
  printf '0\n' > "/sys/fs/resctrl/$group/tasks"
  printf 'group=%s\nschemata=%s\n' "$group" "$(cat "/sys/fs/resctrl/$group/schemata")" > "$base/$run_id.worker-$suffix.cat"
else
  printf 'group=root\n' > "$base/$run_id.worker-$suffix.cat"
fi
printf 'ready\n' > "$ready"
while [[ ! -e "$start" ]]; do sleep 0.02; done
exec numactl --physcpubind="$cpu" --membind="$node" "$base/judger-bench-linux-amd64" \
  --mode execute --run-id "$run_id" --block-id "direct-$mode" --worker "worker-$suffix" \
  --fixture cpp-runtime-v1 --binary /tmp/iris-rdt-20261031/workload \
  --input "$base/input.txt" --expected-output "$base/output.txt" \
  --output "$base/$run_id.worker-$suffix.ndjson" --iterations "$iterations" \
  --judger "$base/libjudger.so" \
  --judger-sha256 2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103 \
  --container-id "$run_id-worker-$suffix" \
  --expected-cgroup-parent /sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service \
  --production-compat --uid 0 --gid 0
