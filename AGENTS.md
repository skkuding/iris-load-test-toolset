# AGENTS.md

Guidance for coding and operations agents working in this repository. Human
documentation is in Korean: start with [README.md](README.md) and
[docs/README.md](docs/README.md). The detailed technical reference is
[docs/reference/TOOLSET-REFERENCE.md](docs/reference/TOOLSET-REFERENCE.md).

## What this repository is

A Go toolset for measuring how reproducible Iris/Judger execution time
(`cpuTime`, `realTime`) is under concurrency on a dedicated benchmark host. A
local controller seals an immutable run plan, drives a host agent over SSH,
collects a hash-verified evidence bundle, and analyzes the all-workers-active
steady window. Python scripts plot and summarize results.

There is exactly one approved benchmark host: SSH alias `codedang8`, host
identity `skkuding-4f-4` (server8). It is detached from Kubernetes and is
shared. Treat every action on it as operations work.

## Non-negotiable safety rules

1. **Production is read-only.** Never mutate production RDS, S3, RabbitMQ,
   Kubernetes, or production nodes. Never publish to or consume from production
   judge queues. Never reuse production database credentials.
   `config/compatibility.yaml` carries the production deny-list.
2. **Never commit secrets or results.** Do not commit `.env`,
   `terraform.tfvars`, `*.tfstate*`, `/tmp/iris-runtime.env`, AWS
   credentials, connection strings, or `runs/` bundles. The repository is
   public.
3. **Ask before changing host or cloud state.** Get explicit user approval
   before any of the following:
   - rebooting `codedang8`;
   - changing turbo (`intel_pstate/no_turbo`), max frequency, or governor;
   - mounting resctrl or creating CAT/monitoring groups;
   - enabling `cpuset` in the delegated cgroup;
   - running anything with `sudo`, including Ansible provisioning;
   - using AWS credentials, `terraform apply`, or creating AWS resources;
   - writing to Notion (reads with `ntn` are fine);
   - pushing to `main` or merging pull requests.
4. **Restore what you change.** Wrap host-setting changes in a `trap` that
   restores the saved values in the same shell. Re-read the values afterwards.
5. **Do not overlap experiments.** Check that the host is idle and that no one
   else is running before starting (see preconditions below).
6. **Do not mix populations.** Every current result is `production-compat`
   with `comparable=false`. Compare stages only within one experiment. Never
   merge absolute values across experiments or with isolated runs.

## Build, test, and lint

Run from the repository root. Go 1.24+ (`go.mod`).

```bash
go build ./... && go vet ./... && go test ./...
gofmt -l .                                   # must print nothing
python3 -m py_compile analysis/*.py scripts/*.py
bash -n scripts/*.sh

go build -o bin/iris-benchctl    ./cmd/iris-benchctl
go build -o bin/iris-bench-agent ./cmd/iris-bench-agent
go build -o bin/judger-bench     ./cmd/judger-bench
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/iris-bench-rdt ./cmd/iris-bench-rdt
```

Offline suites (fake `aws`/`psql`, fake cgroup filesystem; no network):

```bash
fixtures/tests/verify-fixtures.sh
scripts/aws/tests/run-tests.sh
infra/aws/bootstrap/tests/run-tests.sh
infra/aws/bootstrap/tests/validate-policies.sh
infra/aws/iris-benchmark/tests/validate-terraform.sh --static-only
```

Unit tests never exercise real cgroups, Docker, resctrl, or the benchmark
host. Code that touches those paths needs a reviewed live run.

## Commands

### `iris-benchctl` (controller, runs locally)

| Command | Mutates | Purpose |
| --- | --- | --- |
| `plan --config F --host codedang8 --profile P [flags]` | No | Print sealed plan JSON (stdout) and its SHA-256 (stderr). |
| `run ... --dry-run` | No | Print the resolved plan summary; no SSH. |
| `run --config F --host codedang8 --profile P --run-id ID [flags]` | Host | Stage assets to `/tmp/iris-bench-<run-id>/`, then agent `inspect → prepare → run-block… → validate → cleanup → bundle`. Does not collect. |
| `collect --config F --host codedang8 --run ID` | Local only | Download and verify the bundle into `runs/<run-id>`. Never overwrites. |
| `status --config F --run ID [--host codedang8]` | No | Local state, or remote agent state with `--host`. |
| `analyze --run runs/<run-id>` | No | Steady-window statistics as JSON on stdout. |
| `provision`, `qualify`, `resume` | — | Not implemented. Use Ansible. |

Common flags:

- Required digests: `--iris-digest sha256:…` (even for the direct suite),
  `--judger-digest` (alpha.4:
  `2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103`), and
  `--rabbitmq-digest` for the `iris` suite. `--resolve-image` resolves tags
  with `docker buildx`.
- `--production-compat` is required for every current run (stock alpha.4).
- `--ssh-control-path ~/.ssh/sockets/codedang8.sock` reuses an operator socket.
- Direct suite (`judger`): `--benchmark-image sha256:<local image id on the
  host>`, `--local-agent`, `--local-bench-binary` (or `--bench-binary` +
  `--bench-binary-sha256`), exactly one `--fixture name=path` and matching
  `--expected-output name=path`, `--cgroup-parent
  /sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service`,
  `--worker-timeout 5m`. Optional `--qualification-report`.
