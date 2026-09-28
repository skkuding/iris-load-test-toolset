# Iris Runtime Load-Test Toolset

Reproducible benchmark tooling for investigating and stabilizing the runtime
reported by Iris and Judger. Start with `docs/WAVE1-REPLICATION.md` for the
operator procedure. The private benchmark plan and progress log are maintained
outside this repository.

This repository implements the direct Judger foundation and an external-data
full-Iris suite:

- a read-only host qualifier and an idempotent Ansible benchmark-host role;
- live, isolated AWS benchmark resources (`codedang-iris-benchmark` RDS clone,
  testcase bucket, scoped IAM) with a safe fixture workflow;
- an immutable, digest-pinned run plan (`internal/runplan`);
- a controller, `iris-benchctl`, and a host agent, `iris-bench-agent`;
- a checksum-pinned direct runner, `judger-bench`, and its image.
- digest-pinned local RabbitMQ plus production Iris containers, queue-prefilled
  synchronized startup, benchmark RDS/S3 reads, and AMQP message conservation.

See [Full Iris external-data benchmark](docs/FULL-IRIS-EXTERNAL.md) for the
operator procedure and validated runs.

## Safety boundary

- Authorized AWS mutations are limited to the dedicated benchmark RDS clone,
  testcase bucket, and narrowly scoped identities/policies that support them.
- Production RDS, S3, Kubernetes, and production nodes are read-only and must
  never be mutation targets. `config/compatibility.yaml` carries an explicit
  production deny-list.
- Never commit credentials, connection strings, `.env`, `terraform.tfvars`,
  `*.tfstate`, or result bundles. Secrets are referenced by name/ARN and read
  at runtime from AWS Secrets Manager.
- Fixture content committed here must stay sanitized and license-compatible.

## Layout

```text
cmd/iris-benchctl/          controller CLI (plan, run, collect, status, analyze)
cmd/iris-bench-agent/       host agent and worker-exec wrapper
cmd/judger-bench/           direct compile/execute loop and NDJSON samples
internal/                   config, runplan, orchestrator, protocol, transport,
                            containment, experiment, fulliris, artifact,
                            manifest, analyze, telemetry, qualification
analysis/plot-run.py        uv-runnable plot of a collected direct run
analysis/plot-ladder.py     concurrency ladder boxplot
analysis/plot-repetition.py repeated-round aggregate and drift chart
config/
  compatibility.yaml        host compatibility envelope (single source of truth)
  profiles.json             example direct-suite controller config
  run.example.json          minimal example controller config for `run`
  full-iris-external.json   external-data full-Iris ladder profiles
ansible/                    inventory, playbooks, iris_benchmark_host role
images/judger-bench.Dockerfile
fixtures/                   sanitized 568/569/570 testcases, metadata, manifest
fixtures/iris-external-568/ curated external-data full-Iris source fixture
.env.example                live benchmark names/ARNs (no secret values)
scripts/qualify-host.sh     read-only Ansible qualification wrapper
scripts/aws/                snapshot discovery, DB role, export, manifest,
                            upload, runtime env, container psql
infra/aws/iris-benchmark/   dedicated Terraform module and tests
docs/FULL-IRIS-EXTERNAL.md  external-data full-Iris operator guide
```

`runs/` and `bin/` are git-ignored. Generated results stay outside Git.

## Prerequisites

- Go 1.24 or newer (module `go.mod`).
- `ansible-core` with the `ansible.posix` collection (`ansible/requirements.yml`).
- Terraform with providers pinned by
  `infra/aws/iris-benchmark/.terraform.lock.hcl` (`hashicorp/aws` 5.100.0,
  `hashicorp/random` 3.9.1).
