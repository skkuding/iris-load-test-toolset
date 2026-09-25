# Wave 1 Replication Manual (Toolset Operator)

Last reviewed: 2026-09-25

This is the operator manual for reproducing the original **Wave 1** Iris
runtime-reproducibility experiment described in `../PROPOSE.md` and recorded in
`../LOG.md`. It is written for an operator who is new to this toolset.

Read first:

- `../PROPOSE.md` — the benchmark plan (goal, method, gates, success criteria).
- `../LOG.md` — current progress, decisions, and known blockers.
- Original evidence (private, advisory):
  `docs-local/plans/wave1/WAVE1-LOAD-TEST-MANUAL.md` (method),
  `docs-local/plans/wave1/REPRODUCE.md` (exact recipe),
  `docs-local/plans/wave1/WAVE1-LOAD-TEST-LOG.md` (observed results).

> Revalidate every live value (image digest, node identity, snapshot, endpoints)
> before reuse. This manual contains no credentials.

---

## 0. What Wave 1 was, and what "replicate" means

Wave 1 measured how reproducible Iris/Judger per-testcase `cpuTime` and
`realTime` are, and how that reproducibility changes as Iris workers contend on
one physical node.

- **Experiment A (intrinsic):** 1 Iris, 1 message, many identical
  `userTestcases`; compile once, execute N times. Isolates pure execution
  variance.
- **Experiment C (contention ladder):** scale Iris `1→30`, `50 × replicas`
  independent single-testcase messages, warmup before each rung. Measures
  contention and reproducibility loss.
- **Metric:** per-testcase `cpuTime`/`realTime` (ms), summarized as median, CV,
  and p99/median; plus host PSI and `kubectl top`.

Reference results (server8, second ladder run, `wave1-20260917-172616`):

| replicas | n | CPU median ms | CPU CV% | CPU p99/med |
| --- | --- | --- | --- | --- |
| 1 | 50 | 365.0 | 5.33 | 1.104 |
| 2 | 100 | 376.0 | 7.05 | 1.131 |
| 4 | 200 | 379.5 | 6.48 | 1.165 |
| 8 | 400 | 429.0 | 9.31 | 1.317 |
| 16 | 800 | 477.0 | 17.58 | 2.023 |
| 30 | 1500 | 771.0 | 35.21 | 2.642 |

The headline finding: reproducibility holds to ~8 workers and degrades sharply
after, driven by Judger moving submissions into **root-level, unaffined
`/sandbox-*` cgroups** that the 1-CPU pod limit does not bound.

---

## 1. Critical environment fact: server8 is no longer a cluster node

The original Wave 1 ran on `skkuding-4f-4` **inside the `prod` Kubernetes
cluster**, reached as the `codedang8` SSH alias. That node has since been
detached from the cluster (`k3s-agent` disabled). The `prod` cluster now has
`skkuding-4f-1/2/3/5` only.

Consequences:

- The original k8s procedure below (**Track A**) cannot run today against
  server8. It needs a spare, schedulable benchmark node in a cluster.
- The toolset's standalone procedure (**Track B**) targets detached server8 as a
  bare-metal host, using the dedicated `codedang-iris-benchmark` resources
  (`../LOG.md`), not the production cluster.

Choose the track explicitly and record which one the run used.

---

## 2. Prerequisites

Common:

- SSH access to the benchmark host via the operator's existing alias
  (`codedang8`), reusing `~/.ssh/sockets/codedang8.sock`.
- Local AWS admin identity for any RDS action (the `sv3` identity is read-only).
- A pinned Iris image digest resolved from `ghcr.io/skkuding/codedang-iris:stage`
  (record the digest; never rely on the mutable tag).
- The pinned Judger alpha.4 amd64 SHA-256:
  `2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103`.

Track A additionally:

- Cluster write access (`kubectl --context <ctx>`) and a cordonable node.
- A production-derived RDS snapshot ARN (approved, read-only).

Track B additionally:

- The dedicated benchmark RDS clone and S3 bucket are available
  (`../.env.example`), and the read-only DB role is bootstrapped.

---

## 3. Track A — faithful original reproduction (Kubernetes)

