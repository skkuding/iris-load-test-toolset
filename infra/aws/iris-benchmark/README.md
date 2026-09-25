# codedang-iris-benchmark (Terraform)

Dedicated AWS resources for the reproducible Iris runtime benchmark, in
`ap-northeast-2`:

- a benchmark RDS PostgreSQL instance (`codedang-iris-benchmark`) restored from
  a concrete, operator-supplied snapshot of the production instance
  `terraform-20250506182211604800000001`;
- a read-only database role credential secret consumed by
  `scripts/aws/bootstrap-benchmark-db-role.sh`;
- a dedicated, versioned, KMS-encrypted, public-access-blocked S3 bucket
  (`codedang-iris-benchmark-testcases`) for sanitized testcase fixtures;
- a narrow IAM policy limited to the `568/`, `569/`, and `570/` testcase
  prefixes, plus an optional assumable read-only role.

This project is separate from the production Codedang resources. It references
the Codedang VPC outputs only to reuse DB subnets.

## Safety

- Never run `terraform apply` or `terraform destroy` without explicit approval
  and a reviewed plan.
- `source_snapshot_identifier` is a required input. There is no "latest"
  lookup in Terraform; selection happens in the read-only discovery script and
  is reviewed before apply.
- The master password is managed by RDS in Secrets Manager. No master or
  read-only password is stored in the repository or `.env` files.
- `deletion_protection` defaults to `true`. Changing the snapshot does not
  mutate an existing generation; create a new generation with a new
  `rds_identifier` and `benchmark_generation`.
- The testcase bucket has `prevent_destroy`, versioning, KMS encryption, and a
  full public access block.

## Workflow

```bash
# 1. Discover the concrete snapshot identifier (read-only).
../../../scripts/aws/discover-rds-snapshot.sh --format id

# 2. Provide variables (never committed).
cp terraform.tfvars.example terraform.tfvars
# edit cost_owner, resource_owner, source_snapshot_identifier, CIDRs, ...

# 3. Inspect only. Apply requires explicit approval.
terraform fmt -check -recursive
terraform init
terraform validate
terraform plan

# 4. After an approved apply, bootstrap the read-only DB role.
../../../scripts/aws/bootstrap-benchmark-db-role.sh --yes

# 5. Upload sanitized fixtures with hash verification.
../../../scripts/aws/upload-fixtures.sh
```

## Checkpoint refresh

To move to a newer checkpoint, do not edit the snapshot of a live generation.
Instead:

1. Discover the new snapshot identifier.
2. Choose a new `rds_identifier` suffix and a new `benchmark_generation`.
3. Apply the new generation, bootstrap the read-only role, re-upload fixtures,
   and record the new generation in the run plan.

## Validation

`tests/validate-terraform.sh` runs formatting and configuration checks, and
asserts the source contains no nondeterministic snapshot selection, no
committed credentials, and no hardcoded secret connection strings.

## Assumptions and gaps

- The Codedang VPC remote state (`codedang-tf-state` /
  `terraform/vpc.tfstate`) provides `vpc_id` and `db_subnet_ids`. Override with
  `db_subnet_ids` if that state is unavailable.
- The default bucket name must be globally unique in the account. Override
  `testcase_bucket_name` (still prefixed) if it is taken.
- `benchmark_allowed_cidrs` defaults to empty, so no client can reach the
  database until approved addresses are added.
- This module does not manage fixture content. See `fixtures/README.md`.
