terraform {
  required_version = ">= 1.7.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.100"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }

  # Dedicated state key. Never reused by other Codedang Terraform projects.
  backend "s3" {
    bucket         = "codedang-tf-state"
    key            = "terraform/iris-benchmark.tfstate"
    region         = "ap-northeast-2"
    encrypt        = true
    dynamodb_table = "terraform-state-lock"
  }
}

provider "aws" {
  region = var.region
}

data "aws_caller_identity" "current" {}

data "terraform_remote_state" "vpc" {
  backend = "s3"
  config = {
    bucket = "codedang-tf-state"
    key    = "terraform/vpc.tfstate"
    region = "ap-northeast-2"
  }
}

locals {
  vpc_id        = data.terraform_remote_state.vpc.outputs.vpc_id
  db_subnet_ids = length(var.db_subnet_ids) > 0 ? var.db_subnet_ids : data.terraform_remote_state.vpc.outputs.db_subnet_ids

  common_tags = {
    Project             = var.name_prefix
    Environment         = "benchmark"
    ManagedBy           = "terraform"
    AWSAccount          = data.aws_caller_identity.current.account_id
    CostOwner           = var.cost_owner
    ResourceOwner       = var.resource_owner
    BenchmarkGeneration = var.benchmark_generation
    SourceDatabase      = var.source_db_identifier
    SourceSnapshot      = var.source_snapshot_identifier
  }

  readonly_secret_name = "${var.name_prefix}/readonly-db"
}
