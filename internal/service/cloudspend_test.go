package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ashtwentyfour/super-eureka/internal/terraform"
)

type stubResourceSpendLoader struct {
	values       map[string]ResourceSpendAmount
	nameTagSpend []NameTagSpend
	projectSpend []ProjectSpend
}

func (l stubResourceSpendLoader) LoadResourceSpend(_ context.Context, resourceIDs []string, _, _ time.Time) (map[string]ResourceSpendAmount, error) {
	results := map[string]ResourceSpendAmount{}
	for _, id := range resourceIDs {
		if value, ok := l.values[id]; ok {
			results[id] = value
		}
	}
	return results, nil
}

func (l stubResourceSpendLoader) LoadSpendByNameTag(_ context.Context, _ []string, _, _ time.Time) ([]NameTagSpend, error) {
	return l.nameTagSpend, nil
}

func (l stubResourceSpendLoader) LoadSpendByProjectTag(_ context.Context, _ []string, _, _ time.Time) ([]ProjectSpend, error) {
	return l.projectSpend, nil
}

type stubStateLoader struct {
	stateByWorkspace map[string]*terraform.StateSummary
}

func (l stubStateLoader) Load(_ context.Context, _ terraform.S3Backend) (*terraform.StateSummary, error) {
	return l.stateByWorkspace["default"], nil
}

func (l stubStateLoader) LoadForWorkspace(_ context.Context, _ terraform.S3Backend, workspace string) (*terraform.StateSummary, error) {
	if state, ok := l.stateByWorkspace[workspace]; ok {
		return state, nil
	}
	return nil, context.Canceled
}

func TestCloudSpendServiceBuildReportForWorkspace(t *testing.T) {
	service := NewCloudSpendService(stubResourceSpendLoader{
		values: map[string]ResourceSpendAmount{
			"i-1234567890": {AmountUSD: 12.34, Currency: "USD"},
		},
		nameTagSpend: []NameTagSpend{
			{NameTag: "customers", Service: "Amazon Elastic Compute Cloud - Compute", AmountUSD: 12.34, Currency: "USD"},
		},
		projectSpend: []ProjectSpend{
			{ProjectTag: "cloudspend-demo", AmountUSD: 17.89, Currency: "USD"},
		},
	}, nil)
	service.now = func() time.Time { return time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC) }

	report, err := service.BuildReport(context.Background(), &terraform.Analysis{
		Environments: []terraform.EnvironmentAnalysis{
			{
				Name: "dev",
				Resources: []terraform.Resource{
					{Type: "aws_instance", Name: "web"},
				},
			},
		},
		State: &terraform.StateSummary{
			Resources: []terraform.StateResource{
				{
					Type: "aws_instance",
					Name: "web",
					Instances: []struct {
						Attributes map[string]any `json:"attributes"`
					}{
						{Attributes: map[string]any{
							"id":  "i-1234567890",
							"arn": "arn:aws:ec2:::instance/i-1234567890",
							"tags": map[string]any{
								"Name":    "customers",
								"Project": "cloudspend-demo",
							},
						}},
					},
				},
				{
					Type: "aws_s3_bucket",
					Name: "logs",
					Instances: []struct {
						Attributes map[string]any `json:"attributes"`
					}{
						{Attributes: map[string]any{
							"bucket": "demo-logs",
							"tags": map[string]any{
								"Name":    "customers",
								"Project": "cloudspend-demo",
							},
						}},
					},
				},
			},
		},
	}, "dev")
	if err != nil {
		t.Fatalf("BuildReport() error = %v", err)
	}

	if got := len(report.ResourceSpends); got != 2 {
		t.Fatalf("len(ResourceSpends) = %d, want 2", got)
	}
	if report.TotalUSD != 12.34 {
		t.Fatalf("TotalUSD = %.2f, want 12.34", report.TotalUSD)
	}
	if !report.ResourceSpends[0].Supported {
		t.Fatal("expected EC2 instance to be resource-level spend supported")
	}
	if len(report.NameTagSpend) != 1 || report.NameTagSpend[0].NameTag != "customers" {
		t.Fatalf("NameTagSpend = %#v, want customers rollup", report.NameTagSpend)
	}
	if len(report.ProjectSpend) != 1 || report.ProjectSpend[0].ProjectTag != "cloudspend-demo" {
		t.Fatalf("ProjectSpend = %#v, want project rollup", report.ProjectSpend)
	}
}

