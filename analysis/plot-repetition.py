#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["matplotlib>=3.8"]
# ///
"""Aggregate repeated benchmark rounds into boxplots and drift charts.

Each `--run ROUND:LABEL=DIR` entry maps one collected run bundle to a round
number and a label (typically a replica count). Repeating the same ladder across
rounds lets this script show two things the single-round plot cannot:

* an aggregate boxplot of every successful sample pooled across rounds for each
  label; and
* a per-round median trend so round-to-round drift is visible.

Both figures are written outside the bundles to preserve their checksums, and a
drift table is printed to stdout.

Usage:
    uv run analysis/plot-repetition.py \
        --run 1:1=runs/r1-1x --run 1:8=runs/r1-8x ... \
        --run 5:1=runs/r5-1x --run 5:8=runs/r5-8x \
        --out-aggregate reboot-aggregate.png \
        --out-drift reboot-drift.png
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
        metavar="ROUND:LABEL=DIR",
        help="round number, label, and collected run directory (repeatable)",
    )
    parser.add_argument("--out-aggregate", type=Path, default=Path("repetition-aggregate.png"))
    parser.add_argument("--out-drift", type=Path, default=Path("repetition-drift.png"))
    parser.add_argument("--title", default="Iris load test — repeated rounds")
    parser.add_argument("--xlabel", default="Concurrency level")
    parser.add_argument("--dpi", type=int, default=150)
    return parser.parse_args(argv)


MENU = (
    ("cpu", "cpuTimeMs", "CPU time (ms)", 1.0),
    ("real", "realTimeMs", "Real time (ms)", 1.0),
    ("mem", "memoryBytes", "Memory (MiB)", 1.0 / 1048576.0),
    ("e2e", "endToEndMs", "End-to-end (ms)", 1.0),
)


def parse_spec(spec: str) -> tuple[int, str, Path]:
    head, _, directory = spec.partition("=")
    round_text, _, label = head.partition(":")
    if not directory or not round_text or not label:
        raise SystemExit(f"error: --run must be ROUND:LABEL=DIR, got {spec!r}")
    try:
        round_number = int(round_text)
    except ValueError:
        raise SystemExit(f"error: round must be an integer, got {round_text!r}")
    return round_number, label, Path(directory)


def load_samples(run_dir: Path) -> list[dict]:
    samples_dir = run_dir / "samples"
    if not samples_dir.is_dir():
        raise SystemExit(f"error: no samples directory in {run_dir}")
    records: list[dict] = []
    for path in sorted(samples_dir.glob("*.ndjson")):
        for line in path.read_text().splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError:
                continue
            if record.get("status") == "success":
                records.append(record)
    if not records:
        raise SystemExit(f"error: no successful samples in {run_dir}")
    return records


def values(records: list[dict], key: str, scale: float) -> list[float]:
    out = []
    for record in records:
        value = record.get(key)
        if isinstance(value, (int, float)):
            out.append(float(value) * scale)
    return out


def median(values_: list[float]) -> float:
    return statistics.median(values_)


def main(argv: list[str]) -> int:
    args = parse_args(argv)

    labels: list[str] = []
    rounds: list[int] = []
    # data[round][label][key] = list[float]
    data: dict[int, dict[str, dict[str, list[float]]]] = {}
    for spec in args.run:
        round_number, label, run_dir = parse_spec(spec)
        if not run_dir.is_dir():
            raise SystemExit(f"error: run directory not found: {run_dir}")
        if label not in labels:
            labels.append(label)
        if round_number not in rounds:
            rounds.append(round_number)
        records = load_samples(run_dir)
        bucket = data.setdefault(round_number, {}).setdefault(
            label, {key: [] for key, _, _, _ in MENU}
        )
        for key, field, _, scale in MENU:
            bucket[key].extend(values(records, field, scale))
    rounds.sort()

    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    palette = [
        "#b7b1e3", "#8fd3e8", "#a8c8b0", "#f2c88f", "#9fc6e8",
        "#c39bd3", "#f5a9b8", "#a3d9a5", "#f7dc9b", "#b0a8d8",
    ]
    metrics = [(key, ylabel) for key, _, ylabel, _ in MENU if any(
        data[r].get(label, {}).get(key) for r in rounds for label in labels
    )]

    # --- Aggregate boxplot: pooled samples per label -------------------------
    fig, axes = plt.subplots(len(metrics), 1, figsize=(11.5, 3.9 * len(metrics)), squeeze=False)
    fig.suptitle(f"{args.title} — aggregate distributions", fontsize=16, fontweight="bold")
    for ax, (key, ylabel) in zip(axes[:, 0], metrics):
        series = []
        for label in labels:
            pooled: list[float] = []
            for r in rounds:
                pooled.extend(data[r].get(label, {}).get(key, []))
            series.append(pooled)
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
        ax.set_title(ylabel, loc="left", fontsize=13, fontweight="bold")
        ax.set_ylabel(ylabel)
        ax.set_xlabel(args.xlabel)
        ax.grid(True, axis="y", alpha=0.3)
    fig.tight_layout(rect=(0, 0, 1, 0.96))
    args.out_aggregate.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(args.out_aggregate, dpi=args.dpi)
    plt.close(fig)

    # --- Drift chart: per-round median per label -----------------------------
    fig, axes = plt.subplots(len(metrics), 1, figsize=(11.0, 3.4 * len(metrics)), squeeze=False)
    fig.suptitle(f"{args.title} — per-round medians", fontsize=16, fontweight="bold")
    for ax, (key, ylabel) in zip(axes[:, 0], metrics):
        for index, label in enumerate(labels):
            medians = [
                median(data[r][label][key]) if data[r].get(label, {}).get(key) else float("nan")
                for r in rounds
            ]
            ax.plot(rounds, medians, marker="o", label=label, color=palette[index % len(palette)])
        ax.set_title(ylabel, loc="left", fontsize=13, fontweight="bold")
        ax.set_ylabel(ylabel)
        ax.set_xlabel("round")
        ax.set_xticks(rounds)
        ax.grid(True, alpha=0.3)
        ax.legend(title=args.xlabel, ncols=len(labels), fontsize=9)
    fig.tight_layout(rect=(0, 0, 1, 0.96))
    fig.savefig(args.out_drift, dpi=args.dpi)
    plt.close(fig)

    # --- Drift table ---------------------------------------------------------
    for key, ylabel in metrics:
        print(f"\n### {ylabel} — per-round medians")
        header = f"{'level':>6}  " + "  ".join(f"R{r:>2}" for r in rounds) + f"  {'median':>9}{'min':>9}{'max':>9}{'spread%':>9}"
        print(header)
        for label in labels:
            per_round = [median(data[r][label][key]) for r in rounds if data[r].get(label, {}).get(key)]
            if not per_round:
                continue
            center = statistics.median(per_round)
            spread = (max(per_round) - min(per_round)) / center * 100 if center else 0.0
            cells = "  ".join(f"{v:>4.1f}" for v in per_round)
            print(f"{label:>6}  {cells}  {center:>9.1f}{min(per_round):>9.1f}{max(per_round):>9.1f}{spread:>9.2f}")

    print(f"\nwrote {args.out_aggregate}")
    print(f"wrote {args.out_drift}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
