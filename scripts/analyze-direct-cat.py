#!/usr/bin/env python3
"""Summarize paired host-direct Judger CAT runs from staged NDJSON and RDT TSV files."""
import argparse
import csv
import glob
import json
import math
import statistics
from pathlib import Path


def percentile(values, q):
    values = sorted(values)
    index = (len(values) - 1) * q
    lower = math.floor(index)
    upper = math.ceil(index)
    return values[lower] + (values[upper] - values[lower]) * (index - lower)


def summarize(base, run):
    files = sorted(glob.glob(str(base / f"{run}.worker-*.ndjson")))
    if not files:
        raise ValueError(f"no worker samples: {run}")
    samples = [json.loads(line) for path in files for line in open(path)]
    if any(s['status'] != 'success' or not s['outputMatches']
           or s['containmentMode'] != 'production-compat'
           or not s['cgroupPath'].startswith('/sys/fs/cgroup/sandbox-' + run + '-')
           for s in samples):
        raise ValueError(f"invalid sample: {run}")
    workers = {s['worker'] for s in samples}
    if len(workers) != len(files) or len(files) != int(run.rsplit('-', 1)[1]) or len(samples) != 50 * len(files):
        raise ValueError(f"incomplete run: {run}")
    start = max(min(s['startedAtNs'] for s in samples if s['worker'] == worker)
                for worker in workers)
    end = min(max(s['endedAtNs'] for s in samples if s['worker'] == worker)
              for worker in workers)
    steady = [s for s in samples if s['startedAtNs'] >= start and s['endedAtNs'] <= end]
    if not steady:
        raise ValueError(f"empty steady window: {run}")
    result = {'run': run, 'workers': len(workers), 'samples': len(samples),
              'steadySamples': len(steady), 'steadySeconds': round((end-start)/1e9, 2)}
    for key, label in [('cpuTimeMs', 'cpu'), ('realTimeMs', 'real')]:
        values = [s[key] for s in steady]
        result[f'{label}MedianMs'] = round(statistics.median(values), 2)
        result[f'{label}P99Ms'] = round(percentile(values, .99), 2)
        result[f'{label}CvPct'] = round(statistics.pstdev(values) / statistics.mean(values) * 100, 2)

    rows = list(csv.DictReader(open(base / f'{run}.rdt.tsv'), delimiter='\t'))
    active = [r for r in rows if start <= int(r['observedAtNs']) <= end]
    groups = sorted({r['group'] for r in active})
    expected_groups = len(workers) // 2 if '-equal-' in run else 1
    if len(groups) != expected_groups + (1 if '-equal-' in run else 0):
        raise ValueError(f"missing RDT groups: {run}: {groups}")
    selected = [r for r in active if (r['group'] != 'root' if '-equal-' in run else r['group'] == 'root')]
    if '-equal-' in run:
        present = {r['group'] for r in selected if int(r['tasks']) > 0}
        if len(present) != expected_groups:
            raise ValueError(f"CAT groups without tasks: {run}: {present}")
    occupancy_by_time = {}
    rates_by_time = {}
    prior = {}
    for row in selected:
        key = row['group'], row['domain']
        at = int(row['observedAtNs'])
        occupancy_by_time[at] = occupancy_by_time.get(at, 0) + int(row['llcOccupancyBytes'])
        old = prior.get(key)
        if old:
            dt = (at - old[0]) / 1e9
            if dt > 0:
                total = int(row['mbmTotalBytes']) - old[1]
                local = int(row['mbmLocalBytes']) - old[2]
                if total >= 0 and local >= 0:
                    rates = rates_by_time.setdefault(at, [0, 0])
                    rates[0] += total / dt
                    rates[1] += local / dt
        prior[key] = at, int(row['mbmTotalBytes']), int(row['mbmLocalBytes'])
    if occupancy_by_time:
        result['llcMeanMiB'] = round(statistics.mean(occupancy_by_time.values()) / 2**20, 2)
    if rates_by_time:
        result['mbmTotalMiBs'] = round(statistics.mean(v[0] for v in rates_by_time.values()) / 2**20, 2)
        result['mbmLocalMiBs'] = round(statistics.mean(v[1] for v in rates_by_time.values()) / 2**20, 2)
        result['mbmRemoteMiBs'] = round(result['mbmTotalMiBs'] - result['mbmLocalMiBs'], 2)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('directory', type=Path)
    parser.add_argument('runs', nargs='+')
    args = parser.parse_args()
    print(json.dumps([summarize(args.directory, run) for run in args.runs],
                     ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
