#!/usr/bin/env bash
# Reproduce the direct Judger ladder with passive, host-default CMT/MBM samples.
# The remote benchmark host and pinned artifacts are intentionally explicit.
set -euo pipefail

cd "$(dirname "$0")/.."
HOST=codedang8
REMOTE=/tmp/iris-rdt-20261031
PARENT=/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service
SOCKET=/private/tmp/iris-rdt-ssh.sock
IMAGE=sha256:b0d4158bf9698f93a2b94ced2a1aaf0d7ac8f7beb6de636ccf5fab7542156bab
IRIS=sha256:39b743d6e1ffb1efaa18f42b509c2017b91c11976971c862beafda2ae1521250
JUDGER=2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103
WORKLOAD=71100d7247185c5324c4f1f88ea4a149330cb11ca892dad422936053754d7ba3
CURRENT_CONTAINER=
CHANGED_CPUSET=0

cleanup() {
  if [[ -n "$CURRENT_CONTAINER" ]]; then
    ssh -o BatchMode=yes "$HOST" "docker stop $CURRENT_CONTAINER" >/dev/null 2>&1 || true
  fi
  if [[ "$CHANGED_CPUSET" == 1 ]]; then
    ssh -o BatchMode=yes "$HOST" "printf '%s' '-cpuset' > $PARENT/cgroup.subtree_control" || true
  fi
}
trap cleanup EXIT

before=$(ssh -o BatchMode=yes "$HOST" "cat $PARENT/cgroup.subtree_control")
if [[ " $before " != *" cpuset "* ]]; then
  ssh -o BatchMode=yes "$HOST" "printf '%s' '+cpuset' > $PARENT/cgroup.subtree_control"
  CHANGED_CPUSET=1
fi
mkdir -p runs/rdt-20261031

for n in 1 2 4 8 16 30; do
  run="iris-20261031-rdt-$n"
  CURRENT_CONTAINER="iris-rdt-$n"
  echo "Waiting for load1 < 0.7 before $run"
  for attempt in $(seq 1 90); do
    load=$(ssh -o BatchMode=yes "$HOST" 'cat /proc/loadavg' | awk '{print $1}')
    if awk -v x="$load" 'BEGIN { exit !(x < 0.7) }'; then break; fi
    if [[ "$attempt" == 90 ]]; then echo "host load did not settle: $load" >&2; exit 1; fi
    sleep 10
  done
  echo "Starting RDT sampler for $run (load1=$load)"
  ssh -o BatchMode=yes "$HOST" "docker run -d --rm --name $CURRENT_CONTAINER --privileged --network none --label iris-bench.rdt=$run --mount type=bind,src=$REMOTE,dst=$REMOTE --entrypoint $REMOTE/iris-bench-rdt-linux-amd64 ubuntu:24.04 --output $REMOTE/$run.ndjson --ready $REMOTE/$run.ready --interval 1s" >/dev/null
  for attempt in $(seq 1 30); do
    if ssh -o BatchMode=yes "$HOST" "test -r $REMOTE/$run.ready"; then break; fi
    if [[ "$attempt" == 30 ]]; then
      ssh -o BatchMode=yes "$HOST" "docker logs $CURRENT_CONTAINER" >&2 || true
      echo "RDT sampler did not become ready" >&2
      exit 1
    fi
    sleep 1
  done

  echo "Running $run"
  ./iris-benchctl run \
    --config config/wave1-sweep.json --host "$HOST" --profile "ladder-$n" --run-id "$run" \
    --iris-digest "$IRIS" --judger-digest "$JUDGER" --benchmark-image "$IMAGE" \
    --bench-binary "$REMOTE/workload" --bench-binary-sha256 "$WORKLOAD" \
    --fixture cpp-runtime-v1=fixtures/cpp-runtime-v1/input.txt \
    --expected-output cpp-runtime-v1=fixtures/cpp-runtime-v1/output.txt \
    --production-compat --local-agent ./iris-bench-agent-linux-amd64 \
    --cgroup-parent "$PARENT" --worker-timeout 5m --ssh-control-path "$SOCKET" \
    2>&1 | tail -12

  ssh -o BatchMode=yes "$HOST" "docker stop $CURRENT_CONTAINER" >/dev/null
  CURRENT_CONTAINER=
  ./iris-benchctl collect --config config/wave1-sweep.json --host "$HOST" --run "$run" --ssh-control-path "$SOCKET"
  scp -o BatchMode=yes "$HOST:$REMOTE/$run.ndjson" "runs/rdt-20261031/$run.ndjson"
  python3 analysis/summarize-rdt.py \
    --receipt "runs/$run/receipts/ladder-$n-01.json" \
    --rdt "runs/rdt-20261031/$run.ndjson" \
    > "runs/rdt-20261031/$run.summary.json"
  echo "Completed $run"
done
