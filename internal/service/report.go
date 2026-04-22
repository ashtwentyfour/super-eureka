package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ashtwentyfour/super-eureka/internal/terraform"
)

type Report struct {
	GeneratedAt  time.Time
	Request      AnalysisRequest
	Workspace    string
	Analysis     *terraform.Analysis
	Costs        []terraform.EstimatedCost
	Environments []EnvironmentReport
}

type EnvironmentReport struct {
	Name       string
	TFVarsFile string
	Resources  []terraform.Resource
	Costs      []terraform.EstimatedCost
}

func (r Report) Markdown() string {
	return RenderMarkdownReport(r)
}

func RenderMarkdownReport(report Report) string {
	var b strings.Builder
	aggregateResources, aggregateCosts, aggregatedByEnv := aggregateEnvironmentViews(report)

	fmt.Fprintf(&b, "# Terraform Cost Estimation\n\n")
	fmt.Fprintf(&b, "- PR: [#%d %s](%s)\n", report.Request.PullRequestNumber, sanitizeInline(report.Request.PullRequestTitle), report.Request.PullRequestURL)
	fmt.Fprintf(&b, "- Source branch: `%s`\n", report.Request.HeadRef)
	fmt.Fprintf(&b, "- Source commit: `%s`\n", report.Request.HeadSHA)
	fmt.Fprintf(&b, "- Generated at: `%s`\n", report.GeneratedAt.Format(time.RFC3339))

	if report.Analysis.Backend != nil {
		fmt.Fprintf(&b, "- Remote state: `s3://%s/%s` (`region=%s`)\n", report.Analysis.Backend.Bucket, report.Analysis.Backend.Key, report.Analysis.Backend.Region)
	} else {
		fmt.Fprintf(&b, "- Remote state: not detected\n")
	}
	if len(report.Environments) > 0 {
		fmt.Fprintf(&b, "- Environment tfvars detected: `%d`\n", len(report.Environments))
	}

	fmt.Fprintf(&b, "\n## Resources\n\n")
	if len(aggregateResources) == 0 {
		fmt.Fprintf(&b, "No Terraform resources were detected.\n")
	} else {
		sort.Slice(aggregateResources, func(i, j int) bool {
			if aggregateResources[i].Environment == aggregateResources[j].Environment {
				if aggregateResources[i].Type == aggregateResources[j].Type {
					return aggregateResources[i].Address() < aggregateResources[j].Address()
				}
				return aggregateResources[i].Type < aggregateResources[j].Type
			}
			return aggregateResources[i].Environment < aggregateResources[j].Environment
		})

		if aggregatedByEnv {
			fmt.Fprintf(&b, "| Environment | Address | Type | File |\n")
			fmt.Fprintf(&b, "| --- | --- | --- | --- |\n")
			for _, resource := range aggregateResources {
				fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | `%s` |\n", sanitizeInline(resource.Environment), resource.Address(), resource.Type, resource.File)
			}
		} else {
			fmt.Fprintf(&b, "| Address | Type | File |\n")
			fmt.Fprintf(&b, "| --- | --- | --- |\n")
			for _, resource := range aggregateResources {
				fmt.Fprintf(&b, "| `%s` | `%s` | `%s` |\n", resource.Address(), resource.Type, resource.File)
			}
		}
	}

	fmt.Fprintf(&b, "\n## Cost Breakdown\n\n")
	if len(aggregateCosts) == 0 {
		fmt.Fprintf(&b, "No cost estimates could be produced.\n")
		return b.String()
	}

	if aggregatedByEnv {
		environmentSummaries := summarizeEnvironmentCosts(report.Environments)
		fmt.Fprintf(&b, "| Environment | Resources | Monthly Estimate (USD) |\n")
		fmt.Fprintf(&b, "| --- | ---: | ---: |\n")
		for _, item := range environmentSummaries {
			fmt.Fprintf(&b, "| `%s` | `%d` | `%.2f` |\n", sanitizeInline(item.Name), item.ResourceCount, item.TotalUSD)
		}
	} else {
		fmt.Fprintf(&b, "| Resource | Monthly Estimate (USD) | Basis | Assumptions | Notes |\n")
		fmt.Fprintf(&b, "| --- | ---: | --- | --- | --- |\n")
		for _, item := range aggregateCosts {
			fmt.Fprintf(&b, "| `%s` | `%.2f` | %s | %s | %s |\n", item.ResourceAddress, item.MonthlyUSD, sanitizeCell(item.Basis), sanitizeCell(strings.Join(item.Assumptions, "; ")), sanitizeCell(item.Notes))
		}
	}

	var total float64
	for _, item := range aggregateCosts {
		total += item.MonthlyUSD
	}

	fmt.Fprintf(&b, "\n**Estimated monthly total:** `%.2f USD`\n", total)

	if !aggregatedByEnv {
		unsupported := filterUnsupportedViews(aggregateCosts)
		if len(unsupported) > 0 {
			fmt.Fprintf(&b, "\n## Coverage Gaps\n\n")
			for _, item := range unsupported {
				fmt.Fprintf(&b, "- `%s`: %s\n", item.ResourceAddress, sanitizeInline(item.Notes))
			}
		}
	}

	if len(report.Environments) > 0 {
		sort.Slice(report.Environments, func(i, j int) bool { return report.Environments[i].TFVarsFile < report.Environments[j].TFVarsFile })
		fmt.Fprintf(&b, "\n## Environment Breakdown\n")
		for _, env := range report.Environments {
			renderEnvironmentReport(&b, env)
		}
	}

	return b.String()
}

