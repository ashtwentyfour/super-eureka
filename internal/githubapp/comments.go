package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type PullRequestCommenter struct {
	tokenSource tokenSource
	client      *http.Client
	baseURL     string
}

func NewPullRequestCommenter(source tokenSource) *PullRequestCommenter {
	return &PullRequestCommenter{
		tokenSource: source,
		client:      http.DefaultClient,
		baseURL:     githubAPIBaseURL,
	}
}

type CommentResult struct {
	ID      int64  `json:"id"`
	URL     string `json:"url"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

func (c *PullRequestCommenter) CreateComment(ctx context.Context, owner, repo string, number int, markdown string) (CommentResult, error) {
	payload, err := json.Marshal(map[string]string{"body": markdown})
	if err != nil {
		return CommentResult{}, fmt.Errorf("marshal comment payload: %w", err)
	}

	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return CommentResult{}, fmt.Errorf("resolve github installation token: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", c.baseURL, owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return CommentResult{}, fmt.Errorf("build github comment request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return CommentResult{}, fmt.Errorf("post pull request comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return CommentResult{}, fmt.Errorf("post pull request comment: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result CommentResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CommentResult{}, fmt.Errorf("decode comment response: %w", err)
	}
	return result, nil
}
