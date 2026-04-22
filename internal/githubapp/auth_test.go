package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubAppTokenSourceFetchesAndCachesInstallationToken(t *testing.T) {
	privateKeyPEM := testPrivateKeyPEM(t)
	var calls atomic.Int32

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/app/installations/99/access_tokens" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		calls.Add(1)
		payload, _ := json.Marshal(map[string]any{
			"token":      "installation-token",
			"expires_at": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		})
		return &http.Response{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(payload))),
		}, nil
	})}

	source, err := NewGitHubAppTokenSource("12345", "99", privateKeyPEM)
	if err != nil {
		t.Fatalf("NewGitHubAppTokenSource() error = %v", err)
	}
	source.baseURL = "https://api.github.test"
	source.client = client

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() first error = %v", err)
	}
	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() second error = %v", err)
	}

	if first != "installation-token" || second != "installation-token" {
		t.Fatalf("Token() = %q/%q, want installation-token", first, second)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("installation token calls = %d, want 1", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey() error = %v", err)
	}
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}
	return string(pem.EncodeToMemory(block))
}
