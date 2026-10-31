# Passive CMT and MBM measurement

`cmd/iris-bench-rdt` samples Intel resctrl `L3_MON` counters once per second. It reads LLC occupancy and total/local memory bandwidth for each LLC domain. It does not configure CAT, MBA, or CDP. It refuses to run when another resctrl control or monitoring group exists, since the default-group readings would then be incomplete.

The sampler runs in a privileged, network-disabled container and writes NDJSON plus a readiness marker to a host bind mount. The Judger load starts only after the readiness marker appears. `analysis/summarize-rdt.py` filters the samples to the run receipt's steady window and computes mean/max LLC occupancy and MBM rates from counter deltas. `analysis/plot-rdt.py` renders an SVG chart from the six summary JSON files and the corresponding `iris-benchctl analyze` JSON files.

The 2026-10-31 reproduction is captured in `scripts/run-rdt-judger-ladder.sh`. It is an experiment record with explicit host, image, workload, and digest values; review these before reusing it. Its outputs are under `runs/iris-20261031-rdt-*` and `runs/rdt-20261031`. `runs/` is ignored by Git.

These CMT/MBM readings are for all tasks in the host's resctrl default group. They cannot establish Judger-only LLC occupancy or bandwidth. MBA and CDP are allocation controls, so there is no passive MBA/CDP metric in this run. The production-compat run manifests are marked `comparable=false` because no qualification report was supplied; compare ladder stages only within this experiment and do not treat the figures as a qualified production baseline.