- AWS CLI v2 and `jq`. The database scripts need a PostgreSQL client: either
  `psql` on `PATH`, or `scripts/aws/psql-container.sh` with a container runtime
  (see [Export, manifest, upload](#export-manifest-upload)).
- Docker with `buildx` when resolving an image tag with `--resolve-image` and
  when building `images/judger-bench.Dockerfile`. A container runtime (default
  `podman`) or `psql` is needed for the database scripts.
- OpenSSH client and the operator's existing host aliases and sockets. The
  controller requires **non-interactive** auth (`BatchMode=yes`); verify with
  `ssh -o BatchMode=yes -o ConnectTimeout=15 <alias> true`.

## Build and test

```bash
go build ./...
go vet ./...
go test ./...

go build -o bin/iris-benchctl     ./cmd/iris-benchctl
go build -o bin/iris-bench-agent  ./cmd/iris-bench-agent
go build -o bin/judger-bench      ./cmd/judger-bench
```

## Controller CLI

```text
iris-benchctl plan    --config FILE --host ALIAS --profile NAME [flags]
iris-benchctl run     --config FILE --host ALIAS --profile NAME [flags]
iris-benchctl collect --config FILE --host ALIAS --run RUNID [flags]
iris-benchctl status  --config FILE --run RUNID [--host ALIAS]
iris-benchctl analyze --run RUN_DIRECTORY
```

| Command | State | Behavior |
| --- | --- | --- |
| `plan` | Implemented | Resolves and prints the immutable run plan JSON (stdout) and its SHA-256 (stderr). No mutation. |
| `run` | Implemented | Stages sealed direct or external-Iris assets, then runs `inspect` -> `prepare`/qualification -> every `run-block` -> `validate` -> `cleanup` -> `bundle`. Does not `collect` (run `collect` afterward). |
| `collect` | Implemented | Downloads and hash-verifies a bundle inventory, requires qualification/validation/cleanup evidence, secret-scans it, and writes `manifest.json`, `checksums.sha256`, `collection.json`, and `COMPLETE`. |
| `status` | Implemented | Prints local persisted state, or queries the remote agent with `--host`. |
| `analyze` | Implemented | Aggregates collected `samples/*.ndjson` and reports execution timing, optional full-Iris end-to-end latency, and telemetry-backed comparability. |
| `provision`, `qualify`, `resume` | Not implemented | Print `not implemented in the minimum viable core` and exit non-zero. |

`plan` and `run` share the plan flags:

```text
-config FILE            configuration JSON file (required)
-host ALIAS             target SSH alias; must be allowlisted in the config
-profile NAME           profile name from the config (required)
-suite judger|iris      override the profile suite
-run-id ID              explicit run id (default: generated iris-YYYYMMDD-xxxxxxxx)
-iris-digest DIGEST     resolved Iris manifest digest (sha256:<64 hex>)
-rabbitmq-digest DIGEST resolved RabbitMQ manifest digest (Iris suite only)
-resolve-image          resolve the configured Iris and RabbitMQ tags with docker buildx imagetools
-judger-digest DIGEST   Judger artifact SHA-256 (raw hex or sha256:...)
-judger-digest-file F   use the SHA-256 of file F as the Judger digest
-benchmark-image ID     immutable local Docker image ID (exactly sha256:<64hex>)
-seed N                 run random seed (default 1)
-fixture name=path      checksummed fixture input
-expected-output name=path
                        checksummed expected stdout paired by fixture name
-bench-binary PATH      REMOTE precompiled workload binary
-bench-binary-sha256 H  SHA-256 of the REMOTE workload binary
-production-compat      accept stock alpha.4 root /sandbox-* behavior as an
                        uncontained, non-comparable population
```

The `iris` suite additionally accepts:

```text
-iris-source PATH       curated source file (hashed and staged)
-iris-env-file PATH     mode-0600 runtime credential env file (hashed and staged)
-problem-id N           Iris problem id
-language NAME          Iris language (default Cpp)
-time-limit-ms N        Iris time limit in milliseconds
-memory-limit-bytes N   Iris memory limit in bytes
-testcases-per-request N
                        embedded/external testcases per request (external-data uses 1)
-message-id-start N     first numeric AMQP message id
-rds-generation NAME    sealed benchmark RDS generation label
-s3-object-set DIGEST   sealed S3 object-set digest label
-s3-bucket NAME         testcase bucket name
```

`run` additionally accepts the direct-suite flags listed in the `run` section
below (`--cgroup-parent`, `--bench-binary`, `--judger`, and others). The Iris
suite needs none of them: it rejects direct fixtures and workload flags and
requires `--production-compat`. See
[Full Iris external-data benchmark](docs/FULL-IRIS-EXTERNAL.md).

### plan

```bash
go run ./cmd/iris-benchctl plan \
  --config config/profiles.json \
  --host codedang8 \
  --profile isolated-1s \
  --iris-digest sha256:<resolved-manifest-digest> \
  --judger-digest 2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103 \
  --bench-binary /opt/iris-bench/bin/judger-bench \
  --bench-binary-sha256 <workload-sha256>
```

For the direct suite, the plan refuses to seal without a resolved Iris digest, a
Judger digest, and a precompiled workload path and SHA-256. The `iris` suite
requires a resolved Iris and RabbitMQ digest, the sealed source and credential
env files, and the sealed RDS/S3 labels instead of a workload; it requires no
direct fixture. The same inputs always produce the same plan digest.

### run

`run` initializes local state under `runs/.state/<run-id>/state.json`, creates a
private mode-`0700` remote directory `/tmp/iris-bench-<run-id>/`, uploads the
sealed plan and local assets beneath it, then drives the agent
through `inspect` -> `prepare` -> one `run-block` per plan block -> `validate` ->
`cleanup` -> `bundle`. `cleanup` removes only the agent runtime directory, so the
evidence preserved under `/var/lib/iris-bench/<run-id>` stays available to
`collect`. `--dry-run` prints the resolved plan summary and stops before any SSH
call. `--yes` is accepted but currently has no effect: `run` does not prompt.

A direct-suite `run` accepts these remote execution flags:

```text
-cgroup-parent PATH      delegated cgroup v2 subtree (required by run-block)
-cgroup-mount PATH       cgroup v2 unified mount (agent default /sys/fs/cgroup)
-bench-binary PATH       REMOTE precompiled workload binary (requires its SHA)
-bench-binary-sha256 HEX SHA-256 of the REMOTE workload binary
-agent PATH              REMOTE agent path override
-judger PATH             external alpha.4 Judger binary
-judger-sha256 HEX       Judger digest; must agree with the sealed plan
-container-id ID         base CONTAINER_ID (per-worker suffix appended)
-worker-timeout DUR      per-worker outer timeout
```

Alternatively, `--local-bench-binary LOCAL` hashes and uploads a precompiled
workload to its sealed run-directory path, and `--local-agent LOCAL` uploads the
agent used for this run. Local and remote forms are mutually exclusive. The
agent upload is re-hashed remotely before its first execution, and the private
staging directory is removed after every success or failure path. The agent
requires its compiled version to equal the plan tool version and verifies the
sealed workload SHA-256 immediately before execution. Arbitrary source is never
compiled. For the direct suite, `run-block` remains unsupported without
`--cgroup-parent`.

Direct runs require exactly one `--fixture name=input` and matching
`--expected-output name=output`. Both files are hashed into the sealed plan and
uploaded to deterministic files beneath the private run-scoped staging
directory recorded in that plan. The
agent re-hashes both before launch, supplies the input to Judger, and requires
every output to match the expected-output SHA-256.

`--qualification-report LOCAL` similarly hashes and stages the sanitized
Ansible report. Inspect copies it to `qualification/ansible-report.json`, and
prepare accepts it only when `schema` is `iris-benchmark-qualification/v1`,
`compatible=true`, `verification.failures` is empty, and `inventory_hostname`
or `nodename` matches the sealed host identity. A run without this optional
report still executes and can be collected, but its manifest and `COMPLETE`
record are explicitly non-comparable. Comparable acceptance therefore fails
closed. Production compatibility remains accepted and non-comparable.

The default mode is `isolated`: samples must remain beneath their delegated
worker subtree. Host execution remains available in this mode. Supplying
`--benchmark-image sha256:<64hex>` selects OCI workers and seals that exact
local Docker image ID as both `images["judger-bench"].reference` and `.digest`.
The controller forwards the ID and the agent refuses any mismatch. Each worker
uses an argv-only `docker run --rm` invocation with a unique run-scoped name,
privileged host cgroup namespace access, its assigned CPU/NUMA set, and
same-path mounts for cgroup v2, the sealed workload/fixtures, and output.

`--production-compat` requires this OCI mode for the direct suite (the `iris`
suite instead runs the pinned Iris and RabbitMQ containers); unprivileged host
execution cannot claim to reproduce the production root-cgroup behavior. The live alpha.4 Judger
reports `cgroup_path` as a full filesystem path such as
`/sys/fs/cgroup/sandbox-<CONTAINER_ID>/box-*`, while the monitor holds the
mount-relative root `/sandbox-<CONTAINER_ID>`; both forms are normalized and
accepted as the exact run-scoped root or one of its descendants, and prefix
confusion such as `/sandbox-<CONTAINER_ID>-evil` is rejected. Those samples
record `cgroupContained=false`; the manifest records an override and
`comparable=false`. This is an accepted diagnostic population, never comparable
to isolated runs. Live alpha.4 leaves one empty `box-*` child cgroup under the
run-scoped root per iteration, so a leftover root can carry several of them. Its
residue is still safely removed: after every worker exits, the agent removes the
root and its empty `box-<safe-id>` children only through a tightly validated
path. The root and every child must have an empty `cgroup.procs`, each child must
have no grandchild cgroup, and no unexpected directory name may remain.
Validation returns the canonical full filesystem paths children-first,
root-last; any member process, deeper child, or unexpected child leaves every
cgroup in place and fails the block with receipt evidence, and root absence is
verified afterwards.

The stock alpha.4 root and its boxes are root-owned, so an unprivileged manager
can inspect but not `rmdir` them. In OCI mode the agent therefore injects a
cleanup callback into the block coordinator: it first runs the read-only
containment validation above to obtain the ordered canonical full filesystem
paths, then removes them with a single argv-only helper container

```text
docker run --rm --privileged --cgroupns=host --entrypoint /bin/rmdir \
  --mount type=bind,src=<cgroup-mount>,dst=<cgroup-mount> \
  <sealed-image-id> <child-canonical-path>... <root-canonical-path>
```

using only the sealed image ID and no shell. `rmdir` accepts every validated path
in one child-first, root-last invocation. The helper carries a run-scoped,
validated container name plus the `iris-bench.run`/`iris-bench.block` labels, so
a helper left behind by an interrupted run is caught by the same residual OCI
container cleanup as the workers. Host mode removes the validated paths directly
through the manager in the same order.

### collect

```bash
go run ./cmd/iris-benchctl collect \
  --config config/profiles.json --host codedang8 --run iris-20260925-abcdefgh
```

`collect` downloads `<remote-var-root>/<run-id>/bundle-inventory.json`
(default `--remote-var-root /var/lib/iris-bench`), validates it, and downloads
every listed file into a temporary directory. It re-verifies every size and
SHA-256, scans the collected text for credential-shaped values and private key
material, rebuilds `manifest.json` from the collected plan, and writes
`checksums.sha256` and `collection.json`. `checksums.sha256` covers every
regular file in the published bundle, including `COMPLETE`, except itself to
avoid a recursive checksum. Matching `qualified.json`,
`validated.json`, and `cleaned.json` records for the sealed plan are mandatory.
It writes the `COMPLETE` marker only after those checks pass, then atomically renames the directory to
`runs/<run-id>`. An existing result directory is never overwritten. The
inventory is produced by the agent `bundle` verb after `cleanup`, so the cleanup
record is included. Re-running `bundle` removes the old inventory before
rebuilding it.

### status

```bash
go run ./cmd/iris-benchctl status --config config/profiles.json --run <run-id>
go run ./cmd/iris-benchctl status --config config/profiles.json --run <run-id> --host codedang8
```

Without `--host` it prints the local `state.json`. With `--host` it asks the
agent for the run's prepared state.

### analyze

```bash
go run ./cmd/iris-benchctl analyze --run runs/<run-id>
```

The direct-suite analyzer parses `plan.json`, requires the sample and telemetry
file for exactly every planned block, aggregates those samples, and emits JSON with the total count,
failures, successful count, median, MAD, sample standard deviation,
CV percentage, linearly interpolated p90/p95/p99, max, and p99/median for
`cpuTimeMs` and `realTimeMs`. Failed samples contribute to the failure count but
not the timing distributions. A successful sample with absent or invalid timing
fields makes analysis fail rather than silently introducing a zero.

Direct-suite measurements are windowed to the **all-workers-active steady
window**. `judger-bench` records a host-wide wall-clock timestamp immediately
before and after every execute-mode submission (`startedAtNs`/`endedAtNs`).
After a block, the coordinator derives each worker's active interval
`[min startedAtNs, max endedAtNs]` and the window
`[max over workers of first start, min over workers of last end]`. A passing
block requires that every worker contributed at least one sample with valid
positive timestamps, that the window is non-empty, and that its duration is at
least `min(2s, 0.5 × (max lastEnd − min firstStart))`. The receipt records
`steadyWindowStartNs`, `steadyWindowEndNs`, and `steadyWindowSeconds`; a block
whose window is missing, empty, or negligible fails instead of reporting an
unsynchronized window. When timestamps are present, `analyze` computes the
`cpuTimeMs` and `realTimeMs` distributions only from successful samples whose
`[start,end]` falls inside the window and reports the window fields
(`steadyWindowApplied`, `steadyWindowStartNs`, `steadyWindowEndNs`,
`steadyWindowSeconds`) in the JSON summary. Bundles without timestamps keep the
pre-window behavior. The OCI container-start ramp and the drain tail are
excluded from the primary statistics because container/image creation, cgroup
setup, and the final partial iterations measure startup and teardown timing
rather than the closed-loop steady state.

The report is comparable only when the bundle has a valid comparable manifest,
a `COMPLETE` marker, non-empty `qualification/host-facts.json`, and valid thermal
evidence in `telemetry/thermal-<block-id>.ndjson` for every planned block. The
agent captures sysfs thermal-zone temperatures and/or CPU thermal throttle
counters immediately before and after each coordinator run. Each file contains
exactly a `before` and `after` record with the block identity, positive
`monotonicNs`, and concrete source maps, for example:

```json
{"schemaVersion":1,"blockId":"isolated-1s-01","phase":"before","monotonicNs":123456789,"temperaturesMilliC":{"sys/class/thermal/thermal_zone0/temp":42000}}
```

The agent fails closed when neither supported sysfs source is available and
validation rejects missing or malformed block telemetry. A throttle-counter
increase is retained as accepted evidence but marks collection non-comparable.
The analyzer does not hide existing measurements when evidence is missing: it
reports statistics with `comparable=false` and machine-readable reasons.

### plot

```bash
uv run analysis/plot-run.py --run runs/<run-id>
```

`analysis/plot-run.py` turns an already-collected bundle into a two-panel figure
(per-sample timings and distributions) and prints the same summary table. It is
a standalone Python script with PEP 723 inline dependencies, so `uv run` creates
the environment; no manual install is required. The plot is written outside the
bundle (default `<run-id>-timeseries.png`) to preserve bundle immutability and
checksums; override with `--out` and `--format png|svg|pdf`.

### plot-ladder

```bash
uv run analysis/plot-ladder.py \
  --run 1=runs/<run-1> --run 2=runs/<run-2> --run 4=runs/<run-4> \
  --run 8=runs/<run-8> --run 16=runs/<run-16> --run 30=runs/<run-30> \
  --out wave1-direct-sweep.png
```

`plot-ladder.py` builds a boxplot of CPU time, real time, and memory by
concurrency level, matching the layout of the Wave 1 report. When the samples
carry `endToEndMs` (full-Iris bundles), an end-to-end panel is added. Each
`--run` pairs a label with a collected bundle; labels become the x-axis
categories in the given order, and `--xlabel` overrides the axis label.
`config/wave1-sweep.json` provides `ladder-1` … `ladder-30` profiles for a
direct sweep. [Wave 1 comparison](docs/WAVE1-COMPARISON.md) records the original
reference values and the matched reproduction.

### plot-repetition

```bash
uv run analysis/plot-repetition.py \
  --run 1:1=runs/r1-1x --run 1:8=runs/r1-8x --run 1:30=runs/r1-30x \
  --run 2:1=runs/r2-1x --run 2:8=runs/r2-8x --run 2:30=runs/r2-30x \
  --out-aggregate runs-aggregate.png --out-drift runs-drift.png
```

`plot-repetition.py` aggregates repeated rounds. Each `--run ROUND:LABEL=DIR`
maps a collected bundle to a round number and a label (typically a replica
count). It writes a pooled boxplot across rounds and a per-round median drift
chart, and prints a drift table with per-level median, min, max, and spread.
The external-data full-Iris ladder and reboot repetition are described in
[Full Iris external-data benchmark](docs/FULL-IRIS-EXTERNAL.md).

## Configuration

The controller accepts one strict JSON file via `--config`. Unknown fields are
rejected. Defaults are applied before validation and never override explicit
values. Example files: `config/profiles.json`, `config/run.example.json`, and
`config/full-iris-external.json` (the external-data full-Iris ladder).

```jsonc
{
  "schemaVersion": 1,
  "toolVersion": "0.1.0",
  "iris":  {
    "image": "ghcr.io/skkuding/codedang-iris:stage",
    "rabbitmqImage": "rabbitmq:3.13-management-alpine"  // required for suite: iris
  },
  "profiles": {
    "isolated-1s": {
      "suite": "judger",       // judger | iris
      "workers": 1,            // must be <= limits.maxWorkers
      "repetitions": 5,        // samples per worker; must be >= 1
      "cpuList": "2-3",        // optional; required at run-block for judger
      "numaPolicy": "bind",    // default | preferred | bind | interleave
      "turbo": "off"           // on | off
    }
  },
  "hosts": [ { "alias": "codedang8", "hostIdentity": "skkuding-4f-4", "allow": true } ],
  "paths": {
    "varRoot": "/var/lib/iris-bench",
    "runRoot": "/run/iris-bench",
    "binRoot": "/opt/iris-bench/bin",
    "socketDir": "~/.ssh/sockets",
    "sshControlPath": "",          // optional explicit ControlPath (e.g. an operator socket)
    "resultRoot": "runs"
  },
  "limits": {
    "maxWorkers": 32,
    "maxRunSeconds": 3600,
    "maxLoad1": 1.0,
    "maxQueueDepth": 100000,
    "maxDiskMiB": 40960,
    "stopOnErrorRatePct": 0
  }
}
```

At least one host must have `"allow": true`, and the `--host` alias must match.
`limits.maxLoad1` defaults to the conservative value `1.0`, must be positive,
and is sealed into `qualification.maxLoad1`; prepare rejects a missing,
malformed, or higher one-minute host load.
The `iris` suite is the external-data mode documented in
`docs/FULL-IRIS-EXTERNAL.md`. `turbo` is validated then dropped from the plan
(turbo is controlled by the Ansible host role, not the controller).
`numaPolicy` is carried in the plan but the agent currently assigns one
`cpuset.mems` value to every worker (`--cpuset-mems`, default `0`); per-worker
NUMA policy is not applied yet. `cpuList` is split round-robin across workers
and must contain at least `workers` CPUs.

## Image and digest pinning

The default Iris selector is the mutable tag
`ghcr.io/skkuding/codedang-iris:stage`. A run plan is not runnable until the tag
is resolved to an immutable manifest digest:

- `--iris-digest sha256:<64 hex>` supplies an operator-resolved digest.
- `--rabbitmq-digest sha256:<64 hex>` supplies the RabbitMQ manifest digest for
  `suite: iris`; a runnable Iris plan requires both digests.
- `--resolve-image` runs
  `docker buildx imagetools inspect --format '{{.Manifest.Digest}}' <reference>`
  for the Iris reference and, for `suite: iris`, the RabbitMQ reference, and
  uses the results. If a reference already contains `@sha256:...`, that digest
  is used unchanged.
- `.env.example` records `IRIS_BENCHMARK_IRIS_IMAGE_DIGEST` as a **cache only**.
  The controller never reads `.env`; re-resolve before each run.

The Judger digest is mandatory. Supply it with `--judger-digest` (raw hex or
`sha256:...`) or `--judger-digest-file <path>`. The authoritative alpha.4 amd64
digest is pinned in `images/judger-bench.Dockerfile`
(`JUDGER_AMD64_SHA256`) and re-checked at run time by `judger-bench`.

Build the OCI worker locally, then obtain the immutable ID accepted by
`--benchmark-image`:

```bash
docker build -f images/judger-bench.Dockerfile -t judger-bench:alpha.4 .
docker image inspect --format '{{.Id}}' judger-bench:alpha.4
```

OCI mode uses `/app/sandbox/libjudger.so` from that image and still passes the
plan's alpha.4 digest to `judger-bench --judger-sha256`; it never stages or uses
a host Judger. Worker containers carry run/block labels, are force-removed on
failure, and are checked for run-scoped residuals before and after each block.

Every request, event, and artifact carries the sealed plan's SHA-256 (canonical
plan JSON). The agent re-computes and rejects a mismatched staged plan.

## SSH access and `~/.ssh/sockets`

The controller uses the installed OpenSSH client and never embeds an SSH
implementation. For each host it builds:

```text
-o ControlMaster=auto
-o ControlPath=<socketDir>/iris-bench-<sanitized-host>
-o ControlPersist=600
-o ConnectTimeout=15
-o BatchMode=yes
-o ServerAliveInterval=30 -o ServerAliveCountMax=3
```

- `paths.socketDir` defaults to `~/.ssh/sockets`. A leading `~` is expanded, the
  directory is created with mode `0700`, and the resulting control path must be
  shorter than 104 bytes.
- The controller sets `BatchMode=yes`, so authentication must already work
  non-interactively (key/agent, or a control socket at the exact path the tool
  builds). The controller builds `ControlPath=<socketDir>/iris-bench-<sanitized-host>`
  and does not read a socket path from the environment. Ansible uses
  `ControlPath=~/.ssh/sockets/%h.sock` (`ansible/ansible.cfg`), where `%h`
  expands to the host's canonical name, not necessarily the inventory alias.
- To reuse an operator's existing socket directly, set an explicit control path:
  `paths.sshControlPath` in the config, or the `--ssh-control-path` flag on
  `plan`/`run`/`collect`/`status`. When set it replaces the derived
  `ControlPath` (a leading `~` is expanded; the path must be absolute and short
  enough for a unix socket). This is the supported way to point the controller
  at a socket such as `~/.ssh/sockets/codedang8.sock`.
- Ansible builds `ControlPath` from `ansible/ansible.cfg`. To use the same
  operator socket there, override `ssh_args` for the run, for example
  `ANSIBLE_SSH_ARGS='-o ControlPath=~/.ssh/sockets/codedang8.sock -o ControlMaster=auto -o ControlPersist=3600'`,
  or provide key/agent auth. The operator's aliases, `ProxyJump`, and host-key
  policy still apply.

The transport only places validated remote tokens on the command line; plan
JSON and other untrusted data travel over stdin. Uploads are restricted to
validated files directly beneath the fixed run staging directory; run IDs,
remote paths, creation, hashing, and recursive directory removal are validated
before any `ssh`/`scp` invocation.

## Host qualification and provisioning

These are driven by Ansible, not by `iris-benchctl provision`/`qualify` (both
unimplemented).

```bash
# Read-only post-provision assertions; no privilege escalation.
scripts/qualify-host.sh codedang8

# Provision and verify (requires sudo; interactive become password).
ANSIBLE_CONFIG=ansible/ansible.cfg \
  ansible-playbook ansible/playbooks/provision_benchmark_host.yml --ask-become-pass
```

The role runs only when `benchmark_host=true` and the host is in
`iris_bench_compatibility.host.allowed_hosts` in `config/compatibility.yaml`.
It also refuses `forbidden_production_hosts` and `forbidden_production_ips`, so
production nodes cannot be mutation targets. Provisioning is ordered as:
guard -> facts/topology -> compatibility -> packages -> user/dirs -> service
cleanup -> K3s detection -> CPU policy -> IRQ affinity -> cgroup delegation ->
sysctl -> reboot (only if boot-time settings changed) -> effective-state verify
-> report.

`config/compatibility.yaml` is the single source of truth for the supported
envelope: architecture/CPU flags, cgroup v2 controllers, Ubuntu 24.04 and a
kernel range, minimum cores/memory/disk, seccomp and namespaces, container
runtime minimums, clock synchronization, and the CPU policy. The recorded
production-derived turbo policy is `enabled`.

Effective-state verification reads kernel/sysfs/systemd state rather than
config files: governor, turbo, SMT, sysctl, disabled/required services, the
delegated `iris-bench.slice`, required directories, and (optionally strict) IRQ
drift. It writes a JSON report under `IRIS_BENCH_REPORT_DIR` (default
`/tmp/iris-bench-qualification`). K3s state is detected and the service is
disabled; it is never removed.

Privileged real-host validation: `run-block` requires root or a delegated
cgroup v2 subtree, the sealed precompiled workload, and the `judger-bench`
worker command; `judger-bench` uses the external alpha.4 Judger library. None of
that is exercised by unit tests or CI here; it must be validated on a qualified
host with sudo. The unit tests use a fake cgroup filesystem and fake processes
only.

## AWS benchmark resources

All resources are prefixed `codedang-iris-benchmark` in `ap-northeast-2`, and
the Terraform project is separate from production. The RDS instance is restored
from an explicit, operator-supplied snapshot that is first copied into a
durable encrypted manual snapshot; there is deliberately no `most_recent` or
`latest` lookup. `deletion_protection` defaults to `true`, the bucket has
`prevent_destroy`, versioning, KMS encryption, and a full public access block.

These resources are provisioned and live, not just planned: the
`codedang-iris-benchmark` RDS instance is `available` with
`deletion_protection = true`, and `codedang-iris-benchmark-testcases` holds the
six approved fixture objects. `.env.example` records the live names and
non-secret fields (host, port, database, bucket, role ARNs, and secret-variable
names) and contains no secret values; secrets are read at runtime from AWS
Secrets Manager.

### AWS role profiles

The benchmark workflow uses three named local AWS profiles, each of which
assumes a dedicated role through the `codedang-iris-benchmark-deployer` base
user (select one with `AWS_PROFILE`, or pass `--profile`):

- `codedang-iris-benchmark-terraform` assumes the
  `codedang-iris-benchmark-terraform` role for all Terraform plans and applies;
- `codedang-iris-benchmark-upload` assumes the
  `codedang-iris-benchmark-testcase-upload` role for fixture uploads;
- `codedang-iris-benchmark-read` assumes the
  `codedang-iris-benchmark-testcase-read` role for read-only object checks.

Use `AWS_PROFILE=codedang-iris-benchmark-terraform terraform ...` for the
Terraform workflow. The base deployer user carries only `sts:AssumeRole`.

### Apply sequence

The steps below document the bring-up that already ran; they are the procedure
for a new benchmark generation, not a required first step. Run them from the
repository root.

```bash
# 1. Discover the concrete snapshot (READ-ONLY) and copy it into tfvars.
scripts/aws/discover-rds-snapshot.sh --format id
scripts/aws/discover-rds-snapshot.sh --format tfvars

# 2. Provide variables (never committed).
cp infra/aws/iris-benchmark/terraform.tfvars.example \
   infra/aws/iris-benchmark/terraform.tfvars
# edit cost_owner, resource_owner, source_snapshot_identifier, CIDRs, ...

# 3. Inspect only. Apply requires explicit approval and a reviewed plan.
terraform -chdir=infra/aws/iris-benchmark fmt -check -recursive
terraform -chdir=infra/aws/iris-benchmark init
terraform -chdir=infra/aws/iris-benchmark validate
terraform -chdir=infra/aws/iris-benchmark plan
# terraform -chdir=infra/aws/iris-benchmark apply   # only after approval

# 4. After apply: rotate the managed master password once (see below), then
#    bootstrap the read-only DB role (reads secrets at runtime).
source .env   # optional shell env; contains names/ARNs only, no secret values
scripts/aws/bootstrap-benchmark-db-role.sh --yes

# 5. Verify the committed fixtures, then upload them to the dedicated bucket.
#    (To regenerate from the clone instead, see "Export, manifest, upload" and
#    export to a staging directory such as /tmp/benchmark-fixtures.)
fixtures/tests/verify-fixtures.sh
scripts/aws/build-fixture-manifest.sh --check
scripts/aws/upload-fixtures.sh --dry-run
scripts/aws/upload-fixtures.sh --yes
```

`source .env` is optional; the AWS scripts read `IRIS_BENCHMARK_*` names from
the environment. `.env.example` points at the live benchmark resources and
contains non-secret names, ARNs, and identifiers only (copy it to `.env`, which
is git-ignored). If no env is sourced, pass the flags explicitly (`--region`,
`--secret-id`, `--bucket`, and so on).

`discover-rds-snapshot.sh` makes exactly one read-only
`rds describe-db-snapshots` call and deterministically selects the newest
`available` snapshot for the source instance. `terraform.tfvars` is git-ignored.

### RDS bootstrap

`scripts/aws/bootstrap-benchmark-db-role.sh` reads the RDS-managed master
secret and the read-only secret from Secrets Manager. The master password is
passed only through `PGPASSWORD`; the generated read-only password is fed to
`psql` over stdin and never appears as an argument. The SQL
(`scripts/aws/sql/benchmark-readonly-role.sql`) is idempotent and grants only
`CONNECT`/`USAGE`/`SELECT`, forces `default_transaction_read_only = on`, and
revokes write privileges. The script verifies the role and reads
`public.problem_testcase` without printing rows. `--dry-run` contacts no AWS
and reads no secrets.

#### Managed master password after snapshot restore

The benchmark instance sets `manage_master_user_password = true` and reads the
RDS-managed secret through the `IRIS_BENCHMARK_RDS_MASTER_SECRET_ID` variable.
With AWS provider 5.100, the snapshot restore recorded that setting in state but
did not create the managed secret. The reviewed one-time reconciliation enables
managed credentials explicitly, which also replaces the inherited production
password:

```bash
aws rds modify-db-instance \
  --db-instance-identifier codedang-iris-benchmark \
  --manage-master-user-password \
  --master-user-secret-kms-key-id <benchmark-kms-key-arn> \
  --apply-immediately
```

RDS records this as "Reset master credentials". Confirm
`MasterUserSecret.SecretStatus` is `active` and the secret's `LastRotatedDate`
is set before running the bootstrap. Run this only against the benchmark
instance, never against production.

### Export, manifest, upload

- `export-problem-fixtures.sh --out DIR` connects only to the benchmark clone
  (it refuses a non-`codedang-iris-benchmark` secret id, host, or identifier),
  forces a read-only session, and queries problems 568/569/570 and their active
  (`is_outdated = false`) testcases. It writes
  `<out>/<problemId>/<testcaseId>.in|.out` and `metadata.json` atomically,
  sanitizing CR/LF/NUL and rejecting secret-like patterns. `--validate`
  re-checks an extracted tree and rewrites canonical metadata (preserving the
  hidden flag); it refuses symlinks, traversal, unexpected files, and mismatch.
- `build-fixture-manifest.sh --write|--check` pins each object's relative path,
  S3 key, size, SHA-256, content type, and hidden tag. When `metadata.json` is
  present it preserves the database hidden flags instead of hard-coding them.
- `upload-fixtures.sh` iterates the pinned manifest and calls `s3api put-object`
  once per object (never `aws s3 sync`), verifies the local SHA-256 and size
  first, then verifies the stored size and checksum with `head-object`.
  `--dry-run` performs no AWS calls.

The committed fixtures under `fixtures/568`, `fixtures/569`, and `fixtures/570`
are the sanitized clone-derived objects, not placeholders: one active testcase
per problem (`568/15850`, `569/15852`, `570/15873`), each with input and expected
output, plus `fixtures/metadata.json` (problem specifications and testcase
metadata) and `fixtures/manifest.json` (exact S3 key, size, SHA-256, content
type, and `hidden=false` for every object). These six files are the exact
objects uploaded to `codedang-iris-benchmark-testcases`; the upload path never
transforms content.

When the benchmark RDS is not directly reachable, export through an SSH tunnel
with a containerized `psql`. The exporter still validates the secret's
benchmark host before it connects to a loopback endpoint:

```bash
# Terminal 1: tunnel a local port to the benchmark RDS endpoint.
ssh -N -L 15433:<benchmark-rds-host>:5433 <jump-host>

# Terminal 2: export through the tunnel with a disposable postgres container.
scripts/aws/export-problem-fixtures.sh --out /tmp/fixtures \
  --connect-host 127.0.0.1 --connect-port 15433 \
  --psql scripts/aws/psql-container.sh
```

`--connect-host` must be loopback and `--connect-port` requires it. With
`--psql scripts/aws/psql-container.sh`, `PGPASSWORD` is the only credential
crossing into the container (through the environment), and the SQL file is
mounted read-only from the working tree. `psql-container.sh` uses
`$IRIS_BENCHMARK_POSTGRES_IMAGE` (default `postgres:18`) and
`$IRIS_BENCHMARK_CONTAINER_RUNTIME` (default `podman`). The DB-role bootstrap
also accepts `--psql scripts/aws/psql-container.sh` and a loopback `--host` /
`--port` when run through a tunnel.

## Direct suite, agent, and containment

`iris-bench-agent` is not a daemon. The controller invokes it over SSH with one
JSON request on stdin and reads validated NDJSON events on stdout. The agent
accepts only the fixed verbs `inspect`, `prepare`, `run-block`, `validate`,
`bundle`, `cleanup`, and `status`, and takes an exclusive host lock before any
mutating verb.

`run-block` (direct `judger` suite only) refuses to run unless:

- the run is qualified (`qualification/host-facts.json`) and prepared;
- the staged plan digest matches the request;
- the host facts satisfy the sealed qualification requirements;
- the fixture input and expected output match their sealed digests;
- the workload binary matches its sealed SHA-256;
- `--cgroup-parent` names an explicitly delegated cgroup v2 subtree;
- the block has a `cpuList` and the worker command is available.

It splits the CPU list round-robin across workers, creates one flat, run-scoped
cgroup subtree per worker directly beneath `--cgroup-parent` (a single component
such as `<run-id>-<block-id>-<worker-id>`; nested paths cannot inherit the
delegated `cpuset`/`cpu` controllers the benchmark writes and verifies), starts
the workers behind a readiness barrier via the binary's
internal `worker-exec` wrapper, verifies each PID's membership from `/proc`
before releasing it, and writes `samples/<block-id>.ndjson`,
`telemetry/thermal-<block-id>.ndjson`, and a block receipt.
Any process observed outside the intended subtree is a failed run, not a
warning, except for the root sandbox or its descendants accepted by explicit
`production-compat` mode. `judger-bench` rejects non-success result/error codes,
output mismatches, and invalid cgroup evidence. Validation requires a passing
receipt and exact sample hash/count for every planned block.

Each worker is started behind the local readiness barrier and then runs a
closed loop with no client-side delay: as soon as the barrier is released, the
worker submits its next iteration the moment the previous one returns. The
coordinator releases the barrier only after every worker has attached to its
verified subtree and signalled ready, so within the resulting
all-workers-active steady window every worker always has a submission in flight
or pending and the system under test is never idle. The coordinator refuses a
run whose window is missing, empty, or negligible (see
[analyze](#analyze)). Only submissions fully inside
`[max first start, min last end]` feed primary statistics; the OCI
container-start ramp and the drain tail are excluded because they measure
container and cgroup setup plus the final partial iterations, not the
steady-state contention the benchmark exists to observe.

Because the controller reaches the agent through an SSH session, the agent
starts in a `session-*.scope` cgroup that is a sibling of, not a descendant of,
the delegated `user@<uid>.service` parent. Moving workers into that parent would
fail the kernel's common-ancestor permission rule. After parsing its flags and
before reading the request, the agent therefore validates that `--cgroup-parent`
is an absolute path strictly beneath the cgroup v2 mount and, when the current
process is not already inside it, re-executes itself as
`systemd-run --user --scope --quiet --collect -- <agent> <original
args>`. No shell is involved, standard descriptors and exit status are
preserved, and a private recursion guard marks the scoped child. The guarded
child confirms from `/proc` that it is beneath the configured parent before it
proceeds and fails closed otherwise. The internal `worker-exec` subcommand is
dispatched before this check and never re-executes, since it already runs under
the scoped agent.

## Safe cleanup

- The agent's `cleanup` verb removes **only** `/run/iris-bench/<run-id>` after
  verifying the path's basename equals the run ID, then writes a `cleaned`
  record. Evidence under `/var/lib/iris-bench/<run-id>` is preserved. `run`
  invokes `validate`, `cleanup`, and then `bundle`, so cleanup evidence is
  inventoried; `collect` remains a separate
  controller step.
- `collect` downloads into a temporary directory and renames atomically; a
  failure removes only the temporary directory and an existing result directory
  is never overwritten.
- After a successful bundle, `run` removes only the exact run-scoped `/tmp`
  files it uploaded: plan, fixtures, optional workload, report, and agent.
- Ansible disables, but never removes, unrelated desktop/printing/discovery/
  update services, and never deletes K3s state.
- Terraform protects the persistent RDS instance (`deletion_protection = true`,
  final snapshot kept) and the bucket (`prevent_destroy`). A checkpoint refresh
  must create a **new** `rds_identifier` and `benchmark_generation`; editing a
  live generation's snapshot is ignored.
- `runs/`, `bin/`, `.env`, `terraform.tfvars`, and `*.tfstate*` are git-ignored.

## Not supported in this build

- `iris-benchctl provision`, `qualify`, and `resume`.
- Embedded-testcase full-Iris modes and the client-api HTTP submission path.
- Production RabbitMQ TLS/operator clustering; the benchmark broker is a
  digest-pinned, run-local single node on an isolated vhost.
- Topology-aware CPU/NUMA auto-assignment. `cpuList` is operator-supplied and
  `numaPolicy` is recorded but not enforced.
- Privileged real-host validation in tests/CI; only fake-filesystem unit tests
  cover containment.

## Validation

Run from the repository root:

```bash
go build ./... && go vet ./... && go test ./...
fixtures/tests/verify-fixtures.sh
scripts/aws/tests/run-tests.sh
infra/aws/bootstrap/tests/run-tests.sh
infra/aws/bootstrap/tests/validate-policies.sh
infra/aws/iris-benchmark/tests/validate-terraform.sh          # or --static-only
ANSIBLE_CONFIG=ansible/ansible.cfg ANSIBLE_ROLES_PATH=ansible/roles \
  ansible-playbook ansible/playbooks/qualify_host.yml \
  -i ansible/inventory/loadtest/hosts --syntax-check
```

The AWS script tests use a fake `aws`/`psql`; the Terraform checks include
`terraform fmt -check` and `terraform validate`. Nothing in these suites reads
credentials, contacts AWS, or touches a real host.
