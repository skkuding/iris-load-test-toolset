resource "aws_kms_key" "benchmark" {
  description             = "Encryption key for codedang-iris-benchmark testcase storage and RDS-managed secrets."
  deletion_window_in_days = var.kms_deletion_window_in_days
  enable_key_rotation     = true
  tags                    = local.common_tags
}

resource "aws_kms_alias" "benchmark" {
  name          = "alias/${var.name_prefix}"
  target_key_id = aws_kms_key.benchmark.key_id
}
