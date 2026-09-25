# Reproducible Iris Runtime Benchmark Proposal

Last reviewed: 2026-09-25

## Purpose

Build a reproducible benchmark appliance for investigating and stabilizing the
runtime reported by Iris and Judger. The first target is the detached server8,
but the tooling must also qualify and run on another compatible bare-metal
host.

An operator with SSH access runs the tool from a controller machine. The tool
provisions the target through SSH, verifies the effective host state, runs the
experiments, downloads a complete result bundle, and restores the host to an
idle benchmark state.

The benchmark has two separate suites:

1. **Direct Judger suite:** measures compilation and sandbox execution without
   AMQP or Iris service overhead. This is the primary suite for stabilizing the
   recorded `cpuTime` and `realTime` values.
2. **Full Iris suite:** sends normal judge requests through a local RabbitMQ to
   Iris and collects normal judge results. It validates that improvements found
   in the direct suite remain valid through the real service boundary.

The default full-Iris environment uses local RabbitMQ, a persistent dedicated
AWS RDS benchmark instance restored from an approved checkpoint, and production
S3 through a narrowly scoped read-only identity. It must not use production
RabbitMQ, the production database, Kubernetes, or production cluster secrets.

## Current Evidence

- server8 is no longer a production Kubernetes node. `k3s-agent` is disabled
  and inactive.
- The host is Ubuntu 24.04.4 with kernel `7.0.0-31-generic`.
- It has two Xeon E5-2620 v4 sockets, 16 physical cores, 32 hardware threads,
  and two NUMA nodes. NUMA memory is asymmetric: approximately 32 GiB on node 0
  and 16 GiB on node 1.
- SMT and turbo are enabled. The active CPU governor is `schedutil`.
- Docker 29.7.2, containerd 2.3.3, and cgroup v2 are installed.
- Docker currently has no running containers, but the host still runs a full
  desktop and many unrelated services, including GDM, GNOME, CUPS, Avahi,
  ModemManager, fwupd, and power-profiles-daemon.
- The old K3s binary and state have not yet been treated as removed benchmark
  dependencies.
- Wave 1 measured C++ execution CPU-time CV near 5-9% through eight workers,
  17.6% at 16 workers, and 35.2% at 30 workers. The 30-worker case
  oversubscribed the 16 physical cores.
- Wave 1 proved that submission processes entered root-level `/sandbox-*`
  cgroups with affinity to CPUs `0-31`. They were not contained by the Iris
  pod's one-CPU limit.
- Iris currently runs privileged and mounts host `/sys/fs/cgroup` read-write.
  Its entrypoint creates root-level sandbox cgroup state.
- The production Iris image installs Judger from:

  ```text
  https://github.com/skkuding/Judger/releases/download/v1.0.0-alpha.4/libjudger-amd64.so
  ```

  The artifact version is fixed, but the Dockerfile does not currently verify
  a checksum.

These facts mean that pinning only the Iris image is insufficient. Kernel,
microcode, firmware, CPU policy, NUMA placement, cgroup behavior, compiler
versions, background services, and fixture contents all affect the result.

## Success Criteria

The project is successful when all of the following are true:

- A new authorized operator can provision and run the benchmark with one
  documented controller command and SSH/sudo access.
- Provisioning is idempotent. A second run reports no unexpected host changes.
- Every executable, image, fixture, configuration, and external data dependency
  is identified by version and cryptographic digest.
- Every submission PID remains in the intended cgroup, CPU set, and NUMA policy.
  Any escape is a failed run, not a warning.
- Repeating the direct one-worker baseline after reboot produces comparable
  distributions. Initial qualification targets are CPU-time CV at or below 3%,
  no more than 5% change in median and p95 between accepted reboot blocks, and
  no more than 3% drift between the baseline at the beginning and end of one
  block. These thresholds should be revised after the first controlled data.
- The full-Iris suite conserves messages: published requests equal completed
  submissions plus explicitly classified failures, with no unexplained
  duplicate or missing result.
- Results contain enough evidence to distinguish application variance from
  frequency, thermal, scheduler, cgroup, NUMA, RabbitMQ, database, S3, and
  network effects.
- No benchmark credential grants production writes or production control-plane
  access, and no secret appears in a result bundle.

## Non-Goals

- Recreating the old production Kubernetes topology on server8.
- Generating end-to-end frontend or client-api traffic.
- Treating different CPU models, kernel versions, or NUMA layouts as one result
  population.
