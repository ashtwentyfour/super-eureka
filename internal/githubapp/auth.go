package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const githubAPIBaseURL = "https://api.github.com"

type tokenSource interface {
	Token(ctx context.Context) (string, error)
}

type GitHubAppTokenSource struct {
	appID          string
	installationID string
	privateKey     *rsa.PrivateKey
	baseURL        string
	client         *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func NewGitHubAppTokenSource(appID, installationID, privateKeyPEM string) (*GitHubAppTokenSource, error) {
	privateKey, err := parseGitHubAppPrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}

	return &GitHubAppTokenSource{
		appID:          appID,
		installationID: installationID,
		privateKey:     privateKey,
		baseURL:        githubAPIBaseURL,
		client:         http.DefaultClient,
	}, nil
}

func (s *GitHubAppTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.token != "" && time.Until(s.expiresAt) > time.Minute {
		token := s.token
		s.mu.Unlock()
		return token, nil
	}
	s.mu.Unlock()

	token, expiresAt, err := s.fetchInstallationToken(ctx)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.token = token
	s.expiresAt = expiresAt
	s.mu.Unlock()
	return token, nil
}

func (s *GitHubAppTokenSource) fetchInstallationToken(ctx context.Context) (string, time.Time, error) {
	jwt, err := s.signedJWT(time.Now().UTC())
	if err != nil {
		return "", time.Time{}, err
	}

	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", s.baseURL, s.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build github app installation token request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("request github app installation token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", time.Time{}, fmt.Errorf("request github app installation token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", time.Time{}, fmt.Errorf("decode github app installation token response: %w", err)
	}
	if payload.Token == "" {
		return "", time.Time{}, fmt.Errorf("decode github app installation token response: missing token")
	}

	return payload.Token, payload.ExpiresAt, nil
}

func (s *GitHubAppTokenSource) signedJWT(now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": s.appID,
	})
	if err != nil {
		return "", fmt.Errorf("marshal github app jwt claims: %w", err)
	}

	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.privateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign github app jwt: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parseGitHubAppPrivateKey(privateKeyPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.ReplaceAll(privateKeyPEM, `\n`, "\n")))
	if block == nil {
		return nil, fmt.Errorf("parse github app private key: no PEM block found")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse github app private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("parse github app private key: expected RSA private key")
	}
	return key, nil
}
