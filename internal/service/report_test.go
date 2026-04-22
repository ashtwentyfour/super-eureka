package service

import (
	"strings"
	"testing"
	"time"

	"github.com/ashtwentyfour/super-eureka/internal/terraform"
)

func TestRenderMarkdownReport(t *testing.T) {
	report := RenderMarkdownReport(Report{
		GeneratedAt: time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC),
		Request: AnalysisRequest{
			PullRequestNumber: 42,
			PullRequestTitle:  "Add terraform",
			PullRequestURL:    "https://example.com/pr/42",
			HeadRef:           "feature/costing",
			HeadSHA:           "abc123",
		},
		Analysis: &terraform.Analysis{
			Backend: &terraform.S3Backend{
				Bucket: "tf-state",
				Key:    "env/app.tfstate",
				Region: "us-east-1",
			},
			Resources: []terraform.Resource{
				{Type: "aws_instance", Name: "web", File: "main.tf"},
			},
		},
		Costs: []terraform.EstimatedCost{
			{
				ResourceAddress: "aws_instance.web",
				MonthlyUSD:      14.6,
				Basis:           "t3.micro Linux",
				Notes:           "On-demand",
				Assumptions:     []string{"Example assumption"},
			},
		},
	})

	for _, want := range []string{
		"# Terraform Cost Estimation",
		"s3://tf-state/env/app.tfstate",
		"`aws_instance.web`",
		"`14.60`",
		"Assumptions",
		"Example assumption",
		"Estimated monthly total",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

func TestRenderMarkdownReportUsesEnvironmentResourcesAndSummarizedTopLevelCosts(t *testing.T) {
	report := RenderMarkdownReport(Report{
		GeneratedAt: time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC),
		Request: AnalysisRequest{
			PullRequestNumber: 1,
			PullRequestTitle:  "Add env tables",
			PullRequestURL:    "https://example.com/pr/1",
			HeadRef:           "pr-test",
			HeadSHA:           "deadbeef",
		},
		Analysis: &terraform.Analysis{
			Resources: []terraform.Resource{
				{Type: "aws_dynamodb_table", Name: "table", File: "main.tf"},
			},
		},
		Costs: []terraform.EstimatedCost{
			{
				ResourceAddress: "aws_dynamodb_table.table[\"customers\"]",
				MonthlyUSD:      5,
				Basis:           "example",
			},
		},
		Environments: []EnvironmentReport{
			{
				Name:       "development",
				TFVarsFile: "env/dev.tfvars",
				Resources: []terraform.Resource{
					{Type: "aws_dynamodb_table", Name: "table", File: "main.tf", InstanceKey: "customers"},
					{Type: "aws_dynamodb_table", Name: "table", File: "main.tf", InstanceKey: "orders"},
				},
				Costs: []terraform.EstimatedCost{
					{ResourceAddress: `aws_dynamodb_table.table["customers"]`, MonthlyUSD: 5, Basis: "customers"},
					{ResourceAddress: `aws_dynamodb_table.table["orders"]`, MonthlyUSD: 7, Basis: "orders"},
				},
			},
		},
	})

	for _, want := range []string{
		"| Environment | Address | Type | File |",
		"`development` | `aws_dynamodb_table.table[\"customers\"]`",
		"`development` | `aws_dynamodb_table.table[\"orders\"]`",
		"| Environment | Resources | Monthly Estimate (USD) |",
		"`development` | `2` | `12.00`",
		"**Estimated monthly total:** `12.00 USD`",
		"## Environment Breakdown",
		"| Resource | Monthly Estimate (USD) | Basis | Assumptions | Notes |",
		"`aws_dynamodb_table.table[\"customers\"]` | `5.00`",
		"`aws_dynamodb_table.table[\"orders\"]` | `7.00`",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}
