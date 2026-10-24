# Offline checks of the input validation and of what the defaults plan. The
# AWS provider is mocked, so no credentials are needed and no AWS API is
# called. Run with `terraform init -backend=false && terraform test`.
mock_provider "aws" {
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:iam::123456789012:user/test"
    }
  }
  mock_data "aws_vpc" {
    defaults = {
      id = "vpc-0aaaaaaaaaaaaaaa1"
    }
  }
  mock_data "aws_subnets" {
    defaults = {
      ids = ["subnet-0aaaaaaaaaaaaaaa1", "subnet-0aaaaaaaaaaaaaaa2"]
    }
  }
  mock_data "aws_iam_session_context" {
    defaults = {
      issuer_arn = "arn:aws:iam::123456789012:user/test"
    }
  }
}

run "defaults_plan_a_private_endpoint" {
  command = plan

  assert {
    condition     = output.cluster_endpoint_public == false
    error_message = "with no CIDRs the API endpoint should be private"
  }

  assert {
    condition     = output.node_root_volume.volume_size == 50 && output.node_root_volume.encrypted
    error_message = "the node's root volume should be 50 GiB and encrypted"
  }
}

run "endpoint_open_to_one_address" {
  command = plan

  variables {
    cluster_endpoint_public_access_cidrs = ["203.0.113.7/32"]
  }

  assert {
    condition     = output.cluster_endpoint_public == true
    error_message = "with a CIDR the API endpoint should be public"
  }
}

run "endpoint_open_to_the_internet_is_refused" {
  command = plan

  variables {
    cluster_endpoint_public_access_cidrs = ["0.0.0.0/0"]
  }

  expect_failures = [var.cluster_endpoint_public_access_cidrs]
}

run "malformed_cidr_is_refused" {
  command = plan

  variables {
    cluster_endpoint_public_access_cidrs = ["203.0.113.7"]
  }

  expect_failures = [var.cluster_endpoint_public_access_cidrs]
}