- Making 30 workers stable on a 16-core machine. SMT and oversubscription are
  separate characterization experiments, not the initial stabilization target.
- Using production datasets beyond the approved testcase objects and an
  isolated RDS checkpoint.

## Recommended Method

Use three layers rather than one large shell script:

1. **Ansible host role** for persistent, idempotent provisioning over SSH.
2. **Digest-pinned OCI workloads** for Iris, RabbitMQ, toolchains, and benchmark
   agents. Do not install a single-node Kubernetes distribution.
3. **A Go benchmark controller/agent** for experiments, evidence capture,
   result validation, and analysis input.

Ansible fits the stated SSH access model and can verify the resulting host after
reboot. OCI images reproduce user space while retaining the same privileged
cgroup behavior that must be investigated. A compiled Go controller avoids the
quoting, process-lifetime, and partial-cleanup problems present in the Wave 1
Bash harness.

An Ubuntu autoinstall image can be added later for rebuilding hosts from bare
metal. It is not the first implementation because server8 currently lacks a
documented BMC/PXE path. The Ansible role must nevertheless assume a clean,
supported Ubuntu LTS host and explicitly report residual state.

## Proposed Repository Layout

Keep benchmark code with the CodeDANG source so changes to Iris and the
benchmark can be reviewed together. A proposed layout is:

```text
tests/iris-runtime/
├── README.md
├── config/
│   ├── profiles.yaml
│   ├── rabbitmq-definitions.json
│   └── compatibility.yaml
├── fixtures/
│   └── cpp-runtime-v1/
│       ├── source.cpp
│       ├── input.txt
│       ├── output.txt
│       └── manifest.yaml
├── cmd/
│   ├── iris-benchctl/
│   ├── iris-bench-agent/
│   └── judger-bench/
├── compose.yaml
├── images/
│   └── judger-bench.Dockerfile
├── analysis/
└── schemas/
    ├── run-manifest.schema.json
    └── sample.schema.json

infra/ansible/
├── inventory/loadtest/hosts
├── playbooks/iris_benchmark_host.yml
└── roles/iris_benchmark_host/
```

Generated results remain outside Git by default. Only small, sanitized,
license-compatible fixtures and schemas belong in the repository.

## Controller Interface

The intended operator flow is:

```bash
iris-benchctl provision --host codedang8 --ask-become-pass
iris-benchctl qualify --host codedang8
iris-benchctl run --host codedang8 --suite judger --profile isolated-1s
iris-benchctl run --host codedang8 --suite iris --profile isolated-1s
iris-benchctl collect --host codedang8 --run <run-id>
iris-benchctl analyze --run <run-directory>
```

`provision` invokes the reviewed Ansible playbook. The remaining commands use
SSH and a version-matched agent staged on the host. Commands must be resumable:
each phase records state, and cleanup targets only resources carrying the
current run ID.

The controller must refuse to run when qualification fails unless the operator
uses an explicit diagnostic-only override. An overridden run is permanently
marked non-comparable.

## Controller Internals

### Process model

Use two version-matched Go binaries:

- `iris-benchctl` runs on the operator's controller machine. It resolves
  configuration, invokes Ansible, controls SSH, stages artifacts, advances the
  run state machine, downloads evidence, and performs final validation.
- `iris-bench-agent` is copied to the target for the duration of a tool version.
  It performs qualification, local container lifecycle, telemetry, experiments,
  and cleanup. Timing-sensitive work never crosses an SSH request boundary.

`judger-bench` remains a third, narrowly scoped executable inside the benchmark
image. It performs the direct compile/execute loop and emits samples. Do not put
that loop in either the SSH controller or a shell script.

The agent is not a network daemon. `iris-benchctl` invokes it through SSH for a
specific operation. This avoids another listening service, certificate system,
and remote API to secure.

Use the installed OpenSSH client instead of embedding an SSH implementation.
That preserves the operator's host aliases, control socket, ProxyJump, host-key
policy, hardware-backed keys, and interactive authentication. Construct
argument arrays with `exec.CommandContext`; never interpolate a remote shell
command from user input.

### Package layout

The controller code should be split by stable responsibility:

