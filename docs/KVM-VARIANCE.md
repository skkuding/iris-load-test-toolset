# KVM-wrapped Judger variance experiment

Question: does wrapping the judge execution flow (stock alpha.4 `libjudger.so`
driven by `judger-bench`) in KVM virtual machines reduce the variance of the
reported execution time, and how does the answer change with the number of
flows per VM (k), the number of VMs (m), the workload type, the host clock
policy, and per-VM cache allocation (CAT)?

Everything runs directly on `codedang8` (2× Xeon E5-2620 v4, kernel
7.0.0-34-generic) on **socket 0 only**. There is no container, libvirt, Kata,
or other abstraction: each VM is a plain `qemu-system-x86_64` process.

## Execution flow

One flow is `judger-bench --mode execute` → stock alpha.4 Judger (SHA-256
`2c9a4da8…`, `production-compat`, uid/gid 0, `c_cpp` seccomp) → a
precompiled binary. Compilation is out of scope. Limits are raised to 10 s
CPU / 30 s real so that slow contended samples are still measured.

| Workload | Binary and input | Stresses |
| --- | --- | --- |
| `compute` | `cpp-runtime-v1` (`71100d72…`), 160 000 000 iterations | ALU, register-bound |
| `chase6` | `wl chase 6`, 20 000 000 hops | 6 MiB pointer chase (L3) |
| `chase44` | `wl chase 44`, 4 000 000 hops | 44 MiB pointer chase (DRAM latency, TLB) |
| `stream44` | `wl stream 44`, 85 passes | 44 MiB sequential read (DRAM bandwidth) |

`wl` is `iris-variance-exp/wl.c` (`gcc -O2`), the same fixtures as the
2026-10-29 4th test. All flows of one run use the same workload.

**Always busy.** Each flow does 3 discarded warm-up iterations, waits at a
shared barrier, then runs back to back with no fixed count. The host sends
the stop signal only after every flow has at least 52 samples starting inside
the all-flows-active window. Analysis keeps only samples fully inside
`[max(first start), min(last end)]`, so each flow contributes about 50
samples measured while every other flow was running.

## Setups (arms)

CPU slots on socket 0, filled in this fixed order so that every arm uses
identical logical CPUs for a given width:

- `prod` policy (SMT on): `1 2 3 4 5 6 7 17 18 19 20 21 22 23` (cores first,
  then siblings); housekeeping CPUs `0,16`.
- `quiet` policy (SMT off): `1 … 7`; housekeeping CPU `0`.

| Arm | Layout |
| --- | --- |
| B0 | bare metal, one flow pinned per slot (`numactl --physcpubind=<cpu> --membind=0`) |
| B1 | bare metal, m groups of k consecutive slots; k flows float inside their group (`--physcpubind=<group>`) |
| Vk | m VMs × k vCPUs, k flows per VM; vCPU *i* of VM *j* pinned to slot `j·k+i` |
| Vk+1 | m VMs × (k+1) vCPUs, k flows per VM (one spare vCPU), same consecutive-slot rule |

k = 1 … 7 and every m with `width·m ≤ slots`. B1 with k = 1 equals B0. No
vCPU or flow is overcommitted.

## Guest

- `qemu-system-x86_64` 8.2.2 (Ubuntu 1:8.2.2+ds-0ubuntu1.18), `-enable-kvm
  -machine q35 -cpu host,migratable=off -nodefaults -display none`, no network.
- Direct kernel boot of the host's own `vmlinuz-7.0.0-34-generic` (virtio
  drivers built in); read-only `ubuntu-base-24.04.4` root filesystem on
  virtio-blk shared by all guests; `scripts/kvm/guest-init.sh` is PID 1 (no
  systemd). `/tmp` is tmpfs.
- Memory `1024 + 256·k` MiB from `memory-backend-ram,host-nodes=0,policy=bind,
  prealloc=on,merge=off`. QEMU starts with `PR_SET_THP_DISABLE`, so guest RAM is
  backed by **4 KiB pages**; each run records `AnonHugePages` (must be 0) and
  per-node page counts from `numa_maps`.
- QEMU is started paused (`-S`) with affinity on the housekeeping CPUs; vCPU
  threads are pinned through QMP `query-cpus-fast` before `cont`. Main, I/O and
  RCU threads stay on housekeeping CPUs.
- Control and per-flow sample streams use virtio-serial ports (no network,
  no shared filesystem). The host sends its `CLOCK_REALTIME` with START and
  STOP; the guest replies with its own clock, and analysis shifts guest
  timestamps onto the host clock (drift is recorded per run).
