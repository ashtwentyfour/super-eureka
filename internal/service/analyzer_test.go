package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashtwentyfour/super-eureka/internal/terraform"
)

type stubRepoFetcher struct {
	repoDir string
}

func (f stubRepoFetcher) FetchRepositoryRef(_ context.Context, _, _, _, _ string) (string, error) {
	return f.repoDir, nil
}

type stubPricer struct{}

func (stubPricer) Estimate(_ context.Context, analysis *terraform.Analysis) ([]terraform.EstimatedCost, error) {
	return make([]terraform.EstimatedCost, 0, len(analysis.Resources)), nil
}

func TestAnalyzePullRequestHandlesRepositoryWithoutTerraform(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# not terraform\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	analyzer := NewAnalyzer(
		stubRepoFetcher{repoDir: repoDir},
		terraform.NewParser(nil),
		stubPricer{},
		t.TempDir(),
	)

	report, err := analyzer.AnalyzePullRequest(context.Background(), AnalysisRequest{
		RepositoryFullName: "acme/example",
		PullRequestNumber:  1,
		PullRequestTitle:   "docs only",
		PullRequestURL:     "https://example.com/pr/1",
		HeadOwner:          "acme",
		HeadRepo:           "example",
		HeadRef:            "main",
		HeadSHA:            "abc123",
	})
	if err != nil {
		t.Fatalf("AnalyzePullRequest() error = %v", err)
	}

	if report.Analysis == nil {
		t.Fatal("AnalyzePullRequest() returned nil analysis")
	}
	if got := len(report.Analysis.Resources); got != 0 {
		t.Fatalf("len(report.Analysis.Resources) = %d, want 0", got)
	}
	if got := len(report.Costs); got != 0 {
		t.Fatalf("len(report.Costs) = %d, want 0", got)
	}

	markdown := report.Markdown()
	for _, want := range []string{"No Terraform resources were detected.", "No cost estimates could be produced."} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("report markdown missing %q:\n%s", want, markdown)
		}
	}
}
