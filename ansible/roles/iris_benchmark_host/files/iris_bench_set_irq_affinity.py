#!/usr/bin/env python3
"""Route movable IRQ affinity to the benchmark housekeeping CPU set.

Managed by the iris_benchmark_host Ansible role. Read-mostly: it writes only
/proc/irq/<n>/smp_affinity_list for IRQs whose current affinity differs and that
the kernel permits changing. Unwritable, managed, and excluded IRQs are reported
and skipped, never forced.

Prints a single JSON object on stdout.
"""

from __future__ import annotations

import argparse
import glob
import json
import os
import sys


def parse_cpu_list(value: str) -> set[int]:
    cpus: set[int] = set()
    if not value:
        return cpus
    for part in str(value).replace(" ", ",").split(","):
        part = part.strip()
        if not part:
            continue
        if "-" in part:
            low, high = part.split("-", 1)
            cpus.update(range(int(low), int(high) + 1))
        else:
            cpus.add(int(part))
    return cpus


def normalize(value: str) -> str:
    return ",".join(str(cpu) for cpu in sorted(parse_cpu_list(value)))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--cpus", required=True, help="comma/range CPU list")
    parser.add_argument("--irq-root", default="/proc/irq")
    parser.add_argument("--exclude-irqs", default="", help="comma/range IRQs to skip")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    desired = parse_cpu_list(args.cpus)
    if not desired:
        print(json.dumps({"ok": False, "error": "empty housekeeping CPU set"}))
        return 1

    desired_str = normalize(args.cpus)
    exclude = {0} | parse_cpu_list(args.exclude_irqs)
    applied: list[dict] = []
    skipped: dict[str, str] = {}

    for path in sorted(glob.glob(os.path.join(args.irq_root, "*"))):
        name = os.path.basename(path)
        if not name.isdigit():
            continue
        irq = int(name)
        if irq in exclude:
            skipped[name] = "excluded"
            continue
        target = os.path.join(path, "smp_affinity_list")
        if not os.path.exists(target):
            skipped[name] = "no-smp_affinity_list"
            continue
        try:
            with open(target, encoding="ascii") as handle:
                current = handle.read().strip()
        except OSError as exc:
            skipped[name] = f"read-error:{exc.strerror}"
            continue
        if normalize(current) == desired_str:
            continue
        entry = {"irq": irq, "from": current, "to": desired_str}
        if args.dry_run:
            entry["dry_run"] = True
            applied.append(entry)
            continue
        try:
            with open(target, "w", encoding="ascii") as handle:
                handle.write(desired_str)
        except OSError as exc:
            skipped[name] = f"write-error:{exc.strerror}"
            continue
        applied.append(entry)

    print(
        json.dumps(
            {
                "ok": True,
                "desired": desired_str,
                "changed_count": len(applied),
                "applied": applied,
                "skipped": skipped,
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