```text
tests/iris-runtime/
├── cmd/
│   ├── iris-benchctl/main.go
│   ├── iris-bench-agent/main.go
│   └── judger-bench/main.go
└── internal/
    ├── config/       # YAML loading, defaults, strict validation
    ├── runplan/      # resolved immutable plan and digest
    ├── orchestrator/ # state transitions and reconciliation
    ├── transport/    # OpenSSH and file transfer adapters
    ├── protocol/     # versioned request/event/result envelopes
    ├── provision/    # reviewed Ansible invocation
    ├── qualify/      # host facts and policy evaluation
    ├── runtime/      # Compose/container and local service lifecycle
    ├── experiment/   # suites, blocks, trials, stop conditions
    ├── amqp/         # publisher, collector, topology checks
    ├── telemetry/    # sampler coordination and event timestamps
    ├── artifact/     # bundles, hashes, atomic transfer
    ├── manifest/     # run manifest construction and validation
    ├── analyze/      # statistics and population compatibility
    └── secret/       # credential input and redaction boundaries
```

Depend on interfaces only at operating-system boundaries such as command
execution, SSH, clocks, and filesystems. Keep experiment logic concrete; avoid
building a general workflow engine.

### Resolved run plan

The controller first converts user configuration into one immutable JSON run
plan. The plan includes:

- schema and tool versions;
- run ID and random seed;
- target SSH alias and expected host identity;
- suite, profile, fixtures, block order, repetitions, worker assignments, and
  stop conditions;
- exact image and Judger digests;
- RabbitMQ definitions digest;
- RDS generation and S3 object-set identifiers;
- required host facts and qualification thresholds;
- expected result counts and cleanup policy.

Resolve tags to image manifest digests before execution. Resolve topology-aware
CPU assignments only after qualification, then seal the final plan. Write it
locally, upload it, and verify its SHA-256 on both sides. All subsequent events
and artifacts carry that digest.

`iris-benchctl plan` prints the resolved plan without mutation. `run` must show
the same summary and require explicit confirmation for provisioning, reboot, or
AWS-connected profiles unless `--yes` is provided.

### Remote protocol

Use JSON on stdin and NDJSON events on stdout. Human progress output belongs on
the controller's stderr, while machine-readable output remains clean.

Example request:

```json
{
  "protocolVersion": 1,
  "operationId": "01K...",
  "runId": "iris-20260925-...",
  "action": "run-block",
  "planSha256": "...",
  "blockId": "judger-isolated-1x-r03"
}
```

Example events:

```json
{"seq":1,"kind":"phase","phase":"qualify","status":"started"}
{"seq":2,"kind":"check","name":"cgroup-containment","status":"passed"}
{"seq":3,"kind":"artifact","path":"samples/judger.ndjson","sha256":"..."}
{"seq":4,"kind":"result","status":"completed","receiptSha256":"..."}
```

The protocol has a strict schema, bounded field sizes, monotonically increasing
sequence numbers, and one terminal result. The controller rejects unknown major
versions, missing events, changed plan digests, malformed paths, and duplicate
terminal results.

Do not stream sample data through stdout. The agent writes samples atomically on
the target and emits only an artifact receipt. The controller downloads and
verifies the artifact afterward.

### Target state and idempotency

Use these target paths:

```text
/opt/iris-bench/bin/<tool-version>/iris-bench-agent
/var/lib/iris-bench/runs/<run-id>/
/run/iris-bench/<run-id>/
```

`/var/lib` holds non-secret state and artifacts. `/run` is tmpfs-backed and
holds runtime credentials, PIDs, sockets, and generated environment files.

The agent takes an exclusive host lock before mutation. Each operation has an
`operationId`, input digest, start record, and terminal receipt. Repeating a
completed operation with the same inputs returns the existing receipt.
Repeating it with different inputs fails. An interrupted experiment is never
silently retried because that would mix thermal/cache histories; it is marked
invalid and a new trial is created.

Persist state with write-to-temp, `fsync`, and atomic rename. The minimum run
state machine is:

```text
planned
  -> qualified
  -> prepared
  -> running
  -> validating
  -> complete
  -> collected
  -> cleaned
```

`failed` and `interrupted` may be entered from every active state. Cleanup is a
separate idempotent transition and does not rewrite the experimental outcome.

### Command execution

The controller may invoke only fixed agent verbs, for example:

```text
inspect
prepare
run-block
validate
bundle
cleanup
status
```

The agent, in turn, runs commands from typed Go structures. It does not accept
arbitrary command strings from the plan. Every subprocess receives a context,
timeout, explicit environment allowlist, stdout/stderr destination, and process
group. On cancellation, terminate the process group, wait for it, and then
reconcile containers, cgroups, and telemetry processes.

