package terraform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRepositoryExpandsPerEnvironmentTFVars(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "env"), 0o755); err != nil {
		t.Fatal(err)
	}

	mainTF := `
variable "table_names" {
  type = map(string)
  default = {}
}

variable "replica_count" {
  type = number
  default = 1
}

resource "aws_dynamodb_table" "tables" {
  for_each = var.table_names
  name     = each.value
  billing_mode = "PAY_PER_REQUEST"
  hash_key = "id"
}

resource "aws_dynamodb_table" "replicas" {
  count = var.replica_count
  name  = "replica-${count.index}"
  billing_mode = "PAY_PER_REQUEST"
  hash_key = "id"
}
`

	devTFVars := `
table_names = {
  first  = "dev-a"
  second = "dev-b"
}
replica_count = 2
`

	prodTFVars := `
table_names = {
  primary = "prod-a"
}
replica_count = 1
`

	writeFile(t, filepath.Join(root, "main.tf"), mainTF)
	writeFile(t, filepath.Join(root, "env", "dev.tfvars"), devTFVars)
	writeFile(t, filepath.Join(root, "env", "prod.tfvars"), prodTFVars)

	parser := NewParser(nil)
	analysis, err := parser.ParseRepository(context.Background(), root)
	if err != nil {
		t.Fatalf("ParseRepository() error = %v", err)
	}

	if len(analysis.Environments) != 2 {
		t.Fatalf("expected 2 environments, got %d", len(analysis.Environments))
	}

	envs := map[string]EnvironmentAnalysis{}
	for _, env := range analysis.Environments {
		envs[env.Name] = env
	}

	if got := len(envs["dev"].Resources); got != 4 {
		t.Fatalf("dev resources = %d, want 4", got)
	}
	if got := len(envs["prod"].Resources); got != 2 {
		t.Fatalf("prod resources = %d, want 2", got)
	}

	wantDevAddresses := map[string]bool{
		`aws_dynamodb_table.tables["first"]`:  true,
		`aws_dynamodb_table.tables["second"]`: true,
		`aws_dynamodb_table.replicas[0]`:      true,
		`aws_dynamodb_table.replicas[1]`:      true,
	}
	for _, resource := range envs["dev"].Resources {
		delete(wantDevAddresses, resource.Address())
	}
	if len(wantDevAddresses) != 0 {
		t.Fatalf("missing expanded dev resources: %#v", wantDevAddresses)
	}
}

func TestParseRepositoryExpandsLocalToSetForEach(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "env"), 0o755); err != nil {
		t.Fatal(err)
	}

	mainTF := `
variable "table_names" {
  type = list(string)
  default = ["demo"]
}

locals {
  unique_table_names = toset(var.table_names)
}

resource "aws_dynamodb_table" "tables" {
  for_each = local.unique_table_names
  name = each.value
  billing_mode = "PAY_PER_REQUEST"
  hash_key = "pk"
}
`

	devTFVars := `
table_names = ["demo", "customers"]
`

	writeFile(t, filepath.Join(root, "main.tf"), mainTF)
	writeFile(t, filepath.Join(root, "env", "dev.tfvars"), devTFVars)

	parser := NewParser(nil)
	analysis, err := parser.ParseRepository(context.Background(), root)
	if err != nil {
		t.Fatalf("ParseRepository() error = %v", err)
	}

	if len(analysis.Environments) != 1 {
		t.Fatalf("expected 1 environment, got %d", len(analysis.Environments))
	}

	env := analysis.Environments[0]
	if got := len(env.Resources); got != 2 {
		t.Fatalf("resources = %d, want 2", got)
	}

	wantAddresses := map[string]bool{
		`aws_dynamodb_table.tables["customers"]`: true,
		`aws_dynamodb_table.tables["demo"]`:      true,
	}
	for _, resource := range env.Resources {
		delete(wantAddresses, resource.Address())
	}
	if len(wantAddresses) != 0 {
		t.Fatalf("missing expanded local for_each resources: %#v", wantAddresses)
	}
}

func TestParseRepositoryEvaluatesLookupAndToNumberForCapacities(t *testing.T) {
	root := t.TempDir()

	mainTF := `
variable "capacity_by_env" {
  type = map(object({
    read  = string
    write = string
  }))

  default = {
    dev = {
      read  = "6"
      write = "4"
    }
  }
}

locals {
  selected_capacity = lookup(var.capacity_by_env, "dev", {
    read  = "5"
    write = "5"
  })
}

resource "aws_dynamodb_table" "table" {
  name           = "demo"
  billing_mode   = "PROVISIONED"
  hash_key       = "pk"
  read_capacity  = tonumber(local.selected_capacity.read)
  write_capacity = tonumber(local.selected_capacity.write)
}
`

	writeFile(t, filepath.Join(root, "main.tf"), mainTF)

	parser := NewParser(nil)
	analysis, err := parser.ParseRepository(context.Background(), root)
	if err != nil {
		t.Fatalf("ParseRepository() error = %v", err)
	}

	if got := len(analysis.Resources); got != 1 {
		t.Fatalf("resources = %d, want 1", got)
	}

	resource := analysis.Resources[0]
	if got := resource.Attributes["read_capacity"].Literal; got != float64(6) {
		t.Fatalf("read_capacity = %#v, want 6", got)
	}
	if got := resource.Attributes["write_capacity"].Literal; got != float64(4) {
		t.Fatalf("write_capacity = %#v, want 4", got)
	}
}

