# AWS benchmark workflow scripts

Safe helpers for the dedicated `codedang-iris-benchmark` AWS resources in
`ap-northeast-2`. They never read credentials from committed files and never
print secret values.

| Script | Purpose | Mutates AWS? |
| --- | --- | --- |
| `discover-rds-snapshot.sh` | Find the concrete snapshot identifier to restore from | No (read-only) |
| `build-fixture-manifest.sh` | Pin the exact fixture object set with sizes and SHA-256 | No |
| `upload-fixtures.sh` | Upload manifest objects with hash verification | Yes (S3 puts) |
| `bootstrap-benchmark-db-role.sh` | Create/grant the read-only DB role from Secrets Manager | Yes (RDS SQL) |

## Recommended order

```bash
# 1. Discover the snapshot (read-only) and copy the value into terraform.tfvars.
scripts/aws/discover-rds-snapshot.sh --format id
scripts/aws/discover-rds-snapshot.sh --format tfvars

# 2. After an approved `terraform apply`, bootstrap the read-only role.
source .env
scripts/aws/bootstrap-benchmark-db-role.sh --yes

# 3. Verify fixtures locally, then upload.
scripts/aws/build-fixture-manifest.sh --check
scripts/aws/upload-fixtures.sh --dry-run
scripts/aws/upload-fixtures.sh --yes
```

## Safety properties

- `discover-rds-snapshot.sh` makes exactly one AWS call,
  `rds describe-db-snapshots`, and chooses the newest `available` snapshot for
  the source instance deterministically. It never selects "latest" implicitly
  at apply time.
- `upload-fixtures.sh` iterates the pinned manifest and uploads each file
  individually with `s3api put-object` (never `aws s3 sync`). It verifies the
  local SHA-256 before upload and the stored checksum after upload.
- `bootstrap-benchmark-db-role.sh` reads both secrets from Secrets Manager,
  passes the master password only through `PGPASSWORD`, and feeds the role
  password to psql over stdin so it never appears in the process table. The SQL
  is idempotent and grants only CONNECT/USAGE/SELECT with
  `default_transaction_read_only = on`.
- `--dry-run` modes perform no AWS calls and do not read secrets.

## Tests

`scripts/aws/tests/run-tests.sh` exercises every script against a fake AWS CLI
and a fake psql. No network, credentials, or real infrastructure are used.
