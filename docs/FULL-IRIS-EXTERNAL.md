# Full Iris external-data benchmark

This suite runs the production Iris image through a run-local RabbitMQ broker
against the dedicated benchmark RDS and S3 resources. It is separate from the
direct Judger suite and is always a `production-compat`, non-comparable
population because stock Judger alpha.4 creates host-root `/sandbox-*` cgroups.

## Data flow

```text
iris-benchctl
  -> local RabbitMQ /loadtest
  -> production Iris containers
  -> benchmark RDS read-only preflight
  -> codedang-iris-benchmark-testcases S3 objects
  -> Judger alpha.4
  -> local RabbitMQ result queue
  -> validated result bundle
```

Current Iris reads testcase data from S3 first and uses PostgreSQL only as a
fallback. The harness therefore performs a read-only RDS ping and active-row
count before measurement, while the measured judge requests load testcase bytes
from S3. The receipt records only the active-row count, never the connection
string or credentials.

## Load boundary

Each block follows this order:

1. Start digest-pinned RabbitMQ with generated run-local credentials.
2. Declare and purge the isolated `/loadtest` exchange and queues.
3. Start the result collector.
4. Publish every request with `mandatory=true` and publisher confirms.
5. Require the request queue depth to equal the planned request count.
6. Create all Iris containers behind one filesystem barrier.
7. Start every container in one `docker start` invocation, wait for every
   wrapper-ready marker, then atomically release the barrier.
8. Require exactly one testcase result and one terminal result per request.
9. Require zero duplicate, redelivered, unexpected, or missing messages and
   empty request/result queues.
10. Capture logs and telemetry, remove containers, validate and remove the
    exact empty alpha.4 sandbox cgroups, remove the network and runtime secrets,
    and scan persistent evidence for known secret values.

The barrier synchronizes Iris process startup, not the instant at which each
process finishes S3/RDS initialization and opens AMQP. Queue prefill guarantees
that startup skew cannot make a ready consumer idle.

## Runtime credentials

Generate a short-lived env file outside the repository:

```bash
set -a
source .env.example
set +a
scripts/aws/build-iris-runtime-env.sh \
  --out /tmp/iris-runtime.env \
  --secret-profile codedang-iris-benchmark-terraform \
  --s3-profile codedang-iris-benchmark-read \
  --yes
```

The helper reads only the benchmark read-only DB secret and exports temporary
credentials from the S3 read-role profile. It refuses resource names outside
the `codedang-iris-benchmark` boundary and writes mode `0600`. Delete the file
after the run.

## Run

Resolve both image tags immediately before a run. The values below identify the
validated 2026-09-27 population and are examples, not permanent latest values.

```bash
bin/iris-benchctl run \
  --config config/full-iris-external.json \
  --host codedang8 --profile external-1 \
  --iris-digest sha256:98adb52a1f41837def5e7d725b9052e04a8b7b48ed1bcdcaa1d66f0ff5fe41e7 \
  --rabbitmq-digest sha256:606d8c0d6b3c18d1da9afc53bc7cdb2a8d5486df91b5a9830e9e07626c9ae281 \
  --judger-digest 2c9a4da817e06f49daabe6b437f81d10f78b42be517d2e345ab4f868a4189103 \
  --iris-source fixtures/iris-external-568/source.cpp \
  --iris-env-file /tmp/iris-runtime.env \
  --problem-id 568 --language Cpp \
  --time-limit-ms 1000 --memory-limit-bytes 1073741824 \
  --testcases-per-request 1 --message-id-start 2750001 \
  --rds-generation v1 \
  --s3-object-set sha256:5fa11ffb80db063b982cc142d110c7c0fdd46b1abf299cca77a0276dc0cad615 \
  --s3-bucket codedang-iris-benchmark-testcases \
  --production-compat \
  --local-agent bin/iris-bench-agent \
  --ssh-control-path "$HOME/.ssh/sockets/codedang8.sock" \
  --run-id iris-YYYYMMDD-fulliris

bin/iris-benchctl collect \
  --config config/full-iris-external.json --host codedang8 \
  --ssh-control-path "$HOME/.ssh/sockets/codedang8.sock" \
  --run iris-YYYYMMDD-fulliris

bin/iris-benchctl analyze --run runs/iris-YYYYMMDD-fulliris
```