func TestCloudSpendServiceLoadsWorkspaceStateWhenAnalysisStateMissing(t *testing.T) {
	service := NewCloudSpendService(stubResourceSpendLoader{
		values: map[string]ResourceSpendAmount{
			"i-abcdef": {AmountUSD: 9.99, Currency: "USD"},
		},
	}, stubStateLoader{
		stateByWorkspace: map[string]*terraform.StateSummary{
			"development": {
				Resources: []terraform.StateResource{
					{
						Type: "aws_instance",
						Name: "web",
						Instances: []struct {
							Attributes map[string]any `json:"attributes"`
						}{
							{Attributes: map[string]any{"id": "i-abcdef"}},
						},
					},
				},
			},
		},
	})
	service.now = func() time.Time { return time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC) }

	report, err := service.BuildReport(context.Background(), &terraform.Analysis{
		Backend: &terraform.S3Backend{
			Bucket:             "tf-state",
			Key:                "app/terraform.tfstate",
			Region:             "us-east-1",
			WorkspaceKeyPrefix: "env:",
		},
		Environments: []terraform.EnvironmentAnalysis{
			{
				Name: "development",
				Resources: []terraform.Resource{
					{Type: "aws_instance", Name: "web"},
				},
			},
		},
	}, "development")
	if err != nil {
		t.Fatalf("BuildReport() error = %v", err)
	}
	if len(report.ResourceSpends) != 1 {
		t.Fatalf("len(ResourceSpends) = %d, want 1", len(report.ResourceSpends))
	}
	if report.TotalUSD != 9.99 {
		t.Fatalf("TotalUSD = %.2f, want 9.99", report.TotalUSD)
	}
}

func TestCloudSpendServiceFallsBackToLoadedWorkspaceStateWhenTFVarsNameDiffers(t *testing.T) {
	service := NewCloudSpendService(stubResourceSpendLoader{
		values: map[string]ResourceSpendAmount{
			"i-workspace": {AmountUSD: 4.56, Currency: "USD"},
		},
	}, stubStateLoader{
		stateByWorkspace: map[string]*terraform.StateSummary{
			"development": {
				Resources: []terraform.StateResource{
					{
						Type: "aws_instance",
						Name: "web",
						Instances: []struct {
							Attributes map[string]any `json:"attributes"`
						}{
							{Attributes: map[string]any{"id": "i-workspace"}},
						},
					},
				},
			},
		},
	})
	service.now = func() time.Time { return time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC) }

	report, err := service.BuildReport(context.Background(), &terraform.Analysis{
		Backend: &terraform.S3Backend{
			Bucket: "tf-state",
			Key:    "app/terraform.tfstate",
			Region: "us-east-1",
		},
		Environments: []terraform.EnvironmentAnalysis{
			{
				Name: "dev",
				Resources: []terraform.Resource{
					{Type: "aws_instance", Name: "web"},
				},
			},
		},
	}, "development")
	if err != nil {
		t.Fatalf("BuildReport() error = %v", err)
	}
	if len(report.ResourceSpends) != 1 {
		t.Fatalf("len(ResourceSpends) = %d, want 1", len(report.ResourceSpends))
	}
	if report.TotalUSD != 4.56 {
		t.Fatalf("TotalUSD = %.2f, want 4.56", report.TotalUSD)
	}
	if len(report.SelectionNotes) == 0 || !strings.Contains(report.SelectionNotes[0], "resolved from the remote Terraform workspace state") {
		t.Fatalf("SelectionNotes = %#v, want workspace state note", report.SelectionNotes)
	}
}

func TestCloudSpendReportMarkdown(t *testing.T) {
	report := SpendReport{
		Workspace:   "prod",
		GeneratedAt: time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC),
		StartDate:   "2026-04-04",
		EndDate:     "2026-04-18",
		ResourceSpends: []ResourceSpend{
			{
				TerraformAddress: "aws_instance.web",
				ResourceType:     "aws_instance",
				ResourceID:       "i-123",
				ARN:              "arn:aws:ec2:us-east-1:123456789012:instance/i-123",
				NameTag:          "customers",
				ProjectTag:       "cloudspend-demo",
				AmountUSD:        5.67,
				Supported:        true,
				Notes:            "Queried Cost Explorer ResourceId spend for EC2 compute.",
			},
			{
				TerraformAddress: "aws_s3_bucket.logs",
				ResourceType:     "aws_s3_bucket",
				ResourceID:       "demo-logs",
				ARN:              "arn:aws:s3:::demo-logs",
				NameTag:          "customers",
				ProjectTag:       "cloudspend-demo",
				Supported:        false,
				Notes:            "Terraform state exposed demo-logs, but Cost Explorer ResourceId spend is only available for EC2-Compute resources.",
			},
		},
		NameTagSpend: []NameTagSpend{
			{NameTag: "customers", Service: "Amazon S3", AmountUSD: 1.23, Currency: "USD"},
		},
		ProjectSpend: []ProjectSpend{
			{ProjectTag: "cloudspend-demo", AmountUSD: 6.90, Currency: "USD"},
		},
		TotalUSD: 5.67,
	}

	markdown := report.Markdown()
	for _, want := range []string{"# Cloud Spend", "## Project Spend", "## Name Tag Spend", "## Highlights", "Project `cloudspend-demo` total: `6.90 USD`", "aws_instance.web", "customers", "cloudspend-demo", "arn:aws:ec2:us-east-1:123456789012:instance/i-123", "`5.67`", "`state-only`", "Total resource-level spend captured"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}

	summary := report.SummaryMarkdown()
	for _, want := range []string{"Workspace `prod` spend snapshot", "- Project-tag totals:", "`cloudspend-demo`: `6.90 USD`", "Largest Name-tag/service rollup"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}
