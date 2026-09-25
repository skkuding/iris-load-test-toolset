# Dedicated benchmark KMS key.
#
# The key policy is explicit and least privilege. It deliberately does NOT use
# service-principal grants such as `"Service": "rds.amazonaws.com"`; AWS KMS
# documents service access for customer managed keys through `kms:ViaService`
# (plus `kms:GrantIsForAWSResource` for CreateGrant) on account principals,
# which is the same pattern AWS uses for its managed keys. The account-root
# statement preserves IAM delegation so the Terraform deployer role keeps
# administering the key through its scoped IAM policy.
#
# References:
#   RDS customer managed key authorization:
#     https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Overview.Encryption.Keys.html
#   RDS managed master user password permissions:
#     https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-secrets-manager.html
#   Database Insights (Performance Insights) customer managed key:
#     https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PerfInsights.access-control.cmk-policy.html
#   Secrets Manager KMS permissions:
#     https://docs.aws.amazon.com/secretsmanager/latest/userguide/security-encryption.html
#   kms:ViaService and kms:GrantIsForAWSResource:
#     https://docs.aws.amazon.com/kms/latest/developerguide/policy-conditions.html
data "aws_iam_policy_document" "benchmark_kms" {
  # Preserve account-root IAM delegation. Removing this would lock out the
  # Terraform deployer role and every other in-account IAM administrator of the
  # key, and would fail the AWS KMS key-policy lockout safety check.
  statement {
    sid       = "EnableIamUserPermissions"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["arn:aws:iam::${data.aws_caller_identity.current.account_id}:root"]
    }
  }

  # RDS storage encryption/restore and Database Insights (Performance Insights)
  # create KMS grants on the caller's behalf. AWS sets
  # kms:GrantIsForAWSResource=true for these service-initiated grants.
  statement {
    sid       = "AllowRdsAndDatabaseInsightsCreateGrant"
    actions   = ["kms:CreateGrant"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    condition {
      test     = "StringEquals"
      variable = "kms:CallerAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["rds.${var.region}.amazonaws.com"]
    }

    condition {
      test     = "Bool"
      variable = "kms:GrantIsForAWSResource"
      values   = ["true"]
    }
  }

  # RDS uses the key for storage encryption and Database Insights uses it for
  # encrypted telemetry, both through the RDS service on behalf of account
  # principals. Database Insights callers additionally need kms:Decrypt and
  # kms:GenerateDataKey; both are granted here.
  statement {
    sid       = "AllowRdsAndDatabaseInsightsUse"
    actions   = ["kms:DescribeKey", "kms:Decrypt", "kms:GenerateDataKey*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    condition {
      test     = "StringEquals"
      variable = "kms:CallerAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["rds.${var.region}.amazonaws.com"]
    }
  }

  # RDS manages the master user password in Secrets Manager, which encrypts the
  # secret with this key. This mirrors the key policy AWS publishes for the
  # aws/secretsmanager managed key, restricted to this account and the Secrets
  # Manager ViaService.
  statement {
    sid = "AllowSecretsManagerUse"
    actions = [
      "kms:Encrypt",
      "kms:Decrypt",
      "kms:ReEncrypt*",
      "kms:CreateGrant",
      "kms:DescribeKey",
      "kms:GenerateDataKey*",
    ]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    condition {
      test     = "StringEquals"
      variable = "kms:CallerAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${var.region}.amazonaws.com"]
    }
  }

  # S3 applies SSE-KMS to benchmark testcase objects through the bucket default
  # encryption. Effective object access is still gated by the scoped read and
  # upload IAM policies, which additionally condition on this ViaService.
  statement {
    sid       = "AllowS3ObjectEncryption"
    actions   = ["kms:DescribeKey", "kms:Decrypt", "kms:GenerateDataKey*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    condition {
      test     = "StringEquals"
      variable = "kms:CallerAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["s3.${var.region}.amazonaws.com"]
    }
  }
}

resource "aws_kms_key" "benchmark" {
  description             = "Encryption key for codedang-iris-benchmark testcase storage and RDS-managed secrets."
  deletion_window_in_days = var.kms_deletion_window_in_days
  enable_key_rotation     = true
  policy                  = data.aws_iam_policy_document.benchmark_kms.json
  tags                    = local.common_tags
}

resource "aws_kms_alias" "benchmark" {
  name          = "alias/${var.name_prefix}"
  target_key_id = aws_kms_key.benchmark.key_id
}