type costView struct {
	Environment string
	terraform.EstimatedCost
}

type environmentCostSummary struct {
	Name          string
	ResourceCount int
	TotalUSD      float64
}

func aggregateEnvironmentViews(report Report) ([]terraform.Resource, []costView, bool) {
	if len(report.Environments) == 0 {
		costs := make([]costView, 0, len(report.Costs))
		for _, item := range report.Costs {
			costs = append(costs, costView{EstimatedCost: item})
		}
		return report.Analysis.Resources, costs, false
	}

	var resources []terraform.Resource
	var costs []costView
	for _, env := range report.Environments {
		for _, resource := range env.Resources {
			resource.Environment = env.Name
			resources = append(resources, resource)
		}
		for _, item := range env.Costs {
			costs = append(costs, costView{
				Environment:   env.Name,
				EstimatedCost: item,
			})
		}
	}
	return resources, costs, true
}

func summarizeEnvironmentCosts(environments []EnvironmentReport) []environmentCostSummary {
	summaries := make([]environmentCostSummary, 0, len(environments))
	for _, env := range environments {
		summaries = append(summaries, environmentCostSummary{
			Name:          env.Name,
			ResourceCount: len(env.Resources),
			TotalUSD:      sumCosts(env.Costs),
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries
}

func renderEnvironmentReport(b *strings.Builder, env EnvironmentReport) {
	fmt.Fprintf(b, "\n### `%s`\n\n", sanitizeInline(env.Name))
	fmt.Fprintf(b, "- tfvars: `%s`\n", sanitizeInline(env.TFVarsFile))
	fmt.Fprintf(b, "- resources: `%d`\n", len(env.Resources))
	fmt.Fprintf(b, "- estimated monthly total: `%.2f USD`\n\n", sumCosts(env.Costs))

	if len(env.Resources) > 0 {
		sort.Slice(env.Resources, func(i, j int) bool {
			if env.Resources[i].Type == env.Resources[j].Type {
				return env.Resources[i].Address() < env.Resources[j].Address()
			}
			return env.Resources[i].Type < env.Resources[j].Type
		})
		fmt.Fprintf(b, "| Address | Type | File |\n")
		fmt.Fprintf(b, "| --- | --- | --- |\n")
		for _, resource := range env.Resources {
			fmt.Fprintf(b, "| `%s` | `%s` | `%s` |\n", resource.Address(), resource.Type, resource.File)
		}
		fmt.Fprintf(b, "\n")
	}

	if len(env.Costs) == 0 {
		fmt.Fprintf(b, "No cost estimates could be produced for this environment.\n")
		return
	}

	fmt.Fprintf(b, "| Resource | Monthly Estimate (USD) | Basis | Assumptions | Notes |\n")
	fmt.Fprintf(b, "| --- | ---: | --- | --- | --- |\n")
	for _, item := range env.Costs {
		fmt.Fprintf(b, "| `%s` | `%.2f` | %s | %s | %s |\n", item.ResourceAddress, item.MonthlyUSD, sanitizeCell(item.Basis), sanitizeCell(strings.Join(item.Assumptions, "; ")), sanitizeCell(item.Notes))
	}
}

func filterUnsupported(costs []terraform.EstimatedCost) []terraform.EstimatedCost {
	var unsupported []terraform.EstimatedCost
	for _, item := range costs {
		if item.MonthlyUSD == 0 && strings.Contains(strings.ToLower(item.Notes), "unsupported") {
			unsupported = append(unsupported, item)
		}
	}
	return unsupported
}

func filterUnsupportedViews(costs []costView) []terraform.EstimatedCost {
	var raw []terraform.EstimatedCost
	for _, item := range costs {
		raw = append(raw, item.EstimatedCost)
	}
	return filterUnsupported(raw)
}

func sanitizeInline(v string) string {
	return strings.ReplaceAll(v, "\n", " ")
}

func sanitizeCell(v string) string {
	v = sanitizeInline(v)
	return strings.ReplaceAll(v, "|", "\\|")
}

func sumCosts(costs []terraform.EstimatedCost) float64 {
	var total float64
	for _, item := range costs {
		total += item.MonthlyUSD
	}
	return total
}
