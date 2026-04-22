package cloudspend

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ashtwentyfour/super-eureka/internal/service"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	types "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

type Loader struct {
	client *costexplorer.Client
}

func New(ctx context.Context) (*Loader, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &Loader{client: costexplorer.NewFromConfig(cfg)}, nil
}

func (l *Loader) LoadResourceSpend(ctx context.Context, resourceIDs []string, start, end time.Time) (map[string]service.ResourceSpendAmount, error) {
	if len(resourceIDs) == 0 {
		return map[string]service.ResourceSpendAmount{}, nil
	}

	filter := &types.Expression{
		And: []types.Expression{
			{
				Dimensions: &types.DimensionValues{
					Key:    types.DimensionService,
					Values: []string{"Amazon Elastic Compute Cloud - Compute"},
				},
			},
			{
				Dimensions: &types.DimensionValues{
					Key:    types.DimensionResourceId,
					Values: resourceIDs,
				},
			},
		},
	}

	input := &costexplorer.GetCostAndUsageWithResourcesInput{
		TimePeriod: &types.DateInterval{
			Start: aws.String(start.Format("2006-01-02")),
			End:   aws.String(end.Format("2006-01-02")),
		},
		Granularity: types.GranularityDaily,
		Metrics:     []string{"UnblendedCost"},
		Filter:      filter,
		GroupBy: []types.GroupDefinition{
			{
				Type: types.GroupDefinitionTypeDimension,
				Key:  aws.String(string(types.DimensionResourceId)),
			},
		},
	}

	results := map[string]service.ResourceSpendAmount{}
	for {
		page, err := l.client.GetCostAndUsageWithResources(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("get cost and usage with resources: %w", err)
		}
		for _, period := range page.ResultsByTime {
			for _, group := range period.Groups {
				if len(group.Keys) == 0 {
					continue
				}
				metric, ok := group.Metrics["UnblendedCost"]
				if !ok || metric.Amount == nil {
					continue
				}
				amount, err := strconv.ParseFloat(*metric.Amount, 64)
				if err != nil {
					continue
				}
				resourceID := group.Keys[0]
				current := results[resourceID]
				current.AmountUSD += amount
				if metric.Unit != nil && *metric.Unit != "" {
					current.Currency = *metric.Unit
				} else if current.Currency == "" {
					current.Currency = "USD"
				}
				results[resourceID] = current
			}
		}
		if page.NextPageToken == nil || *page.NextPageToken == "" {
			break
		}
		input.NextPageToken = page.NextPageToken
	}

	return results, nil
}

func (l *Loader) LoadSpendByNameTag(ctx context.Context, nameTags []string, start, end time.Time) ([]service.NameTagSpend, error) {
	if len(nameTags) == 0 {
		return nil, nil
	}

	input := &costexplorer.GetCostAndUsageInput{
		TimePeriod: &types.DateInterval{
			Start: aws.String(start.Format("2006-01-02")),
			End:   aws.String(end.Format("2006-01-02")),
		},
		Granularity: types.GranularityDaily,
		Metrics:     []string{"UnblendedCost"},
		Filter: &types.Expression{
			Tags: &types.TagValues{
				Key:    aws.String("Name"),
				Values: nameTags,
			},
		},
		GroupBy: []types.GroupDefinition{
			{Type: types.GroupDefinitionTypeTag, Key: aws.String("Name")},
			{Type: types.GroupDefinitionTypeDimension, Key: aws.String(string(types.DimensionService))},
		},
	}

	var results []service.NameTagSpend
	for {
		page, err := l.client.GetCostAndUsage(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("get cost and usage by Name tag: %w", err)
		}
		for _, period := range page.ResultsByTime {
			for _, group := range period.Groups {
				if len(group.Keys) < 2 {
					continue
				}
				metric, ok := group.Metrics["UnblendedCost"]
				if !ok || metric.Amount == nil {
					continue
				}
				amount, err := strconv.ParseFloat(*metric.Amount, 64)
				if err != nil {
					continue
				}
				results = append(results, service.NameTagSpend{
					NameTag:   normalizeTagGroupKey(group.Keys[0]),
					Service:   group.Keys[1],
					AmountUSD: amount,
					Currency:  metricUnit(metric),
				})
			}
		}
		if page.NextPageToken == nil || *page.NextPageToken == "" {
			break
		}
		input.NextPageToken = page.NextPageToken
	}

	return mergeNameTagSpend(results), nil
}

