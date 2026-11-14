#!/usr/bin/env python3
"""Summarize KVM variance runs.

usage: analyze-kvm.py <runs-root> [<runs-root> ...] --out summary.json [--csv cells.csv]

<runs-root> holds <run-id>/meta.json directories written by kvmexp-run.py
(one root per phase round, or any parent; directories are found recursively).

Per run: samples fully inside the all-flows-active window
[max(first start), min(last end)], with guest timestamps shifted to host time.
Per cell (phase, arm, k, m, workload): pooled statistics over rounds, per-round
medians, solo-relative tail drift, and overhead against bare metal.
"""
import argparse
import csv
import glob
import json
import math
import os
import re
import statistics
from collections import defaultdict

FLOW_RE = re.compile(r"(?:vm(\d+)-)?flow-(\d+)\.ndjson$")


def pct(xs, p):
    xs = sorted(xs)
    if not xs:
        return math.nan
    i = (len(xs) - 1) * p
    lo, hi = math.floor(i), math.ceil(i)
    return xs[lo] + (xs[hi] - xs[lo]) * (i - lo)


def stats(xs):
    if not xs:
        return {"n": 0}
    med = statistics.median(xs)
    mean = statistics.fmean(xs)
    sd = statistics.stdev(xs) if len(xs) > 1 else 0.0
    return {"n": len(xs), "mean": mean, "sd": sd, "cv": sd / mean if mean else math.nan,
            "median": med, "mad": statistics.median(abs(x - med) for x in xs),
            "p05": pct(xs, 0.05), "p95": pct(xs, 0.95), "p99": pct(xs, 0.99),
            "min": min(xs), "max": max(xs), "p99_over_median": pct(xs, 0.99) / med if med else math.nan}


def load_run(d):
    meta = json.load(open(f"{d}/meta.json"))
    if meta.get("status") not in ("ok", "degraded"):
        return None
    flows = {}
    for path in glob.glob(f"{d}/*flow-[0-9][0-9].ndjson"):
        m = FLOW_RE.search(os.path.basename(path))
        vm = m.group(1)
        off = meta["guests"][str(int(vm))]["offset_ns"] if vm else 0
        rows = [json.loads(l) for l in open(path) if l.strip()]
        for r in rows:
            r["s"], r["e"] = r["startedAtNs"] + off, r["endedAtNs"] + off
        flows[os.path.basename(path)] = rows
    ws = max(r[0]["s"] for r in flows.values())
    we = min(r[-1]["e"] for r in flows.values())
    inside = [r for rows in flows.values() for r in rows if r["s"] >= ws and r["e"] <= we]
    per_flow = [sum(1 for r in rows if r["s"] >= ws and r["e"] <= we) for rows in flows.values()]
    bad = [r for r in inside if r["status"] != "success" or not r.get("outputMatches")]
    spec = meta["spec"]
    out = {"run_id": spec["run_id"], "phase": spec.get("phase", spec["run_id"].split("-")[0]),
           "round": spec.get("round"), "policy": spec["policy"], "cat": spec["cat"],
           "arm": spec["arm"], "k": spec["k"], "m": spec["m"], "workload": spec["workload"],
           "flows": meta["total_flows"], "status": meta["status"],
           "window_s": (we - ws) / 1e9, "min_flow_samples": min(per_flow), "bad_samples": len(bad),
           "cpu": [r["cpuTimeMs"] for r in inside if r["status"] == "success"],
           "real": [r["realTimeMs"] for r in inside if r["status"] == "success"],
           "idle_busy": meta["idle_gate"]["socket0_busy"]}
    if "guests" in meta:
        steal, waitf, huge, offdrift = [], [], [], []
        for g in meta["guests"].values():
            a, b = g["stat_start"], g["stat_stop"]
            tot = sum(b[x] - a[x] for x in b)
            steal.append((b["steal"] - a["steal"]) / tot if tot else 0)
            r0 = sum(v["run_ns"] for v in g["schedstat_start"].values())
            r1 = sum(v["run_ns"] for v in g["schedstat_stop"].values())
            w0 = sum(v["wait_ns"] for v in g["schedstat_start"].values())
            w1 = sum(v["wait_ns"] for v in g["schedstat_stop"].values())
            waitf.append((w1 - w0) / (r1 - r0) if r1 > r0 else 0)
            huge.append(g.get("anon_huge_kb"))
            offdrift.append(abs(g["stop_offset_ns"] - g["offset_ns"]) / 1e6)
        out.update(guest_steal_frac=max(steal), vcpu_wait_frac=max(waitf),
                   anon_huge_kb_max=max(h or 0 for h in huge), clock_offset_drift_ms=max(offdrift),
                   boot_to_ready_s=meta.get("boot_to_ready_s"))
    samp = [json.loads(l) for l in open(f"{d}/sampler.ndjson") if l.strip()]
    samp = [s for s in samp if ws <= s["t"] <= we]
    if samp:
        out["freq_mhz_mean"] = statistics.fmean(v for s in samp for v in s["freq_khz"].values()) / 1000
        if len(samp) > 1:
            dt = (samp[-1]["t"] - samp[0]["t"]) / 1e9
            out["mbm_root_mib_s"] = (samp[-1]["root"]["mbm_total_bytes"] - samp[0]["root"]["mbm_total_bytes"]) / dt / 2**20
    return out


