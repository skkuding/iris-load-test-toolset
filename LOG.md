# Iris Load Test Toolset Progress

Last updated: 2026-09-25

## Goal

Implement the reproducible Iris runtime benchmark described in `PROPOSE.md` in
this repository, beginning with the direct, contained Judger benchmark and the
host qualification/provisioning foundation.

## Approved Decisions

- Restore the benchmark database from the latest available snapshot of the
  production RDS instance `terraform-20250506182211604800000001` in
  `ap-northeast-2`.
- Create dedicated AWS resources prefixed `codedang-iris-benchmark`, with
  deletion protection and narrowly scoped access.
- Create one sanitized, repository-safe testcase fixture for each target
  problem: 568, 569, and 570. Derive problem specifications and testcase shape
  from the cloned benchmark database, then upload identical fixture content to
  the dedicated S3 bucket.
- Default the Iris image selector to
  `ghcr.io/skkuding/codedang-iris:stage`, resolve it to an immutable manifest
  digest before execution, and record that digest in every run plan.
- Inspect production nodes only through read-only commands. Use the existing
  SSH control sockets in `~/.ssh/sockets/`. Production nodes are not mutation
  targets.
- Derive the benchmark host's default turbo policy from the effective policy on
  production nodes.

## Safety Boundary

- Authorized AWS mutations are limited to the dedicated benchmark RDS clone,
  S3 bucket, and narrowly scoped identities/policies required by those
  resources.
- Production RDS, S3, Kubernetes, and production nodes remain read-only.
- Secrets and connection strings must not be committed or written to result
  bundles. `.env.example` contains only non-secret names, endpoints, ARNs, and
  reviewed digests.
- Benchmark fixture content committed to this repository must be sanitized and
  license-compatible.

## Progress

- 2026-09-25: Created the standalone repository worktree and moved the reviewed
  proposal into it.
- 2026-09-25: Recorded AWS, fixture, image, turbo-inspection, and access-scope
  decisions supplied by the operator.
- 2026-09-25: Inspected production nodes `codedang5`, `codedang6`, `codedang7`,
  and `codedang9` through existing SSH control sockets. Every node had turbo
  enabled (`intel_pstate/no_turbo=0` or `cpufreq/boost=1`), so the benchmark
  host default is now `turbo_policy: enabled`.
- 2026-09-25: Selected automated snapshot
  `rds:terraform-20250506182211604800000001-2026-09-24-16-05` and implemented
  a durable encrypted manual copy named
  `codedang-iris-benchmark-source-20260924` before RDS restore.
- 2026-09-25: Resolved `ghcr.io/skkuding/codedang-iris:stage` to OCI index
  digest `sha256:39b743d6e1ffb1efaa18f42b509c2017b91c11976971c862beafda2ae1521250`.
- 2026-09-25: Implemented the Go controller, SSH agent protocol, immutable run
  plans, direct alpha.4 Judger runner, cgroup containment checks, atomic result
  handling, and unit/integration tests. Full Iris/AMQP remains out of scope for
  this implementation slice.
- 2026-09-25: Implemented the guarded Ansible host role and read-only qualifier.
  Production hosts are deny-listed; server8 is the only mutable inventory host.
- 2026-09-25: Implemented Terraform for the benchmark RDS, encrypted snapshot,
  KMS key, S3 bucket, network allowlist, read/upload roles, and managed secrets,
  plus cloned-database fixture export and checksum/tag-aware upload tooling.
- 2026-09-25: Created IAM user `codedang-iris-benchmark-deployer`, assumable
  role `codedang-iris-benchmark-terraform`, and three size-bounded scoped
  policies. Local AWS profiles use the base user only to assume the role; all
  Terraform mutations use profile `codedang-iris-benchmark-terraform`.
- 2026-09-25: Applied the first Terraform slice. The encrypted manual snapshot
  is `available`, and state contains the KMS key, private S3 bucket, public
  access block, read/upload IAM roles, read-only secret, parameter group, subnet
  group, and random read-only password.
- 2026-09-25: Added `CONTRIBUTING.md` and the pull request template from the
  CodeDANG repository and split work into Angular-convention commits.
- 2026-09-25: Cleared the three operator-approved false taints caused by denied
  post-create reads. Every follow-up plan was required to contain zero destroys
  before apply.
- 2026-09-25: Completed the encrypted RDS clone
  `codedang-iris-benchmark`. It is available, deletion-protected, restricted to
  server8's `/32`, and uses the benchmark KMS key for storage, Performance
  Insights, and its active RDS-managed master secret. The inherited production
  password was replaced without reading or reusing it.
- 2026-09-25: Bootstrapped and verified database role `benchmark_ro` through an
  SSH tunnel to server8. Its sessions force read-only transactions and it can
  select `public.problem_testcase` in database `skkuding`.
- 2026-09-25: Exported problem specifications and active testcase metadata from
  the clone. Kept one repository-safe fixture per problem: `568/15850` is a
  small spec-derived case because the DB row had empty inline data; `569/15852`
  and `570/15873` are public sample rows. Hidden production cases were excluded.
- 2026-09-25: Uploaded and checksum-verified the six fixture objects in the
  versioned, SSE-KMS bucket `codedang-iris-benchmark-testcases`. A read-only role
  independently listed the 568 prefix and verified `hidden=false` tagging.

## Current Work

- Finish operator documentation and repository-wide validation.
- Run read-only qualification on server8 before any provisioning decision.
- Resolve the Judger containment blocker before accepting measurements.

## Blockers

- Stock Judger alpha.4 does not accept an explicit delegated cgroup parent. The
  runner now fails closed when its reported path is outside the per-worker
  subtree; a corrected Judger build or private cgroup mount arrangement must be
  validated on server8 before a measurement can be accepted.