func (l *Loader) LoadSpendByProjectTag(ctx context.Context, projectTags []string, start, end time.Time) ([]service.ProjectSpend, error) {
	if len(projectTags) == 0 {
		return nil, nil
	}

	input := &costexplorer.GetCostAndUsageInput{
		TimePeriod: &types.DateInterval{
			Start: aws.String(start.Format("2006-01-02")),
			End:   aws.String(end.Format("2006-01-02")),
		},
		Granularity: types.GranularityDaily,
		Metrics:     []string{"UnblendedCost"},
		Filter: &types.Expression{
			Tags: &types.TagValues{
				Key:    aws.String("Project"),
				Values: projectTags,
			},
		},
		GroupBy: []types.GroupDefinition{
			{Type: types.GroupDefinitionTypeTag, Key: aws.String("Project")},
		},
	}

	var results []service.ProjectSpend
	for {
		page, err := l.client.GetCostAndUsage(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("get cost and usage by Project tag: %w", err)
		}
		for _, period := range page.ResultsByTime {
			for _, group := range period.Groups {
				if len(group.Keys) == 0 {
					continue
				}
				metric, ok := group.Metrics["UnblendedCost"]
				if !ok || metric.Amount == nil {
					continue
				}
				amount, err := strconv.ParseFloat(*metric.Amount, 64)
				if err != nil {
					continue
				}
				results = append(results, service.ProjectSpend{
					ProjectTag: normalizeTagGroupKey(group.Keys[0]),
					AmountUSD:  amount,
					Currency:   metricUnit(metric),
				})
			}
		}
		if page.NextPageToken == nil || *page.NextPageToken == "" {
			break
		}
		input.NextPageToken = page.NextPageToken
	}

	return mergeProjectSpend(results), nil
}

func metricUnit(metric types.MetricValue) string {
	if metric.Unit != nil && *metric.Unit != "" {
		return *metric.Unit
	}
	return "USD"
}

func normalizeTagGroupKey(value string) string {
	if i := strings.Index(value, "$"); i >= 0 {
		return value[i+1:]
	}
	return value
}

func mergeNameTagSpend(values []service.NameTagSpend) []service.NameTagSpend {
	type key struct {
		nameTag string
		service string
	}
	merged := map[key]service.NameTagSpend{}
	for _, item := range values {
		k := key{nameTag: item.NameTag, service: item.Service}
		current := merged[k]
		current.NameTag = item.NameTag
		current.Service = item.Service
		current.AmountUSD += item.AmountUSD
		if current.Currency == "" {
			current.Currency = item.Currency
		}
		merged[k] = current
	}
	out := make([]service.NameTagSpend, 0, len(merged))
	for _, item := range merged {
		out = append(out, item)
	}
	return out
}

func mergeProjectSpend(values []service.ProjectSpend) []service.ProjectSpend {
	merged := map[string]service.ProjectSpend{}
	for _, item := range values {
		current := merged[item.ProjectTag]
		current.ProjectTag = item.ProjectTag
		current.AmountUSD += item.AmountUSD
		if current.Currency == "" {
			current.Currency = item.Currency
		}
		merged[item.ProjectTag] = current
	}
	out := make([]service.ProjectSpend, 0, len(merged))
	for _, item := range merged {
		out = append(out, item)
	}
	return out
}
