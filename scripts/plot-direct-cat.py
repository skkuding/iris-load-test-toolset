#!/usr/bin/env python3
"""Render paired baseline/CAT summary JSON as portable SVG charts."""
import argparse
import html
import json
from pathlib import Path


BLUE, ORANGE = '#2874a6', '#d97721'


def element(tag, attrs=None, value=None):
    attrs = attrs or {}
    pairs = ' '.join(f'{key.replace("_", "-")}="{html.escape(str(val))}"' for key, val in attrs.items())
    return f'<{tag} {pairs}>{html.escape(str(value))}</{tag}>' if value is not None else f'<{tag} {pairs}/>'


def chart(pairs, title, ylabel, metric, path):
    width, height = 1120, 620
    maxval = max(v[metric] for pair in pairs for v in pair) * 1.17
    left, right, top, bottom = 132, 1000, 95, 520
    body = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}">',
            element('rect', {'width': width, 'height': height, 'fill': '#ffffff'}),
            element('text', {'x': 55, 'y': 48, 'font_size': 28, 'font_weight': 'bold', 'fill': '#17232d'}, title),
            element('text', {'x': left, 'y': 75, 'font_size': 16, 'fill': '#55606b'}, ylabel)]
    for k in range(5):
        x = left + (right - left) * k / 4
        value = maxval * k / 4
        body += [element('line', {'x1': x, 'y1': top, 'x2': x, 'y2': bottom, 'stroke': '#e4e9ed'}),
                 element('text', {'x': x, 'y': bottom + 27, 'font_size': 14, 'text_anchor': 'middle', 'fill': '#55606b'}, f'{value:.0f}')]
    for i, (base, cat) in enumerate(pairs):
        y = top + i * 137
        body.append(element('text', {'x': 55, 'y': y + 43, 'font_size': 23, 'font_weight': 'bold', 'fill': '#17232d'}, f'n={base["workers"]}'))
        for j, (result, color, label) in enumerate(((base, BLUE, 'Baseline'), (cat, ORANGE, 'CAT equal'))):
            val = result[metric]
            bar_y = y + j * 47
            bar_w = (right - left) * val / maxval
            body += [element('rect', {'x': left, 'y': bar_y, 'width': bar_w, 'height': 31, 'rx': 4, 'fill': color}),
                     element('text', {'x': left + bar_w + 9, 'y': bar_y + 23, 'font_size': 17, 'font_weight': 'bold', 'fill': color}, f'{val:.1f}'),
                     element('text', {'x': left - 11, 'y': bar_y + 22, 'font_size': 13, 'text_anchor': 'end', 'fill': '#55606b'}, label)]
    body.append(element('text', {'x': 55, 'y': 592, 'font_size': 14, 'fill': '#55606b'}, 'Same direct-host workload; each bar is one 50-iteration run per worker.'))
    body.append('</svg>')
    Path(path).write_text('\n'.join(body))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('summary', type=Path)
    parser.add_argument('outdir', type=Path)
    args = parser.parse_args()
    data = json.loads(args.summary.read_text())
    pairs = [(next(x for x in data if x['workers'] == n and '-base-' in x['run']),
              next(x for x in data if x['workers'] == n and '-equal-' in x['run'])) for n in (8, 16, 30)]
    args.outdir.mkdir(parents=True, exist_ok=True)
    chart(pairs, 'Judger CPU time: baseline vs equal CAT', 'Steady-window median, ms; lower is better', 'cpuMedianMs', args.outdir / 'cpu-median.svg')
    chart(pairs, 'Judger tail latency: baseline vs equal CAT', 'Steady-window P99 CPU time, ms; lower is better', 'cpuP99Ms', args.outdir / 'cpu-p99.svg')
    chart(pairs, 'LLC occupancy observed by Intel CMT', 'Mean MiB during steady window; baseline includes host traffic', 'llcMeanMiB', args.outdir / 'llc-occupancy.svg')
    chart(pairs, 'Memory traffic observed by Intel MBM', 'Total MiB/s during steady window; baseline includes host traffic', 'mbmTotalMiBs', args.outdir / 'memory-bandwidth.svg')


if __name__ == '__main__':
    main()
