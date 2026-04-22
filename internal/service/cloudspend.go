package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ashtwentyfour/super-eureka/internal/terraform"
)

type ResourceSpend struct {
	TerraformAddress string
	ResourceType     string
	ResourceID       string
	ARN              string
	NameTag          string
	ProjectTag       string
	AmountUSD        float64
	Currency         string
	Supported        bool
	Notes            string
}

type SpendReport struct {
	Workspace         string
	GeneratedAt       time.Time
	StartDate         string
	EndDate           string
	ResourceSpends    []ResourceSpend
	NameTagSpend      []NameTagSpend
	ProjectSpend      []ProjectSpend
	TotalUSD          float64
	SelectionNotes    []string
	AvailabilityNotes []string
}

type ResourceSpendLoader interface {
	LoadResourceSpend(ctx context.Context, resourceIDs []string, start, end time.Time) (map[string]ResourceSpendAmount, error)
	LoadSpendByNameTag(ctx context.Context, nameTags []string, start, end time.Time) ([]NameTagSpend, error)
	LoadSpendByProjectTag(ctx context.Context, projectTags []string, start, end time.Time) ([]ProjectSpend, error)
}

type ResourceSpendAmount struct {
	AmountUSD float64
	Currency  string
}

type NameTagSpend struct {
	NameTag   string
	Service   string
	AmountUSD float64
	Currency  string
}

type ProjectSpend struct {
	ProjectTag string
	AmountUSD  float64
	Currency   string
}

type CloudSpendService struct {
	loader      ResourceSpendLoader
	stateLoader terraform.StateLoader
	now         func() time.Time
}

func NewCloudSpendService(loader ResourceSpendLoader, stateLoader terraform.StateLoader) *CloudSpendService {
	return &CloudSpendService{
		loader:      loader,
		stateLoader: stateLoader,
		now:         func() time.Time { return time.Now().UTC() },
	}
}

func (s *CloudSpendService) BuildReport(ctx context.Context, analysis *terraform.Analysis, workspace string) (SpendReport, error) {
	report := SpendReport{
		Workspace:   workspace,
		GeneratedAt: s.now(),
	}
	if analysis == nil {
		report.SelectionNotes = append(report.SelectionNotes, "No Terraform analysis was available.")
		return report, nil
	}
	if analysis.State == nil {
		if analysis.Backend != nil && s.stateLoader != nil {
			state, err := s.stateLoader.LoadForWorkspace(ctx, *analysis.Backend, workspace)
			if err == nil {
				analysis = cloneAnalysisWithState(analysis, state)
			} else {
				report.SelectionNotes = append(report.SelectionNotes, fmt.Sprintf("Remote Terraform state for workspace %q could not be loaded from `s3://%s/%s`: %s", workspace, analysis.Backend.Bucket, workspaceStateKey(*analysis.Backend, workspace), err))
			}
		}
	}
	if analysis.State == nil {
		report.SelectionNotes = append(report.SelectionNotes, "No remote Terraform state was loaded, so resource-level spend could not be derived.")
		return report, nil
	}

	selected, notes, err := selectStateResourcesForWorkspace(analysis, workspace)
	if err != nil {
		return report, err
	}
	report.SelectionNotes = append(report.SelectionNotes, notes...)

	end := s.now().UTC()
	start := end.AddDate(0, 0, -14)
	report.StartDate = start.Format("2006-01-02")
	report.EndDate = end.Format("2006-01-02")

	descriptors := describeStateResources(selected)
	ec2IDs := make([]string, 0, len(descriptors))
	resourceByID := map[string]int{}
	nameTags := map[string]bool{}
	projectTags := map[string]bool{}
	for i := range descriptors {
		if descriptors[i].Supported {
			resourceByID[descriptors[i].ResourceID] = i
			ec2IDs = append(ec2IDs, descriptors[i].ResourceID)
		}
		if descriptors[i].NameTag != "" {
			nameTags[descriptors[i].NameTag] = true
		}
		if descriptors[i].ProjectTag != "" {
			projectTags[descriptors[i].ProjectTag] = true
		}
	}

	if len(ec2IDs) > 0 {
		spendByID, err := s.loader.LoadResourceSpend(ctx, ec2IDs, start, end)
		if err != nil {
			return report, fmt.Errorf("load AWS Cost Explorer spend: %w", err)
		}
		for id, spend := range spendByID {
			index, ok := resourceByID[id]
			if !ok {
				continue
			}
			descriptors[index].AmountUSD = spend.AmountUSD
			descriptors[index].Currency = spend.Currency
		}
	}

	nameTagList := sortedKeys(nameTags)
	if len(nameTagList) > 0 {
		rollups, err := s.loader.LoadSpendByNameTag(ctx, nameTagList, start, end)
		if err != nil {
			return report, fmt.Errorf("load AWS Cost Explorer name-tag spend: %w", err)
		}
		sort.Slice(rollups, func(i, j int) bool {
			if rollups[i].AmountUSD == rollups[j].AmountUSD {
				if rollups[i].NameTag == rollups[j].NameTag {
					return rollups[i].Service < rollups[j].Service
				}
				return rollups[i].NameTag < rollups[j].NameTag
			}
			return rollups[i].AmountUSD > rollups[j].AmountUSD
		})
		report.NameTagSpend = rollups
	}

	projectTagList := sortedKeys(projectTags)
	if len(projectTagList) > 0 {
		projectRollups, err := s.loader.LoadSpendByProjectTag(ctx, projectTagList, start, end)
		if err != nil {
			return report, fmt.Errorf("load AWS Cost Explorer project-tag spend: %w", err)
		}
		sort.Slice(projectRollups, func(i, j int) bool {
			if projectRollups[i].AmountUSD == projectRollups[j].AmountUSD {
				return projectRollups[i].ProjectTag < projectRollups[j].ProjectTag
			}
			return projectRollups[i].AmountUSD > projectRollups[j].AmountUSD
		})
		report.ProjectSpend = projectRollups
	}

	report.AvailabilityNotes = append(report.AvailabilityNotes,
		"AWS Cost Explorer resource-level ResourceId data is only available for Amazon EC2 - Compute and only for the last 14 days when resource-level data is enabled in Cost Explorer.",
		"Non-EC2 resources extracted from Terraform state are included in the report, but their per-resource spend is marked unsupported because Cost Explorer does not expose comparable ResourceId spend for them.",
		"Cost Explorer can group by tags and service, but not by arbitrary Terraform resource type. This report therefore shows Name-tag spend grouped by AWS service from Cost Explorer, and separately shows Terraform resource type plus Name tag from state metadata.",
	)

	sort.Slice(descriptors, func(i, j int) bool {
		if descriptors[i].AmountUSD == descriptors[j].AmountUSD {
			return descriptors[i].TerraformAddress < descriptors[j].TerraformAddress
		}
		return descriptors[i].AmountUSD > descriptors[j].AmountUSD
	})

	for _, item := range descriptors {
		report.TotalUSD += item.AmountUSD
	}
	report.ResourceSpends = descriptors
	return report, nil
}

