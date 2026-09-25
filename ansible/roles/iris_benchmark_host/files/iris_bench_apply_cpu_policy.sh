#!/usr/bin/env bash
# Apply and report the effective CPU policy for the Iris benchmark host.
#
# Managed by the iris_benchmark_host Ansible role. Runs as root from
# iris-bench-cpu-policy.service and interactively during provisioning.
#
# Supported arguments:
#   --governor <name>            target cpufreq governor (e.g. performance)
#   --turbo <enabled|disabled|placeholder>
#   --smt <enabled|disabled|detect>
#
# `placeholder` and `detect` are read-only: the current effective state is
# recorded, nothing is written, and the run is marked non-comparable.
#
# Prints a single JSON object on stdout. Exit status is 0 unless arguments are
# invalid.
set -euo pipefail

GOVERNOR=""
TURBO="placeholder"
SMT="detect"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --governor) GOVERNOR="${2:-}"; shift 2 ;;
    --turbo) TURBO="${2:-}"; shift 2 ;;
    --smt) SMT="${2:-}"; shift 2 ;;
    --help|-h)
      echo "usage: $0 --governor NAME --turbo enabled|disabled|placeholder --smt enabled|disabled|detect" >&2
      exit 0 ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2 ;;
  esac
done

changed=0

# ---- governor -------------------------------------------------------------
governor_effective="unavailable"
if [[ -n "$GOVERNOR" ]]; then
  shopt -s nullglob
  policies=(/sys/devices/system/cpu/cpufreq/policy*/scaling_governor)
  if [[ ${#policies[@]} -gt 0 ]]; then
    governor_effective="$GOVERNOR"
    for f in "${policies[@]}"; do
      current="$(cat "$f" 2>/dev/null || echo unknown)"
      if [[ "$current" != "$GOVERNOR" ]]; then
        if printf '%s' "$GOVERNOR" > "$f" 2>/dev/null; then
          changed=1
        fi
      fi
      current="$(cat "$f" 2>/dev/null || echo unknown)"
      if [[ "$current" != "$GOVERNOR" ]]; then
        governor_effective="$current"
      fi
    done
  fi
fi

# ---- turbo ----------------------------------------------------------------
turbo_effective="unavailable"
if [[ -r /sys/devices/system/cpu/intel_pstate/no_turbo ]]; then
  no_turbo="$(cat /sys/devices/system/cpu/intel_pstate/no_turbo)"
  turbo_effective="$([[ "$no_turbo" == "1" ]] && echo disabled || echo enabled)"
  case "$TURBO" in
    disabled) [[ "$no_turbo" == "1" ]] || { printf '1' > /sys/devices/system/cpu/intel_pstate/no_turbo && changed=1; } ;;
    enabled)  [[ "$no_turbo" == "0" ]] || { printf '0' > /sys/devices/system/cpu/intel_pstate/no_turbo && changed=1; } ;;
  esac
  no_turbo="$(cat /sys/devices/system/cpu/intel_pstate/no_turbo 2>/dev/null || echo unknown)"
  [[ "$no_turbo" == "1" ]] && turbo_effective="disabled" || turbo_effective="enabled"
elif [[ -r /sys/devices/system/cpu/cpufreq/boost ]]; then
  boost="$(cat /sys/devices/system/cpu/cpufreq/boost)"
  turbo_effective="$([[ "$boost" == "0" ]] && echo disabled || echo enabled)"
  case "$TURBO" in
    disabled) [[ "$boost" == "0" ]] || { printf '0' > /sys/devices/system/cpu/cpufreq/boost && changed=1; } ;;
    enabled)  [[ "$boost" == "1" ]] || { printf '1' > /sys/devices/system/cpu/cpufreq/boost && changed=1; } ;;
  esac
  boost="$(cat /sys/devices/system/cpu/cpufreq/boost 2>/dev/null || echo unknown)"
  [[ "$boost" == "0" ]] && turbo_effective="disabled" || turbo_effective="enabled"
fi

# ---- SMT ------------------------------------------------------------------
smt_effective="unavailable"
if [[ -r /sys/devices/system/cpu/smt/control ]]; then
  smt_effective="$(cat /sys/devices/system/cpu/smt/control)"
  case "$SMT" in
    disabled)
      if [[ "$smt_effective" != "off" && "$smt_effective" != "forceoff" ]]; then
        printf 'off' > /sys/devices/system/cpu/smt/control 2>/dev/null && changed=1 || true
      fi ;;
    enabled)
      if [[ "$smt_effective" != "on" ]]; then
        printf 'on' > /sys/devices/system/cpu/smt/control 2>/dev/null && changed=1 || true
      fi ;;
  esac
  smt_effective="$(cat /sys/devices/system/cpu/smt/control 2>/dev/null || echo unavailable)"
fi

printf '{"ok":true,"changed":%s,"governor":{"policy":"%s","effective":"%s"},"turbo":{"policy":"%s","effective":"%s"},"smt":{"policy":"%s","effective":"%s"}}\n' \
  "$changed" "$GOVERNOR" "$governor_effective" "$TURBO" "$turbo_effective" "$SMT" "$smt_effective"
