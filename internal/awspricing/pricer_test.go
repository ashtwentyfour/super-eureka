package awspricing

import (
	"context"
	"strings"
	"testing"

	"github.com/ashtwentyfour/super-eureka/internal/terraform"
)

func TestInferAWSServiceFromTerraformType(t *testing.T) {
	tests := map[string]string{
		"aws_dynamodb_table":  "AmazonDynamoDB",
		"aws_lambda_function": "AWSLambda",
		"aws_s3_bucket":       "AmazonS3",
		"aws_sqs_queue":       "AmazonSQS",
		"aws_sns_topic":       "AmazonSNS",
		"aws_ecr_repository":  "AmazonECR",
		"aws_eks_cluster":     "AmazonEKS",
	}

	for resourceType, want := range tests {
		if got := inferAWSServiceFromTerraformType(resourceType); got != want {
			t.Fatalf("%s: got %q want %q", resourceType, got, want)
		}
	}
}

func TestIsLikelyFreeControlPlaneResource(t *testing.T) {
	if !isLikelyFreeControlPlaneResource("aws_iam_role") {
		t.Fatal("expected iam role to be treated as a control-plane resource")
	}
	if isLikelyFreeControlPlaneResource("aws_dynamodb_table") {
		t.Fatal("expected dynamodb table to remain billable")
	}
}

func TestEstimateGenericAWSResource_ClassifiedBillableService(t *testing.T) {
	pricer := &Pricer{}
	resource := terraform.Resource{
		Type: "aws_cloudfront_distribution",
		Name: "cdn",
	}

	got := pricer.estimateGenericAWSResource(resource)

	if got.ResourceAddress != "aws_cloudfront_distribution.cdn" {
		t.Fatalf("ResourceAddress = %q, want %q", got.ResourceAddress, "aws_cloudfront_distribution.cdn")
	}
	if got.MonthlyUSD != 0 {
		t.Fatalf("MonthlyUSD = %v, want 0", got.MonthlyUSD)
	}
	if got.Basis != "Best-effort classification only" {
		t.Fatalf("Basis = %q, want %q", got.Basis, "Best-effort classification only")
	}
	for _, want := range []string{"aws_cloudfront_distribution", "AmazonCloudFront"} {
		if !strings.Contains(got.Notes, want) {
			t.Fatalf("Notes = %q, want substring %q", got.Notes, want)
		}
	}
}

func TestEstimateGenericAWSResource_ControlPlane(t *testing.T) {
	pricer := &Pricer{}
	resource := terraform.Resource{
		Type: "aws_security_group",
		Name: "web",
	}

	got := pricer.estimateGenericAWSResource(resource)

	if got.Basis != "Control-plane resource" {
		t.Fatalf("Basis = %q, want %q", got.Basis, "Control-plane resource")
	}
	if got.MonthlyUSD != 0 {
		t.Fatalf("MonthlyUSD = %v, want 0", got.MonthlyUSD)
	}
	if !strings.Contains(got.Notes, "No direct recurring infrastructure charge") {
		t.Fatalf("Notes = %q, want control-plane explanation", got.Notes)
	}
}

func TestEstimateGenericAWSResource_UnknownAWSService(t *testing.T) {
	pricer := &Pricer{}
	resource := terraform.Resource{
		Type: "aws_foo_bar",
		Name: "example",
	}

	got := pricer.estimateGenericAWSResource(resource)

	if got.Basis != "Unknown AWS pricing model" {
		t.Fatalf("Basis = %q, want %q", got.Basis, "Unknown AWS pricing model")
	}
	if !strings.Contains(got.Notes, "No service mapping is implemented yet") {
		t.Fatalf("Notes = %q, want unknown-service explanation", got.Notes)
	}
}

func TestEstimateGenericAWSResource_NonAWSResource(t *testing.T) {
	pricer := &Pricer{}
	resource := terraform.Resource{
		Type: "random_pet",
		Name: "name",
	}

	got := pricer.estimateGenericAWSResource(resource)

	if got.Basis != "Non-AWS resource" {
		t.Fatalf("Basis = %q, want %q", got.Basis, "Non-AWS resource")
	}
	if !strings.Contains(got.Notes, "only handles Terraform resources backed by AWS") {
		t.Fatalf("Notes = %q, want non-AWS explanation", got.Notes)
	}
}

func TestEstimateFallsBackToGenericForUnknownResourceTypes(t *testing.T) {
	pricer := &Pricer{}
	analysis := &terraform.Analysis{
		Resources: []terraform.Resource{
			{Type: "aws_cloudfront_distribution", Name: "cdn"},
			{Type: "aws_security_group", Name: "web"},
			{Type: "aws_foo_bar", Name: "example"},
			{Type: "random_pet", Name: "name"},
		},
	}

	got, err := pricer.Estimate(context.Background(), analysis)
	if err != nil {
		t.Fatalf("Estimate() error = %v", err)
	}

	if len(got) != 4 {
		t.Fatalf("len(Estimate()) = %d, want 4", len(got))
	}

	wantBases := []string{
		"Best-effort classification only",
		"Control-plane resource",
		"Unknown AWS pricing model",
		"Non-AWS resource",
	}
	for i, want := range wantBases {
		if got[i].Basis != want {
			t.Fatalf("Estimate()[%d].Basis = %q, want %q", i, got[i].Basis, want)
		}
	}
}
