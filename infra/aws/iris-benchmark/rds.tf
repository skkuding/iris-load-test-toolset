# Password for the read-only benchmark role. Generated once, stored only in
# AWS Secrets Manager, and never committed or written to .env files.
resource "random_password" "readonly_db" {
  length  = 32
  special = false
}

resource "aws_db_parameter_group" "benchmark" {
  name        = var.parameter_group_name
  family      = var.parameter_group_family
  description = "Benchmark parameters mirroring the source Codedang database."

  parameter {
    name         = "rds.force_ssl"
    value        = "1"
    apply_method = "pending-reboot"
  }

  parameter {
    name         = "max_replication_slots"
    value        = "20"
    apply_method = "pending-reboot"
  }

  parameter {
    name         = "max_logical_replication_workers"
    value        = "4"
    apply_method = "pending-reboot"
  }

  parameter {
    name         = "max_worker_processes"
    value        = "16"
    apply_method = "pending-reboot"
  }

  tags = local.common_tags
}

# Automated source snapshots expire with the production retention window. Copy
# the selected snapshot once into a durable, encrypted benchmark checkpoint and
# restore only from that immutable manual copy.
resource "aws_db_snapshot_copy" "benchmark" {
  source_db_snapshot_identifier = var.source_snapshot_arn
  target_db_snapshot_identifier = var.benchmark_snapshot_identifier
  kms_key_id                    = aws_kms_key.benchmark.arn
  copy_tags                     = true
  tags                          = local.common_tags
}

resource "aws_db_instance" "benchmark" {
  identifier = var.rds_identifier

  engine                     = var.engine
  engine_version             = var.engine_version
  auto_minor_version_upgrade = var.auto_minor_version_upgrade
  instance_class             = var.instance_class
  allocated_storage          = var.allocated_storage
  max_allocated_storage      = var.max_allocated_storage
  storage_type               = var.storage_type
  storage_encrypted          = true
  kms_key_id                 = aws_kms_key.benchmark.arn
  parameter_group_name       = aws_db_parameter_group.benchmark.name
  port                       = var.port

  # The username and application database name are inherited from the snapshot.
  # RDS rotates the inherited master password into a managed secret.
  manage_master_user_password   = true
  master_user_secret_kms_key_id = aws_kms_key.benchmark.arn

  # Explicit snapshot input discovered by scripts/aws/discover-rds-snapshot.sh.
  # There is deliberately no nondeterministic "latest" data source here.
  snapshot_identifier = aws_db_snapshot_copy.benchmark.id

  db_subnet_group_name   = aws_db_subnet_group.benchmark.name
  vpc_security_group_ids = [aws_security_group.benchmark.id]
  availability_zone      = var.availability_zone
  publicly_accessible    = var.publicly_accessible

  backup_retention_period = var.backup_retention_period
  backup_window           = var.backup_window

  performance_insights_enabled    = var.performance_insights_enabled
  performance_insights_kms_key_id = var.performance_insights_enabled ? aws_kms_key.benchmark.arn : null

  skip_final_snapshot       = var.skip_final_snapshot
  final_snapshot_identifier = var.final_snapshot_identifier

  deletion_protection = var.deletion_protection
  apply_immediately   = var.apply_immediately

  tags = local.common_tags

  lifecycle {
    # A new checkpoint must be a new benchmark generation with a new identifier;
    # changing the snapshot in place is intentionally ignored.
    ignore_changes = [
      snapshot_identifier,
      availability_zone,
      db_name,
      username,
    ]
  }
}
