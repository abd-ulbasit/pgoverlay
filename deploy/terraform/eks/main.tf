# Minimal single-node EKS cluster for running the pgoverlay stack
# (branchd + ghook + prod postgres + branch pods, hostpath storage mode).
# docs/eks.md walks through it; in short, keep the inputs in a
# terraform.tfvars here (gitignored) so plan, upgrade and destroy all see the
# same values:
#
#   cat > terraform.tfvars <<'EOF'
#   cluster_version                      = "1.36"
#   cluster_endpoint_public_access_cidrs = ["203.0.113.7/32"]
#   EOF
#   terraform init
#   terraform plan -out tfplan && terraform apply tfplan
#   aws eks update-kubeconfig --name pgoverlay --region ap-south-1
#
# Without cluster_endpoint_public_access_cidrs the API endpoint is private
# (reachable from inside the VPC only).
#
# Cost while running: EKS control plane (~$0.10/h) + 1× t3.large (~$0.09/h)
# + the Classic ELBs Kubernetes creates for the proxy/webhook Services. The
# budget alarm is NOT here: it lives in ../account, which is not destroyed
# with the cluster, so it still sees whatever outlives `terraform destroy`.
# `terraform destroy` removes only what this state holds: delete the
# LoadBalancer Services (and, in csi mode, the PVCs) first, then check for
# leftovers as docs/eks.md "Teardown" shows.

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

# The Kubernetes API endpoint is public only for these CIDRs. The default,
# none, makes it private-only: reachable from inside the VPC (the node, a
# bastion, a VPN into the VPC) and not from the internet. Give your own
# egress address as a /32 to run kubectl from a laptop. 0.0.0.0/0 is refused:
# the endpoint authenticates, but an internet-wide endpoint is exposed to
# every pre-auth bug and credential-stuffing attempt there is.
variable "cluster_endpoint_public_access_cidrs" {
  description = "CIDRs allowed to reach the public EKS API endpoint; empty (the default) keeps the endpoint private."
  type        = list(string)
  default     = []

  validation {
    condition = alltrue([
      for c in var.cluster_endpoint_public_access_cidrs :
      can(cidrhost(c, 0)) && !endswith(c, "/0")
    ])
    error_message = "cluster_endpoint_public_access_cidrs must be valid CIDRs and none may be /0 (the whole internet)."
  }
}

locals {
  # Public endpoint only when someone is allowed in. A local, and an output,
  # so the tests assert on what the module is given rather than on the
  # variable's default.
  endpoint_public = length(var.cluster_endpoint_public_access_cidrs) > 0

  # The node's root volume, which in hostpath mode holds every branch, the
  # seed and the registry. It goes on the launch template: the module uses a
  # custom launch template by default and then ignores `disk_size`, so the
  # node would get the AMI's 20 GiB. An output too, for the tests (a test
  # cannot read the launch template inside the module).
  node_root_volume = {
    volume_size           = 50
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
  }
}

provider "aws" {
  region = var.region

  # Every resource Terraform creates says who owns it and where it is
  # defined. The load balancers and security groups that the AWS cloud
  # controller creates for LoadBalancer Services are not Terraform's and do
  # not get these: they carry kubernetes.io/cluster/<cluster_name> instead
  # (docs/eks.md, Teardown, finds them by that tag).
  default_tags {
    tags = {
      project    = "pgoverlay-demo"
      managed-by = "terraform"
      repository = "github.com/abd-ulbasit/pgoverlay"
    }
  }
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

  cluster_endpoint_private_access      = true
  cluster_endpoint_public_access       = local.endpoint_public
  cluster_endpoint_public_access_cidrs = var.cluster_endpoint_public_access_cidrs

  # The identity that runs `apply` gets cluster-admin through an EKS access
  # entry. Fine for a single-operator demo; a shared cluster should list its
  # admins in access_entries instead.
  enable_cluster_creator_admin_permissions = true

  vpc_id     = data.aws_vpc.default.id
  subnet_ids = data.aws_subnets.default.ids

  eks_managed_node_groups = {
    main = {
      # one node: pgoverlay hostpath mode pins all data here anyway
      instance_types = ["t3.large"]
      # Amazon Linux 2023, whose root filesystem is XFS with reflink, so the
      # node kernel is a security dependency of hostpath mode
      # (docs/security.md, CVE-2026-64600). The node group takes the newest
      # AMI release when it is created and on each version upgrade; between
      # those, roll it with `aws eks update-nodegroup-version`.
      ami_type     = "AL2023_x86_64_STANDARD"
      min_size     = 1
      max_size     = 1
      desired_size = 1
      # AL2023's root device; see local.node_root_volume
      block_device_mappings = {
        xvda = {
          device_name = "/dev/xvda"
          ebs         = local.node_root_volume
        }
      }
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

output "cluster_endpoint_public" {
  description = "Whether the API endpoint is reachable from the internet (only from cluster_endpoint_public_access_cidrs)."
  value       = local.endpoint_public
}

output "node_root_volume" {
  description = "The node's root EBS volume as set on its launch template."
  value       = local.node_root_volume
}
