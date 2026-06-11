# Minimal single-node EKS cluster for running the pgbranch stack
# (branchd + ghook + prod postgres + branch pods, hostpath storage mode).
#
#   terraform init && terraform apply
#   aws eks update-kubeconfig --name pgbranch --region ap-south-1
#
# Cost while running: EKS control plane (~$0.10/h) + 1× t3.large (~$0.09/h)
# + NLBs for the proxy/webhook services. `terraform destroy` removes it all
# (delete the LoadBalancer Services first so the NLBs go away).

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

variable "region" {
  default = "ap-south-1"
}

variable "cluster_name" {
  default = "pgbranch"
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
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.0"

  cluster_name    = var.cluster_name
  cluster_version = "1.31"

  cluster_endpoint_public_access           = true
  enable_cluster_creator_admin_permissions = true

  vpc_id     = data.aws_vpc.default.id
  subnet_ids = data.aws_subnets.default.ids

  eks_managed_node_groups = {
    main = {
      # one node: pgbranch hostpath mode pins all data here anyway
      instance_types = ["t3.large"]
      min_size       = 1
      max_size       = 1
      desired_size   = 1
      disk_size      = 50
    }
  }

  tags = {
    project = "pgbranch-demo"
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
