# Account-level guard rails for the EKS demo, kept apart from ../eks on
# purpose: `terraform destroy` there must not take the budget with it. What
# outlives a cluster teardown (load balancers and security groups Kubernetes
# created, EBS volumes from PVCs) is outside that state, so only an alarm
# that outlives the cluster can catch it.
#
#   cd deploy/terraform/account
#   cat > terraform.tfvars <<'EOF'
#   budget_alert_emails = ["you@example.com"]
#   EOF
#   terraform init
#   terraform plan -out tfplan && terraform apply tfplan
#
# Apply this once, before the cluster, and leave it. Keep its state file
# (local, gitignored): it is how you later change or remove the budget.

terraform {
  required_version = ">= 1.11.0, < 2.0.0"

  required_providers {
    aws = {
      source = "hashicorp/aws"
      # The exact build and its hashes are pinned by .terraform.lock.hcl
      # (committed), the same provider build ../eks uses.
      version = "~> 5.0"
    }
  }

  # Local state, as in ../eks; see the backend note there for anything
  # shared.
}

variable "region" {
  description = "AWS region for the provider. AWS Budgets is global; this only picks the API endpoint region."
  type        = string
  default     = "ap-south-1"

  validation {
    condition     = can(regex("^[a-z]{2}(-[a-z]+)+-[0-9]$", var.region))
    error_message = "region must be an AWS region name such as ap-south-1."
  }
}

variable "budget_name" {
  description = "Name of the budget in AWS Budgets."
  type        = string
  default     = "pgoverlay-account-monthly"
}

variable "monthly_budget_usd" {
  description = "Monthly AWS cost (USD, whole account) above which the budget alerts."
  type        = number
  default     = 50

  validation {
    condition     = var.monthly_budget_usd > 0
    error_message = "monthly_budget_usd must be greater than 0."
  }
}

# Required, with no default: a cluster nobody is told about is how a demo
# keeps billing for months.
variable "budget_alert_emails" {
  description = "Email addresses AWS Budgets notifies at 80% of actual and 100% of forecast monthly spend."
  type        = list(string)

  validation {
    condition = length(var.budget_alert_emails) > 0 && alltrue([
      for e in var.budget_alert_emails : can(regex("^[^@[:space:]]+@[^@[:space:]]+\\.[^@[:space:]]+$", e))
    ])
    error_message = "budget_alert_emails needs at least one email address."
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      project    = "pgoverlay-demo"
      managed-by = "terraform"
      repository = "github.com/abd-ulbasit/pgoverlay"
    }
  }
}

# Whole-account on purpose: the spend that matters after a teardown is on
# resources Kubernetes created, which carry none of this project's tags, so
# a budget filtered on those tags would miss it. On an account that runs
# other things, raise the limit or add a cost filter.
resource "aws_budgets_budget" "monthly" {
  name         = var.budget_name
  budget_type  = "COST"
  limit_amount = tostring(var.monthly_budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = var.budget_alert_emails
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = var.budget_alert_emails
  }
}

output "budget_name" {
  value = aws_budgets_budget.monthly.name
}
