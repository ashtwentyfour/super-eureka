package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	ListenAddr              string
	GitHubAppID             string
	GitHubAppInstallationID string
	GitHubAppPrivateKeyPEM  string
	WebhookSecret           string
	AWSRegion               string
	WorkspaceParent         string
}

func FromEnv() (Config, error) {
	cfg := Config{
		ListenAddr:              getenvDefault("LISTEN_ADDR", ":8080"),
		GitHubAppID:             os.Getenv("GITHUB_APP_ID"),
		GitHubAppInstallationID: os.Getenv("GITHUB_APP_INSTALLATION_ID"),
		GitHubAppPrivateKeyPEM:  normalizePEMEnv(os.Getenv("GITHUB_APP_PRIVATE_KEY_PEM")),
		WebhookSecret:           os.Getenv("GITHUB_WEBHOOK_SECRET"),
		AWSRegion:               getenvDefault("AWS_REGION", "us-east-1"),
		WorkspaceParent:         getenvDefault("WORKSPACE_PARENT", os.TempDir()),
	}

	var errs []error
	if cfg.GitHubAppID == "" {
		errs = append(errs, errors.New("GITHUB_APP_ID is required"))
	}
	if cfg.GitHubAppInstallationID == "" {
		errs = append(errs, errors.New("GITHUB_APP_INSTALLATION_ID is required"))
	}
	if cfg.GitHubAppPrivateKeyPEM == "" {
		errs = append(errs, errors.New("GITHUB_APP_PRIVATE_KEY_PEM is required"))
	}
	if cfg.WebhookSecret == "" {
		errs = append(errs, errors.New("GITHUB_WEBHOOK_SECRET is required"))
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}

	return cfg, nil
}

func getenvDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func normalizePEMEnv(value string) string {
	return strings.ReplaceAll(value, `\n`, "\n")
}
