#!/usr/bin/env python3
"""Render the Judger RDT ladder as a dependency-free SVG chart."""

import argparse
import json
from html import escape
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--results", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    workers = [1, 2, 4, 8, 16, 30]
    cpu, mbm, llc = [], [], []
    for n in workers:
        name = f"iris-20261031-rdt-{n}"
        analysis = json.loads((args.results / f"{name}.analysis.json").read_text())
        rdt = json.loads((args.results / f"{name}.summary.json").read_text())
        if analysis["summary"]["failures"]:
            parser.error(f"{name} has failed submissions")
        domains = list(rdt["domains"].values())
        cpu.append(analysis["summary"]["cpuTimeMs"]["median"])
        mbm.append(sum(d["mbmTotalMiBPerSec"] for d in domains))
        llc.append(sum(d["llcMeanMiB"] for d in domains))

    parts = ['<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="990" viewBox="0 0 1200 990">',
             '<rect width="1200" height="990" fill="#f8fafc"/>',
             '<text x="60" y="66" font-family="Arial" font-size="30" font-weight="bold" fill="#172033">Judger load ladder: CPU, MBM, CMT</text>',
             '<text x="60" y="97" font-family="Arial" font-size="16" fill="#516176">2026-10-31 · codedang8 · passive host-default resctrl monitoring · 1 s sampling</text>']

    def chart(y_top: int, title: str, values: list[float], maximum: float, unit: str, color: str, decimals: int = 0) -> None:
        left, right, bottom = 105, 1110, y_top + 190
        parts.append(f'<text x="60" y="{y_top-18}" font-family="Arial" font-size="20" font-weight="bold" fill="#172033">{escape(title)}</text>')
        for i in range(5):
            y = bottom - i * 40
            tick = maximum * i / 4
            parts.append(f'<line x1="{left}" x2="{right}" y1="{y}" y2="{y}" stroke="#dbe3ee"/>')
            parts.append(f'<text x="{left-12}" y="{y+5}" text-anchor="end" font-family="Arial" font-size="12" fill="#64748b">{tick:.0f}</text>')
        points = []
        for i, (n, value) in enumerate(zip(workers, values)):
            x = left + i * (right - left) / 5
            y = bottom - value / maximum * 160
            points.append(f"{x:.1f},{y:.1f}")
            parts.append(f'<circle cx="{x:.1f}" cy="{y:.1f}" r="5" fill="{color}"/>')
            parts.append(f'<text x="{x:.1f}" y="{y-10:.1f}" text-anchor="middle" font-family="Arial" font-size="13" font-weight="bold" fill="{color}">{value:.{decimals}f}</text>')
            parts.append(f'<text x="{x:.1f}" y="{bottom+24}" text-anchor="middle" font-family="Arial" font-size="13" fill="#516176">{n}</text>')
        parts.append(f'<polyline points="{" ".join(points)}" fill="none" stroke="{color}" stroke-width="3"/>')
        parts.append(f'<text x="{right}" y="{bottom+45}" text-anchor="end" font-family="Arial" font-size="12" fill="#64748b">workers · {escape(unit)}</text>')

    chart(150, "CPU time median (ms)", cpu, 650, "ms", "#2563eb", 1)
    chart(420, "MBM total, sum of two LLC domains (MiB/s)", mbm, 5500, "MiB/s", "#e05a36", 0)
    chart(690, "CMT LLC occupancy, sum of two LLC domains (MiB)", llc, 50, "MiB", "#16a086", 1)
    parts.append('<text x="60" y="970" font-family="Arial" font-size="12" fill="#64748b">CMT/MBM include all host tasks in the resctrl default group; LLC readings near 40 MiB should not be attributed to Judger alone.</text>')
    parts.append('</svg>')
    args.output.write_text("\n".join(parts) + "\n")


if __name__ == "__main__":
    main()