Use `systemd-run` scopes or direct systemd D-Bus calls for long-lived local
components so ownership and cleanup survive an SSH disconnect. Container names,
systemd units, cgroups, and files all include the run ID and validated block ID.

### Privilege boundary

Most agent operations run as the dedicated benchmark user. A small reviewed
privileged helper, or explicit `sudoers` command rules, handles only:

- CPU governor/turbo and IRQ settings;
- delegated cgroup creation and verification;
- service lifecycle required by the benchmark profile;
- reboot and selected system inventory reads.

Do not grant the whole agent unrestricted passwordless sudo. Host provisioning
continues to use interactive Ansible become. Each privileged operation validates
CPU lists, unit names, cgroup paths, and run IDs before changing state.

### Secret flow

The controller obtains or prompts for secrets only when a selected profile
needs them. Send a secret envelope through the SSH process's stdin separately
from the non-secret plan. The agent writes values to
`/run/iris-bench/<run-id>/secrets.env` with mode `0600`, passes them to the
specific container, and removes the file during cleanup.

Secrets must not be accepted as command-line flags, included in process titles,
stored in `/var/lib`, returned in events, or inherited by unrelated subprocesses.
The artifact validator scans text outputs for known secret values and connection
URL patterns before marking a bundle complete.

### Suite execution

For the direct suite, one `run-block` operation performs the whole controlled
sequence locally:

1. verify qualification has not drifted;
2. start telemetry and record a monotonic start marker;
3. create worker cgroups and verify effective CPU/memory sets;
4. start all `judger-bench` workers behind a local barrier;
5. release the barrier after every worker reports ready;
6. monitor stop conditions and every sandbox PID;
7. wait for expected samples and worker exits;
8. stop telemetry and verify cgroup/process cleanup;
9. atomically finalize samples and the block receipt.

For the Iris suite, the agent additionally starts local RabbitMQ and Iris,
verifies topology and effective configuration, purges queues, starts the result
collector, publishes with confirms, and waits for semantic completion. Reuse the
Wave 1 AMQP code only after moving it into `internal/amqp` and adding:

- per-request publish timestamps and confirmation status;
- response-body parsing and correlation by numeric `submissionId`;
- expected judge-result and terminal-submission counts per request;
- duplicate, redelivery, missing-result, and unexpected-ID detection;
- queue-depth checks before and after a trial;
- context cancellation without polling sleeps or channel-closing races.

The collector must start before requests are published. For synchronized worker
tests, prefill the request queue, start all workers behind a readiness barrier,
then enable consumption at one recorded monotonic boundary.

### Artifact transfer

The agent creates a deterministic bundle inventory containing relative path,
size, SHA-256, media type, and sensitivity classification. The controller first
downloads the inventory, validates every path, then transfers artifacts into a
temporary local run directory. It verifies all sizes and hashes before an atomic
rename to the final run directory.

Use SFTP/SCP through OpenSSH initially. Add resumable chunk transfer only if
telemetry sizes justify it. Never download by a remote path supplied in an event
without checking that it is beneath the run directory.

### Recovery behavior

`iris-benchctl status` compares local state, the remote state file, active
systemd units, containers, cgroups, and PIDs. It reports drift rather than
guessing.

`iris-benchctl resume` may continue collection, validation, bundling, or cleanup.
It must not resume halfway through a measurement block. A partially executed
block is closed as interrupted, retained for diagnosis, and replaced by a new
block with a new ID.

After an SSH disconnect, local systemd scopes continue to enforce timeouts. On
reconnection the controller obtains the operation receipt and reconciles state.
If the host rebooted unexpectedly, the boot ID mismatch invalidates the active
block.

### Controller tests

The controller requires more than unit tests:

- strict config and protocol schema tests;
- golden resolved-plan and manifest tests;
- fake-command tests proving argument separation and environment redaction;
- an OpenSSH integration test against a disposable VM;
- agent interruption tests at every state transition;
- idempotent replay and conflicting-operation tests;
- corrupt, truncated, duplicate, and path-traversal artifact tests;
- Compose integration tests for RabbitMQ publish/collect conservation;
- reboot/disconnect recovery tests;
- end-to-end secret scanning of a synthetic result bundle;
- a small non-privileged CI mode using a fake Judger, with privileged cgroup
  validation run separately on the qualified benchmark host.

The CLI itself should remain thin. Command handlers load configuration, create a
run plan, and call the orchestrator; they must not contain SSH, AMQP, or
experiment logic.