This is the original method. It mutates a cluster: cordon/drain a node, create
rabbitmq.com CRs, deploy `iris-bench`, and restore a temp RDS. Treat it as a
production operation and get explicit approval for the target node and time
window. Do not run it against a node that still serves production traffic
without a drain plan.

Condensed procedure (full commands: `docs-local/plans/wave1/REPRODUCE.md`):

1. **Capture preflight (read-only).** Nodes, `top nodes`, PDBs, pods on the
   target node, and host state over the control socket (date, uptime, `lscpu`,
   `numactl --hardware`, `/proc/pressure/*`, top processes).
2. **Isolate the node.** `kubectl cordon`; evict non-DaemonSet pods. Keep
   node-local `local-path` PVC workloads (grafana, loki-0, n8n). Capture
   post-drain PSI over a 60 s window.
3. **RabbitMQ `/loadtest` topology.** vhost `loadtest`; exchange
   `iris.e.direct.judge` (direct, durable); request queue
   `client.q.judge.benchmark` (key `judge.benchmark`); result queue
   `iris.q.judge.benchmark-result` (key `judge.benchmark.result`).
   **Never touch `/vh` or `client.q.judge.submission`.**
4. **Temporary RDS.** Restore from the approved snapshot into a new instance,
   apply the production parameter group, rotate the master password, and create
   a separate Secret. **Never reuse the production DB endpoint or Secret.**
5. **Baseline fixture.** Fixed problem, fixed accepted source, fixed one
   testcase, `timeLimit`/`memoryLimit` fixed. Keep code, testcase, language,
   limits, queues, and duration constant; vary only the replica count.
6. **Deploy `iris-bench`** with the pinned digest, `nodeName` pinned to the
   target node, `privileged: true`, host `/sys/fs/cgroup` mounted read-write.
   Verify the effective DB host, vhost, request queue, and routing key before
   sending anything.
7. **Warmup** before each rung (discard warmup results).
8. **Run A** (1 Iris, 1 message, N identical testcases) and **C** (ladder
   `1/2/4/8/16/30`, `count = 50 × replicas`, queue pre-filled so all workers
   start together).
9. **Record** results NDJSON, Iris logs (`--prefix=true`), PSI, `kubectl top`,
   and one cgroup/affinity snapshot during an active run.
10. **Cleanup.** Delete `iris-bench`, its ConfigMap/Secret, and the RabbitMQ
    CRs/bindings; delete the temp RDS; uncordon the node only after the Wave 1
    topology is gone. Verify production Iris returns to full replicas.

Hard constraints:

- RabbitMQ `max_message_size = 16 MiB`; keep Experiment A ≤ ~150 testcases.
- AMQP `messageId` must be numeric (Iris derives `submissionId` via `Atoi`).
- `bindings.rabbitmq.com` cannot be deleted in a combined multi-kind delete;
  delete the two bindings explicitly.
- Do not mount the production `database-credentials` Secret into `iris-bench`
  (last `envFrom` wins and would override the benchmark URL).

---

## 4. Track B — toolset standalone reproduction (current, gated)

This is the direction in `../PROPOSE.md`: detached server8, dedicated AWS
resources, no production cluster.

Implemented and runnable now:

- Read-only host qualification:
  `scripts/qualify-host.sh codedang8`.
- Controller plan resolution (immutable run plan + SHA-256):
  `iris-benchctl plan --config config/profiles.json --host codedang8 --profile isolated-1s ...`.
- Dedicated fixture integrity and upload:
  `fixtures/tests/verify-fixtures.sh`, `scripts/aws/upload-fixtures.sh --dry-run`.

Gated (do not claim success):

- `iris-benchctl run` reaches `run-block`, which **fails closed** because Judger
  alpha.4 writes `/sys/fs/cgroup/sandbox-<CONTAINER_ID>` at the cgroup root and
  offers no way to select a delegated parent (see section 9).
- The full-Iris/AMQP suite is not implemented, so the original A/C ladder cannot
  be driven through the toolset yet.

When Track B is unblocked, the intended flow is:

1. `provision` → `qualify` the host (Ansible role; requires sudo/become).
2. `plan` with the pinned Iris digest and Judger digest.
3. `run --suite judger` for the direct baseline; then `run --suite iris` for the
   full path.
