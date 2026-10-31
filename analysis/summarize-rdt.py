#!/usr/bin/env python3
"""Summarize passive CMT/MBM samples inside a Judger steady window."""

import argparse
import json
from pathlib import Path
from statistics import mean


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--rdt", type=Path, required=True)
    args = parser.parse_args()

    receipt = json.loads(args.receipt.read_text())
    start = receipt["steadyWindowStartNs"]
    end = receipt["steadyWindowEndNs"]
    records = [json.loads(line) for line in args.rdt.read_text().splitlines() if line]
    records = [r for r in records if start <= r["observedAtNs"] <= end]
    if len(records) < 2:
        parser.error("fewer than two RDT observations inside the steady window")
    if any(r["schemaVersion"] != 1 or r["scope"] != "resctrl-default-host" for r in records):
        parser.error("unsupported RDT record schema or scope")

    seconds = (records[-1]["observedAtNs"] - records[0]["observedAtNs"]) / 1e9
    if seconds <= 0:
        parser.error("RDT timestamps are not increasing")
    domains = sorted(records[0]["domains"])
    if any(sorted(r["domains"]) != domains for r in records):
        parser.error("RDT cache domains changed during the block")

    result = {
        "scope": "resctrl-default-host",
        "steadyWindowSeconds": (end - start) / 1e9,
        "observationSeconds": seconds,
        "observations": len(records),
        "domains": {},
    }
    for domain in domains:
        occupancy = [r["domains"][domain]["llcOccupancyBytes"] for r in records]
        first = records[0]["domains"][domain]
        last = records[-1]["domains"][domain]
        total_delta = last["mbmTotalBytes"] - first["mbmTotalBytes"]
        local_delta = last["mbmLocalBytes"] - first["mbmLocalBytes"]
        if total_delta < 0 or local_delta < 0:
            parser.error(f"MBM counter decreased in {domain}")
        result["domains"][domain] = {
            "llcMeanMiB": round(mean(occupancy) / 2**20, 3),
            "llcMaxMiB": round(max(occupancy) / 2**20, 3),
            "mbmTotalMiBPerSec": round(total_delta / 2**20 / seconds, 3),
            "mbmLocalMiBPerSec": round(local_delta / 2**20 / seconds, 3),
            "mbmRemoteMiBPerSec": round(max(0, total_delta - local_delta) / 2**20 / seconds, 3),
        }
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
