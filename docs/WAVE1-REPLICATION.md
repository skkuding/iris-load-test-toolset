# Wave 1 Replication Manual (Toolset Operator)

Last reviewed: 2026-09-27

> **Read this first — what is reproducible today.** Track A's original
> Kubernetes artifacts remain outside this repository, and isolated Judger
> containment still fails closed. Track B supports both the direct Judger suite
> and the standalone external-data full-Iris suite documented in
> `FULL-IRIS-EXTERNAL.md`. Both use explicit `--production-compat` mode with
> stock alpha.4 and are marked uncontained and non-comparable.

This is the operator manual for reproducing the original **Wave 1** Iris
runtime-reproducibility experiment. It is written so that a new operator, on a
different machine, can use this repository alone. The private benchmark plan and
progress log are maintained separately and are **not** part of this repository.

What is inside this repository:

- `README.md` — toolset overview, commands, and AWS setup.
- `docs/WAVE1-REPLICATION.md` — this manual.
- `config/profiles.json`, `config/run.example.json` — controller configuration.
- `fixtures/` — sanitized testcases, metadata, and the pinned manifest.
- `.env.example` — non-secret names, endpoints, ARNs, and the reviewed Iris
  digest.

Revalidate every live value (image digest, host identity, snapshot, endpoints)
before reuse. This manual contains no credentials.

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
  and p99/median; plus host PSI and node/pod CPU.

Reference results from the original run (the specific machine no longer exists;
treat as a target shape, not a guarantee):

| replicas | n | CPU median ms | CPU CV% | CPU p99/med |
| --- | --- | --- | --- | --- |
| 1 | 50 | 365.0 | 5.33 | 1.104 |
| 2 | 100 | 376.0 | 7.05 | 1.131 |
| 4 | 200 | 379.5 | 6.48 | 1.165 |
| 8 | 400 | 429.0 | 9.31 | 1.317 |
| 16 | 800 | 477.0 | 17.58 | 2.023 |
| 30 | 1500 | 771.0 | 35.21 | 2.642 |

Headline finding: reproducibility holds to ~8 workers and degrades sharply
after, driven by Judger moving submissions into **root-level, unaffined
`/sandbox-*` cgroups** that the 1-CPU pod limit does not bound.

---

## 1. Choose a track, and know the environment fact

There are two ways to reproduce the experiment. Choose one and record which one
the run used.

- **Track A — original method (Kubernetes).** A dedicated node in a cluster,
  drained, running a pinned `iris-bench` Deployment against a temporary RDS
  restored from a snapshot, driven through RabbitMQ. This is what Wave 1
  literally did.
- **Track B — standalone toolset (this repository).** A detached bare-metal
  host qualified by the Ansible role, with the dedicated
  `codedang-iris-benchmark` RDS/S3 resources, driven by `iris-benchctl` and the
  host agent.

Environment fact: the original Wave 1 node (`server8`, alias `codedang8`) has
since been **detached from the Kubernetes cluster**. Track A therefore cannot
use it today; Track A needs a spare, schedulable cluster node. Track B targets
detached server8 as a bare-metal host. Do not silently swap one for the other.

---

## 2. Prerequisites

Build the tools (from the repository root):

```bash
go build ./...
go build -o bin/iris-benchctl    ./cmd/iris-benchctl
go build -o bin/iris-bench-agent ./cmd/iris-bench-agent
go build -o bin/judger-bench     ./cmd/judger-bench
```

Common:

- Go toolchain (see `go.mod`) and `g++` for compile tests.
- `ansible-core`, plus the collection the host role requires:
  `ansible-galaxy collection install -r ansible/requirements.yml`
  (`ansible.posix` is required by the role's sysctl task and is **not**
  committed to the repository).
- Docker with `buildx` **only** if you use `--resolve-image` or build
  `images/judger-bench.Dockerfile`; otherwise supply digests explicitly.
- **SSH access to the benchmark host is provided by the operator's
  environment.** The operator is expected to have a working, non-interactive
  SSH path to the target (an `~/.ssh/config` entry with any `ProxyJump`, plus a
  control socket). Confirm it works before anything else. The controller and
  Ansible each use a configurable control socket; align them with the operator's
  existing socket or provide key/agent authentication. The socket location is
  environment-specific and is not fixed by this repository.