## Host Provisioning

### Compatibility envelope

`compatibility.yaml` describes supported facts rather than silently assuming
server8:

- x86-64 with required instruction set;
- cgroup v2;
- supported Ubuntu LTS and kernel range;
- at least one complete physical core after reserving housekeeping CPUs;
- known NUMA topology;
- enough local memory and disk;
- required virtualization, seccomp, and namespace features;
- supported Docker/containerd versions;
- synchronized clock;
- no Kubernetes agent or production monitoring process running.

CPU model, socket count, core count, SMT state, NUMA topology, microcode,
firmware, kernel, and container runtime become comparison keys. A different
value starts a new benchmark population even if the host passes compatibility.

### Benchmark host role

The Ansible role should:

1. Confirm host identity and require an explicit `benchmark_host=true` inventory
   variable before making changes.
2. Remove or disable K3s and stale production agents only after a separate
   reviewed cleanup task confirms rollback data is no longer needed.
3. Install pinned runtime and measurement packages.
4. Disable unrelated desktop, printing, discovery, indexing, update, and remote
   desktop services. Keep SSH, networking, time synchronization, logging, and
   the selected container runtime.
5. Configure a stable CPU policy. The initial controlled profile uses the
   `performance` governor, SMT disabled, and one documented turbo policy. Turbo
   should initially be disabled for stability; a turbo-enabled profile is a
   separate comparison.
6. Reserve physical cores for the OS, SSH, interrupts, Docker, RabbitMQ, and
   Iris. Select benchmark cores by topology, not by a server8-only constant.
7. Route movable IRQs away from benchmark cores and verify effective affinity.
8. Create a dedicated benchmark user, directories, systemd slice, and cgroup v2
   delegation needed by the agent.
9. Set resource and file limits explicitly.
10. Reboot when boot or CPU topology settings change.
11. Run post-reboot assertions against effective state. Configuration-file
    contents alone are not proof.

Do not enable `isolcpus`, `nohz_full`, or `rcu_nocbs` in the first implementation.
Start with verified cpusets and IRQ affinity. Add kernel isolation as a separate
profile only if scheduler evidence shows it is needed.

### Qualification capture

Every run records at least:

- hardware model, CPU model/stepping, cache topology, SMT, NUMA CPU and memory
  maps, DIMM totals, NIC, and disk model;
- BIOS/firmware and CPU microcode versions;
- OS image, package snapshot, kernel, command line, mitigations, and cgroup
  controllers;
- Docker, containerd, runc, Iris image manifest, Judger SHA-256, and compiler
  versions;
- governor, min/max/current frequency, turbo, C/P-state policy, power profile,
  swap, huge pages, and transparent huge page settings;
- CPU sets, systemd slices, IRQ affinity, network queues, and NUMA policy;
- temperatures, throttling counters, machine-check status, load, PSI, run queue,
  memory, and disk state;
- running services, processes, containers, cron jobs, and timers;
- fixture and configuration hashes;
- RDS and S3 dependency fingerprints without credentials.

Qualification includes an idle settling window. Runs fail if load, thermal
state, frequency, memory pressure, or unexpected processes are outside the
profile's bounds.

## Judger Containment Blocker

Valid stabilization work cannot proceed while submissions can escape to
root-level, unrestricted `/sandbox-*` cgroups.

Before building the experiment ladder:

1. Pin the alpha.4 amd64 Judger artifact URL and obtain its authoritative
   SHA-256. Verify the checksum in image builds and again in run manifests.
2. Characterize exactly how alpha.4 chooses, creates, and removes cgroups on
   cgroup v2.
3. Add a stress test that observes every spawned compiler and submission PID,
   cgroup path, effective CPU list, memory node list, and lifetime.
4. Make Judger accept an explicit delegated cgroup parent, or upgrade it to a
   reviewed version that does. The benchmark must not race to patch a cgroup
   after execution has started.
5. Use a unique cgroup subtree per worker and per run. Put each worker on an
   assigned physical core set and NUMA memory node.
6. Verify cleanup and limit enforcement for successful, timed-out, crashed, and
   malicious submissions.

Keep an unchanged alpha.4 `production-compat` profile to reproduce Wave 1. Use
the corrected cgroup behavior in an `isolated` profile. This A/B comparison is
part of the investigation and prevents a containment change from being mistaken
for unexplained host improvement.

