# Terraform PR Cost Service

This repository contains a RESTful Go service that accepts GitHub webhooks, downloads the source branch of a PR using GitHub App authentication, parses Terraform files, detects Terraform S3 remote state, optionally loads deployed state from S3, estimates AWS pricing, and can publish both PR comments and GitHub Checks.

## What it does

1. Verifies the GitHub webhook signature.
2. Accepts `pull_request` events for `opened`, `reopened`, `edited`, and `synchronize`.
3. Downloads the PR head branch archive from GitHub with a GitHub App installation token.
4. Parses `*.tf` files to identify:
   - `terraform { backend "s3" { ... } }`
   - Terraform `resource` blocks
   - simple variable defaults and `locals`
   - simple `count` and `for_each` expansion
5. Parses `.tfvars` files and produces environment-specific resource expansions when counts vary by environment.
6. Loads remote state from S3 when the backend contains a concrete bucket and key.
7. Uses the AWS Pricing API to estimate monthly cost for a supported subset of resources.
8. Can post the markdown report back to the PR as a comment using the same GitHub App installation token flow.
9. Can return the same analysis as JSON for callers that want structured output.
10. Can respond to PR comments like `cloudspend -w dev`, inspect Terraform state for the selected workspace, query AWS Cost Explorer, and publish a detailed GitHub Check on the PR head commit.
11. Reuses the same GitHub Check for a given commit and workspace so repeated `cloudspend` calls update the existing check instead of creating duplicates.

## Current pricing coverage

The service now uses a mix of exact and assumption-based estimators:

- More exact pricing rules:
  - `aws_instance`
  - `aws_ebs_volume`
  - `aws_db_instance`
  - `aws_rds_cluster_instance`
- Assumption-based estimators:
  - `aws_dynamodb_table`
  - `aws_lambda_function`
  - `aws_s3_bucket`
  - `aws_sqs_queue`
  - `aws_sns_topic`
  - `aws_ecr_repository`
- Expanded compute, network, and database approximations:
  - `aws_ecs_service` for Fargate services when task definition CPU and memory can be resolved
  - `aws_eks_cluster`
  - `aws_eks_node_group`
  - `aws_nat_gateway`
  - `aws_vpc_endpoint`
  - `aws_lb*` / `aws_alb*` / `aws_elb*`
  - `aws_elasticache_cluster`
  - `aws_elasticache_replication_group`
  - `aws_opensearch_domain`
  - `aws_elasticsearch_domain`
  - `aws_redshift_cluster`
  - `aws_docdb_cluster_instance`
  - `aws_memorydb_cluster`
- Control-plane or classification-only handling remains for some resources that need workload context before a useful estimate can be produced, such as:
  - `aws_ecs_cluster`
  - `aws_ecs_task_definition`
  - `aws_eks_fargate_profile`
  - many less common `aws_*` resources outside the families above
- Remaining `aws_*` resources are still included in the markdown report and classified as either:
  - likely control-plane resources with no direct recurring charge, or
  - resources that need service-specific assumptions before a reliable estimate can be produced

This means every AWS Terraform resource is surfaced in the report, and many common compute, network, and data services now receive a first-pass monthly approximation. Exact pricing is still not guaranteed because Terraform alone usually does not encode runtime traffic, storage growth, or request volume.

## Cloudspend coverage

The `cloudspend` workflow is state-driven rather than tfvars-driven:

- `cloudspend -w <workspace>` loads the matching remote Terraform workspace state from the S3 backend.
- Resource IDs, ARNs, and `Name` / `Project` tags are extracted from deployed state.
- Resource-level spend from Cost Explorer is currently only supported for EC2 resource IDs.
- Additional spend rollups are produced from Cost Explorer by:
  - `Project` tag totals
  - `Name` tag grouped by AWS `SERVICE`

The resulting GitHub Check includes:

- a compact summary section
- a richer full markdown body
- project totals
- name-tag/service rollups
- a detailed resource table including Terraform resource type, resource ID, ARN, `Name`, and `Project`

## Environment-aware expansion

If the repository contains `.tfvars` files, the service now creates an environment breakdown per tfvars file and expands simple `count` and `for_each` expressions using:

- Terraform variable defaults from `variable` blocks
- tfvars overrides from each `.tfvars` file
- simple `locals` that can be resolved from those values

This is intended to catch cases where, for example, `env/dev.tfvars` creates two resources while `env/prod.tfvars` creates one.

## Environment variables

