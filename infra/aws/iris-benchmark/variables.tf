variable "region" {
  description = "AWS region for all benchmark resources."
  type        = string
  default     = "ap-northeast-2"
}

variable "name_prefix" {
  description = "Prefix for dedicated benchmark resources and secret names."
  type        = string
  default     = "codedang-iris-benchmark"

  validation {
    condition     = startswith(var.name_prefix, "codedang-iris-benchmark")
    error_message = "name_prefix must start with 'codedang-iris-benchmark'."
  }
}

variable "cost_owner" {
  description = "Cost owner tag value. Required; there is intentionally no default."
  type        = string

  validation {
    condition     = length(trimspace(var.cost_owner)) > 0
    error_message = "cost_owner must be set to a real owner or cost center."
  }
}

variable "resource_owner" {
  description = "Resource owner tag value. Required; there is intentionally no default."
  type        = string

  validation {
    condition     = length(trimspace(var.resource_owner)) > 0
    error_message = "resource_owner must be set to a real owner."
  }
}

variable "benchmark_generation" {
  description = "Benchmark data generation label. Bump when restoring a new checkpoint instead of mutating an existing one."
  type        = string
  default     = "v1"
}

variable "source_db_identifier" {
  description = "Source production RDS instance the benchmark checkpoint came from. Used for discovery and tags only; never accessed by Terraform."
  type        = string
  default     = "terraform-20250506182211604800000001"
}

variable "source_snapshot_identifier" {
  description = "Concrete RDS snapshot identifier to restore the benchmark instance from. Discover it with scripts/aws/discover-rds-snapshot.sh and pass it explicitly; do not select 'latest' at apply time."
  type        = string

  validation {
    condition = (
      can(regex("^[A-Za-z0-9][A-Za-z0-9:_-]{0,254}$", var.source_snapshot_identifier)) &&
      !can(regex("(?i)latest", var.source_snapshot_identifier)) &&
      !can(regex("(?i)most[_-]?recent", var.source_snapshot_identifier))
    )
    error_message = "source_snapshot_identifier must be a concrete RDS snapshot identifier and must not be 'latest' or 'most_recent'."
  }
}

variable "source_snapshot_arn" {
  description = "ARN of the concrete source snapshot copied into the durable encrypted benchmark checkpoint."
  type        = string
  default     = "arn:aws:rds:ap-northeast-2:219857217698:snapshot:rds:terraform-20250506182211604800000001-2026-09-24-16-05"
}

variable "benchmark_snapshot_identifier" {
  description = "Durable encrypted manual snapshot copied from the selected automated source snapshot."
  type        = string
  default     = "codedang-iris-benchmark-source-20260924"
}

variable "rds_identifier" {
  description = "Dedicated benchmark RDS instance identifier."
  type        = string
  default     = "codedang-iris-benchmark"

  validation {
    condition     = startswith(var.rds_identifier, "codedang-iris-benchmark")
    error_message = "rds_identifier must start with 'codedang-iris-benchmark'."
  }
}

variable "app_db_name" {
  description = "Application database restored from the source snapshot."
  type        = string
  default     = "skkuding"
}

variable "engine" {
  description = "RDS engine (source-compatible)."
  type        = string
  default     = "postgres"
}

variable "engine_version" {
  description = "RDS engine version (source-compatible)."
  type        = string
  default     = "18.4"
}

variable "auto_minor_version_upgrade" {
  description = "Allow RDS minor version auto-upgrades (source-compatible: false)."
  type        = bool
  default     = false
}

variable "instance_class" {
  description = "RDS instance class (source-compatible)."
  type        = string
  default     = "db.t4g.small"
}

variable "allocated_storage" {
  description = "Allocated storage in GiB (source-compatible)."
  type        = number
  default     = 30
}

variable "max_allocated_storage" {
  description = "Maximum autoscaled storage in GiB (source-compatible)."
  type        = number
  default     = 100
}

variable "storage_type" {
  description = "RDS storage type (source-compatible)."
  type        = string
  default     = "gp3"
}

variable "port" {
  description = "Database port (source-compatible)."
  type        = number
  default     = 5433
  sensitive   = true
}