func cloneAnalysisWithState(analysis *terraform.Analysis, state *terraform.StateSummary) *terraform.Analysis {
	if analysis == nil {
		return nil
	}
	cloned := *analysis
	cloned.State = state
	return &cloned
}

func workspaceStateKey(backend terraform.S3Backend, workspace string) string {
	if workspace == "" || workspace == "default" {
		return backend.Key
	}
	prefix := backend.WorkspaceKeyPrefix
	if prefix == "" {
		prefix = "env:"
	}
	return fmt.Sprintf("%s/%s/%s", prefix, workspace, backend.Key)
}

func (r SpendReport) Markdown() string {
	var b strings.Builder

	supportedCount := 0
	unsupportedCount := 0
	for _, item := range r.ResourceSpends {
		if item.Supported {
			supportedCount++
		} else {
			unsupportedCount++
		}
	}

	fmt.Fprintf(&b, "# Cloud Spend\n\n")
	fmt.Fprintf(&b, "> Resource-level spend snapshot derived from remote Terraform state and AWS Cost Explorer.\n\n")
	if r.Workspace != "" {
		fmt.Fprintf(&b, "- Workspace: `%s`\n", sanitizeInline(r.Workspace))
	}
	if r.StartDate != "" && r.EndDate != "" {
		fmt.Fprintf(&b, "- Time window: `%s` to `%s`\n", r.StartDate, r.EndDate)
	}
	fmt.Fprintf(&b, "- Generated at: `%s`\n", r.GeneratedAt.Format(time.RFC3339))
	if len(r.ResourceSpends) > 0 {
		fmt.Fprintf(&b, "- Captured resources: `%d`\n", len(r.ResourceSpends))
		fmt.Fprintf(&b, "- Resource-level spend support: `%d supported` / `%d unsupported`\n", supportedCount, unsupportedCount)
	}

	if len(r.ResourceSpends) > 0 {
		fmt.Fprintf(&b, "\n## Highlights\n\n")
		fmt.Fprintf(&b, "- Total resource-level spend captured for supported resources: `%.2f USD`\n", r.TotalUSD)
		if top := richestResource(r.ResourceSpends); top != nil {
			fmt.Fprintf(&b, "- Highest captured resource spend: `%s` at `%.2f USD`\n", sanitizeInline(top.TerraformAddress), top.AmountUSD)
		}
		if len(r.ProjectSpend) > 0 {
			for _, item := range r.ProjectSpend {
				fmt.Fprintf(&b, "- Project `%s` total: `%.2f USD`\n", sanitizeInline(item.ProjectTag), item.AmountUSD)
			}
		}
	}

	if len(r.SelectionNotes) > 0 {
		fmt.Fprintf(&b, "\n## Scope\n\n")
		for _, note := range r.SelectionNotes {
			fmt.Fprintf(&b, "- %s\n", sanitizeInline(note))
		}
	}

	if len(r.ResourceSpends) == 0 {
		fmt.Fprintf(&b, "\nNo state-backed AWS resources were selected for this workspace.\n")
		return b.String()
	}

	if len(r.ProjectSpend) > 0 {
		fmt.Fprintf(&b, "\n## Project Spend\n\n")
		fmt.Fprintf(&b, "| Project Tag | 14d Spend (USD) |\n")
		fmt.Fprintf(&b, "| --- | ---: |\n")
		for _, item := range r.ProjectSpend {
			fmt.Fprintf(&b, "| `%s` | `%.2f` |\n", sanitizeInline(item.ProjectTag), item.AmountUSD)
		}
	}

	if len(r.NameTagSpend) > 0 {
		fmt.Fprintf(&b, "\n## Name Tag Spend\n\n")
		fmt.Fprintf(&b, "| Name Tag | AWS Service | 14d Spend (USD) |\n")
		fmt.Fprintf(&b, "| --- | --- | ---: |\n")
		for _, item := range r.NameTagSpend {
			fmt.Fprintf(&b, "| `%s` | `%s` | `%.2f` |\n", sanitizeInline(item.NameTag), sanitizeInline(item.Service), item.AmountUSD)
		}
	}

	fmt.Fprintf(&b, "\n## Resource Spend\n\n")
	fmt.Fprintf(&b, "| Terraform Resource | AWS Type | Name Tag | Project Tag | Resource ID | ARN | 14d Spend (USD) | Coverage | Notes |\n")
	fmt.Fprintf(&b, "| --- | --- | --- | --- | --- | --- | ---: | --- | --- |\n")
	for _, item := range r.ResourceSpends {
		spend := "`n/a`"
		coverage := "`state-only`"
		if item.Supported {
			spend = fmt.Sprintf("`%.2f`", item.AmountUSD)
			coverage = "`cost-explorer`"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | `%s` | `%s` | `%s` | %s | %s | %s |\n",
			sanitizeInline(item.TerraformAddress),
			sanitizeInline(item.ResourceType),
			sanitizeInline(item.NameTag),
			sanitizeInline(item.ProjectTag),
			sanitizeInline(item.ResourceID),
			sanitizeInline(item.ARN),
			spend,
			coverage,
			sanitizeCell(item.Notes),
		)
	}

	fmt.Fprintf(&b, "\n**Total resource-level spend captured:** `%.2f USD`\n", r.TotalUSD)

	if len(r.AvailabilityNotes) > 0 {
		fmt.Fprintf(&b, "\n## Notes\n\n")
		for _, note := range r.AvailabilityNotes {
			fmt.Fprintf(&b, "- %s\n", sanitizeInline(note))
		}
	}

	return b.String()
}

