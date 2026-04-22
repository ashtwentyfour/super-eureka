package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type CheckRunPublisher struct {
	tokenSource tokenSource
	client      *http.Client
	baseURL     string
}

func NewCheckRunPublisher(source tokenSource) *CheckRunPublisher {
	return &CheckRunPublisher{
		tokenSource: source,
		client:      http.DefaultClient,
		baseURL:     githubAPIBaseURL,
	}
}

type CheckRunResult struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
	Status  string `json:"status"`
}

func (p *CheckRunPublisher) UpsertCompletedCheckRun(ctx context.Context, owner, repo, headSHA, name, title, summaryMarkdown, markdown string) (CheckRunResult, error) {
	token, err := p.tokenSource.Token(ctx)
	if err != nil {
		return CheckRunResult{}, fmt.Errorf("resolve github installation token: %w", err)
	}

	payload := map[string]any{
		"name":         name,
		"head_sha":     headSHA,
		"status":       "completed",
		"conclusion":   "success",
		"completed_at": time.Now().UTC().Format(time.RFC3339),
		"output": map[string]string{
			"title":   title,
			"summary": truncateMarkdown(summaryMarkdown, 65000),
			"text":    truncateMarkdown(markdown, 65000),
		},
	}

	existing, err := p.findCheckRunByName(ctx, token, owner, repo, headSHA, name)
	if err != nil {
		return CheckRunResult{}, err
	}

	if existing != nil {
		delete(payload, "head_sha")
		return p.updateCheckRun(ctx, token, owner, repo, existing.ID, payload)
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return CheckRunResult{}, fmt.Errorf("marshal check run payload: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/check-runs", p.baseURL, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return CheckRunResult{}, fmt.Errorf("build github check run request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return CheckRunResult{}, fmt.Errorf("create check run: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return CheckRunResult{}, fmt.Errorf("create check run: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result CheckRunResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CheckRunResult{}, fmt.Errorf("decode check run response: %w", err)
	}
	return result, nil
}

func (p *CheckRunPublisher) findCheckRunByName(ctx context.Context, token, owner, repo, headSHA, name string) (*CheckRunResult, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", p.baseURL, owner, repo, headSHA)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build github check run lookup request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list check runs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("list check runs: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		CheckRuns []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			HTMLURL string `json:"html_url"`
			Status  string `json:"status"`
		} `json:"check_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode check run lookup response: %w", err)
	}

	for _, item := range payload.CheckRuns {
		if item.Name == name {
			return &CheckRunResult{
				ID:      item.ID,
				HTMLURL: item.HTMLURL,
				Status:  item.Status,
			}, nil
		}
	}
	return nil, nil
}

func (p *CheckRunPublisher) updateCheckRun(ctx context.Context, token, owner, repo string, checkRunID int64, payload map[string]any) (CheckRunResult, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return CheckRunResult{}, fmt.Errorf("marshal check run update payload: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/check-runs/%d", p.baseURL, owner, repo, checkRunID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(raw))
	if err != nil {
		return CheckRunResult{}, fmt.Errorf("build github check run update request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return CheckRunResult{}, fmt.Errorf("update check run: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return CheckRunResult{}, fmt.Errorf("update check run: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result CheckRunResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return CheckRunResult{}, fmt.Errorf("decode check run update response: %w", err)
	}
	return result, nil
}

func truncateMarkdown(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit-3] + "..."
}