4. `collect`, then analyze; write an immutable run bundle.

Until then, Track B produces qualification evidence and a resolved plan, not an
accepted measurement.

---

## 5. Metrics and acceptance

Report per testcase `cpuTime` and `realTime`: count, failures, median, MAD,
stdev, CV, p90/p95/p99/max, and p99/median. Keep raw samples.

Initial qualification targets (from `../PROPOSE.md`, to be revised after first
controlled data):

- direct one-worker `cpuTime` CV ≤ 3%;
- ≤ 5% change in median and p95 between accepted reboot blocks;
- ≤ 3% drift between the baseline at the start and end of one block.

For a Wave 1-style ladder, record the rung at which CV/p99 first departs
materially and correlate it with measured resource effects (run queue, PSI
counter deltas, cgroup throttling, NUMA locality).

---

## 6. Evidence bundle

```text
runs/<run-id>/
├── manifest.json          # versions, digests, host facts, parameters
├── qualification/         # host facts and readiness
├── config/                # resolved plan and profiles
├── samples/               # judger.ndjson, iris.ndjson
├── telemetry/             # pressure/PSI, frequency, thermal
├── logs/                  # iris-<rung>.log
├── analysis/              # statistics output
├── checksums.sha256
└── COMPLETE               # only after counts, conservation, checks, cleanup
```

Partial runs stay available but are marked invalid with a machine-readable
reason. Never write credentials or full connection strings into a bundle.

---

## 7. Cleanup and restore

- Track A: delete only resources labeled `codedang.com/wave1=true` and the
  `iris-bench*` objects, delete the temp RDS, then uncordon. Confirm the
  production vhost set is back to `vh` only and production Iris is at full
  replicas.
- Track B: preserve immutable downloaded evidence; remove only the current
  `run-id`-scoped runtime directory, containers, queues, and delegated cgroups.
- The approved RDS snapshot is kept, never deleted.

---

## 8. Safety rules

- Production RDS, S3, RabbitMQ, and cluster nodes are read-only unless the run
  explicitly authorizes a mutation and a target.
- Never consume or publish to the production judge queues (`/vh`,
  `client.q.judge.submission`).
- Never reuse production database credentials for a benchmark.
- Any cgroup escape is a failed run, not a warning.
- Stop on thermal throttling, machine checks, unexpected service activation,
  fixture mismatch, or missing results.

---

## 9. Known blockers and gates (as of this review)

1. **Judger containment (Gate 0).** alpha.4 hardcodes the root-level sandbox
   cgroup and has no delegated-parent option. The direct suite refuses to accept
   a sample outside the intended subtree, so it cannot currently produce an
   accepted measurement. Resolution is a reviewed Judger patch/upgrade, a
   private-cgroup mount, or a KVM fallback (see `../PROPOSE.md`).
2. **server8 detached.** Track A cannot run against server8 until a benchmark
   node is returned to a cluster (or Track B is completed).
3. **Full-Iris suite unimplemented.** The A/C ladder cannot be driven through the
   toolset yet; only qualification and planning are available.
4. **Privileged validation unperformed.** No real-host cgroup/delegation or
   container lifecycle has been exercised in tests.
5. **cgroup-related measurement subtlety.** alpha.4 defines `cpu_time` as
   `ru_utime` only (user time, excluding system time); account for this when
   interpreting `cpuTime` for syscall-heavy work.

---

## 10. Fresh-operator quickstart checklist

- [ ] Read `../PROPOSE.md`, `../LOG.md`, and this manual.
- [ ] Decide and record Track A or Track B.
- [ ] Confirm the target (node/alias, cluster context, or detached host).
- [ ] Verify access: SSH alias/socket, kubectl capability (Track A), AWS admin
      (temp RDS), and the dedicated resources (Track B).
- [ ] Resolve and record the Iris image digest and Judger SHA-256.
- [ ] Capture read-only preflight before any mutation.
- [ ] Run warmups; discard them.
- [ ] Run A then C (or the direct baseline), capturing raw samples and PSI.
- [ ] Analyze; write the evidence bundle; mark COMPLETE only if all checks pass.
- [ ] Clean up and verify production/exit state.
