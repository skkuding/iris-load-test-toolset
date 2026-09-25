# Narrow read-only identity policy for the benchmark testcase bucket.
#
# Object reads are limited to the approved problem prefixes. Listing is limited
# to those same prefixes, plus a prefix-less bucket existence check (HeadBucket)
# that the Iris loader performs at startup.
data "aws_iam_policy_document" "testcase_read" {
  statement {
    sid       = "CheckBucketExistence"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.testcase.arn]

    condition {
      test     = "Null"
      variable = "s3:prefix"
      values   = ["true"]
    }
  }

  statement {
    sid       = "ListApprovedTestcasePrefixes"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.testcase.arn]

    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = [for problem in var.testcase_problem_ids : "${problem}/*"]
    }
  }

  statement {
    sid       = "ReadApprovedTestcaseObjects"
    actions   = ["s3:GetObject", "s3:GetObjectTagging"]
    resources = [for problem in var.testcase_problem_ids : "${aws_s3_bucket.testcase.arn}/${problem}/*"]
  }

  statement {
    sid       = "DecryptApprovedTestcaseObjects"
    actions   = ["kms:Decrypt"]
    resources = [aws_kms_key.benchmark.arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["s3.${var.region}.amazonaws.com"]
    }
  }

  # Defense in depth: this identity can never write, delete, or alter the
  # bucket even if another policy would otherwise allow it.
  statement {
    sid    = "DenyTestcaseWritesAndDeletes"
    effect = "Deny"
    actions = [
      "s3:PutObject",
      "s3:PutObjectAcl",
      "s3:PutObjectTagging",
      "s3:DeleteObject",
      "s3:DeleteObjectVersion",
      "s3:PutBucketPolicy",
      "s3:DeleteBucketPolicy",
      "s3:PutBucketAcl",
      "s3:PutLifecycleConfiguration",
    ]

    resources = [
      aws_s3_bucket.testcase.arn,
      "${aws_s3_bucket.testcase.arn}/*",
    ]
  }
}

resource "aws_iam_policy" "testcase_read" {
  name        = "${var.name_prefix}-testcase-read"
  description = "Read-only access to approved benchmark testcase prefixes (problems ${join(", ", var.testcase_problem_ids)})."
  policy      = data.aws_iam_policy_document.testcase_read.json
  tags        = local.common_tags
}

# Optional assumable role for short-lived STS credentials on the benchmark host.
# Created only when at least one trusted principal is supplied.
data "aws_iam_policy_document" "assume_role" {
  statement {
    sid     = "AllowBenchmarkPrincipals"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "AWS"
      identifiers = var.assume_role_principal_arns
    }
  }
}

resource "aws_iam_role" "testcase_readonly" {
  name                 = "${var.name_prefix}-testcase-read"
  description          = "Assumable read-only identity for benchmark testcase objects."
  assume_role_policy   = data.aws_iam_policy_document.assume_role.json
  max_session_duration = 3600

  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "testcase_readonly" {
  role       = aws_iam_role.testcase_readonly.name
  policy_arn = aws_iam_policy.testcase_read.arn
}

data "aws_iam_policy_document" "testcase_upload" {
  statement {
    sid       = "ListBenchmarkFixturePrefixes"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.testcase.arn]

    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = [for problem in var.testcase_problem_ids : "${problem}/*"]
    }
  }

  statement {
    sid       = "UploadBenchmarkFixtures"
    actions   = ["s3:PutObject", "s3:PutObjectTagging", "s3:GetObject", "s3:GetObjectTagging"]
    resources = [for problem in var.testcase_problem_ids : "${aws_s3_bucket.testcase.arn}/${problem}/*"]
  }

  statement {
    sid       = "UseBenchmarkKeyForFixtures"
    actions   = ["kms:GenerateDataKey", "kms:Decrypt"]
    resources = [aws_kms_key.benchmark.arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["s3.${var.region}.amazonaws.com"]
    }
  }
}

resource "aws_iam_policy" "testcase_upload" {
  name        = "${var.name_prefix}-testcase-upload"
  description = "Write and verify sanitized benchmark fixtures only."
  policy      = data.aws_iam_policy_document.testcase_upload.json
  tags        = local.common_tags
}

resource "aws_iam_role" "testcase_uploader" {
  name                 = "${var.name_prefix}-testcase-upload"
  description          = "Assumable role for uploading sanitized benchmark fixtures."
  assume_role_policy   = data.aws_iam_policy_document.assume_role.json
  max_session_duration = 3600
  tags                 = local.common_tags
}

resource "aws_iam_role_policy_attachment" "testcase_uploader" {
  role       = aws_iam_role.testcase_uploader.name
  policy_arn = aws_iam_policy.testcase_upload.arn
}
