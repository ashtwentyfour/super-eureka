package githubapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPullRequestCommenterCreateCommentUsesTokenSource(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer installation-token" {
			t.Fatalf("Authorization = %q, want bearer installation token", got)
		}
		if r.URL.Path != "/repos/owner/repo/issues/7/comments" {
			t.Fatalf("path = %q, want comment endpoint", r.URL.Path)
		}
		payload, _ := json.Marshal(CommentResult{
			ID:      1,
			URL:     "https://example.com/api/comments/1",
			HTMLURL: "https://example.com/comments/1",
			Body:    "hello",
		})
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(payload))),
		}, nil
	})}

	commenter := NewPullRequestCommenter(staticTokenSource{token: "installation-token"})
	commenter.baseURL = "https://api.github.test"
	commenter.client = client

	result, err := commenter.CreateComment(context.Background(), "owner", "repo", 7, "hello")
	if err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if result.ID != 1 {
		t.Fatalf("CreateComment().ID = %d, want 1", result.ID)
	}
}
