#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = ["matplotlib>=3.8"]
# ///
"""Plot direct-suite benchmark samples from a collected run bundle.

The controller writes immutable result bundles under runs/<run-id>/; this script
only reads them and writes a plot outside the bundle so checksums and the
COMPLETE marker stay valid.

Usage:
    uv run analysis/plot-run.py --run runs/iris-20260926-alpha4pc8
    uv run analysis/plot-run.py --run runs/iris-20260926-alpha4pc8 --out /tmp/plot.png

By default the plot is written to <run-id>-timeseries.png in the current
directory and a summary table is printed to stdout.

When samples carry host-wide timestamps, only samples inside the
all-workers-active steady window (max per-worker first start through min
per-worker last end) are plotted and summarized, and the window duration is
annotated. Without timestamps the script keeps its pre-window behavior.
"""

from __future__ import annotations

import argparse
import json
import statistics
import sys
from pathlib import Path


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", required=True, type=Path, help="collected run directory")
    parser.add_argument("--out", type=Path, default=None, help="output image path")
    parser.add_argument("--format", default="png", choices=["png", "svg", "pdf"], help="image format")
    return parser.parse_args(argv)


def load_manifest(run_dir: Path) -> dict:
    path = run_dir / "manifest.json"
    if not path.is_file():
        return {}
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        print(f"warning: cannot read manifest.json: {exc}", file=sys.stderr)
        return {}


def load_samples(run_dir: Path) -> list[dict]:
    samples_dir = run_dir / "samples"
    if not samples_dir.is_dir():
        raise SystemExit(f"error: no samples directory in {run_dir}")
    files = sorted(samples_dir.glob("*.ndjson"))
    if not files:
        raise SystemExit(f"error: no *.ndjson samples in {samples_dir}")

    samples: list[dict] = []
    for path in files:
        for line_number, line in enumerate(path.read_text().splitlines(), start=1):
            line = line.strip()
            if not line:
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError as exc:
                print(f"warning: {path.name}:{line_number}: {exc}", file=sys.stderr)
                continue
            record["_source"] = path.name
            samples.append(record)
    if not samples:
        raise SystemExit("error: no sample records found")
    return samples


def steady_window(records: list[dict]) -> tuple[int, int] | None:
    """Return the all-workers-active window as (startNs, endNs).

    The window is the max of per-worker first starts through the min of
    per-worker last ends. It is None when any record lacks valid positive
    timestamps, so an untimestamped bundle keeps its pre-window behavior.
    """
    per_worker: dict[str, tuple[int, int]] = {}
    for record in records:
        started = record.get("startedAtNs")
        ended = record.get("endedAtNs")
        if not isinstance(started, int) or not isinstance(ended, int):
            return None
        if started <= 0 or ended <= 0 or ended < started:
            return None
        worker = str(record.get("worker", ""))
        first, last = per_worker.get(worker, (started, ended))
        per_worker[worker] = (min(first, started), max(last, ended))
    if not per_worker:
        return None
    start = max(first for first, _ in per_worker.values())
    end = min(last for _, last in per_worker.values())
    if end <= start:
        return None
    return start, end


def numeric(samples: list[dict], key: str) -> list[float]:
    values = []
    for record in samples:
        value = record.get(key)
        if isinstance(value, (int, float)):
            values.append(float(value))
    return values


def summarize(values: list[float]) -> dict[str, float]:
    ordered = sorted(values)
    median = statistics.median(ordered)
    stddev = statistics.stdev(ordered) if len(ordered) > 1 else 0.0
    mean = statistics.fmean(ordered)
    mad = statistics.median([abs(v - median) for v in ordered])

    def percentile(p: float) -> float:
        if len(ordered) == 1:
            return ordered[0]
        position = p * (len(ordered) - 1)
        lower = int(position)
        upper = min(lower + 1, len(ordered) - 1)
        return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)

    return {
        "n": float(len(ordered)),
        "median": median,
        "mad": mad,
        "mean": mean,
        "stddev": stddev,
        "cv": (stddev / mean * 100.0) if mean else 0.0,
        "p90": percentile(0.90),
        "p95": percentile(0.95),
        "p99": percentile(0.99),
        "max": ordered[-1],
    }