- `GITHUB_APP_ID`: GitHub App ID.
- `GITHUB_APP_INSTALLATION_ID`: Installation ID for the GitHub App in the target repository or organization.
- `GITHUB_APP_PRIVATE_KEY_PEM`: PEM-encoded GitHub App private key. Literal `\n` sequences are accepted and normalized.
- `GITHUB_WEBHOOK_SECRET`: Secret used to validate `X-Hub-Signature-256`.
- `AWS_REGION`: AWS SDK region for the Pricing client. Defaults to `us-east-1`.
- `LISTEN_ADDR`: HTTP listen address. Defaults to `:8080`.
- `WORKSPACE_PARENT`: Optional temp workspace parent directory. Defaults to the OS temp dir.

The service also requires standard AWS credentials in its runtime environment so it can call:

- AWS Pricing
- S3 for Terraform remote state
- AWS Cost Explorer for `cloudspend`

## Run

```bash
go mod tidy
go test ./...
go run ./cmd/server
```

## Docker

Build the container:

```bash
docker build -t terraform-pr-cost-service .
```

Run it locally:

```bash
docker run --rm -p 8080:8080 \
  -e GITHUB_APP_ID=123456 \
  -e GITHUB_APP_INSTALLATION_ID=98765432 \
  -e GITHUB_APP_PRIVATE_KEY_PEM="$(cat app-private-key.pem)" \
  -e GITHUB_WEBHOOK_SECRET=topsecret \
  -e AWS_REGION=us-east-1 \
  -e AWS_ACCESS_KEY_ID=... \
  -e AWS_SECRET_ACCESS_KEY=... \
  -e AWS_SESSION_TOKEN=... \
  terraform-pr-cost-service
```

If your private key is multiline, passing it through an env file or your orchestrator's secret manager is usually more reliable than inline shell expansion.

## Webhook setup

Configure GitHub webhooks for:

- `pull_request` events to:

```text
POST /webhooks/github/pricing-estimate/comment
POST /webhooks/github/pricing-estimate/json
```

- `/webhooks/github/pricing-estimate/comment`: analyzes the PR, renders markdown, and posts that markdown as a PR comment through the GitHub Issues Comments API. The HTTP response is JSON and includes both the created comment metadata and the structured report.
- `/webhooks/github/pricing-estimate/json`: analyzes the PR and returns the structured report as JSON, including the rendered markdown.

- `issue_comment` events to:

```text
POST /webhooks/github/cloudspend/comment
```

- `/webhooks/github/cloudspend/comment`: accepts GitHub `issue_comment` webhooks for pull requests. When the comment body matches `cloudspend -w <workspace>`, the service loads the PR head revision, loads the remote Terraform workspace state, queries AWS Cost Explorer for supported resource-level and tag-based spend, and publishes or updates a GitHub Check on the PR head commit.

For the `cloudspend` workflow, the GitHub App should subscribe to the `issue_comment` webhook and have `checks:write`, `pull_requests:read`, and `contents:read` permissions in addition to the permissions already needed for archive download and PR comments.

A sample triggering PR comment looks like:

```text
cloudspend -w development
```

## Notes and limitations

- The implementation uses static Terraform parsing, so values built from variables, locals, modules, or expressions may not resolve to concrete pricing inputs.
- Nested Terraform blocks such as `scaling_config` and `cluster_config` are now captured for estimation, but module outputs and provider-computed values are still best-effort only.
- `count` / `for_each` expansion is best-effort and currently targets simple expressions resolvable from variable defaults, `.tfvars`, and simple locals. It does not fully evaluate arbitrary module graphs or full Terraform plan semantics.
- Terraform plans are not executed; the service estimates based on source configuration only.
- Repositories with no Terraform files are treated as a successful no-op analysis. The report will explicitly say that no Terraform resources were detected instead of failing the webhook.
- AWS Pricing API filters are exact-match and occasionally brittle across services and regions, so some supported resources may still come back as unsupported when AWS catalog metadata differs.
- Many of the newer family estimators are intentionally assumption-based. For example, NAT gateways, interface endpoints, load balancers, ECS Fargate services, and several managed data stores require traffic, concurrency, or storage assumptions that Terraform does not fully specify.
- The `cloudspend` endpoint uses AWS Cost Explorer resource-level data. Per-resource spend is only available when Cost Explorer resource-level data is enabled, and AWS documents `RESOURCE_ID` support as an opt-in feature limited to the last 14 days for EC2-Compute usage.
- Non-EC2 resources are still extracted from Terraform state and listed in the GitHub Check, but are marked unsupported for per-resource spend.
- Cost Explorer can group by tags and dimensions such as `SERVICE`, but not by arbitrary Terraform resource type. The report therefore combines:
  - Cost Explorer data for `Project` tag totals
  - Cost Explorer data for `Name` tag grouped by AWS service
  - Terraform state metadata for resource type, resource ID, ARN, and tags