variable "parameter_group_name" {
  description = "Dedicated benchmark parameter group name."
  type        = string
  default     = "codedang-iris-benchmark-postgres18"

  validation {
    condition     = startswith(var.parameter_group_name, "codedang-iris-benchmark")
    error_message = "parameter_group_name must start with 'codedang-iris-benchmark'."
  }
}

variable "parameter_group_family" {
  description = "Parameter group family (source-compatible)."
  type        = string
  default     = "postgres18"
}

variable "backup_retention_period" {
  description = "Automated backup retention in days (source-compatible)."
  type        = number
  default     = 7
}

variable "backup_window" {
  description = "Daily backup window in UTC (source-compatible)."
  type        = string
  default     = "16:00-17:00"
}

variable "availability_zone" {
  description = "AZ for the benchmark instance (source-compatible)."
  type        = string
  default     = "ap-northeast-2b"
}

variable "publicly_accessible" {
  description = "Expose the benchmark instance publicly. Source was publicly accessible for on-premise Iris. Keep network access restricted with benchmark_allowed_cidrs."
  type        = bool
  default     = true
}

variable "performance_insights_enabled" {
  description = "Enable Performance Insights (source-compatible)."
  type        = bool
  default     = true
}

variable "deletion_protection" {
  description = "Protect the persistent benchmark instance from destruction. Required by the approved controls."
  type        = bool
  default     = true
}

variable "skip_final_snapshot" {
  description = "Skip the final snapshot on destroy. Source-compatible default keeps a final snapshot."
  type        = bool
  default     = false
}

variable "final_snapshot_identifier" {
  description = "Final snapshot identifier used when the benchmark instance is destroyed."
  type        = string
  default     = "codedang-iris-benchmark-final-snapshot"
}

variable "apply_immediately" {
  description = "Apply RDS modifications immediately instead of during the maintenance window. Keep false for stable benchmark generations."
  type        = bool
  default     = false
}

variable "db_subnet_ids" {
  description = "Optional explicit DB subnet IDs. Defaults to the Codedang VPC db_subnet_ids from remote state."
  type        = list(string)
  default     = []
}

variable "benchmark_allowed_cidrs" {
  description = "CIDR blocks allowed to reach the benchmark database port. Empty means no network client is authorized yet."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for cidr in var.benchmark_allowed_cidrs : can(cidrnetmask(cidr))])
    error_message = "benchmark_allowed_cidrs entries must be valid IPv4 CIDR blocks."
  }
}

variable "testcase_bucket_name" {
  description = "Dedicated, versioned testcase bucket name."
  type        = string
  default     = "codedang-iris-benchmark-testcases"

  validation {
    condition     = startswith(var.testcase_bucket_name, "codedang-iris-benchmark")
    error_message = "testcase_bucket_name must start with 'codedang-iris-benchmark'."
  }
}

variable "testcase_problem_ids" {
  description = "Problem IDs whose testcase prefixes the benchmark identity may read."
  type        = list(string)
  default     = ["568", "569", "570"]

  validation {
    condition     = length(var.testcase_problem_ids) > 0 && alltrue([for p in var.testcase_problem_ids : can(regex("^[0-9]+$", p))])
    error_message = "testcase_problem_ids must be a non-empty list of numeric problem IDs."
  }
}

variable "readonly_db_username" {
  description = "Read-only benchmark database role name bootstrapped by scripts/aws/bootstrap-benchmark-db-role.sh."
  type        = string
  default     = "benchmark_ro"

  validation {
    condition     = can(regex("^[a-z_][a-z0-9_]{0,62}$", var.readonly_db_username))
    error_message = "readonly_db_username must be a lowercase PostgreSQL identifier."
  }
}

variable "assume_role_principal_arns" {
  description = "IAM principals allowed to assume the testcase read and upload roles."
  type        = list(string)
  default     = ["arn:aws:iam::219857217698:user/tasoo-skkuding-claude"]

  validation {
    condition     = length(var.assume_role_principal_arns) > 0
    error_message = "at least one approved principal is required for benchmark object access"
  }
}

variable "kms_deletion_window_in_days" {
  description = "KMS key deletion window for the benchmark key."
  type        = number
  default     = 30
}

variable "s3_noncurrent_version_expiration_days" {
  description = "Expire noncurrent object versions after this many days (source fixtures are versioned)."
  type        = number
  default     = 90
}
