resource "aws_s3_bucket" "testcase" {
  bucket        = var.testcase_bucket_name
  force_destroy = false

  tags = local.common_tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_ownership_controls" "testcase" {
  bucket = aws_s3_bucket.testcase.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "testcase" {
  bucket = aws_s3_bucket.testcase.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "testcase" {
  bucket = aws_s3_bucket.testcase.id

  versioning_configuration {
    status = "Enabled"
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "testcase" {
  bucket = aws_s3_bucket.testcase.id

  rule {
    bucket_key_enabled = true

    apply_server_side_encryption_by_default {
      kms_master_key_id = aws_kms_key.benchmark.arn
      sse_algorithm     = "aws:kms"
    }
  }

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "testcase" {
  bucket = aws_s3_bucket.testcase.id

  rule {
    id     = "abort-incomplete-multipart-uploads"
    status = "Enabled"

    filter {}

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  rule {
    id     = "expire-noncurrent-versions"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = var.s3_noncurrent_version_expiration_days
    }
  }
}

data "aws_iam_policy_document" "testcase_bucket" {
  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]

    resources = [
      aws_s3_bucket.testcase.arn,
      "${aws_s3_bucket.testcase.arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  statement {
    sid     = "DenyUploadsWithoutBenchmarkKMSKey"
    effect  = "Deny"
    actions = ["s3:PutObject"]

    resources = ["${aws_s3_bucket.testcase.arn}/*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    # Deny only when an uploader explicitly presents a header that is not the
    # benchmark key. Requests that omit the header use the bucket's default
    # SSE-KMS configuration and are allowed.
    condition {
      test     = "Null"
      variable = "s3:x-amz-server-side-encryption-aws-kms-key-id"
      values   = ["false"]
    }

    condition {
      test     = "StringNotEquals"
      variable = "s3:x-amz-server-side-encryption-aws-kms-key-id"
      values   = [aws_kms_key.benchmark.arn]
    }
  }
}

resource "aws_s3_bucket_policy" "testcase" {
  bucket = aws_s3_bucket.testcase.id
  policy = data.aws_iam_policy_document.testcase_bucket.json

  depends_on = [aws_s3_bucket_public_access_block.testcase]

  lifecycle {
    prevent_destroy = true
  }
}
