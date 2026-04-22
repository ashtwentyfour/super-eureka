package service

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ashtwentyfour/super-eureka/internal/terraform"
)

type AnalysisRequest struct {
	RepositoryFullName string
	PullRequestNumber  int
	PullRequestTitle   string
	PullRequestURL     string
	HeadOwner          string
	HeadRepo           string
	HeadRef            string
	HeadSHA            string
	BaseRef            string
	BaseRepoFullName   string
}

type RepoFetcher interface {
	FetchRepositoryRef(ctx context.Context, owner, repo, ref, destRoot string) (string, error)
}

type TerraformParser interface {
	ParseRepository(ctx context.Context, root string) (*terraform.Analysis, error)
}

type Pricer interface {
	Estimate(ctx context.Context, analysis *terraform.Analysis) ([]terraform.EstimatedCost, error)
}

type Analyzer struct {
	fetcher         RepoFetcher
	parser          TerraformParser
	pricer          Pricer
	workspaceParent string
}

func NewAnalyzer(fetcher RepoFetcher, parser TerraformParser, pricer Pricer, workspaceParent string) *Analyzer {
	return &Analyzer{
		fetcher:         fetcher,
		parser:          parser,
		pricer:          pricer,
		workspaceParent: workspaceParent,
	}
}

func (a *Analyzer) AnalyzePullRequest(ctx context.Context, req AnalysisRequest) (Report, error) {
	analysis, err := a.LoadPullRequestAnalysis(ctx, req)
	if err != nil {
		return Report{}, err
	}

	costs, err := a.pricer.Estimate(ctx, analysis)
	if err != nil {
		return Report{}, fmt.Errorf("estimate pricing: %w", err)
	}

	var environmentReports []EnvironmentReport
	for _, env := range analysis.Environments {
		envAnalysis := &terraform.Analysis{
			Backend:   analysis.Backend,
			State:     analysis.State,
			Resources: env.Resources,
		}
		envCosts, err := a.pricer.Estimate(ctx, envAnalysis)
		if err != nil {
			return Report{}, fmt.Errorf("estimate pricing for %s: %w", env.TFVarsFile, err)
		}
		environmentReports = append(environmentReports, EnvironmentReport{
			Name:       env.Name,
			TFVarsFile: env.TFVarsFile,
			Resources:  env.Resources,
			Costs:      envCosts,
		})
	}

	return Report{
		GeneratedAt:  time.Now().UTC(),
		Request:      req,
		Workspace:    filepath.Join("", req.HeadRepo),
		Analysis:     analysis,
		Costs:        costs,
		Environments: environmentReports,
	}, nil
}

func (a *Analyzer) LoadPullRequestAnalysis(ctx context.Context, req AnalysisRequest) (*terraform.Analysis, error) {
	repoDir, err := a.fetcher.FetchRepositoryRef(ctx, req.HeadOwner, req.HeadRepo, req.HeadRef, a.workspaceParent)
	if err != nil {
		return nil, fmt.Errorf("fetch repository ref: %w", err)
	}

	analysis, err := a.parser.ParseRepository(ctx, repoDir)
	if err != nil {
		return nil, fmt.Errorf("parse terraform: %w", err)
	}
	return analysis, nil
}
