package githubapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCheckRunPublisherUpsertCreatesWhenMissing(t *testing.T) {
	var calls []string
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/sha123/check-runs":
			payload, _ := json.Marshal(map[string]any{"check_runs": []any{}})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(string(payload))),
			}, nil
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/check-runs":
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"summary":"markdown"`) {
				t.Fatalf("summary unexpectedly duplicated full markdown: %s", body)
			}
			payload, _ := json.Marshal(CheckRunResult{ID: 11, HTMLURL: "https://example/checks/11", Status: "completed"})
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(string(payload))),
			}, nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})}

	publisher := NewCheckRunPublisher(staticTokenSource{token: "installation-token"})
	publisher.baseURL = "https://api.github.test"
	publisher.client = client

	result, err := publisher.UpsertCompletedCheckRun(context.Background(), "owner", "repo", "sha123", "cloudspend (dev)", "title", "summary", "markdown")
	if err != nil {
		t.Fatalf("UpsertCompletedCheckRun() error = %v", err)
	}
	if result.ID != 11 {
		t.Fatalf("result.ID = %d, want 11", result.ID)
	}
	if len(calls) != 2 || calls[1] != "POST /repos/owner/repo/check-runs" {
		t.Fatalf("calls = %#v, want GET then POST", calls)
	}
}

func TestCheckRunPublisherUpsertUpdatesWhenExisting(t *testing.T) {
	var calls []string
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/commits/sha123/check-runs":
			payload, _ := json.Marshal(map[string]any{
				"check_runs": []map[string]any{
					{"id": 42, "name": "cloudspend (dev)", "html_url": "https://example/checks/42", "status": "completed"},
				},
			})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(string(payload))),
			}, nil
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/owner/repo/check-runs/42":
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "head_sha") {
				t.Fatalf("update payload unexpectedly contained head_sha: %s", body)
			}
			if strings.Contains(string(body), `"summary":"markdown"`) {
				t.Fatalf("summary unexpectedly duplicated full markdown: %s", body)
			}
			payload, _ := json.Marshal(CheckRunResult{ID: 42, HTMLURL: "https://example/checks/42", Status: "completed"})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(string(payload))),
			}, nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})}

	publisher := NewCheckRunPublisher(staticTokenSource{token: "installation-token"})
	publisher.baseURL = "https://api.github.test"
	publisher.client = client

	result, err := publisher.UpsertCompletedCheckRun(context.Background(), "owner", "repo", "sha123", "cloudspend (dev)", "title", "summary", "markdown")
	if err != nil {
		t.Fatalf("UpsertCompletedCheckRun() error = %v", err)
	}
	if result.ID != 42 {
		t.Fatalf("result.ID = %d, want 42", result.ID)
	}
	if len(calls) != 2 || calls[1] != "PATCH /repos/owner/repo/check-runs/42" {
		t.Fatalf("calls = %#v, want GET then PATCH", calls)
	}
}