func (r SpendReport) SummaryMarkdown() string {
	var b strings.Builder

	fmt.Fprintf(&b, "Workspace `%s` spend snapshot for `%s` to `%s`.\n\n", sanitizeInline(r.Workspace), r.StartDate, r.EndDate)
	fmt.Fprintf(&b, "- Resource-level EC2 spend captured: `%.2f USD`\n", r.TotalUSD)
	fmt.Fprintf(&b, "- Total resources in workspace state: `%d`\n", len(r.ResourceSpends))

	supportedCount := 0
	for _, item := range r.ResourceSpends {
		if item.Supported {
			supportedCount++
		}
	}
	fmt.Fprintf(&b, "- Cost Explorer resource-level coverage: `%d/%d`\n", supportedCount, len(r.ResourceSpends))

	if top := richestResource(r.ResourceSpends); top != nil {
		fmt.Fprintf(&b, "- Highest EC2 resource spend: `%s` at `%.2f USD`\n", sanitizeInline(top.TerraformAddress), top.AmountUSD)
	}

	if len(r.ProjectSpend) > 0 {
		fmt.Fprintf(&b, "- Project-tag totals:\n")
		for _, item := range r.ProjectSpend {
			fmt.Fprintf(&b, "  - `%s`: `%.2f USD`\n", sanitizeInline(item.ProjectTag), item.AmountUSD)
		}
	}

	if len(r.NameTagSpend) > 0 {
		top := r.NameTagSpend[0]
		fmt.Fprintf(&b, "- Largest Name-tag/service rollup: `%s` on `%s` at `%.2f USD`\n", sanitizeInline(top.NameTag), sanitizeInline(top.Service), top.AmountUSD)
	}

	return b.String()
}

