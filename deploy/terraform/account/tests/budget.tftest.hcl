# Offline checks of the budget's inputs. The AWS provider is mocked, so no
# credentials are needed and no AWS API is called. Run with
# `terraform init -backend=false && terraform test`.
mock_provider "aws" {}

variables {
  budget_alert_emails = ["owner@example.com"]
}

run "defaults_plan_a_50_usd_budget" {
  command = plan

  assert {
    condition     = aws_budgets_budget.monthly.limit_amount == "50"
    error_message = "the default monthly budget should be 50 USD"
  }

  assert {
    condition     = length(aws_budgets_budget.monthly.notification) == 2
    error_message = "the budget should alert on actual and on forecast spend"
  }
}

run "budget_needs_a_recipient" {
  command = plan

  variables {
    budget_alert_emails = []
  }

  expect_failures = [var.budget_alert_emails]
}

run "budget_refuses_a_malformed_address" {
  command = plan

  variables {
    budget_alert_emails = ["owner"]
  }

  expect_failures = [var.budget_alert_emails]
}

run "budget_must_be_positive" {
  command = plan

  variables {
    monthly_budget_usd = 0
  }

  expect_failures = [var.monthly_budget_usd]
}
