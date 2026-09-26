#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["matplotlib>=3.8"]
# ///
"""Plot a concurrency ladder as a Notion-style three-panel boxplot.

Each `--run LABEL=DIR` pairs a label (for example a worker count) with a
collected run bundle. Panels mirror the Wave 1 report: CPU time, real time, and
memory, each by concurrency level.

Usage:
    uv run analysis/plot-ladder.py \
        --run 1=runs/iris-20260926-wave1-1 \
        --run 2=runs/iris-20260926-wave1-2 \
        --run 4=runs/iris-20260926-wave1-4 \
        --run 8=runs/iris-20260926-wave1-8 \
        --run 16=runs/iris-20260926-wave1-16 \
        --run 30=runs/iris-20260926-wave1-30 \
        --out wave1-direct-sweep.png

Only successful samples contribute to the timing and memory distributions. The
plot is written outside the bundles to preserve their checksums.
"""

from __future__ import annotations

import argparse
import json
import statistics
import sys
from pathlib import Path


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--run",
        action="append",
        required=True,
        metavar="LABEL=DIR",
        help="label and collected run directory (repeatable, order preserved)",
    )
    parser.add_argument("--out", type=Path, default=Path("ladder-boxplot.png"))
    parser.add_argument("--title", default="Iris load test — raw-sample distributions")
    parser.add_argument("--format", default="png", choices=["png", "svg", "pdf"])
    return parser.parse_args(argv)


def load_run(run_dir: Path) -> dict[str, list[float]]:
    samples_dir = run_dir / "samples"
    if not samples_dir.is_dir():
        raise SystemExit(f"error: no samples directory in {run_dir}")
    files = sorted(samples_dir.glob("*.ndjson"))
    if not files:
        raise SystemExit(f"error: no *.ndjson samples in {samples_dir}")

    cpu: list[float] = []
    real: list[float] = []
    mem: list[float] = []
    for path in files:
        for line in path.read_text().splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError:
                continue
            if record.get("status") != "success":
                continue
            if isinstance(record.get("cpuTimeMs"), (int, float)):
                cpu.append(float(record["cpuTimeMs"]))
            if isinstance(record.get("realTimeMs"), (int, float)):
                real.append(float(record["realTimeMs"]))
            if isinstance(record.get("memoryBytes"), (int, float)):
                mem.append(float(record["memoryBytes"]) / 1048576.0)
    return {"cpu": cpu, "real": real, "mem": mem}


def percentile(values: list[float], p: float) -> float:
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = p * (len(ordered) - 1)
    lower = int(position)
    upper = min(lower + 1, len(ordered) - 1)
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def summarize(values: list[float]) -> list[float]:
    ordered = sorted(values)
    median = statistics.median(ordered)
    stddev = statistics.pstdev(ordered)
    mean = statistics.fmean(ordered)
    mad = statistics.median([abs(v - median) for v in ordered])
    return [
        len(ordered), mean, stddev, (100 * stddev / mean) if mean else 0.0,
        median, mad, percentile(ordered, 0.95), percentile(ordered, 0.99),
        ordered[-1], (percentile(ordered, 0.99) / median) if median else 0.0,
    ]


HEADER = ["n", "mean", "stddev", "CV%", "median", "MAD", "p95", "p99", "max", "p99/med"]


def print_table(labels: list[str], data: dict[str, dict[str, list[float]]]) -> None:
    for metric, name in (("cpu", "CPU time (ms)"), ("real", "Real time (ms)"), ("mem", "Memory (MiB)")):
        print(f"\n### {name}")
        print(f"{'level':>6}  " + "  ".join(f"{h:>8}" for h in HEADER))
        for label in labels:
            values = data[label][metric]
            if not values:
                continue
            row = summarize(values)
            cells = []
            for index, value in enumerate(row):
                if index == 0:
                    cells.append(f"{int(value):>8}")
                elif index in (3, 9):
                    cells.append(f"{value:>8.2f}")
                else:
                    cells.append(f"{value:>8.1f}")
            print(f"{label:>6}  " + "  ".join(cells))


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    labels: list[str] = []
    data: dict[str, dict[str, list[float]]] = {}
    for spec in args.run:
        label, sep, directory = spec.partition("=")
        if not sep or not label or not directory:
            raise SystemExit(f"error: --run must be LABEL=DIR, got {spec!r}")
        run_dir = Path(directory)
        if not run_dir.is_dir():
            raise SystemExit(f"error: run directory not found: {run_dir}")
        labels.append(label)
        data[label] = load_run(run_dir)

    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    palette = [
        "#b7b1e3", "#8fd3e8", "#a8c8b0", "#f2c88f", "#9fc6e8",
        "#c39bd3", "#f5a9b8", "#a3d9a5", "#f7dc9b", "#b0a8d8",
    ]

    fig, axes = plt.subplots(3, 1, figsize=(11.5, 12.6))
    fig.suptitle(args.title, fontsize=17, fontweight="bold")

    panels = [
        ("cpu", "CPU time (ms) by worker count", "CPU time (ms)"),
        ("real", "Real time (ms) by worker count", "Real time (ms)"),
        ("mem", "Memory (MiB) by worker count", "Memory (MiB)"),
    ]
    for ax, (key, subtitle, ylabel) in zip(axes, panels):
        series = [data[label][key] for label in labels]
        usable = [values for values in series if values]
        if not usable:
            ax.set_visible(False)
            continue
        box = ax.boxplot(
            series,
            tick_labels=labels,
            widths=0.6,
            patch_artist=True,
            showfliers=True,
            flierprops={"marker": ".", "markersize": 3, "markerfacecolor": "#555555", "markeredgecolor": "#555555"},
            medianprops={"color": "#222222"},
        )
        for index, patch in enumerate(box["boxes"]):
            patch.set_facecolor(palette[index % len(palette)])
            patch.set_alpha(0.9)
            patch.set_edgecolor("#333333")
        ax.set_title(subtitle, loc="left", fontsize=13, fontweight="bold")
        ax.set_ylabel(ylabel)
        ax.set_xlabel("Workers (direct Judger, production-compat)")
        ax.grid(True, axis="y", alpha=0.3)
        ax.margins(y=0.08)

    fig.tight_layout(rect=(0, 0, 1, 0.97))
    args.out.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(args.out, dpi=150)
    plt.close(fig)

    print_table(labels, data)
    print(f"\nwrote {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
