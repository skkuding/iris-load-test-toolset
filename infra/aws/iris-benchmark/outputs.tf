output "rds_identifier" {
  description = "Benchmark RDS instance identifier."
  value       = aws_db_instance.benchmark.identifier
}

output "rds_address" {
  description = "Benchmark RDS endpoint hostname."
  value       = aws_db_instance.benchmark.address
}

output "rds_port" {
  description = "Benchmark RDS port."
  value       = aws_db_instance.benchmark.port
}

output "rds_db_name" {
  description = "Benchmark database name."
  value       = var.app_db_name
}

output "rds_master_username" {
  description = "Benchmark RDS master username (source-compatible)."
  value       = aws_db_instance.benchmark.username
  sensitive   = true
}

output "rds_master_secret_arn" {
  description = "ARN of the RDS-managed master credential secret."
  value       = try(aws_db_instance.benchmark.master_user_secret[0].secret_arn, null)
}

output "readonly_db_secret_arn" {
  description = "ARN of the read-only benchmark database credential secret."
  value       = aws_secretsmanager_secret.readonly_db.arn
}

output "readonly_db_username" {
  description = "Read-only benchmark database role name."
  value       = var.readonly_db_username
}

output "source_snapshot_identifier_used" {
  description = "Snapshot identifier this generation was restored from."
  value       = var.source_snapshot_identifier
}

output "benchmark_snapshot_identifier" {
  description = "Durable encrypted manual snapshot used for this benchmark generation."
  value       = aws_db_snapshot_copy.benchmark.id
}

output "testcase_bucket_name" {
  description = "Dedicated benchmark testcase bucket name."
  value       = aws_s3_bucket.testcase.bucket
}

output "testcase_bucket_arn" {
  description = "Dedicated benchmark testcase bucket ARN."
  value       = aws_s3_bucket.testcase.arn
}

output "kms_key_arn" {
  description = "Benchmark KMS key ARN."
  value       = aws_kms_key.benchmark.arn
}

output "kms_key_alias" {
  description = "Benchmark KMS key alias."
  value       = aws_kms_alias.benchmark.name
}

output "testcase_read_policy_arn" {
  description = "Narrow read-only testcase policy ARN."
  value       = aws_iam_policy.testcase_read.arn
}

output "testcase_read_role_arn" {
  description = "Assumable read-only testcase role ARN."
  value       = aws_iam_role.testcase_readonly.arn
}

output "testcase_upload_role_arn" {
  description = "Assumable fixture upload role ARN."
  value       = aws_iam_role.testcase_uploader.arn
}
