package githubapp

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type staticTokenSource struct {
	token string
}

func (s staticTokenSource) Token(context.Context) (string, error) {
	return s.token, nil
}

func TestArchiveFetcherFetchRepositoryRefUsesTokenSource(t *testing.T) {
	archiveBytes := zipballBytes(t, map[string]string{
		"owner-repo-sha/main.tf": `resource "aws_s3_bucket" "bucket" { bucket = "demo" }`,
	})

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer installation-token" {
			t.Fatalf("Authorization = %q, want bearer installation token", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(archiveBytes)),
		}, nil
	})}

	fetcher := NewArchiveFetcher(staticTokenSource{token: "installation-token"})
	fetcher.baseURL = "https://api.github.test"
	fetcher.client = client

	repoDir, err := fetcher.FetchRepositoryRef(context.Background(), "owner", "repo", "sha", t.TempDir())
	if err != nil {
		t.Fatalf("FetchRepositoryRef() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, "main.tf")); err != nil {
		t.Fatalf("expected extracted main.tf: %v", err)
	}
}

func zipballBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, contents := range files {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatalf("writer.Create(%q): %v", name, err)
		}
		if _, err := io.WriteString(file, contents); err != nil {
			t.Fatalf("io.WriteString(%q): %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close(): %v", err)
	}
	return buf.Bytes()
}