def cell_key(r):
    return (r["phase"], r["arm"], r["k"], r["m"], r["workload"])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("roots", nargs="+")
    ap.add_argument("--out", required=True)
    ap.add_argument("--csv")
    a = ap.parse_args()
    runs = []
    for root in a.roots:
        for meta in sorted(glob.glob(f"{root}/**/meta.json", recursive=True)):
            if ".fail-" in meta:
                continue
            r = load_run(os.path.dirname(meta))
            if r:
                runs.append(r)
    cells = defaultdict(list)
    for r in runs:
        cells[cell_key(r)].append(r)
    summary = {}
    for key, rs in cells.items():
        phase, arm, k, m, wl = key
        c = {"phase": phase, "arm": arm, "k": k, "m": m, "workload": wl,
             "flows": rs[0]["flows"], "policy": rs[0]["policy"], "cat": rs[0]["cat"],
             "rounds": len(rs), "degraded_runs": sum(r["status"] != "ok" for r in rs),
             "bad_samples": sum(r["bad_samples"] for r in rs)}
        for metric in ("cpu", "real"):
            pooled = [x for r in rs for x in r[metric]]
            c[metric] = stats(pooled)
            meds = [statistics.median(r[metric]) for r in rs if r[metric]]
            c[metric]["round_medians"] = meds
            c[metric]["round_median_range"] = (max(meds) - min(meds)) / statistics.median(meds) if meds else math.nan
        for extra in ("guest_steal_frac", "vcpu_wait_frac", "anon_huge_kb_max", "clock_offset_drift_ms",
                      "boot_to_ready_s", "freq_mhz_mean", "mbm_root_mib_s", "window_s", "min_flow_samples"):
            vals = [r[extra] for r in rs if r.get(extra) is not None]
            if vals:
                c[extra] = max(vals) if extra in ("anon_huge_kb_max", "clock_offset_drift_ms") else statistics.fmean(vals)
        summary["/".join(map(str, key))] = c

    def get(phase, arm, k, m, wl):
        return summary.get(f"{phase}/{arm}/{k}/{m}/{wl}")

    for c in summary.values():
        ph, wl = c["phase"], c["workload"]
        refs = {"bare_solo": get(ph, "b0", 1, 1, wl),
                "vm_solo": get(ph, "vk", 1, 1, wl),
                "own_solo": get(ph, c["arm"], c["k"], 1, wl) if c["arm"] != "b0" else get(ph, "b0", 1, 1, wl),
                "bare_same_n": get(ph, "b0", 1, c["flows"], wl),
                "b1_same_layout": get(ph, "b1", c["k"], c["m"], wl) if c["k"] > 1 else get(ph, "b0", 1, c["m"], wl)}
        for metric in ("cpu", "real"):
            s = c[metric]
            for name, ref in refs.items():
                if not ref or not ref[metric].get("n"):
                    continue
                base = ref[metric]["median"]
                s[f"median_vs_{name}"] = s["median"] / base - 1
                s[f"tail_drift_vs_{name}"] = max(abs(s["p05"] / base - 1), abs(s["p95"] / base - 1))
            s["passes_1106_vs_own_solo"] = (s.get("tail_drift_vs_own_solo", math.inf) <= 0.05
                                            and s["cv"] <= 0.05 and c["bad_samples"] == 0)
    json.dump({"runs": [{k: v for k, v in r.items() if k not in ("cpu", "real")} for r in runs],
               "cells": summary}, open(a.out, "w"), indent=1, sort_keys=True, default=str)
    if a.csv:
        cols = ["phase", "policy", "cat", "arm", "k", "m", "flows", "workload", "rounds", "bad_samples"]
        mcols = ["n", "median", "cv", "mad", "p05", "p95", "p99", "max", "p99_over_median",
                 "round_median_range", "median_vs_bare_solo", "median_vs_bare_same_n", "median_vs_b1_same_layout",
                 "tail_drift_vs_own_solo", "tail_drift_vs_bare_solo", "passes_1106_vs_own_solo"]
        xcols = ["guest_steal_frac", "vcpu_wait_frac", "anon_huge_kb_max", "freq_mhz_mean", "mbm_root_mib_s"]
        with open(a.csv, "w", newline="") as f:
            w = csv.writer(f)
            w.writerow(cols + [f"cpu_{x}" for x in mcols] + [f"real_{x}" for x in mcols] + xcols)
            for c in sorted(summary.values(), key=lambda c: (c["phase"], c["workload"], c["arm"], c["k"], c["m"])):
                w.writerow([c[x] for x in cols] + [c["cpu"].get(x) for x in mcols]
                           + [c["real"].get(x) for x in mcols] + [c.get(x) for x in xcols])
    print(f"{len(runs)} runs, {len(summary)} cells")


if __name__ == "__main__":
    main()
