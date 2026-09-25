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
  bundles. `.env.example` contains names and placeholders only.
- Benchmark fixture content committed to this repository must be sanitized and
  license-compatible.

## Progress

- 2026-09-25: Created the standalone repository worktree and moved the reviewed
  proposal into it.
- 2026-09-25: Recorded AWS, fixture, image, turbo-inspection, and access-scope
  decisions supplied by the operator.

## Current Work

- Audit live read-only facts: production turbo policy, latest RDS snapshot,
  database/testcase schema, and the `stage` Iris image digest.
- Implement the controller/agent foundation, host role, AWS resources, fixture
  workflow, direct runner, schemas, and validation.

## Blockers

- None recorded yet.
