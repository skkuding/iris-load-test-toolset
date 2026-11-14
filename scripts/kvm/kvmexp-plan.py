#!/usr/bin/env python3
"""Generate one randomized round plan for the KVM variance experiment.

usage: kvmexp-plan.py <phase> <round> [--only-arms a,b] [--workloads w,..] [--max-n N]

Phases:
  p1   prod policy (SMT on, turbo on), no CAT     arms b0 b1 vk vk1
  p2   quiet policy (SMT off, turbo off), no CAT  arms b0 b1 vk vk1
  p3a  prod policy, CAT per group                 arms b0 b1 vk
  p3b  quiet policy, CAT per group                arms b0 b1 vk
"""
import argparse
import json
import random
import zlib

PHASES = {
    "p1": ("prod", False, ("b0", "b1", "vk", "vk1")),
    "p2": ("quiet", False, ("b0", "b1", "vk", "vk1")),
    "p3a": ("prod", True, ("b0", "b1", "vk")),
    "p3b": ("quiet", True, ("b0", "b1", "vk")),
}
SLOTS = {"prod": 14, "quiet": 7}
WORKLOADS = ("compute", "chase6", "chase44", "stream44")
K_MAX = 7


def cells(policy, arms):
    n = SLOTS[policy]
    out = []
    if "b0" in arms:
        out += [("b0", 1, m) for m in range(1, n + 1)]
    for k in range(1, K_MAX + 1):
        for m in range(1, n // k + 1):
            if "vk" in arms:
                out.append(("vk", k, m))
            if "b1" in arms and k >= 2:
                out.append(("b1", k, m))
        if "vk1" in arms:
            out += [("vk1", k, m) for m in range(1, n // (k + 1) + 1)]
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("phase", choices=PHASES)
    ap.add_argument("round", type=int)
    ap.add_argument("--only-arms")
    ap.add_argument("--workloads", default=",".join(WORKLOADS))
    ap.add_argument("--max-n", type=int)
    ap.add_argument("--tag", default="")
    a = ap.parse_args()
    policy, cat, arms = PHASES[a.phase]
    if a.only_arms:
        arms = tuple(x for x in arms if x in a.only_arms.split(","))
    runs = []
    for arm, k, m in cells(policy, arms):
        width = k + 1 if arm == "vk1" else k
        if a.max_n and width * m > a.max_n:
            continue
        for wl in a.workloads.split(","):
            runs.append({"run_id": f"{a.phase}{a.tag}-r{a.round}-{arm}-k{k}-m{m}-{wl}",
                         "phase": a.phase, "round": a.round, "policy": policy, "cat": cat,
                         "arm": arm, "k": k, "m": m, "workload": wl})
    seed = zlib.crc32(f"{a.phase}{a.tag}-r{a.round}".encode())
    random.Random(seed).shuffle(runs)
    print(json.dumps({"phase": a.phase, "round": a.round, "seed": seed, "runs": runs}, indent=1))


if __name__ == "__main__":
    main()
