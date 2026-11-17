#!/usr/bin/env python3
"""Render the KVM variance summary (analyze-kvm.py output) as Markdown tables.

usage: report-kvm.py summary.json > tables.md
"""
import json
import statistics
import sys

WORKLOADS = ("compute", "chase6", "chase44", "stream44")
PHASES = {"p1": "prod, no CAT", "p2": "quiet, no CAT", "p3a": "prod + CAT", "p3b": "quiet + CAT"}


def pct(x, digits=1):
    return f"{100 * x:+.{digits}f}%"


def rng(xs, scale=100, unit="%"):
    if not xs:
        return "—"
    return (f"{scale * statistics.median(xs):+.1f}{unit} "
            f"[{scale * min(xs):+.1f}, {scale * max(xs):+.1f}]")


def main():
    data = json.load(open(sys.argv[1]))
    cells, runs = data["cells"], data["runs"]
    get = lambda ph, arm, k, m, wl: cells.get(f"{ph}/{arm}/{k}/{m}/{wl}")  # noqa: E731
    phases = [p for p in PHASES if any(c["phase"] == p for c in cells.values())]
    out = []
    w = out.append

    w("## Run inventory\n")
    w("| Phase | Settings | Runs | ok | degraded | bad samples | max AnonHugePages (KiB) | max guest steal | max vCPU wait | max clock drift (ms) |")
    w("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
    for ph in phases:
        rs = [r for r in runs if r["phase"] == ph]
        cs = [c for c in cells.values() if c["phase"] == ph]
        mx = lambda key: max((c.get(key) or 0) for c in cs)  # noqa: E731
        w(f"| {ph} | {PHASES[ph]} | {len(rs)} | {sum(r['status'] == 'ok' for r in rs)} | "
          f"{sum(r['status'] != 'ok' for r in rs)} | {sum(c['bad_samples'] for c in cs)} | "
          f"{mx('anon_huge_kb_max'):.0f} | {mx('guest_steal_frac'):.4f} | {mx('vcpu_wait_frac'):.4f} | "
          f"{mx('clock_offset_drift_ms'):.1f} |")

    w("\n## VM against bare metal on identical CPUs (Vk vs B1, or B0 when k = 1)\n")
    w("Median [min, max] over all (k, m) cells. CPU time.\n")
    w("| Phase | Workload | cells | median overhead | CV difference (pp) | p95 difference | VM CV ≤ bare CV |")
    w("| --- | --- | ---: | --- | --- | --- | ---: |")
    for ph in phases:
        for wl in WORKLOADS:
            ov, cvd, p95, better = [], [], [], 0
            for c in cells.values():
                if c["phase"] != ph or c["workload"] != wl or c["arm"] != "vk":
                    continue
                r = get(ph, "b1", c["k"], c["m"], wl) if c["k"] > 1 else get(ph, "b0", 1, c["m"], wl)
                if not r:
                    continue
                ov.append(c["cpu"]["median"] / r["cpu"]["median"] - 1)
                cvd.append(c["cpu"]["cv"] - r["cpu"]["cv"])
                p95.append(c["cpu"]["p95"] / r["cpu"]["p95"] - 1)
                better += c["cpu"]["cv"] <= r["cpu"]["cv"]
            if ov:
                w(f"| {ph} | {wl} | {len(ov)} | {rng(ov)} | {rng(cvd, unit=' pp')} | {rng(p95)} | {better}/{len(ov)} |")

    w("\n## Spare vCPU: Vk+1 against Vk at the same (k, m)\n")
    w("| Phase | Workload | cells | median shift | CV difference (pp) |")
    w("| --- | --- | ---: | --- | --- |")
    for ph in phases:
        for wl in WORKLOADS:
            md, cvd = [], []
            for c in cells.values():
                if c["phase"] == ph and c["workload"] == wl and c["arm"] == "vk1":
                    r = get(ph, "vk", c["k"], c["m"], wl)
                    if r:
                        md.append(c["cpu"]["median"] / r["cpu"]["median"] - 1)
                        cvd.append(c["cpu"]["cv"] - r["cpu"]["cv"])
            if md:
                w(f"| {ph} | {wl} | {len(md)} | {rng(md)} | {rng(cvd, unit=' pp')} |")

    pairs = [("p3a", "p1"), ("p3b", "p2")]
    if any(a in phases and b in phases for a, b in pairs):
        w("\n## CAT per group against no CAT (same policy, arm, k, m)\n")
        w("| Policy | Arm | Workload | cells | median shift | CV difference (pp) |")
        w("| --- | --- | --- | ---: | --- | --- |")
        for a, b in pairs:
            if a not in phases or b not in phases:
                continue
            for arm in ("b0", "b1", "vk"):
                for wl in WORKLOADS:
                    md, cvd = [], []
                    for c in cells.values():
                        if c["phase"] == a and c["workload"] == wl and c["arm"] == arm:
                            r = get(b, arm, c["k"], c["m"], wl)
                            if r:
                                md.append(c["cpu"]["median"] / r["cpu"]["median"] - 1)
                                cvd.append(c["cpu"]["cv"] - r["cpu"]["cv"])
                    if md:
                        w(f"| {PHASES[b].split(',')[0]} | {arm} | {wl} | {len(md)} | {rng(md)} | {rng(cvd, unit=' pp')} |")

    w("\n## CPU-time CV (%) by flows per VM (k) and VM count (m)\n")
    w("Rows: B0 (by flow count), then Vk for each k, then Vk+1. Pooled over rounds.\n")
    for ph in phases:
        width = max(c["m"] for c in cells.values() if c["phase"] == ph)
        for wl in WORKLOADS:
            w(f"\n### {ph} ({PHASES[ph]}), {wl}\n")
            w("| row | " + " | ".join(f"m={m}" for m in range(1, width + 1)) + " |")
            w("| --- |" + " ---: |" * width)
            rows = [("B0", "b0", 1)] + [(f"Vk k={k}", "vk", k) for k in range(1, 8)] \
                + [(f"B1 k={k}", "b1", k) for k in range(2, 8)] + [(f"Vk+1 k={k}", "vk1", k) for k in range(1, 8)]
            for label, arm, k in rows:
                vals = [get(ph, arm, k, m, wl) for m in range(1, width + 1)]
                if not any(vals):
                    continue
                w(f"| {label} | " + " | ".join(f"{v['cpu']['cv'] * 100:.1f}" if v else "" for v in vals) + " |")

    w("\n## 2026-11-06 reference (tail drift ≤ 5 % vs own solo, CV ≤ 5 %, no failures)\n")
    w("| Phase | Workload | B0 | B1 | Vk | Vk+1 |")
    w("| --- | --- | --- | --- | --- | --- |")
    for ph in phases:
        for wl in WORKLOADS:
            row = []
            for arm in ("b0", "b1", "vk", "vk1"):
                cs = [c for c in cells.values() if c["phase"] == ph and c["workload"] == wl and c["arm"] == arm]
                row.append(f"{sum(c['cpu']['passes_1106_vs_own_solo'] for c in cs)}/{len(cs)}" if cs else "—")
            w(f"| {ph} | {wl} | " + " | ".join(row) + " |")
    print("\n".join(out))


if __name__ == "__main__":
    main()
