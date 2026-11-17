#!/bin/bash
# PID 1 of a KVM variance guest. No systemd, no network, no containers.
#
# Kernel command line parameters (set by kvmexp-run.py):
#   kvmexp.flows=<k>       execution flows in this VM
#   kvmexp.workload=<name> workload label (see /opt/kvmexp/workloads/<name>)
#   kvmexp.run=<run-id>    run identifier
#   kvmexp.vm=<NN>         VM index
#   kvmexp.warmup=<n>      discarded warm-up iterations per flow
#
# Protocol on /dev/virtio-ports/ctl (one line per message):
#   guest -> host  READY <guest_realtime_ns>
#   host  -> guest START <host_realtime_ns>
#   guest -> host  STARTED <host_ns> <guest_ns> <cpu line of /proc/stat>
#   host  -> guest STOP <host_realtime_ns>
#   guest -> host  STOPPED <host_ns> <guest_ns> <cpu line of /proc/stat>
#   guest -> host  DONE <failed-flow-count>
# Each flow streams NDJSON samples to /dev/virtio-ports/flow-<NN>.
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mount -t tmpfs -o size=512m tmpfs /tmp
mount -t tmpfs tmpfs /run
mount -t cgroup2 cgroup2 /sys/fs/cgroup
echo "+cpuset +cpu +io +memory +pids" > /sys/fs/cgroup/cgroup.subtree_control

fatal() {
  echo "kvmexp-guest: $*" > /dev/ttyS0
  [[ -e /dev/virtio-ports/ctl ]] && echo "FAIL $*" > /dev/virtio-ports/ctl
  echo o > /proc/sysrq-trigger
  sleep 60
}

declare -A P
for kv in $(cat /proc/cmdline); do
  case "$kv" in kvmexp.*=*) k=${kv%%=*}; P[${k#kvmexp.}]=${kv#*=} ;; esac
done
flows=${P[flows]:?} workload=${P[workload]:?} run=${P[run]:?} vm=${P[vm]:?}
warmup=${P[warmup]:-3}
base=/opt/kvmexp
wl=$base/workloads/$workload
[[ -x $wl/bin ]] || fatal "unknown workload $workload"

# Without udev, create the named /dev/virtio-ports links ourselves.
for _ in $(seq 1 100); do
  mkdir -p /dev/virtio-ports
  for p in /sys/class/virtio-ports/vport*; do
    [[ -r $p/name ]] && ln -sf "/dev/${p##*/}" "/dev/virtio-ports/$(cat "$p/name")"
  done
  printf -v last 'flow-%02d' "$flows"
  [[ -e /dev/virtio-ports/ctl && -e /dev/virtio-ports/$last ]] && break
  sleep 0.05
done
exec 3<> /dev/virtio-ports/ctl || fatal "no control port"

pids=()
for ((f = 1; f <= flows; f++)); do
  printf -v nn '%02d' "$f"
  port=/dev/virtio-ports/flow-$nn
  [[ -e $port ]] || fatal "missing $port"
  (
    common=(--mode execute --run-id "$run" --block-id "vm-$vm" --worker "vm$vm-flow$nn"
      --fixture "$workload" --binary "$wl/bin" --input "$wl/input.txt"
      --expected-output "$wl/output.txt" --judger "$base/libjudger.so"
      --container-id "$run-vm$vm-flow$nn"
      --expected-cgroup-parent /sys/fs/cgroup/kvmexp.slice
      --production-compat --uid 0 --gid 0
      --max-cpu-time 10000 --max-real-time 30000)
    for a in $(cat "$wl/args" 2>/dev/null); do common+=(--run-args "$a"); done
    mkdir -p "/tmp/warm-$nn" "/tmp/work-$nn"
    "$base/judger-bench" "${common[@]}" --work-dir "/tmp/warm-$nn" \
      --iterations "$warmup" --output "/tmp/warm-$nn.ndjson" > "/tmp/warm-$nn.log" 2>&1 || exit 90
    : > "/tmp/ready-$nn"
    while [[ ! -e /tmp/start ]]; do sleep 0.02; done
    exec "$base/judger-bench" "${common[@]}" --work-dir "/tmp/work-$nn" \
      --iterations 1000000 --stop-file /tmp/stop --stream-output "$port" \
      --output "/tmp/flow-$nn.ndjson" > "/tmp/flow-$nn.log" 2>&1
  ) &
  pids+=("$!")
done

for ((f = 1; f <= flows; f++)); do
  printf -v nn '%02d' "$f"
  until [[ -e /tmp/ready-$nn ]]; do
    kill -0 "${pids[f-1]}" 2>/dev/null || fatal "flow $nn failed during warm-up: $(tail -c 300 /tmp/warm-$nn.log | tr '\n' ' ')"
    sleep 0.05
  done
done
echo "READY $(date +%s%N)" >&3

read -r cmd host_ns <&3
[[ $cmd == START ]] || fatal "expected START, got $cmd"
guest_ns=$(date +%s%N)
: > /tmp/start
echo "STARTED $host_ns $guest_ns $(head -1 /proc/stat)" >&3

read -r cmd host_ns <&3
[[ $cmd == STOP ]] || fatal "expected STOP, got $cmd"
guest_ns=$(date +%s%N)
: > /tmp/stop
stat_line=$(head -1 /proc/stat)
failed=0
for pid in "${pids[@]}"; do wait "$pid" || failed=$((failed + 1)); done
echo "STOPPED $host_ns $guest_ns $stat_line" >&3
echo "DONE $failed" >&3
sync
sleep 0.2
echo o > /proc/sysrq-trigger
sleep 60