- Optional: `cp .env.example .env`. `.env` feeds the AWS shell scripts (not the
  controller) and is git-ignored.
- For qualification/provisioning: `sudo`/become access on the host. Server hosts
  in this environment have **no passwordless sudo**, so `--ask-become-pass` and
  an interactive terminal are required.
- A pinned Iris image digest. `.env.example` records a reviewed cache
  (`IRIS_BENCHMARK_IRIS_IMAGE_DIGEST`). `--resolve-image` needs Docker with
  `buildx`; if Docker is unavailable, supply `--iris-digest` explicitly and
  re-resolve before a real run.
- The pinned Judger alpha.4 amd64 SHA-256:
  `2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103`.

Track A additionally:

- Cluster write access (`kubectl --context <ctx>`) and a cordonable node whose
  API is reachable from your operator machine.
- A production-derived RDS snapshot identifier (approved, read-only to
  discover).
- `jq`, `base64`, and a publisher/collector driver. **The Track A manifests,
  RabbitMQ CRs, and driver are not part of this repository**; see §3 and the
  portability limits in §11.

Track B additionally:

- The dedicated benchmark RDS clone and S3 bucket exist and are reachable
  (`README.md`; `.env.example`), and the read-only DB role is bootstrapped.
  **Creating that infrastructure is documented in `README.md` and `infra/aws/`;
  this manual assumes it already exists in your account.** A different account
  or operator cannot create it from this manual alone.
- For the AWS fixture/secret scripts: AWS CLI v2, `jq`, and `psql` (a
  containerized `psql` wrapper is provided as `scripts/aws/psql-container.sh`).

---

## 3. Track A — original method (Kubernetes)

This mutates a cluster: cordon/drain a node, create `rabbitmq.com` CRs, deploy
`iris-bench`, and restore a temp RDS. Treat it as a production operation; get
explicit approval for the target node and time window, and never drain a node
that still serves production without a drain plan.

The Kubernetes manifests, RabbitMQ custom resources, and the AMQP
publisher/collector driver used by the original run are **not in this
repository**. Track A is documented here for context and reconstruction; a
fresh machine must author or obtain those artifacts. This is called out again in
§11.

Summary of the procedure:

1. **Preflight (read-only).** Nodes, `top nodes`, PDBs, pods on the target node,
   and host state over SSH (date, uptime, `lscpu`, `numactl --hardware`,
   `/proc/pressure/*`, top processes). Confirm the cluster API is reachable
   before promising a run.
2. **Isolate the node.** Cordon; evict non-DaemonSet pods. Keep node-local
   `local-path` PVC workloads. Capture a clean 60 s PSI window after drain.
3. **RabbitMQ `/loadtest` topology.** vhost `loadtest`; exchange
   `iris.e.direct.judge` (direct, durable); request queue
   `client.q.judge.benchmark` (key `judge.benchmark`); result queue
   `iris.q.judge.benchmark-result` (key `judge.benchmark.result`).
   **Never touch the production vhost or the production submission queue.**
4. **Temporary RDS.** Restore from the approved snapshot into a new instance,
   apply the production parameter group, rotate the master password, and create
   a separate Secret. **Never reuse the production DB endpoint or Secret.**
5. **Baseline fixture.** Fixed problem, fixed accepted source, fixed one
   testcase, fixed `timeLimit`/`memoryLimit`. Keep code, testcase, language,
   limits, queues, and duration constant; vary only the replica count.
6. **Deploy `iris-bench`** with a pinned digest, pinned to the target node,
   `privileged: true`, host `/sys/fs/cgroup` mounted read-write. Verify the
   effective DB host, vhost, request queue, and routing key before sending
   anything.
7. **Warmup** before each rung; discard warmup results.
8. **Run A** (1 Iris, 1 message, N identical testcases) and **C** (ladder
   `1/2/4/8/16/30`, `count = 50 × replicas`, queue pre-filled so all workers
   start together).
9. **Record** results, Iris logs (`--prefix=true`), PSI, and one
   cgroup/affinity snapshot during an active run.
10. **Cleanup.** Delete `iris-bench`, its ConfigMap/Secret, and the Wave 1
    RabbitMQ CRs/bindings; delete the temp RDS; uncordon only after the Wave 1
    topology is gone. Verify production returns to full replicas.

Hard constraints:

- RabbitMQ `max_message_size = 16 MiB`; keep Experiment A ≤ ~150 testcases.
- AMQP `messageId` must be numeric (Iris derives `submissionId` via `Atoi`).
- Node-local PVC workloads cannot be rescheduled; decide explicitly to keep
  them.
- Do not mount the production database Secret into `iris-bench` (the last
  `envFrom` source wins and would override the benchmark URL).

---

## 4. Track B — standalone toolset (current, gated)

Implemented and runnable now:

```bash
# 1. Local correctness and integrity.
go test ./...
fixtures/tests/verify-fixtures.sh
scripts/aws/upload-fixtures.sh --dry-run --bucket codedang-iris-benchmark-testcases

# 2. Resolve and seal an immutable run plan (digests are mandatory).
go run ./cmd/iris-benchctl plan \
  --config config/profiles.json \
  --host codedang8 \
  --profile isolated-1s \
  --iris-digest sha256:39b743d6e1ffb1efaa18f42b509c2017b91c11976971c862beafda2ae1521250 \
  --judger-digest 2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103 \
  --benchmark-image sha256:<local-judger-bench-image-id> \
  --bench-binary /opt/iris-bench/bin/judger-bench \
  --bench-binary-sha256 <workload-sha256> \
  --fixture cpp-runtime-v1=/absolute/shared/path/15850.in \
  --expected-output cpp-runtime-v1=/absolute/shared/path/15850.out \
  --production-compat

# 3. Inspect the plan without mutation (same mandatory digests).
go run ./cmd/iris-benchctl run \
  --config config/run.example.json \
  --host codedang8 --profile isolated-1s --dry-run \
  --iris-digest sha256:39b743d6e1ffb1efaa18f42b509c2017b91c11976971c862beafda2ae1521250 \
  --judger-digest 2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103 \
  --benchmark-image sha256:<local-judger-bench-image-id> \
  --bench-binary /opt/iris-bench/bin/judger-bench \
  --bench-binary-sha256 <workload-sha256> \
  --fixture cpp-runtime-v1=/absolute/shared/path/15850.in \
  --expected-output cpp-runtime-v1=/absolute/shared/path/15850.out \
  --production-compat
```

Notes from real use:

- `plan` and `run` require `--iris-digest`, `--judger-digest`, and either
  `--local-bench-binary` or both `--bench-binary` and
  `--bench-binary-sha256`; the placeholders in older notes are not optional.
  Exactly one `--fixture` and matching `--expected-output` are mandatory for a
  runnable direct Judger plan.
- `--production-compat` additionally requires an exact local Docker image ID
  from `docker image inspect --format '{{.Id}}' judger-bench:alpha.4`. The
  controller seals and forwards it; the agent runs one privileged,
  host-cgroup-namespace container per worker and uses the image's
  `/app/sandbox/libjudger.so` while re-checking the sealed alpha.4 digest.
- `--resolve-image` needs Docker with `buildx`. If it is unavailable, pass
  `--iris-digest` explicitly (the `.env.example` value is a reviewed cache, not
  a guarantee).
- The `isolated-1s` profile uses `cpuList: "2-3"` (two CPUs). For a strict
  one-CPU baseline, use the `isolated-1s-single-cpu` profile.
- `iris-benchctl provision`, `qualify`, and `resume` are **not
  implemented**. Host provisioning/qualification use the Ansible role.
- `scripts/qualify-host.sh` is read-only but runs `become: true` and ends in
  post-provision assertions, so it **only passes after** `provision`. On a fresh
  host it fails at `Gathering Facts` (no credentials) or at the assertions.

Host qualification and provisioning (requires sudo/become and a provisioned
host):

```bash
# Read-only report; requires a password for become.
scripts/qualify-host.sh codedang8

# Provision (idempotent; reboot may occur on first run).
ANSIBLE_CONFIG=ansible/ansible.cfg \
  ansible-playbook ansible/playbooks/provision_benchmark_host.yml --ask-become-pass
```

Important: the qualification playbook verifies **post-provision** state (the
delegated `iris-bench.slice`, benchmark directories, the CPU-policy unit, and
sysctls). On a host that has not been provisioned it cannot pass. Order is
`provision` **then** `qualify`.

Gated (do not claim success):