`config/full-iris-external.json` defines `external-1`, `external-2`, `external-4`,
`external-8`, `external-16`, and `external-30`. Every profile submits 50
requests per replica, so the totals are 50/100/200/400/800/1500. Each Iris
container is assigned one CPU; the alpha.4 submission cgroups remain
uncontained, matching production behavior.

## Validated runs

A single 1/2/4/8/16/30 ladder and five reboot rounds of the same ladder were
collected on 2026-09-27. Every run conserved all messages: no duplicates,
redeliveries, missing or unexpected records, empty final queues, and zero
residual containers, networks, or sandbox cgroups. RDS reported two active rows
for problem 568 in every run.

Single-ladder medians (50 submissions per replica):

| Replicas | Requests | Duration | CPU median | Real median | E2E median |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 50 | 55.2 s | 383 ms | 417 ms | 24322 ms |
| 2 | 100 | 57.9 s | 395 ms | 432 ms | 25835 ms |
| 4 | 200 | 61.9 s | 399 ms | 433.5 ms | 28387 ms |
| 8 | 400 | 67.9 s | 440.5 ms | 480 ms | 32620 ms |
| 16 | 800 | 82.7 s | 496 ms | 540.5 ms | 39866 ms |
| 30 | 1500 | 104.9 s | 555 ms | 611 ms | 58087 ms |

Five reboot rounds (bundles `runs/iris-20260927-r{1..5}-{1,2,4,8,16,30}x/`)
gave the per-level median-of-medians and spread below.

| Replicas | CPU median | CPU spread | Real median | Real spread | E2E median | E2E spread |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 383 ms | 0.26 % | 415.5 ms | 0.24 % | 26262 ms | 1.70 % |
| 2 | 394 ms | 2.03 % | 432.5 ms | 3.01 % | 25648 ms | 1.33 % |
| 4 | 404 ms | 2.23 % | 438 ms | 2.28 % | 28352 ms | 1.82 % |
| 8 | 442 ms | 0.68 % | 479 ms | 1.25 % | 32377 ms | 2.63 % |
| 16 | 497 ms | 0.20 % | 541 ms | 0.55 % | 40215 ms | 0.92 % |
| 30 | 553 ms | 0.54 % | 611 ms | 0.33 % | 58101 ms | 4.15 % |

Figures: `docs/images/fulliris-ladder.png` (single ladder),
`docs/images/fulliris-reboot-aggregate.png` (5-round pooled distributions), and
`docs/images/fulliris-reboot-drift.png` (per-round medians). Reproduce them with
`analysis/plot-ladder.py` and `analysis/plot-repetition.py`.

The first 30-replica attempt measured and conserved correctly but failed in
teardown because it removed one sandbox root per worker inside a shared 30 s
budget. Teardown now validates every root and removes them all with a single
privileged helper on a dedicated 5-minute budget; the re-run
`iris-20260927-ladder-30x-r2` finished with zero residue.

All manifests are non-comparable because the current host qualification report
failed an unrelated delegated `cpuset` check and because production-compatible
alpha.4 execution is uncontained.

## Limitations

- This is not the client-api HTTP flow and does not mutate submission rows.
- Local RabbitMQ is non-TLS and single-node; production uses operator-managed
  TLS RabbitMQ.
- Iris result messages do not identify the replica, so samples use
  `workerId: "unknown"`.
- End-to-end latency starts at queue publication. It intentionally includes
  queue wait, Iris startup, S3 access, compilation, execution, and result
  publication.
- The curated source is valid only for approved testcase `568/15850`; it is a
  deterministic benchmark fixture, not a general problem solution.
