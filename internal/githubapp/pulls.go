package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ashtwentyfour/super-eureka/internal/service"
)

type PullRequestClient struct {
	tokenSource tokenSource
	client      *http.Client
	baseURL     string
}

func NewPullRequestClient(source tokenSource) *PullRequestClient {
	return &PullRequestClient{
		tokenSource: source,
		client:      http.DefaultClient,
		baseURL:     githubAPIBaseURL,
	}
}

type PullRequestDetails struct {
	Number  int
	Title   string
	HTMLURL string
	Head    struct {
		Ref  string
		SHA  string
		Repo struct {
			Name  string
			Owner struct {
				Login string
			}
		}
	}
	Base struct {
		Ref  string
		Repo struct {
			FullName string
		}
	}
}

func (c *PullRequestClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (PullRequestDetails, error) {
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return PullRequestDetails{}, fmt.Errorf("resolve github installation token: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PullRequestDetails{}, fmt.Errorf("build github pull request request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.client.Do(req)
	if err != nil {
		return PullRequestDetails{}, fmt.Errorf("get pull request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return PullRequestDetails{}, fmt.Errorf("get pull request: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var details PullRequestDetails
	if err := json.NewDecoder(resp.Body).Decode(&details); err != nil {
		return PullRequestDetails{}, fmt.Errorf("decode pull request response: %w", err)
	}
	return details, nil
}

func (p PullRequestDetails) ToAnalysisRequest(repositoryFullName string) service.AnalysisRequest {
	return service.AnalysisRequest{
		RepositoryFullName: repositoryFullName,
		PullRequestNumber:  p.Number,
		PullRequestTitle:   p.Title,
		PullRequestURL:     p.HTMLURL,
		HeadOwner:          p.Head.Repo.Owner.Login,
		HeadRepo:           p.Head.Repo.Name,
		HeadRef:            p.Head.Ref,
		HeadSHA:            p.Head.SHA,
		BaseRef:            p.Base.Ref,
		BaseRepoFullName:   p.Base.Repo.FullName,
	}
}
