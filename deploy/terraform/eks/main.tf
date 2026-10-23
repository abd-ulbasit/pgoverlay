# Minimal single-node EKS cluster for running the pgoverlay stack
# (branchd + ghook + prod postgres + branch pods, hostpath storage mode).
#
#   terraform init && terraform apply
#   aws eks update-kubeconfig --name pgoverlay --region ap-south-1
#
# Cost while running: EKS control plane (~$0.10/h) + 1× t3.large (~$0.09/h)
# + NLBs for the proxy/webhook services. `terraform destroy` removes it all
# (delete the LoadBalancer Services first so the NLBs go away).

terraform {
  # 1.11 is the floor for S3-native state locking (use_lockfile, see the
  # backend note below); nothing here needs a newer feature.
  required_version = ">= 1.11.0, < 2.0.0"

  required_providers {
    aws = {
      source = "hashicorp/aws"
      # Major-version constraint only: the exact provider build (and its
      # hashes) is pinned by .terraform.lock.hcl, which is committed.
      version = "~> 5.0"
    }
  }

  # State is local on purpose: this is a throwaway, single-operator demo
  # cluster, and a state bucket would outlive what it describes. Anything
  # shared or long-lived should keep state remote, encrypted and locked:
  #
  #   backend "s3" {
  #     bucket       = "<state-bucket>"
  #     key          = "pgoverlay/eks/terraform.tfstate"
  #     region       = "ap-south-1"
  #     encrypt      = true
  #     use_lockfile = true # S3-native locking; DynamoDB locking is deprecated
  #   }
}

variable "region" {
  description = "AWS region for the cluster."
  type        = string
  default     = "ap-south-1"

  validation {
    condition     = can(regex("^[a-z]{2}(-[a-z]+)+-[0-9]$", var.region))
    error_message = "region must be an AWS region name such as ap-south-1."
  }
}

variable "cluster_name" {
  description = "EKS cluster name (also the prefix of the node group and its IAM role)."
  type        = string
  default     = "pgoverlay"

  validation {
    condition     = can(regex("^[a-zA-Z][a-zA-Z0-9-]{0,99}$", var.cluster_name))
    error_message = "cluster_name must start with a letter and contain only letters, digits and hyphens (at most 100 characters)."
  }
}

# NEW clusters: never below 1.36 — pick the newest version in EKS standard
# support (`aws eks describe-cluster-versions`). Older versions age into
# AWS "extended support", billed ~6x the control-plane rate.
#
# EXISTING clusters upgrade one minor at a time: apply with
# -var cluster_version= stepping 1.33 → 1.34 → ... Each node-group rollover
# RECYCLES the node — hostpath-mode branch data and the branchd registry
# live on its disk and are lost (re-seed afterwards).
variable "cluster_version" {
  description = "Kubernetes minor version of the EKS control plane, e.g. 1.36."
  type        = string
  default     = "1.36"

  validation {
    condition     = can(regex("^1\\.[0-9]{2}$", var.cluster_version))
    error_message = "cluster_version must be a Kubernetes minor version such as 1.36."
  }
}

provider "aws" {
  region = var.region
}

# Default VPC + its subnets: this is throwaway demo infra; a real deployment
# would bring its own VPC.
data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
}

module "eks" {
  source = "terraform-aws-modules/eks/aws"
  # Exact version: the lock file pins providers, not modules, so a range here
  # would resolve to whatever the registry serves on the next `init`.
  version = "20.37.2"

  cluster_name    = var.cluster_name
  cluster_version = var.cluster_version

  cluster_endpoint_public_access           = true
  enable_cluster_creator_admin_permissions = true

  vpc_id     = data.aws_vpc.default.id
  subnet_ids = data.aws_subnets.default.ids

  eks_managed_node_groups = {
    main = {
      # one node: pgoverlay hostpath mode pins all data here anyway
      instance_types = ["t3.large"]
      min_size       = 1
      max_size       = 1
      desired_size   = 1
      disk_size      = 50
    }
  }

  tags = {
    project = "pgoverlay-demo"
  }
}

output "cluster_name" {
  value = module.eks.cluster_name
}

output "cluster_endpoint" {
  value = module.eks.cluster_endpoint
}

output "node_group_role_arn" {
  value = module.eks.eks_managed_node_groups["main"].iam_role_arn
}
