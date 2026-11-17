#!/usr/bin/env bash
# Unattended phase driver, run from the operator workstation.
# usage: KVMEXP_SUDO_PASSWORD=... driver.sh <phase>... (phases: p1 p2 p3a p3b)
# For every phase and round 1..3: reboot the host, apply the phase policy,
# generate the randomized round plan, start kvmexp-run.py as a transient
# systemd unit, poll it, and copy results back. Re-running resumes: a running
# round is only polled, and a finished round is skipped.
set -uo pipefail
: "${KVMEXP_SUDO_PASSWORD:?set KVMEXP_SUDO_PASSWORD}"
host=${KVMEXP_HOST:-codedang8}
stage=/var/tmp/iris-kvm
here=$(cd "$(dirname "$0")" && pwd)
local_runs=${KVMEXP_LOCAL_RUNS:-$here/../../runs/kvm}
rounds=${KVMEXP_ROUNDS:-1 2 3}
ssh_opts=(-o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=15 -o ServerAliveCountMax=4
  -o ControlMaster=no -o ControlPath=none)
mkdir -p "$local_runs"

log() { echo "$(date '+%F %T') $*"; }
r() { ssh "${ssh_opts[@]}" "$host" "$@"; }
rsudo() { printf '%s\n' "$KVMEXP_SUDO_PASSWORD" | ssh "${ssh_opts[@]}" "$host" "sudo -S -p '' $*"; }
retry() { local i; for i in $(seq 1 30); do "$@" && return 0; sleep 60; done; return 1; }

policy_of() { case $1 in p1|p3a) echo prod ;; p2|p3b) echo quiet ;; *) return 1 ;; esac; }

round_state() { # prints: finished | running | incomplete
  local phase=$1 round=$2 unit=kvmexp-$1-r$2
  r "systemctl is-active --quiet $unit" 2>/dev/null && { echo running; return; }
  r "python3 -c 'import json,sys; d=json.load(open(\"$stage/runs/$phase-r$round/progress.json\")); sys.exit(0 if d.get(\"finished\") and d[\"done\"]==d[\"total\"] else 1)'" 2>/dev/null \
    && { echo finished; return; }
  echo incomplete
}

reboot_host() {
  log "rebooting $host"
  rsudo "systemctl reboot" > /dev/null 2>&1 || true
  sleep 30
  for i in $(seq 1 60); do
    if r true 2>/dev/null; then
      local up; up=$(r "cut -d. -f1 /proc/uptime" 2>/dev/null || echo 99999)
      if (( up < 1800 )); then log "$host is back (uptime ${up}s)"; sleep 90; return 0; fi
    fi
    sleep 20
  done
  log "host did not come back"; return 1
}

sync_round() {
  retry rsync -a -e "ssh ${ssh_opts[*]}" "$host:$stage/runs/$1-r$2" "$local_runs/"
}

for phase in "$@"; do
  policy=$(policy_of "$phase") || { log "unknown phase $phase"; exit 2; }
  for round in $rounds; do
    unit=kvmexp-$phase-r$round
    state=$(retry round_state "$phase" "$round")
    log "$phase round $round: $state"
    if [[ $state == finished ]]; then sync_round "$phase" "$round"; continue; fi
    if [[ $state == incomplete ]]; then
      retry reboot_host || exit 1
      retry rsudo "bash $stage/scripts/host-policy.sh $policy" || { log "policy failed"; exit 1; }
      retry r "mkdir -p $stage/plans && python3 $stage/scripts/kvmexp-plan.py $phase $round > $stage/plans/$phase-r$round.json" || exit 1
      retry rsudo "systemd-run --unit=$unit --collect --setenv=KVMEXP_STAGE=$stage \
        -p StandardOutput=append:$stage/runs/$unit.log -p StandardError=append:$stage/runs/$unit.log \
        python3 $stage/scripts/kvmexp-run.py --plan $stage/plans/$phase-r$round.json --out $stage/runs/$phase-r$round" \
        || { log "launch failed"; exit 1; }
      log "started $unit"
    fi
    while true; do
      sleep 300
      if r "systemctl is-active --quiet $unit" 2>/dev/null; then
        r "python3 -c 'import json; d=json.load(open(\"$stage/runs/$phase-r$round/progress.json\")); print(\"progress\", d[\"done\"], \"/\", d[\"total\"], \"ok\", d[\"ok\"], \"degraded\", d[\"degraded\"], \"failed\", len(d[\"failed\"]), d.get(\"current\"))'" 2>/dev/null \
          | while read -r line; do log "$unit $line"; done
        continue
      fi
      state=$(retry round_state "$phase" "$round")
      [[ $state == running ]] && continue
      break
    done
    sync_round "$phase" "$round"
    if [[ $state != finished ]]; then
      log "$unit stopped before finishing; see $stage/runs/$unit.log"; exit 1
    fi
    log "$unit finished"
  done
done
log "all requested phases finished"