- Isolated `run-block` **fails closed** because Judger alpha.4 writes
  `/sandbox-<CONTAINER_ID>` at the cgroup root and offers no way to select a
  delegated parent (see §9). `--production-compat` accepts that root or a
  descendant such as `/sandbox-<CONTAINER_ID>/box-*` in either the
  mount-relative form the monitor holds or the full filesystem form the live
  Judger reports (`/sys/fs/cgroup/sandbox-<CONTAINER_ID>/...`), but marks
  samples uncontained and the run non-comparable. Production-compat
  requires the sealed OCI worker image because unprivileged host execution does
  not reproduce this root cgroup quirk. The agent also requires an already
  staged binary at `/opt/iris-bench/bin/<toolVersion>/iris-bench-agent` or an
  executable supplied with `--local-agent`, a `--cgroup-parent` delegated
  subtree and a qualified host; OCI mode does not stage or use a host Judger.
  For an actual run, omit `--dry-run`, provide those flags, provision then
  qualify the host, and pass the sanitized report with
  `--qualification-report` if comparable qualification evidence is required.

The Track B flow is:

1. `provision` → `qualify` the host.
2. `plan` with the pinned digests and fixture.
3. `run --suite judger` for the direct baseline, or `run --suite iris
   --production-compat` for the external-data full-Iris suite documented in
   `FULL-IRIS-EXTERNAL.md`. Both are production-compat and non-comparable until
   Judger accepts a delegated cgroup parent.
4. `collect`, then analyze; write an immutable run bundle.

Until Judger supports a delegated parent, Track B can produce accepted
production-compat diagnostics, but not an accepted isolated measurement.

---

## 5. Metrics and acceptance

Report per testcase `cpuTime` and `realTime`: count, failures, median, MAD,
stdev, CV, p90/p95/p99/max, and p99/median. Keep raw samples.

Direct-suite blocks enforce a **steady measurement window**. Every worker is
started behind the readiness barrier and then runs closed-loop with no
client-side delay, so within the all-workers-active window every worker always
has a submission in flight or pending and the system under test is never idle.
`judger-bench` timestamps each submission with a host-wide clock
(`startedAtNs`/`endedAtNs`). The coordinator takes the window as
`[max over workers of first start, min over workers of last end]` and refuses
the block when any worker has no valid positive timestamp, the window is empty,
or its duration is below `min(2s, 0.5 × (max lastEnd − min firstStart))`. The
receipt records `steadyWindowStartNs`, `steadyWindowEndNs`, and
`steadyWindowSeconds`. Analyze and both plot scripts compute the primary timing
distributions only from samples whose `[start,end]` falls inside the window.
The OCI container-start ramp and the drain tail are excluded from these primary
statistics because container/image creation, cgroup setup, and the final
partial iterations measure startup and teardown latency rather than the
closed-loop steady state the experiment is about. Bundles without timestamps
keep the pre-window behavior.