def plot(run_dir: Path, samples: list[dict], manifest: dict, out_path: Path):
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    successful = [s for s in samples if s.get("status") == "success"]
    if not successful:
        raise SystemExit("error: no successful samples to plot")

    window = steady_window(successful)
    window_note = "no timestamped steady window"
    if window is not None:
        window_start, window_end = window
        successful = [
            s
            for s in successful
            if window_start <= s.get("startedAtNs", 0) and s.get("endedAtNs", 0) <= window_end
        ]
        window_note = f"steady window {((window_end - window_start) / 1e9):.2f} s"
    if not successful:
        raise SystemExit("error: no successful samples inside the steady window")

    cpu = numeric(successful, "cpuTimeMs")
    real = numeric(successful, "realTimeMs")
    if not cpu or not real:
        raise SystemExit("error: successful samples are missing cpuTimeMs or realTimeMs")

    block_ids = sorted({str(s.get("blockId", "block")) for s in successful})
    run_id = manifest.get("runId") or run_dir.name
    mode = manifest.get("containmentMode", "unknown")
    comparable = manifest.get("comparable")
    fixture = (manifest.get("fixtures") or [{}])[0].get("name", "unknown")

    # Order points by iteration, then worker, for a stable series.
    ordered = sorted(successful, key=lambda s: (str(s.get("blockId", "")), int(s.get("iteration", 0)), str(s.get("worker", ""))))
    x = list(range(len(ordered)))
    cpu_series = [float(s["cpuTimeMs"]) for s in ordered]
    real_series = [float(s["realTimeMs"]) for s in ordered]

    fig, (ax_series, ax_dist) = plt.subplots(1, 2, figsize=(13.5, 5.2))
    fig.suptitle(
        f"{run_id}\nmode={mode}  comparable={comparable}  fixture={fixture}  blocks={len(block_ids)}\n{window_note}",
        fontsize=12,
    )

    cpu_color, real_color = "#1f77b4", "#d62728"
    ax_series.plot(x, cpu_series, marker="o", color=cpu_color, label="cpuTimeMs")
    ax_series.plot(x, real_series, marker="s", color=real_color, label="realTimeMs")
    ax_series.axhline(summarize(cpu)["median"], color=cpu_color, linestyle="--", linewidth=1, alpha=0.6)
    ax_series.axhline(summarize(real)["median"], color=real_color, linestyle="--", linewidth=1, alpha=0.6)
    ax_series.set_xlabel("sample index")
    ax_series.set_ylabel("time (ms)")
    ax_series.set_title("per-sample timings")
    ax_series.grid(True, alpha=0.3)
    ax_series.legend()

    ax_dist.boxplot([cpu, real], tick_labels=["cpuTimeMs", "realTimeMs"], widths=0.5)
    for i, (values, color) in enumerate(((cpu, cpu_color), (real, real_color)), start=1):
        jitter = [i + (j % 5 - 2) * 0.02 for j in range(len(values))]
        ax_dist.scatter(jitter, values, color=color, alpha=0.7, zorder=3)
    ax_dist.set_ylabel("time (ms)")
    ax_dist.set_title("distribution")
    ax_dist.grid(True, axis="y", alpha=0.3)

    cpu_sum = summarize(cpu)
    real_sum = summarize(real)
    lines = [
        f"n = {int(cpu_sum['n'])}",
        window_note,
        "",
        "cpuTimeMs",
        f"  median {cpu_sum['median']:.1f}  mad {cpu_sum['mad']:.1f}  cv {cpu_sum['cv']:.2f}%",
        f"  p95 {cpu_sum['p95']:.1f}  p99 {cpu_sum['p99']:.1f}  max {cpu_sum['max']:.1f}",
        "realTimeMs",
        f"  median {real_sum['median']:.1f}  mad {real_sum['mad']:.1f}  cv {real_sum['cv']:.2f}%",
        f"  p95 {real_sum['p95']:.1f}  p99 {real_sum['p99']:.1f}  max {real_sum['max']:.1f}",
    ]
    ax_series.text(
        1.02, 0.98, "\n".join(lines), transform=ax_series.transAxes,
        va="top", ha="left", family="monospace", fontsize=9,
        bbox={"boxstyle": "round", "facecolor": "white", "alpha": 0.9},
    )

    fig.tight_layout(rect=(0, 0, 0.86, 0.93))
    out_path.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(out_path, dpi=150)
    plt.close(fig)

    header = f"{'metric':<12}{'n':>4}{'median':>10}{'mad':>8}{'stddev':>10}{'cv%':>8}{'p95':>9}{'p99':>9}{'max':>9}"
    print(header)
    print("-" * len(header))
    for name, summary in (("cpuTimeMs", cpu_sum), ("realTimeMs", real_sum)):
        print(
            f"{name:<12}{int(summary['n']):>4}{summary['median']:>10.1f}{summary['mad']:>8.1f}"
            f"{summary['stddev']:>10.3f}{summary['cv']:>8.2f}{summary['p95']:>9.1f}"
            f"{summary['p99']:>9.1f}{summary['max']:>9.1f}"
        )


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    run_dir = args.run
    if not run_dir.is_dir():
        raise SystemExit(f"error: run directory not found: {run_dir}")

    manifest = load_manifest(run_dir)
    samples = load_samples(run_dir)
    run_id = manifest.get("runId") or run_dir.name
    out_path = args.out or Path(f"{run_id}-timeseries.{args.format}")
    plot(run_dir, samples, manifest, out_path)
    print(f"\nwrote {out_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