If Judger cannot be changed promptly, a pinned KVM guest with host-pinned vCPUs
is the fallback containment boundary. A privileged Docker container alone is
not sufficient when Judger moves work into unrestricted host-root cgroups.

## Dependency Design

### RabbitMQ

Run RabbitMQ locally in a digest-pinned container with a checked-in definitions
file. Pin all behavior relevant to the test:

- RabbitMQ and Erlang versions;
- vhost, exchange, queues, bindings, durability, and queue type;
- prefetch, acknowledgement, publisher confirms, retries, dead lettering, and
  maximum message size;
- credentials generated for the run and held in a root-only runtime directory.

Purge and verify both queues before each trial. RabbitMQ runs only on
housekeeping CPUs and must not share benchmark cores.

### Persistent benchmark RDS

Create one dedicated benchmark RDS instance from an approved checkpoint and
keep it between campaigns. Manage it as infrastructure, not as an ad hoc test
resource.

Required controls:

- dedicated identifier, tags, subnet/security group, parameter group, alarms,
  backup policy, deletion protection, and cost owner;
- network access only from approved benchmark/controller addresses;
- a benchmark database role with the minimum required permissions;
- read-only transactions for judge and run suites;
- no Iris `generate` requests against the persistent checkpoint unless they use
  a separate disposable database or resettable schema;
- a checkpoint identity and a data fingerprint queried before every block;
- a documented refresh process that creates a new benchmark generation rather
  than silently changing the current one;
- engine version, instance class, storage, AZ, parameter group, endpoint class,
  and relevant database statistics in the run manifest.

Persistence removes restore time from every test but does not make state
immutable. Read-only credentials and fingerprint checks are mandatory.

### Production S3 read-only access

Use a dedicated IAM policy limited to the required testcase bucket and prefixes:

- allow only the bucket listing required for those prefixes;
- allow `GetObject`, `GetObjectTagging`, and any KMS decrypt operation required
  for those objects;
- deny writes and deletes;
- grant no access to unrelated buckets or AWS services.

Prefer short-lived STS credentials obtained by the controller. Inject them into
the Iris container from a tmpfs runtime file and delete them at teardown. Never
write them to the result bundle, Compose file, shell history, or repository.

Before each external-data run, record object keys, version IDs where available,
sizes, ETags, and content hashes. Production S3 is an external mutable
dependency, so those runs form a separate variant. The direct suite and the
default embedded-`userTestcases` full-Iris suite should use a committed,
checksummed sanitized fixture and perform no testcase fetch during the measured
interval.

## Direct Judger Suite

Build `judger-bench` from the same Iris commit and language configuration used
by the selected Iris image. Run it in a digest-pinned image containing the same
compiler/runtime versions and the checksum-verified Judger binary.

It provides two operations:

1. **Compile:** compile a fixture once and record compile CPU/wall time and
   output digest.
2. **Execute:** run the compiled artifact repeatedly with fixed input, limits,
   environment, uid/gid, seccomp policy, cgroup parent, CPU set, and NUMA policy.

Each iteration emits NDJSON containing:

- run, block, worker, fixture, and iteration IDs;
- Judger `cpuTime`, `realTime`, memory, signal, exit code, and status;
- controller monotonic elapsed time;
- PID, CPU set, observed CPUs, cgroup, NUMA policy, context switches, migrations,
  faults, and throttling deltas;
- frequency and thermal samples linked by monotonic timestamp.

Run cold-cache and warm-cache tests separately. Never drop host page cache in
the middle of an otherwise comparable block without labeling it as a different
profile.

## Full Iris Suite

Run one or more digest-pinned Iris containers against local RabbitMQ. Use the
same Iris commit, Judger binary, fixture, limits, and cgroup assignment as the
direct suite.

The full suite has three modes:

1. **Embedded intrinsic:** one judge request with many identical
   `userTestcases`; compile once and execute repeatedly.
2. **Embedded submissions:** many requests with one embedded testcase each;
   includes Iris setup, compilation, cleanup, and AMQP handling without DB/S3
   testcase fetches.
3. **External-data parity:** requests omit `userTestcases`, causing Iris to read
   metadata from the persistent benchmark RDS and testcase objects from S3.

Instrumentation can perturb timing. Keep application OTel disabled for the
primary runtime suite and enable it only in a separately labeled observability
profile.

The driver records publish confirmation, queue entry, delivery, Iris start,
compile interval, execution interval, result publish, and collection where the
application exposes those events. Use monotonic clocks for local duration.
Cross-machine timestamps require a recorded synchronization error.

