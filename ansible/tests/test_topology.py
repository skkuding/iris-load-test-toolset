#!/usr/bin/env python3
"""Unit tests for the topology helper.

Run:
  python3 ansible/tests/test_topology.py
"""

from __future__ import annotations

import json
import subprocess
import sys
import unittest
from pathlib import Path

HELPER = (
    Path(__file__).resolve().parent.parent
    / "roles"
    / "iris_benchmark_host"
    / "files"
    / "iris_bench_topology.py"
)


def run_helper(lscpu: dict, *args: str) -> dict:
    proc = subprocess.run(
        [sys.executable, str(HELPER), *args],
        input=json.dumps(lscpu),
        capture_output=True,
        text=True,
        check=False,
    )
    assert proc.returncode == 0, proc.stderr
    return json.loads(proc.stdout)


def build_lscpu(sockets: int, cores_per_socket: int, threads_per_core: int) -> dict:
    cpus = []
    cpu = 0
    for socket in range(sockets):
        for core in range(cores_per_socket):
            for _ in range(threads_per_core):
                cpus.append(
                    {
                        "cpu": cpu,
                        "node": socket if sockets > 1 else 0,
                        "socket": socket,
                        "core": core,
                        "online": True,
                    }
                )
                cpu += 1
    return {"cpus": cpus}


def build_server8_lscpu() -> dict:
    """Real server8-like layout: node0 primaries 0-7, node1 primaries 8-15,
    node0 siblings 16-23, node1 siblings 24-31."""
    cpus = []
    for socket in range(2):
        node = 0 if socket == 0 else 1
        primary_base = 0 if socket == 0 else 8
        sibling_base = 16 if socket == 0 else 24
        for core in range(8):
            cpus.append({"cpu": primary_base + core, "node": node, "socket": socket, "core": core, "online": True})
            cpus.append({"cpu": sibling_base + core, "node": node, "socket": socket, "core": core, "online": True})
    cpus.sort(key=lambda entry: entry["cpu"])
    return {"cpus": cpus}


class TopologyTest(unittest.TestCase):
    def test_server8_like_two_socket_smt(self) -> None:
        result = run_helper(build_server8_lscpu())
        self.assertTrue(result["ok"])
        self.assertEqual(result["sockets"], 2)
        self.assertEqual(result["physical_cores"], 16)
        self.assertEqual(result["logical_cpus"], 32)
        self.assertTrue(result["smt_active"])
        # One physical core per socket reserved: core 0 on each socket.
        self.assertEqual(result["housekeeping_cpus"], [0, 8, 16, 24])
        self.assertEqual(len(result["benchmark_cpus"]), 28)
        self.assertTrue(result["sets_disjoint"])
        self.assertEqual(len(result["benchmark_physical_cores"]), 14)

    def test_single_socket_no_smt(self) -> None:
        result = run_helper(build_lscpu(sockets=1, cores_per_socket=4, threads_per_core=1))
        self.assertEqual(result["housekeeping_cpus"], [0])
        self.assertEqual(result["benchmark_cpus"], [1, 2, 3])
        self.assertFalse(result["smt_active"])

    def test_two_housekeeping_cores_per_socket(self) -> None:
        result = run_helper(
            build_lscpu(sockets=2, cores_per_socket=4, threads_per_core=1),
            "--housekeeping-physical-cores-per-socket",
            "2",
        )
        self.assertEqual(result["housekeeping_cpus"], [0, 1, 4, 5])
        self.assertEqual(result["benchmark_cpus"], [2, 3, 6, 7])

    def test_benchmark_override(self) -> None:
        result = run_helper(
            build_lscpu(sockets=1, cores_per_socket=4, threads_per_core=1),
            "--benchmark-cpus",
            "2,3",
        )
        self.assertEqual(result["benchmark_cpus"], [2, 3])
        self.assertTrue(result["source"]["benchmark_override"])

    def test_invalid_json_is_reported(self) -> None:
        proc = subprocess.run(
            [sys.executable, str(HELPER)],
            input="not json",
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertFalse(json.loads(proc.stdout)["ok"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
