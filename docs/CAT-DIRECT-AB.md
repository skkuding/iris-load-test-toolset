# Host-direct Judger CAT A/B experiment

This experiment runs the pinned `cpp-runtime-v1` workload through the stock
`libjudger.so` on `codedang8`, with one direct host process per worker. The
baseline and equal-CAT runs use the same worker count, CPU placement, NUMA
memory binding, input, binary, library, and 50 iterations per worker.

## CAT allocation

The machine has two LLC domains with 20 ways (`cbm_mask=fffff`) and 16 CLOSIDs.
The top two ways are marked shareable (`c0000`), so this experiment uses the
other 18. At `n` workers, each LLC domain gets `n/2` workers and each worker
gets one disjoint mask of `floor(18/(n/2))` ways. A class ID is reused across
the two domains, but at most one active worker per domain enters that class.

| Workers | Classes | Ways per worker and LLC domain |
| ---: | ---: | ---: |
| 2 | 1 | 18 |
| 8 | 4 | 4 |
| 16 | 8 | 2 |
| 30 | 15 | 1 |

The masks are equal within a stage. Unused ways remain outside the experiment.
The default resctrl group retains full access, so host background processes may
still use the reserved masks. CAT groups control cache *allocation*, not a
strict residency quota or a bandwidth limit.

## Reproduction

Build `cmd/judger-bench` for Linux amd64 from this repository. Stage that binary,
`scripts/{cat-groups,cat-sample,direct-cat-worker,run-direct-cat-ab}.sh`, the
sealed workload binary, its input and expected output, and the pinned
`libjudger.so` at the paths in the scripts. The scripts verify these SHA-256
digests before executing:

- `libjudger.so`: `2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103`
- `cpp-runtime-v1` workload: `71100d7247185c5324c4f1f88ea4a149330cb11ca892dad422936053754d7ba3`

On the host, mount resctrl if it is not already mounted. Check that no
non-default resctrl groups exist before starting. The experiment requires root
because the stock Judger creates cgroups at the host cgroup root and CAT setup
writes to resctrl. Run it in an interactive sudo session:

```bash
sudo mount -t resctrl resctrl /sys/fs/resctrl
sudo bash /tmp/iris-cat-20261101/scripts/run-direct-cat-ab.sh \
  8:base:r1 8:equal:r1 16:base:r1 16:equal:r1 30:base:r1 30:equal:r1
```

The runner waits for host load average to fall below 0.7 before every run,
starts workers on a shared barrier, samples CMT/MBM once a second, then removes
its sandbox cgroups and CAT groups. For each run it writes per-worker NDJSON,
CAT mask proofs, logs, and an RDT TSV under `/tmp/iris-cat-20261101`.

Copy completed run files to one local directory and summarize them with:

```bash
python3 scripts/analyze-direct-cat.py runs/cat-direct-20261101 \
  iris-cat-20261101-r1-base-8 iris-cat-20261101-r1-equal-8 \
  iris-cat-20261101-r1-base-16 iris-cat-20261101-r1-equal-16 \
  iris-cat-20261101-r1-base-30 iris-cat-20261101-r1-equal-30 \
  > runs/cat-direct-20261101/summary.json
python3 scripts/plot-direct-cat.py \
  runs/cat-direct-20261101/summary.json runs/cat-direct-20261101/charts
```

The summary follows the toolset's steady-window rule: intersect each worker's
active interval, then include only samples wholly inside that interval. CMT
reports LLC occupancy. MBM reports memory-controller traffic, split into local
and remote NUMA traffic; it is not LLC traffic. The baseline's root resctrl
group also contains host background traffic, whereas the CAT-group sum targets
the assigned workers. Interpret baseline versus CAT CMT/MBM differences with
that scope caveat.

This is a direct-host `production-compat` experiment. The stock library creates
root-level sandbox cgroups, so the runner records `comparable=false` and
`cgroupContained=false` under the toolset's isolated-container checks. The
analysis separately validates each reported cgroup path and successful output.
Results can compare this A/B pair, but absolute values should not be combined
with the earlier container ladder. The pinned runtime fixture is mainly
compute-bound; this experiment alone cannot establish a benefit for
cache-sensitive judging workloads.

## 2026-10-31 preliminary result

The 8-, 16-, and 30-worker A/B pairs each completed 50 successful iterations
per worker. Steady-window CPU medians were 434→437, 497→498, and 570→571 ms
for baseline→CAT. CAT did not improve this compute-bound fixture in these
single paired runs. The machine-readable summary and four charts are in
`results/cat-direct-20261031/`; the [Notion report](https://app.notion.com/p/2026-10-31-Judger-CAT-A-B-40aef9cff54581c1b157cc9b48214af9)
contains the full table, interpretation, and limitations.