Use both workload models, clearly separated:

- closed-loop or prefilled-queue tests for direct comparison with Wave 1;
- open-loop arrival-rate tests for sustainable capacity and latency without
  coordinated omission.

## Experiment Matrix

Run the matrix in stages. Do not begin with maximum concurrency.

### Stage A: fixture and limit correctness

- one C++ fixture with a target runtime long enough to dominate timer
  granularity, initially 500 ms to 2 s;
- accepted output on every repetition;
- CPU, real-time, memory, output, process, and seccomp limits verified with
  adversarial fixtures;
- no network access from submission processes.

### Stage B: one-worker stabilization

- one physical core, SMT off, one NUMA node, fixed governor/turbo policy;
- compile-once execution repetitions;
- independent compile-and-run submissions;
- baseline before and after each experiment block;
- repeated blocks across at least five clean reboots for initial qualification.

### Stage C: topology characterization

- one worker on NUMA node 0;
- one worker on NUMA node 1;
- workers confined to one socket;
- workers spread across both sockets;
- SMT off, then SMT on as a separate profile;
- turbo off, then turbo on as a separate profile.

### Stage D: controlled concurrency

- 1, 2, 4, 8, and 16 workers, each assigned a disjoint physical core;
- no 30-worker result in the primary curve because the machine has 16 physical
  cores;
- 24/30/32 workers only in an explicitly labeled oversubscription study;
- constant work per worker and enough queue depth to keep all workers active.

Randomize concurrency order within a block after the initial diagnostic ladder.
Insert the one-worker baseline between high-load cases to detect thermal or
frequency drift.

### Stage E: full Iris and dependencies

- repeat accepted direct-suite profiles through embedded full Iris;
- add persistent RDS access while keeping S3 out of the measured path;
- add production S3 read-only access last;
- compare execution distributions with the direct suite and attribute added
  queue, compile, network, DB, and S3 time separately.

## Statistics And Reporting

Retain every raw sample. Report at least:

- count, failures, median, MAD, standard deviation, CV, p90, p95, p99, max, and
  p99/median for CPU time and real time;
- bootstrap confidence intervals for median and tail estimates;
- throughput, queue wait, end-to-end latency, and achieved arrival rate;
- per-worker and per-CPU distributions;
- CPU frequency, temperature, throttling, run queue, context switches,
  migrations, cgroup throttling, PSI totals, and NUMA locality;
- before/after baseline drift and between-reboot variance.

PSI `some` on a many-CPU machine is not a direct utilization percentage. Use
its counter deltas alongside run queue, CPU utilization, and cgroup data rather
than treating the prior approximately 95% value as proof of saturation.

Comparisons must declare their population key: hardware, firmware, microcode,
kernel, host profile, image digest, Judger digest, toolchain, fixture, and
dependency generation. The analyzer refuses to merge incompatible populations
unless explicitly asked for a cross-environment comparison.

## Result Bundle

Each run is immutable after collection:

```text
runs/<run-id>/
├── manifest.json
├── qualification/
├── config/
├── samples/
│   ├── judger.ndjson
│   └── iris.ndjson
├── telemetry/
├── logs/
├── analysis/
├── checksums.sha256
└── COMPLETE
```

Write `COMPLETE` only after expected sample counts, message conservation,
checksums, secret scanning, and cleanup verification pass. Partial runs remain
available but are marked invalid with a machine-readable reason.

The manifest includes controller and agent versions, Git commit, dirty-tree
state, timestamps, all digests, effective host controls, profile, experiment
parameters, dependency fingerprints, validation outcomes, and override flags.
It contains no secret values or full connection strings.

## Safety Controls

- Require an explicit allowlist of benchmark host identities and verify host
  keys. Refuse production Kubernetes nodes.
- Keep the host unable to reach production Kubernetes control planes and
  management credentials.
- Use no production database credential and no production RabbitMQ credential.
- Use read-only RDS and S3 identities for judge experiments.
- Restrict submission egress independently from Iris's required AWS access.
- Run sanitized, reviewed fixture source only. Treat all sandbox tests as
  hostile despite that restriction.
- Keep credentials in memory or root-only tmpfs and redact command output.
- Set maximum run duration, queue size, worker count, disk use, temperature,
  and error-rate stop conditions.
- Stop immediately on cgroup escape, thermal throttling, machine-check errors,
  unexpected service activation, fixture mismatch, dependency fingerprint
  drift, or missing results.
