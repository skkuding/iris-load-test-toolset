#!/usr/bin/env python3
"""Plot the KVM variance summary produced by analyze-kvm.py.

usage: plot-kvm.py summary.json <out-dir>

Per phase it writes:
  <phase>-cv-by-vmcount.png   CPU-time CV against VM count m, one line per k
                              (Vk solid, Vk+1 dashed), B0 against flow count
  <phase>-cv-by-flows.png     the same lines against total flows k·m
  <phase>-band-by-flows.png   CPU-time median and p05-p95 against total flows
                              for B0, B1, Vk and Vk+1
  <phase>-cv-heatmap.png      Vk CPU-time CV over k x m
"""
import json
import sys
from collections import defaultdict
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

WORKLOADS = ("compute", "chase6", "chase44", "stream44")
ARM_STYLE = {"b0": ("black", "o"), "b1": ("tab:green", "s"), "vk": ("tab:blue", "^"), "vk1": ("tab:orange", "v")}
TITLE = {"p1": "prod (SMT on, turbo on), no CAT", "p2": "quiet (SMT off, turbo off), no CAT",
         "p3a": "prod + CAT per group", "p3b": "quiet + CAT per group"}


def main():
    summary = json.load(open(sys.argv[1]))["cells"]
    out = Path(sys.argv[2])
    out.mkdir(parents=True, exist_ok=True)
    by_phase = defaultdict(list)
    for c in summary.values():
        by_phase[c["phase"]].append(c)
    for phase, cells in by_phase.items():
        title = TITLE.get(phase, phase)
        cmap = plt.get_cmap("viridis")

        fig, axes = plt.subplots(2, 2, figsize=(13, 9), sharex=True)
        for ax, wl in zip(axes.flat, WORKLOADS):
            cs = [c for c in cells if c["workload"] == wl]
            b0 = sorted((c["m"], c["cpu"]["cv"] * 100) for c in cs if c["arm"] == "b0")
            if b0:
                ax.plot(*zip(*b0), color="black", marker="o", lw=2, label="B0 (x = flows)")
            for arm, ls in (("vk", "-"), ("vk1", "--")):
                for k in range(1, 8):
                    pts = sorted((c["m"], c["cpu"]["cv"] * 100) for c in cs if c["arm"] == arm and c["k"] == k)
                    if pts:
                        ax.plot(*zip(*pts), ls=ls, marker=".", color=cmap((k - 1) / 6),
                                label=f"{'Vk' if arm == 'vk' else 'Vk+1'} k={k}")
            ax.set_title(wl)
            ax.set_ylabel("CPU time CV (%)")
            ax.set_yscale("symlog", linthresh=1)
            ax.grid(alpha=0.3)
        for ax in axes[1]:
            ax.set_xlabel("VM count m (B0: flow count)")
        h, l = axes[0, 0].get_legend_handles_labels()
        fig.legend(h, l, loc="center right", fontsize=8)
        fig.suptitle(f"{phase}: CPU-time CV by VM count — {title}")
        fig.tight_layout(rect=(0, 0, 0.88, 0.96))
        fig.savefig(out / f"{phase}-cv-by-vmcount.png", dpi=130)
        plt.close(fig)

        # Same lines with x = total concurrent flows (k·m): shows that CV follows
        # the total load on the socket rather than how flows are packed into VMs.
        fig, axes = plt.subplots(2, 2, figsize=(13, 9), sharex=True)
        for ax, wl in zip(axes.flat, WORKLOADS):
            cs = [c for c in cells if c["workload"] == wl]
            b0 = sorted((c["flows"], c["cpu"]["cv"] * 100) for c in cs if c["arm"] == "b0")
            if b0:
                ax.plot(*zip(*b0), color="black", marker="o", lw=2.5, label="B0", zorder=5)
            for arm, ls in (("vk", "-"), ("vk1", "--")):
                for k in range(1, 8):
                    pts = sorted((c["flows"], c["cpu"]["cv"] * 100) for c in cs if c["arm"] == arm and c["k"] == k)
                    if pts:
                        ax.plot(*zip(*pts), ls=ls, marker=".", color=cmap((k - 1) / 6),
                                label=f"{'Vk' if arm == 'vk' else 'Vk+1'} k={k}")
            ax.set_title(wl)
            ax.set_ylabel("CPU time CV (%)")
            ax.set_yscale("symlog", linthresh=1)
            ax.grid(alpha=0.3)
        for ax in axes[1]:
            ax.set_xlabel("total execution flows k·m (B0: flow count)")
        h, l = axes[0, 0].get_legend_handles_labels()
        fig.legend(h, l, loc="center right", fontsize=8)
        fig.suptitle(f"{phase}: CPU-time CV by total flows — {title}")
        fig.tight_layout(rect=(0, 0, 0.88, 0.96))
        fig.savefig(out / f"{phase}-cv-by-flows.png", dpi=130)
        plt.close(fig)

        fig, axes = plt.subplots(2, 2, figsize=(13, 9), sharex=True)
        for ax, wl in zip(axes.flat, WORKLOADS):
            cs = [c for c in cells if c["workload"] == wl]
            offsets = {"b0": -0.27, "b1": -0.09, "vk": 0.09, "vk1": 0.27}
            for arm, (color, marker) in ARM_STYLE.items():
                pts = [c for c in cs if c["arm"] == arm]
                if not pts:
                    continue
                xs = [c["flows"] + offsets[arm] for c in pts]
                med = [c["cpu"]["median"] for c in pts]
                lo = [c["cpu"]["median"] - c["cpu"]["p05"] for c in pts]
                hi = [c["cpu"]["p95"] - c["cpu"]["median"] for c in pts]
                ax.errorbar(xs, med, yerr=[lo, hi], fmt=marker, color=color, ms=4, lw=0.8, alpha=0.8,
                            label={"b0": "B0", "b1": "B1", "vk": "Vk", "vk1": "Vk+1"}[arm])
            ax.set_title(wl)
            ax.set_ylabel("CPU time (ms): median, p05–p95")
            ax.grid(alpha=0.3)
        for ax in axes[1]:
            ax.set_xlabel("total execution flows")
        axes[0, 0].legend()
        fig.suptitle(f"{phase}: CPU time by total flows (all k pooled per arm) — {title}")
        fig.tight_layout(rect=(0, 0, 1, 0.96))
        fig.savefig(out / f"{phase}-band-by-flows.png", dpi=130)
        plt.close(fig)

        # Paired comparison on identical CPUs: Vk (k, m) against B1 (k, m), or B0 m when k = 1.
        idx = {(c["arm"], c["k"], c["m"], c["workload"]): c for c in cells}
        fig, axes = plt.subplots(1, 2, figsize=(13, 5.5))
        for wl, marker in zip(WORKLOADS, "o^sD"):
            pairs = []
            for (arm, k, m, w), c in idx.items():
                if arm != "vk" or w != wl:
                    continue
                ref = idx.get(("b1", k, m, wl)) if k > 1 else idx.get(("b0", 1, m, wl))
                if ref:
                    pairs.append((ref, c))
            if not pairs:
                continue
            axes[0].scatter([r["cpu"]["cv"] * 100 for r, _ in pairs], [c["cpu"]["cv"] * 100 for _, c in pairs],
                            marker=marker, label=wl, alpha=0.75)
            axes[1].scatter([r["flows"] for r, _ in pairs],
                            [(c["cpu"]["median"] / r["cpu"]["median"] - 1) * 100 for r, c in pairs],
                            marker=marker, label=wl, alpha=0.75)
        lim = max(axes[0].get_xlim()[1], axes[0].get_ylim()[1])
        axes[0].plot([0.1, lim], [0.1, lim], color="grey", lw=0.8, ls="--")
        axes[0].set_xscale("log")
        axes[0].set_yscale("log")
        axes[0].set_xlabel("bare metal, same CPUs and grouping: CPU CV (%)")
        axes[0].set_ylabel("Vk: CPU CV (%)")
        axes[0].set_title("variance: above the diagonal = VM is noisier")
        axes[1].axhline(0, color="grey", lw=0.8)
        axes[1].set_xlabel("total execution flows")
        axes[1].set_ylabel("Vk median vs bare metal, same layout (%)")
        axes[1].set_title("overhead of the VM wrapper")
        for ax in axes:
            ax.grid(alpha=0.3)
            ax.legend()
        fig.suptitle(f"{phase}: Vk against bare metal on identical CPUs — {title}")
        fig.tight_layout(rect=(0, 0, 1, 0.94))
        fig.savefig(out / f"{phase}-vm-vs-bare.png", dpi=130)
        plt.close(fig)

        fig, axes = plt.subplots(1, 4, figsize=(18, 4.5))
        for ax, wl in zip(axes, WORKLOADS):
            cs = {(c["k"], c["m"]): c["cpu"]["cv"] * 100 for c in cells if c["workload"] == wl and c["arm"] == "vk"}
            if not cs:
                continue
            km, mm = max(k for k, _ in cs), max(m for _, m in cs)
            grid = [[cs.get((k, m), float("nan")) for m in range(1, mm + 1)] for k in range(1, km + 1)]
            im = ax.imshow(grid, origin="lower", cmap="magma_r", aspect="auto",
                           extent=(0.5, mm + 0.5, 0.5, km + 0.5))
            for (k, m), v in cs.items():
                ax.text(m, k, f"{v:.1f}", ha="center", va="center", fontsize=7,
                        color="white" if v > 10 else "black")
            ax.set_title(wl)
            ax.set_xlabel("VM count m")
            ax.set_ylabel("flows per VM k")
            fig.colorbar(im, ax=ax, label="CV (%)")
        fig.suptitle(f"{phase}: Vk CPU-time CV (%) — {title}")
        fig.tight_layout()
        fig.savefig(out / f"{phase}-cv-heatmap.png", dpi=130)
        plt.close(fig)


if __name__ == "__main__":
    main()
