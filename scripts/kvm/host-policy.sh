#!/usr/bin/env bash
# Apply and verify a host policy on codedang8. Run as root.
# usage: host-policy.sh prod|quiet
#   prod   SMT on, turbo on, governor performance   (production nodes)
#   quiet  SMT off, turbo off, governor performance (11/06 baseline)
# Also mounts resctrl and refuses to continue if foreign resctrl groups exist.
# Nothing here persists across reboot.
set -euo pipefail
policy=${1:?prod|quiet}
case $policy in
  prod) smt=on no_turbo=0 ;;
  quiet) smt=off no_turbo=1 ;;
  *) echo "unknown policy $policy" >&2; exit 2 ;;
esac
cur=$(cat /sys/devices/system/cpu/smt/control)
[[ $cur == "$smt" ]] || echo "$smt" > /sys/devices/system/cpu/smt/control
echo "$no_turbo" > /sys/devices/system/cpu/intel_pstate/no_turbo
for g in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do echo performance > "$g"; done
mountpoint -q /sys/fs/resctrl || mount -t resctrl resctrl /sys/fs/resctrl
foreign=$(find /sys/fs/resctrl -mindepth 1 -maxdepth 1 -type d ! -name info ! -name mon_data ! -name mon_groups ! -name 'kvmexp-g*')
[[ -z $foreign ]] || { echo "foreign resctrl groups: $foreign" >&2; exit 1; }
sleep 2
printf 'policy=%s smt_active=%s no_turbo=%s governors=%s online=%s\n' "$policy" \
  "$(cat /sys/devices/system/cpu/smt/active)" \
  "$(cat /sys/devices/system/cpu/intel_pstate/no_turbo)" \
  "$(sort -u /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor | tr '\n' ',')" \
  "$(cat /sys/devices/system/cpu/online)"