- `cleanup` removes run containers, queues, temporary credentials, processes,
  and delegated cgroups but preserves immutable evidence already downloaded.

## Implementation Phases

### Phase 0: freeze inputs and resolve containment

1. Select the Iris commit/image used as the production-compat baseline.
2. Record and verify the Judger alpha.4 SHA-256.
3. Select and sanitize the initial C++ fixture.
4. Reproduce and document alpha.4 cgroup behavior outside Kubernetes.
5. Implement or select explicit cgroup-parent support.

**Gate 0:** every compiler and submission PID can be proven contained before it
executes user code.

### Phase 1: host role and qualification

1. Add the separate load-test inventory and benchmark host role.
2. Encode service cleanup, CPU policy, housekeeping CPUs, IRQ affinity,
   benchmark directories, and post-reboot checks.
3. Implement inventory and idle-stability capture.
4. Test idempotence and recovery from interrupted provisioning.

**Gate 1:** server8 passes qualification after two consecutive reboots, and a
second provisioning run reports no unexplained change.

### Phase 2: direct Judger minimum viable benchmark

1. Add the fixture manifest and `judger-bench` image.
2. Emit versioned NDJSON and a complete run manifest.
3. Add correctness, timeout, memory, process, output, seccomp, and cleanup
   tests.
4. Run the one-worker reboot study.

**Gate 2:** correctness and containment are 100%, no run is thermally invalid,
and the baseline meets the initial stability thresholds or produces sufficient
evidence to explain why it does not.

### Phase 3: topology and concurrency

1. Add topology-aware CPU/NUMA allocation.
2. Run one-socket, two-socket, SMT, turbo, and concurrency profiles.
3. Randomize blocks and insert repeated baselines.
4. Identify the first concurrency level where the distribution materially
   changes and correlate it with measured resource effects.

**Gate 3:** timing discontinuities are attributable to measured controls rather
than unobserved placement or cgroup escape.

### Phase 4: full Iris local integration

1. Add digest-pinned local RabbitMQ definitions and the AMQP driver.
2. Run embedded intrinsic and embedded-submission modes.
3. Validate message conservation, retries, queue reset, and worker identity.
4. Compare direct Judger and Iris execution distributions.

**Gate 4:** all requests are accounted for, and service overhead is separated
from the execution metric.

### Phase 5: persistent AWS parity

1. Provision and document the persistent benchmark RDS generation.
2. Create the read-only DB role and data fingerprint.
3. Create the prefix-scoped production S3 read-only IAM policy and short-lived
   credential flow.
4. Add DB-backed and S3-backed variants one at a time.

**Gate 5:** no production write is possible, dependency identity is stable, and
external latency does not contaminate the direct execution metric.

### Phase 6: portable-host validation

1. Run qualification on a second compatible host.
2. Confirm provisioning is topology-aware and contains all work.
3. Keep each hardware/kernel population separate in analysis.
4. Document host replacement and clean teardown procedures.

**Gate 6:** another operator can reproduce the procedure and obtain a complete,
auditable result bundle without server8-specific edits.

## Required Decisions Before Implementation

1. Name the approved persistent RDS checkpoint, owner, monthly cost owner, and
   refresh cadence.
2. Select the S3 bucket prefixes and whether object versioning is available.
3. Choose the initial turbo policy. This proposal recommends turbo off for the
   stabilization baseline and turbo on as a separate production-parity profile.
4. Decide whether the corrected Judger is developed as an alpha.4-compatible
   patch or an upstream release. Do not hide the behavioral change in the
   harness.
5. Confirm that Gate 6 of the server8 migration is complete before deleting old
   K3s state or disabling/removing additional host services.
6. Select the initial fixture and confirm that its source/input/output may be
   committed as a sanitized benchmark asset.

## First Deliverable

The first PR should not attempt the complete full-Iris environment. It should
contain only:

- the host compatibility schema and read-only qualifier;
- the Ansible benchmark-host role with post-reboot verification;
- the checksum-pinned Judger benchmark image;
- one sanitized C++ fixture;
- the direct one-worker runner and result schema;
- containment tests proving effective cgroup, CPU, and NUMA placement;
- documentation for provisioning, running, collecting, and restoring server8.

That slice directly addresses the unresolved source of runtime variance. Local
RabbitMQ and AWS parity should be added only after the direct measurement is
both contained and repeatable.