- Full Iris suite (`iris`): `--iris-source`, `--iris-env-file` (mode 0600),
  `--problem-id`, `--language`, `--time-limit-ms`, `--memory-limit-bytes`,
  `--testcases-per-request 1`, `--message-id-start` (unique per run),
  `--rds-generation`, `--s3-object-set`, `--s3-bucket`.

### Configurations

| File | Profiles | Use |
| --- | --- | --- |
| `config/wave1-sweep.json` | `ladder-1` … `ladder-30` (`cpuList 0-31`; 150 reps at 1 worker, else 50) | Direct Judger ladder (1st experiment, RDT) |
| `config/full-iris-external.json` | `external-1` … `external-30` (50 requests per replica) | Full Iris external-data ladder (2nd, 3rd) |
| `config/profiles.json`, `config/run.example.json` | `isolated-1s`, `isolated-1s-single-cpu`, `concurrency-4` | Examples and dry runs |

Configs are strict JSON; unknown fields fail. Only `codedang8` is allowlisted.
`limits.maxLoad1` (default 1.0) makes `prepare` reject a busy host.

### Analysis

```bash
uv run analysis/plot-run.py --run runs/<id>
uv run analysis/plot-ladder.py --run 1=runs/<a> ... --run 30=runs/<f> --out ladder.png
uv run analysis/plot-repetition.py --run <round>:<label>=runs/<id> ... --out-aggregate a.png --out-drift d.png
python3 analysis/summarize-rdt.py --receipt runs/<id>/receipts/<block>.json --rdt <samples>.ndjson
python3 analysis/plot-rdt.py --results <dir> --output rdt.svg
python3 scripts/analyze-direct-cat.py <dir> <run>... > summary.json
python3 scripts/plot-direct-cat.py summary.json <outdir>
```

### Experiment scripts (records, not general tools)

| Script | Runs where | Requirements |
| --- | --- | --- |
| `scripts/run-rdt-judger-ladder.sh` | Locally | `./iris-benchctl` and `./iris-bench-agent-linux-amd64` in the repo root; sampler and workload staged in `codedang8:/tmp/iris-rdt-20261031/`; may toggle `cpuset` delegation; hardcodes uid 1001, digests, and a macOS socket path. |
| `scripts/run-direct-cat-ab.sh` (+ `cat-groups.sh`, `cat-sample.sh`, `direct-cat-worker.sh`) | On `codedang8` as root | Staged under `/tmp/iris-cat-20261101/`; resctrl mounted with no other groups; checks the exact CPU/LLC topology and SHA-256 of `libjudger.so` and the workload. |
| `scripts/qualify-host.sh [alias]` | Locally | Read-only Ansible qualification. Only passes after provisioning; may need a become password. |
| `scripts/aws/build-iris-runtime-env.sh` | Locally | AWS profiles `codedang-iris-benchmark-terraform` and `codedang-iris-benchmark-read`; writes a 0600 env file. Delete it after use. |

Review hardcoded hosts, paths, uids, and digests before reusing a script.

## Preconditions before a live run

Verify all of these and report them to the user:

- Network reachability and non-interactive SSH:
  `ssh -o BatchMode=yes -o ConnectTimeout=15 codedang8 true`.
- Host idle: `cat /proc/loadavg` shows load1 < 0.7 (runs wait for this between
  stages).
- No leftovers: no containers labeled `iris-bench.run`, no
  `/sys/fs/cgroup/sandbox-*`, no resctrl mount or `iris-cat-*` groups.
- Baseline CPU policy: `no_turbo=0`, governor `performance`, SMT on.
- The judger-bench image exists on `codedang8` and its ID matches
  `--benchmark-image`.
- Digests, fixture hashes, and the workload SHA-256 are recorded.
- AWS profiles work (Full Iris suite only).
- The user approved the plan, the expected duration, and every state change.

After the run, re-check the leftovers list and CPU policy, delete runtime
credential files, and record run IDs.

## Asking the user before the next experiment

Do not pick the next experiment yourself. Follow
[docs/NEXT-EXPERIMENT.md](docs/NEXT-EXPERIMENT.md):

1. Summarize the current state and offer the candidate list.
2. Ask for the question and decision the experiment supports, the path (direct
   Judger, Full Iris, host-direct), workload, concurrency levels, repetitions,
   and success criteria.
3. Ask for explicit approval of each state change listed in the safety rules,
   and confirm the host is free for the expected duration.
4. Write the plan using the template in that document and get confirmation
   before running.
5. Afterwards, add `docs/experiments/<YYYY-MM-DD>-<slug>.md` (Korean) and a row
   in `docs/experiments/README.md`. Ask before writing to Notion.

## Repository conventions

- Documentation for humans is Korean; `AGENTS.md` and the operator guides under
  `docs/*.md` are English. Keep README short and put detail in `docs/`.
- Experiment history lives in `docs/experiments/`. Do not copy raw bundles into
  Git. Small, reviewed summaries may go under `results/<experiment>/`.
- Commit messages follow Conventional Commits (`feat:`, `fix:`, `docs:`, ...).
  Use a branch and a pull request for changes to `main`.
- Run IDs follow `iris-<YYYYMMDD>-<description>` and are never reused.
- The isolated containment mode fails closed with stock alpha.4. Do not weaken
  containment checks to make a run pass.
