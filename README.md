# Iris Runtime Load-Test Toolset

Reproducible benchmark tooling for investigating and stabilizing the runtime
reported by Iris and Judger. Start with `docs/WAVE1-REPLICATION.md` for the
operator procedure. The private benchmark plan and progress log are maintained
outside this repository.

This repository currently implements the **direct Judger foundation**:

- a read-only host qualifier and an idempotent Ansible benchmark-host role;
- live, isolated AWS benchmark resources (`codedang-iris-benchmark` RDS clone,
  testcase bucket, scoped IAM) with a safe fixture workflow;
- an immutable, digest-pinned run plan (`internal/runplan`);
- a controller, `iris-benchctl`, and a host agent, `iris-bench-agent`;
- a checksum-pinned direct runner, `judger-bench`, and its image.

The full-Iris/AMQP suite, controller-side agent staging, topology
auto-assignment, and privileged real-host validation are **not** implemented in
this build. See [Not supported in this build](#not-supported-in-this-build).

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
cmd/iris-benchctl/          controller CLI (plan, run, collect, status)
cmd/iris-bench-agent/       host agent and worker-exec wrapper
cmd/judger-bench/           direct compile/execute loop and NDJSON samples
internal/                   config, runplan, orchestrator, protocol, transport,
                            containment, experiment, artifact, manifest
config/
  compatibility.yaml        host compatibility envelope (single source of truth)
  profiles.json             example controller config with profiles
  run.example.json          minimal example controller config for `run`
ansible/                    inventory, playbooks, iris_benchmark_host role
images/judger-bench.Dockerfile
fixtures/                   sanitized 568/569/570 testcases, metadata, manifest
.env.example                live benchmark names/ARNs (no secret values)
scripts/qualify-host.sh     read-only Ansible qualification wrapper
scripts/aws/                snapshot discovery, DB role, export, manifest,
                            upload, container psql
infra/aws/iris-benchmark/   dedicated Terraform module and tests
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
```

| Command | State | Behavior |
| --- | --- | --- |
| `plan` | Implemented | Resolves and prints the immutable run plan JSON (stdout) and its SHA-256 (stderr). No mutation. |
| `run` | Implemented | `inspect` -> upload plan -> `prepare` -> every `run-block` -> `validate` -> `bundle` -> `cleanup`, forwarding the required direct-suite flags. Requires a pre-staged agent; does not `collect` (run `collect` afterward). |
| `collect` | Implemented | Downloads and hash-verifies a bundle inventory into `runs/<run-id>`, secret-scans it, and writes `manifest.json`, `checksums.sha256`, `collection.json`, and `COMPLETE`. |
| `status` | Implemented | Prints local persisted state, or queries the remote agent with `--host`. |
| `provision`, `qualify`, `analyze`, `resume` | Not implemented | Print `not implemented in the minimum viable core` and exit non-zero. |

`plan` and `run` share the plan flags:

```text
-config FILE            configuration JSON file (required)
-host ALIAS             target SSH alias; must be allowlisted in the config
-profile NAME           profile name from the config (required)
-suite judger|iris      override the profile suite
-run-id ID              explicit run id (default: generated iris-YYYYMMDD-xxxxxxxx)
-iris-digest DIGEST     resolved Iris manifest digest (sha256:<64 hex>)
-resolve-image          resolve the configured Iris tag with docker buildx imagetools
-judger-digest DIGEST   Judger artifact SHA-256 (raw hex or sha256:...)
-judger-digest-file F   use the SHA-256 of file F as the Judger digest
-seed N                 run random seed (default 1)
-fixture name=path      checksummed fixture input (repeatable)
```

`run` additionally accepts the direct-suite flags listed in the `run` section
below (`--cgroup-parent`, `--bench-binary`, `--judger`, and others).

### plan

```bash
go run ./cmd/iris-benchctl plan \
  --config config/profiles.json \
  --host codedang8 \
  --profile isolated-1s \
  --iris-digest sha256:<resolved-manifest-digest> \
  --judger-digest 2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103
```

The plan refuses to seal without a resolved Iris digest and a Judger digest
(`ValidateRunnable`). The same inputs always produce the same plan digest.

### run

`run` initializes local state under `runs/.state/<run-id>/state.json`, uploads
the sealed plan to `/tmp/iris-bench-<run-id>.plan.json`, then drives the agent
through `inspect` -> `prepare` -> one `run-block` per plan block -> `validate` ->
`bundle` -> `cleanup`. `cleanup` removes only the agent runtime directory, so the
evidence preserved under `/var/lib/iris-bench/<run-id>` stays available to
`collect`. `--dry-run` prints the resolved plan summary and stops before any SSH
call. `--yes` is accepted but currently has no effect: `run` does not prompt.

A direct-suite `run` forwards the flags `run-block` needs to every agent
invocation (in addition to `--plan-file`):

```text
-cgroup-parent PATH      delegated cgroup v2 subtree (required by run-block)
-cgroup-mount PATH       cgroup v2 unified mount (agent default /sys/fs/cgroup)
-bench-binary PATH       precompiled direct-suite worker binary
-judger PATH             external alpha.4 Judger binary
-judger-sha256 HEX       Judger digest; must agree with the sealed plan
-container-id ID         base CONTAINER_ID (per-worker suffix appended)
-worker-timeout DUR      per-worker outer timeout
```

Without `--cgroup-parent`, or without a worker binary (`--bench-binary`, or an
agent `--worker-arg` template reachable only by invoking the agent directly),
`run-block` returns an `unsupported` terminal result and the run stops. There is
still no controller command that installs the agent; it must already exist at
`/opt/iris-bench/bin/<toolVersion>/iris-bench-agent` (override with `--agent`),
which is why controller-side agent staging is listed as unsupported below.

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
`checksums.sha256` and `collection.json`. It writes the `COMPLETE` marker only
after those checks pass, then atomically renames the directory to
`runs/<run-id>`. An existing result directory is never overwritten. The
inventory is produced by the agent `bundle` verb, which `run` invokes before
`cleanup`.

### status

```bash
go run ./cmd/iris-benchctl status --config config/profiles.json --run <run-id>
go run ./cmd/iris-benchctl status --config config/profiles.json --run <run-id> --host codedang8
```

Without `--host` it prints the local `state.json`. With `--host` it asks the
agent for the run's prepared state.

## Configuration

The controller accepts one strict JSON file via `--config`. Unknown fields are
rejected. Defaults are applied before validation and never override explicit
values. Example files: `config/profiles.json` and `config/run.example.json`.

```jsonc
{
  "schemaVersion": 1,
  "toolVersion": "0.1.0",
  "iris":  { "image": "ghcr.io/skkuding/codedang-iris:stage" },
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
  "hosts": [ { "alias": "codedang8", "hostIdentity": "server8", "allow": true } ],
  "paths": {
    "varRoot": "/var/lib/iris-bench",
    "runRoot": "/run/iris-bench",
    "binRoot": "/opt/iris-bench/bin",
    "socketDir": "~/.ssh/sockets",
    "resultRoot": "runs"
  },
  "limits": {
    "maxWorkers": 32,
    "maxRunSeconds": 3600,
    "maxQueueDepth": 100000,
    "maxDiskMiB": 40960,
    "stopOnErrorRatePct": 0
  }
}
```

At least one host must have `"allow": true`, and the `--host` alias must match.
Note: the config accepts `suite: "iris"` and a `turbo` value, but the agent
refuses a non-`judger` block, and `turbo` is validated then dropped from
the plan (turbo is controlled by the Ansible host role, not the controller).
`numaPolicy` is carried in the plan but the agent currently assigns one
`cpuset.mems` value to every worker (`--cpuset-mems`, default `0`); per-worker
NUMA policy is not applied yet. `cpuList` is split round-robin across workers
and must contain at least `workers` CPUs.

## Image and digest pinning

The default Iris selector is the mutable tag
`ghcr.io/skkuding/codedang-iris:stage`. A run plan is not runnable until the tag
is resolved to an immutable manifest digest:

- `--iris-digest sha256:<64 hex>` supplies an operator-resolved digest.
- `--resolve-image` runs
  `docker buildx imagetools inspect --format '{{.Manifest.Digest}}' <reference>`
  and uses the result. If the reference already contains `@sha256:...`, that
  digest is used unchanged.
- `.env.example` records `IRIS_BENCHMARK_IRIS_IMAGE_DIGEST` as a **cache only**.
  The controller never reads `.env`; re-resolve before each run.

The Judger digest is mandatory. Supply it with `--judger-digest` (raw hex or
`sha256:...`) or `--judger-digest-file <path>`. The authoritative alpha.4 amd64
digest is pinned in `images/judger-bench.Dockerfile`
(`JUDGER_AMD64_SHA256`) and re-checked at run time by `judger-bench`.

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
  non-interactively (agent/key or an existing control socket). Reusing the
  operator's existing sockets, aliases, `ProxyJump`, and host-key policy is the
  intended model.
- Ansible uses `-o ControlPath=~/.ssh/sockets/%h.sock` with `ControlPersist=3600`
  (`ansible/ansible.cfg`), so qualification and provisioning reuse the same
  sockets.

The transport only places validated remote tokens on the command line; plan
JSON and other untrusted data travel over stdin. Remote paths must be absolute
and are validated before any `ssh`/`scp` invocation.

## Host qualification and provisioning

These are driven by Ansible, not by `iris-benchctl provision`/`qualify` (both
unimplemented).

```bash
# Read-only, but become: true and post-provision assertions; needs a password.
scripts/qualify-host.sh codedang8 --ask-become-pass

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
cgroup v2 subtree and a worker command (`--bench-binary` or a `--worker-arg`
template), and `judger-bench` uses the external alpha.4 Judger library. None of
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

# 5. Export sanitized fixtures from the clone, then pin and upload them.
scripts/aws/export-problem-fixtures.sh --out fixtures
scripts/aws/export-problem-fixtures.sh --validate --fixtures-dir fixtures
scripts/aws/build-fixture-manifest.sh --write
fixtures/tests/verify-fixtures.sh
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
- `--cgroup-parent` names an explicitly delegated cgroup v2 subtree;
- the block has a `cpuList` and a worker binary (`--bench-binary` or a
  `--worker-arg` template).

It splits the CPU list round-robin across workers, creates one cgroup subtree
per worker, starts the workers behind a readiness barrier via the binary's
internal `worker-exec` wrapper, verifies each PID's membership from `/proc`
before releasing it, and writes `samples/judger.ndjson` plus a block receipt.
Any process observed outside the intended subtree is a failed run, not a
warning. `judger-bench` re-checks the pinned Judger SHA-256 and rejects samples
whose reported cgroup is not beneath the expected parent.

## Safe cleanup

- The agent's `cleanup` verb removes **only** `/run/iris-bench/<run-id>` after
  verifying the path's basename equals the run ID, then writes a `cleaned`
  record. Evidence under `/var/lib/iris-bench/<run-id>` is preserved. `run`
  invokes `validate`, `bundle`, and `cleanup`; `collect` remains a separate
  controller step.
- `collect` downloads into a temporary directory and renames atomically; a
  failure removes only the temporary directory and an existing result directory
  is never overwritten.
- `run` creates a local temporary plan file and removes it; the remote staged
  plan at `/tmp/iris-bench-<run-id>.plan.json` is not removed by the CLI.
- Ansible disables, but never removes, unrelated desktop/printing/discovery/
  update services, and never deletes K3s state.
- Terraform protects the persistent RDS instance (`deletion_protection = true`,
  final snapshot kept) and the bucket (`prevent_destroy`). A checkpoint refresh
  must create a **new** `rds_identifier` and `benchmark_generation`; editing a
  live generation's snapshot is ignored.
- `runs/`, `bin/`, `.env`, `terraform.tfvars`, and `*.tfstate*` are git-ignored.

## Not supported in this build

- `iris-benchctl provision`, `qualify`, `analyze`, and `resume`.
- The full-Iris suite and AMQP. There is no `internal/amqp` package, no
  RabbitMQ lifecycle, no publisher/collector, and no message-conservation
  validation. The agent refuses any block whose suite is not `judger`.
  `iris.rabbitmqImage` is carried into a plan when configured but is unused.
- RDS- and S3-backed judge data paths in the runner. The AWS workflow prepares
  the database and fixtures, but the agent does not read from them.
- Controller-side agent installation or version staging. `run` calls the agent
  at the versioned remote path but never uploads it; the operator must stage the
  version-matched binary first.
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