- Guest kernel has `CONFIG_PARAVIRT_TIME_ACCOUNTING` unset, so host
  preemption of a vCPU would be counted as guest CPU time — which is what Iris
  would report in such a VM. Guest steal time and host vCPU-thread run-queue
  wait (`schedstat`) are recorded to show how much of that occurred.

## Phases

| Phase | Host policy | CAT | Arms | Runs per round |
| --- | --- | --- | --- | ---: |
| p1 | prod: SMT on, turbo on, governor performance | none | B0 B1 Vk Vk+1 | 356 |
| p2 | quiet: SMT off, turbo off, governor performance | none | B0 B1 Vk Vk+1 | 164 |
| p3a | prod | equal, disjoint L3 ways per group | B0 B1 Vk | 272 |
| p3b | quiet | equal, disjoint L3 ways per group | B0 B1 Vk | 128 |

Three rounds per phase; the host is rebooted before every round and the run
order is shuffled with a per-round seed. Before each run: 10 s cool-down,
then socket 0 must be ≥ 98 % idle for 5 s with no leftover QEMU/Judger
process. CAT groups (p3) split the 18 non-shareable ways of the socket-0 L3
equally among the m groups (`floor(18/m)` ways, e.g. 1 way at m = 14); for
VMs only the vCPU threads join the group. CMT/MBM and per-CPU frequency are
sampled every second.

## Reproduction

```bash
# workstation
CGO_ENABLED=0 go build -trimpath -o bin/judger-bench-linux-amd64 ./cmd/judger-bench
scp scripts/kvm/* codedang8:/var/tmp/iris-kvm/scripts/
scp bin/judger-bench-linux-amd64 codedang8:/var/tmp/iris-kvm/judger-bench.upload
# codedang8 (qemu-system-x86 and qemu-utils installed)
bash /var/tmp/iris-kvm/scripts/stage-assets.sh /var/tmp/iris-kvm/judger-bench.upload
sudo bash /var/tmp/iris-kvm/scripts/build-guest.sh
# workstation: reboots, applies policy, runs and collects every round
KVMEXP_SUDO_PASSWORD=… scripts/kvm/driver.sh p1 p2 p3a p3b
python3 scripts/kvm/analyze-kvm.py runs/kvm/p1-r{1,2,3} … --out summary.json --csv cells.csv
python3 scripts/kvm/plot-kvm.py summary.json charts/
```

`kvmexp-run.py` resumes a partially finished round, retries a failed run
once, and records failures in `errors.log`. Raw run directories stay out of
Git (`runs/`).

## Metrics

Per cell (phase, arm, k, m, workload), pooled over the three rounds, for
both `cpuTime` and `realTime`: n, mean, SD, CV, median, MAD, p05, p95, p99,
max, p99/median, and the range of per-round medians. Relative columns:

- `median_vs_bare_solo`, `tail_drift_vs_bare_solo`: against B0 with one flow.
- `tail_drift_vs_own_solo`: against the same arm and k with m = 1 (B0/B1:
  bare solo; Vk/Vk+1: the single-VM cell), max(|p05|, |p95|) relative to that
  median.
- `median_vs_bare_same_n`: overhead against B0 at the same total flows.
- `median_vs_b1_same_layout`: against bare metal on exactly the same CPUs and
  grouping (Vk vs B1), isolating the virtualization effect from partitioning.
- `passes_1106_vs_own_solo`: the 2026-11-06 reference (tail drift ≤ 5 %,
  CV ≤ 5 %, zero failures). Reported only; the team has not adopted a
  threshold.

## 2026-11-14 – 2026-11-16 result

All 2,760 runs (p1 1,068, p2 492, p3a 816, p3b 384) completed with zero
degraded runs and zero failed samples. Guest RAM was 4 KiB-backed in every
run, guest steal and host vCPU wait stayed below 0.2 %, and per-cell medians
varied by a median 0.3–0.6 % between rebooted rounds.

**KVM did not reduce execution-time variance.** On identical CPUs and
grouping, Vk's CPU-time CV matched bare metal (B1/B0) within about ±0.5 pp for
`compute`, `chase44`, and `stream44`, and was slightly higher in most cells.
Variance followed the total number of concurrent flows on socket 0 (SMT
sibling sharing, L3 thrash, memory bandwidth), not the number of VMs or flows
per VM. A spare vCPU (Vk+1) did not help, and per-group CAT behaved the same
with and without VMs. The VM wrapper cost +0.7–2 % for most workloads and a
consistent +15–19 % for the DRAM-latency workload (`chase44`, 4 KiB guest
pages). Details, tables, and charts: `results/kvm-variance/report.md`,
`tables.md`, `cells.csv`, and `charts/`.
