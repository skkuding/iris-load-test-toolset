resource "aws_db_subnet_group" "benchmark" {
  name       = "${var.name_prefix}-subnet-group"
  subnet_ids = local.db_subnet_ids
  tags       = local.common_tags
}

resource "aws_security_group" "benchmark" {
  name        = "${var.name_prefix}-rds"
  description = "Benchmark RDS access restricted to approved benchmark/controller CIDRs."
  vpc_id      = local.vpc_id

  dynamic "ingress" {
    for_each = var.benchmark_allowed_cidrs
    content {
      description = "Benchmark client ${ingress.value}"
      from_port   = var.port
      to_port     = var.port
      protocol    = "tcp"
      cidr_blocks = [ingress.value]
    }
  }

  egress {
    description = "Default egress"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = local.common_tags
}
