#!/usr/bin/env python3
"""Derive topology-aware housekeeping and benchmark CPU sets.

Managed by the iris_benchmark_host Ansible role. Reads the JSON emitted by
`lscpu -e -J` on stdin and prints one JSON object on stdout. Read-only.

Housekeeping CPUs carry the OS, SSH, interrupts, Docker, RabbitMQ, and Iris.
Benchmark CPUs carry Judger submissions. Selection is by physical core per
socket, never by a server8-only CPU constant.
"""

from __future__ import annotations

import argparse
import json
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


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--housekeeping-physical-cores-per-socket", type=int, default=1)
    parser.add_argument("--housekeeping-cpus", default="")
    parser.add_argument("--benchmark-cpus", default="")
    args = parser.parse_args()

    try:
        data = json.load(sys.stdin)
    except (json.JSONDecodeError, ValueError) as exc:
        print(json.dumps({"ok": False, "error": f"invalid lscpu JSON: {exc}"}))
        return 1

    cpus = data.get("cpus") or []
    if not cpus:
        print(json.dumps({"ok": False, "error": "lscpu -e -J returned no CPUs"}))
        return 1

    cores: dict[tuple[int, int], dict] = {}
    online_cpus: list[int] = []
    numa_node_cpus: dict[int, list[int]] = {}
    cpu_to_socket: dict[str, int] = {}
    cpu_to_node: dict[str, int] = {}

    for entry in cpus:
        cpu = int(entry["cpu"])
        socket = int(entry.get("socket", 0))
        core = int(entry.get("core", cpu))
        node = int(entry.get("node", 0))
        online = bool(entry.get("online", True))
        cpu_to_socket[str(cpu)] = socket
        cpu_to_node[str(cpu)] = node
        key = (socket, core)
        cores.setdefault(key, {"socket": socket, "core": core, "node": node, "cpus": []})
        cores[key]["cpus"].append(cpu)
        if online:
            online_cpus.append(cpu)
            numa_node_cpus.setdefault(node, []).append(cpu)

    for value in cores.values():
        value["cpus"].sort()
    online_cpus = sorted(online_cpus)
    for node in numa_node_cpus:
        numa_node_cpus[node] = sorted(numa_node_cpus[node])

    sockets = sorted({value["socket"] for value in cores.values()})
    threads_per_core = max(len(value["cpus"]) for value in cores.values())
    smt_active = threads_per_core > 1

    auto_housekeeping: set[int] = set()
    if args.housekeeping_physical_cores_per_socket > 0:
        for socket in sockets:
            socket_cores = sorted(
                value["core"] for value in cores.values() if value["socket"] == socket
            )
            chosen = set(socket_cores[: args.housekeeping_physical_cores_per_socket])
            for value in cores.values():
                if value["socket"] == socket and value["core"] in chosen:
                    auto_housekeeping.update(value["cpus"])

    override_housekeeping = parse_cpu_list(args.housekeeping_cpus)
    override_benchmark = parse_cpu_list(args.benchmark_cpus)
    online_set = set(online_cpus)

    if override_housekeeping:
        housekeeping = override_housekeeping & online_set
    else:
        housekeeping = auto_housekeeping & online_set

    if override_benchmark:
        benchmark = override_benchmark & online_set
    else:
        benchmark = online_set - housekeeping

    housekeeping_nodes = sorted(
        node for node, node_cpus in numa_node_cpus.items() if set(node_cpus) & housekeeping
    )
    benchmark_nodes = sorted(
        node for node, node_cpus in numa_node_cpus.items() if set(node_cpus) & benchmark
    )

    benchmark_physical_cores = []
    for key in sorted(cores):
        value = cores[key]
        if set(value["cpus"]) & benchmark:
            benchmark_physical_cores.append(
                {
                    "socket": value["socket"],
                    "core": value["core"],
                    "node": value["node"],
                    "cpus": value["cpus"],
                    "primary": min(value["cpus"]),
                }
            )

    result = {
        "ok": True,
        "schema": "iris-bench-topology/v1",
        "smt_active": smt_active,
        "threads_per_core": threads_per_core,
        "sockets": len(sockets),
        "socket_list": sockets,
        "physical_cores": len(cores),
        "logical_cpus": len(online_cpus),
        "online_cpus": online_cpus,
        "cores_per_socket": {
            str(socket): sum(1 for value in cores.values() if value["socket"] == socket)
            for socket in sockets
        },
        "numa_node_cpus": {str(node): value for node, value in sorted(numa_node_cpus.items())},
        "housekeeping_cpus": sorted(housekeeping),
        "benchmark_cpus": sorted(benchmark),
        "housekeeping_numa_nodes": housekeeping_nodes,
        "benchmark_numa_nodes": benchmark_nodes,
        "benchmark_physical_cores": benchmark_physical_cores,
        "cpu_to_socket": cpu_to_socket,
        "cpu_to_node": cpu_to_node,
        "sets_disjoint": not bool(housekeeping & benchmark),
        "source": {
            "housekeeping_physical_cores_per_socket": args.housekeeping_physical_cores_per_socket,
            "housekeeping_override": bool(override_housekeeping),
            "benchmark_override": bool(override_benchmark),
        },
    }
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