func TestParseRepositoryEvaluatesCompactLocalForEach(t *testing.T) {
	root := t.TempDir()

	mainTF := `
variable "queue_names" {
  type = list(string)
  default = ["orders", "", "payments"]
}

locals {
  queue_names        = compact(var.queue_names)
  unique_queue_names = toset(local.queue_names)
}

resource "aws_sqs_queue" "queues" {
  for_each = local.unique_queue_names
  name     = each.value
}
`

	writeFile(t, filepath.Join(root, "main.tf"), mainTF)

	parser := NewParser(nil)
	analysis, err := parser.ParseRepository(context.Background(), root)
	if err != nil {
		t.Fatalf("ParseRepository() error = %v", err)
	}

	wantAddresses := map[string]bool{
		`aws_sqs_queue.queues["orders"]`:   true,
		`aws_sqs_queue.queues["payments"]`: true,
	}

	if got := len(analysis.Resources); got != len(wantAddresses) {
		t.Fatalf("resources = %d, want %d", got, len(wantAddresses))
	}

	for _, resource := range analysis.Resources {
		delete(wantAddresses, resource.Address())
	}
	if len(wantAddresses) != 0 {
		t.Fatalf("missing expanded queue resources: %#v", wantAddresses)
	}
}

func TestParseRepositoryHandlesEmptyToSetForEach(t *testing.T) {
	root := t.TempDir()

	mainTF := `
variable "queue_names" {
  type = list(string)
  default = ["", ""]
}

locals {
  queue_names        = compact(var.queue_names)
  unique_queue_names = toset(local.queue_names)
}

resource "aws_sqs_queue" "queues" {
  for_each = local.unique_queue_names
  name     = each.value
}
`

	writeFile(t, filepath.Join(root, "main.tf"), mainTF)

	parser := NewParser(nil)
	analysis, err := parser.ParseRepository(context.Background(), root)
	if err != nil {
		t.Fatalf("ParseRepository() error = %v", err)
	}

	if got := len(analysis.Resources); got != 0 {
		t.Fatalf("resources = %d, want 0", got)
	}
}

func TestParseRepositoryReportsUnresolvedLocalErrors(t *testing.T) {
	root := t.TempDir()

	mainTF := `
locals {
  queue_names        = unsupported(var.queue_names)
  unique_queue_names = toset(local.queue_names)
}

resource "aws_sqs_queue" "queues" {
  for_each = local.unique_queue_names
  name     = each.value
}
`

	writeFile(t, filepath.Join(root, "main.tf"), mainTF)

	parser := NewParser(nil)
	_, err := parser.ParseRepository(context.Background(), root)
	if err == nil {
		t.Fatal("ParseRepository() error = nil, want unresolved local error")
	}

	for _, want := range []string{
		"could not resolve locals",
		"queue_names",
		"unsupported",
		"unique_queue_names",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ParseRepository() error = %q, want substring %q", err.Error(), want)
		}
	}
}

func TestParseRepositoryMaterializesNestedBlocks(t *testing.T) {
	root := t.TempDir()

	mainTF := `
resource "aws_eks_node_group" "workers" {
  cluster_name    = "demo"
  node_group_name = "workers"
  instance_types  = ["t3.medium"]

  scaling_config {
    desired_size = 2
    max_size     = 3
    min_size     = 1
  }
}
`

	writeFile(t, filepath.Join(root, "main.tf"), mainTF)

	parser := NewParser(nil)
	analysis, err := parser.ParseRepository(context.Background(), root)
	if err != nil {
		t.Fatalf("ParseRepository() error = %v", err)
	}

	if got := len(analysis.Resources); got != 1 {
		t.Fatalf("resources = %d, want 1", got)
	}

	scalingConfig, ok := analysis.Resources[0].Attributes["scaling_config"].Literal.(map[string]any)
	if !ok {
		t.Fatalf("scaling_config literal type = %T, want map[string]any", analysis.Resources[0].Attributes["scaling_config"].Literal)
	}
	if got := scalingConfig["desired_size"]; got != float64(2) {
		t.Fatalf("desired_size = %#v, want 2", got)
	}
}

func TestParseRepositoryDetectsWorkspaceKeyPrefixBackend(t *testing.T) {
	root := t.TempDir()

	mainTF := `
terraform {
  backend "s3" {
    bucket               = "tf-state"
    key                  = "app/terraform.tfstate"
    region               = "us-east-1"
    workspace_key_prefix = "env:"
  }
}
`

	writeFile(t, filepath.Join(root, "main.tf"), mainTF)

	parser := NewParser(nil)
	analysis, err := parser.ParseRepository(context.Background(), root)
	if err != nil {
		t.Fatalf("ParseRepository() error = %v", err)
	}
	if analysis.Backend == nil {
		t.Fatal("expected backend to be detected")
	}
	if got := analysis.Backend.WorkspaceKeyPrefix; got != "env:" {
		t.Fatalf("WorkspaceKeyPrefix = %q, want %q", got, "env:")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
