# codedang-iris-benchmark deployer bootstrap

This directory holds the IAM policy documents, tests, and documentation for the
one-time bootstrap of the Terraform deployer identity used by
`infra/aws/iris-benchmark/`.

The bootstrap is intentionally **not** part of the Terraform module. An existing
administrator profile creates the identities; Terraform then runs as the
assumable role. The identities are:

- IAM user `codedang-iris-benchmark-deployer` — the only permission it carries
  is `sts:AssumeRole` on the Terraform role;
- IAM role `codedang-iris-benchmark-terraform` — trusts only that user and
  carries a set of customer-managed policies covering every Terraform
  permission;
- customer-managed policies
  `codedang-iris-benchmark-terraform-permissions-<suffix>` — attached to the
  role, never to the user. Permissions are split across the set so each
  rendered document stays below the IAM 6144-character non-whitespace limit.

## Files

```text
infra/aws/bootstrap/
├── README.md
├── policies/
│   ├── deployer-assume-role-policy.json      # user inline policy (sts:AssumeRole only)
│   ├── terraform-role-trust-policy.json      # role trust policy (deployer user only)
│   ├── terraform-role-policy-backend.json    # customer-managed: state/lock/identity
│   ├── terraform-role-policy-data.json       # customer-managed: RDS/KMS/secrets
│   └── terraform-role-policy-iam-network.json # customer-managed: S3/IAM/EC2
└── tests/
    ├── fake-aws/aws                          # stateful fake AWS CLI (no network)
    ├── run-tests.sh                          # fake-AWS integration tests
    └── validate-policies.sh                  # policy JSON/render/size/scoping checks
```

Each `terraform-role-policy-<suffix>.json` template becomes the customer-managed
policy `${name_prefix}-terraform-permissions-<suffix>`. Add a template with a new
suffix to extend the set; the script discovers, renders, creates, versions, tags,
and attaches every template it finds.

The policy JSON files contain `__PLACEHOLDER__` tokens. The script renders them
with the resolved account, region, prefix, state bucket/key, lock table,
snapshot ARN, user, and role names. Rendered documents are written only to a
private temporary directory and are deleted on exit.

## Usage

Run from the repository root with an administrative profile:

```bash
# Show the resolved plan without touching AWS.
scripts/aws/bootstrap-deployer.sh --dry-run --account-id <ACCOUNT_ID>

# Bootstrap the user, role, and customer-managed policies. No access key.
scripts/aws/bootstrap-deployer.sh --admin-profile <ADMIN_PROFILE> --yes

# Optional: create an access key and import the local profile.
scripts/aws/bootstrap-deployer.sh --admin-profile <ADMIN_PROFILE> \
  --create-access-key --yes

# Rotate an existing key (destructive): delete active keys, then create one.
scripts/aws/bootstrap-deployer.sh --admin-profile <ADMIN_PROFILE> \
  --create-access-key --rotate-access-key --yes

# Render the policy documents for inspection/validation (no AWS calls).
scripts/aws/bootstrap-deployer.sh --render-only \
  --output-dir /tmp/iris-bootstrap --account-id <ACCOUNT_ID>

# Use the bootstrapped role with Terraform.
AWS_PROFILE=codedang-iris-benchmark-deployer \
  terraform -chdir=infra/aws/iris-benchmark init
```

Run it a second time to confirm idempotence. Each customer-managed policy update
is guarded by a `PolicyDocumentHash` tag so an unchanged policy is not versioned
again.

## Access key safety

`--create-access-key` is required to create a key; the default run creates none.
When used:

- existing active keys cause the script to **fail safely** and create nothing;
  `--rotate-access-key` is required to delete them first;
- the secret is written only to a `0600` CSV under a private temporary
  directory, imported with `aws configure import`, and then removed;
- the secret is never printed, never placed on a command line, never written to
  this repository, and never stored in Terraform state. It lives only in the
  local AWS shared credentials file (`~/.aws/credentials`, or
  `$AWS_SHARED_CREDENTIALS_FILE`);
- role assumption is verified end to end (via the new profile) when a key is
  created, or with `iam:SimulatePrincipalPolicy` otherwise.

The deployer user identity is treated as dedicated: by default the script
detaches foreign managed policies and deletes foreign inline policies from the
user and the role so they carry only the bootstrap policies. Use
`--no-enforce-exclusive` to skip that reconciliation.

## Tests

No AWS account, credentials, or network are used.

```bash
infra/aws/bootstrap/tests/validate-policies.sh
infra/aws/bootstrap/tests/run-tests.sh
```

`validate-policies.sh` renders the templates with sample values and checks JSON
validity, per-policy 6144-character non-whitespace size, required actions, tight
resource scoping, no wildcard actions, and no credential material. `run-tests.sh`
drives the script against a stateful fake AWS CLI and checks creation,
idempotence, exclusivity, recovery from a partially completed bootstrap,
access-key handling, secret non-disclosure, and fail-safe behavior.

## Residual permission risks

These are known trade-offs in the granted policy. Review them before applying.

- `kms:CreateKey` and the read-only `Describe*`/`List*` actions sit on
  `Resource: "*"` because IAM/KMS/EC2 describe APIs cannot be resource-scoped.
  `kms:CreateKey` is additionally constrained to requests tagged with
  `Project = codedang-iris-benchmark`.
- KMS key management is scoped to `key/*` with a `aws:ResourceTag/Project`
  condition. If a key is created without the tag, subsequent management calls
  fail; Terraform tags the key, so keep `common_tags` on the key.
- The RDS-managed master secret created by `manage_master_user_password` is not
  benchmark-prefixed. Reading it is scoped to `secret:rds!db-*`, which could
  match other RDS-managed secrets in the account. Narrow this when the concrete
  secret ARN is known, or bootstrap the DB role with an administrator profile.
- `rds:CopyDBSnapshot` cannot be conditioned on the source snapshot in every
  region. The resource list includes the explicit source snapshot ARN and the
  benchmark-prefixed target snapshots, but review the source/target split if the
  account gains unrelated snapshots.
- `iam:CreateServiceLinkedRole` is limited to `AWSServiceRoleForRDS` with
  `iam:AWSServiceName = rds.amazonaws.com`. Other RDS features that need a
  different service-linked role will require a reviewed policy update.
- `iam:ListRoles` and `iam:ListPolicies` are account-wide reads. Terraform does
  not strictly require them; remove them if provider reads are proven not to
  need them.
- The deployer role can create and modify IAM roles and policies under the
  `codedang-iris-benchmark-*` prefix. This is required to create the read/upload
  roles but is the most powerful part of the policy; keep the prefix scope.
- The policy grants `s3:DeleteObject` on the testcase bucket and backend state.
  Terraform needs state rollback/refresh; the bucket retains `prevent_destroy`.
- `--no-enforce-exclusive` leaves foreign policies attached. The default
  exclusive reconciliation is destructive by design on the dedicated user/role.