Initial qualification targets (targets, to be revised after first
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
├── plan.json              # sealed resolved run plan
├── samples/               # <block-id>.ndjson
├── receipts/              # <block-id>.json
├── telemetry/             # thermal-<block-id>.ndjson
├── checksums.sha256
└── COMPLETE               # only after counts, conservation, checks, cleanup
```

`checksums.sha256` covers every regular published file, including `COMPLETE`,
except itself; the checksum list is not recursively checksummed.

Partial runs stay available but are marked invalid with a machine-readable
reason. Never write credentials or full connection strings into a bundle.

---

## 7. Cleanup and restore

- Track A: delete only resources labeled `codedang.com/wave1=true` and the
  `iris-bench*` objects, delete the temp RDS, then uncordon. Confirm the
  production vhost set is back to the original only, and production Iris is at
  full replicas.
- Track B: preserve immutable downloaded evidence; remove only the current
  `run-id`-scoped runtime directory, containers, queues, and delegated cgroups.
- The approved RDS snapshot is kept, never deleted.

---

## 8. Safety rules

- Production RDS, S3, RabbitMQ, and cluster nodes are read-only unless the run
  explicitly authorizes a mutation and a target.
- Never consume or publish to the production judge queues.
- Never reuse production database credentials for a benchmark.
- Any cgroup escape is a failed run, not a warning.
- Stop on thermal throttling, machine checks, unexpected service activation,
  fixture mismatch, or missing results.

---

## 9. Known blockers and gates (as of this review)

1. **Judger containment (Gate 0).** alpha.4 hardcodes the root-level sandbox
   cgroup and has no delegated-parent option. Isolated mode refuses it.
   `--production-compat` accepts `/sandbox-<CONTAINER_ID>` or its descendants and labels the
   result uncontained/non-comparable; validation normalizes the live filesystem
   form `/sys/fs/cgroup/sandbox-<CONTAINER_ID>/...` and the mount-relative form
   to the same run-scoped root. Live alpha.4 leaves one empty `box-*` child per
   iteration under that root, but the residue is safely removed after the
   evidence is read: the agent removes the empty root and its empty
   `box-<safe-id>` children only after proving `cgroup.procs` is empty for
   each, that no box has a grandchild cgroup, and that no unexpected directory
   name remains. Validation returns canonical full filesystem paths
   children-first, root-last, and root absence is verified afterwards. Any
   residual member, deeper child, or unexpected child fails the block and is
   preserved rather than blindly deleted. Because the stock root is root-owned,
   the unprivileged manager cannot `rmdir` it directly in OCI mode; the agent
   instead validates the paths read-only and removes them with a single
   argv-only privileged helper container from the sealed image (`docker run
   --rm --privileged --cgroupns=host --entrypoint /bin/rmdir --mount
   type=bind,src=<mount>,dst=<mount> <image> <child>... <root>`, no shell; host
   mode removes the same validated paths in order). The run-scoped helper name
   and labels make residual cleanup remove an interrupted helper. This does not
   resolve the isolated gate.
2. **server8 detached.** Track A cannot run against it until a benchmark node is
   returned to a cluster; Track B targets it standalone.
3. **Isolated measurement still gated.** The direct Judger and external-data
   full-Iris suites are runnable and analyzable only in production-compat mode;
   an isolated A/C ladder still requires a Judger that accepts a delegated
   cgroup parent.
4. **Privileged validation unperformed.** No real-host cgroup/delegation or
   container lifecycle has been exercised in tests.
5. **cgroup-related measurement subtlety.** alpha.4 defines `cpu_time` as
   `ru_utime` only (user time, excluding system time); account for this when
   interpreting `cpuTime` for syscall-heavy work.

---

## 10. Fresh-operator quickstart checklist

- [ ] Read `README.md` and this manual.
- [ ] Decide and record Track A or Track B.
- [ ] Confirm the target (host alias, cluster context, or detached host).
- [ ] Build the tools and verify `go test ./...`.
- [ ] Verify SSH reachability to the host (do not assume a socket is reused).
- [ ] Confirm privilege path (sudo/become) and AWS access if mutating.
- [ ] Resolve and record the Iris image digest and Judger SHA-256.
- [ ] Capture read-only preflight before any mutation.
- [ ] Run warmups; discard them.
- [ ] Run A then C (or the direct baseline), capturing raw samples and PSI.
- [ ] Analyze; write the evidence bundle; mark COMPLETE only if all checks pass.
- [ ] Clean up and verify the exit state.

---

## 11. Portability limits

This repository is intended to be operable on another machine by another person.
As of this review, the following are honest limits:

- **Track A is not self-contained.** The Kubernetes manifests, RabbitMQ custom
  resources, and publisher/collector driver are not in this repository. Only the
  method is documented.
- **Track B assumes existing AWS resources.** The
  `codedang-iris-benchmark` RDS clone, S3 bucket, KMS key, and IAM roles are
  created from `README.md`/`infra/aws/` and are account-specific. Another
  account must create its own.
- **The Iris digest is a cache, not a resolution.** Without Docker/buildx you
  cannot re-resolve `ghcr.io/skkuding/codedang-iris:stage` from the repo alone.
- **The agent and Judger are external.** The controller can stage an explicit
  `--local-agent` and verifies its remote SHA-256 before execution. The alpha.4
  Judger binary is downloaded by the image build; neither ships as a committed
  binary.
- **Isolated measurement remains gated.** Production-compat direct and
  full-Iris diagnostics can be accepted but are non-comparable. Gate 0 still
  blocks an isolated Wave 1 end-to-end measurement.
- **Hand off a clean tree.** Use a normal clone or an archive that excludes
  untracked, secret-bearing files such as `terraform.tfvars`, `*.tfstate*`,
  `.env`, and `.terraform/`. Do not package a live working directory as-is.