func richestResource(resources []ResourceSpend) *ResourceSpend {
	var best *ResourceSpend
	for i := range resources {
		if !resources[i].Supported {
			continue
		}
		if best == nil || resources[i].AmountUSD > best.AmountUSD {
			best = &resources[i]
		}
	}
	return best
}

func selectStateResourcesForWorkspace(analysis *terraform.Analysis, workspace string) ([]terraform.StateResource, []string, error) {
	if analysis.State == nil {
		return nil, nil, nil
	}
	if workspace == "" {
		return analysis.State.Resources, []string{"No workspace was specified; all resources in the loaded Terraform state were considered."}, nil
	}
	return analysis.State.Resources, []string{
		fmt.Sprintf("Workspace %q was resolved from the remote Terraform workspace state, so all %d state resources in that workspace were considered.", workspace, len(analysis.State.Resources)),
	}, nil
}

func describeStateResources(resources []terraform.StateResource) []ResourceSpend {
	var items []ResourceSpend
	for _, resource := range resources {
		address := rootStateResourceAddress(resource)
		for _, instance := range resource.Instances {
			id, arn := extractResourceIdentity(resource.Type, instance.Attributes)
			nameTag := extractTag(instance.Attributes, "Name")
			projectTag := extractTag(instance.Attributes, "Project")
			item := ResourceSpend{
				TerraformAddress: address,
				ResourceType:     resource.Type,
				ResourceID:       id,
				ARN:              arn,
				NameTag:          nameTag,
				ProjectTag:       projectTag,
				Currency:         "USD",
			}

			if resource.Type == "aws_instance" && strings.HasPrefix(id, "i-") {
				item.Supported = true
				item.Notes = "Queried Cost Explorer ResourceId spend for EC2 compute."
			} else {
				item.Notes = unsupportedSpendNote(resource.Type, id, arn)
			}

			items = append(items, item)
		}
	}
	return items
}

func rootResourceAddress(address string) string {
	if i := strings.Index(address, "["); i >= 0 {
		return address[:i]
	}
	return address
}

func rootStateResourceAddress(resource terraform.StateResource) string {
	address := resource.Type + "." + resource.Name
	if resource.Module != "" {
		address = resource.Module + "." + address
	}
	return rootResourceAddress(address)
}

func extractResourceIdentity(resourceType string, attrs map[string]any) (string, string) {
	arn := firstStringValue(attrs, "arn")

	switch resourceType {
	case "aws_instance":
		return firstStringValue(attrs, "id", "instance_id"), arn
	case "aws_ebs_volume":
		return firstStringValue(attrs, "id", "volume_id"), arn
	case "aws_db_instance":
		return firstStringValue(attrs, "resource_id", "id", "identifier"), arn
	case "aws_rds_cluster":
		return firstStringValue(attrs, "cluster_identifier", "id"), arn
	case "aws_dynamodb_table":
		return firstStringValue(attrs, "name", "id"), arn
	case "aws_s3_bucket":
		return firstStringValue(attrs, "bucket", "id"), arn
	case "aws_sqs_queue":
		return firstStringValue(attrs, "id", "name"), arn
	case "aws_sns_topic":
		return firstStringValue(attrs, "arn", "name"), arn
	case "aws_lambda_function":
		return firstStringValue(attrs, "function_name", "arn", "id"), arn
	case "aws_ecs_service":
		return firstStringValue(attrs, "id", "name"), arn
	case "aws_eks_cluster":
		return firstStringValue(attrs, "id", "name"), arn
	default:
		return firstStringValue(attrs, "id", "name"), arn
	}
}

func unsupportedSpendNote(resourceType, id, arn string) string {
	target := firstNonEmpty(id, arn)
	if target == "" {
		return fmt.Sprintf("No stable resource identifier could be extracted from Terraform state for %s.", resourceType)
	}
	return fmt.Sprintf("Terraform state exposed %s, but Cost Explorer ResourceId spend is only available for EC2-Compute resources.", target)
}

func firstStringValue(attrs map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := attrs[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func extractTag(attrs map[string]any, key string) string {
	for _, tagField := range []string{"tags_all", "tags"} {
		raw, ok := attrs[tagField]
		if !ok {
			continue
		}
		tagMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if value, ok := tagMap[key]; ok {
			if s, ok := value.(string); ok {
				return s
			}
		}
	}
	return ""
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
