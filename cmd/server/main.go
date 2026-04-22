package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/ashtwentyfour/super-eureka/internal/awspricing"
	"github.com/ashtwentyfour/super-eureka/internal/cloudspend"
	"github.com/ashtwentyfour/super-eureka/internal/config"
	"github.com/ashtwentyfour/super-eureka/internal/githubapp"
	"github.com/ashtwentyfour/super-eureka/internal/service"
	"github.com/ashtwentyfour/super-eureka/internal/terraform"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx := context.Background()
	pricer, err := awspricing.New(ctx, cfg.AWSRegion)
	if err != nil {
		log.Fatalf("create pricing client: %v", err)
	}
	spendLoader, err := cloudspend.New(ctx)
	if err != nil {
		log.Fatalf("create cloud spend client: %v", err)
	}

	tfStateLoader, err := terraform.NewStateLoader(ctx)
	if err != nil {
		log.Fatalf("create state loader: %v", err)
	}

	tokenSource, err := githubapp.NewGitHubAppTokenSource(cfg.GitHubAppID, cfg.GitHubAppInstallationID, cfg.GitHubAppPrivateKeyPEM)
	if err != nil {
		log.Fatalf("create github app token source: %v", err)
	}

	analyzer := service.NewAnalyzer(
		githubapp.NewArchiveFetcher(tokenSource),
		terraform.NewParser(tfStateLoader),
		pricer,
		cfg.WorkspaceParent,
	)

	handler := githubapp.NewWebhookHandler(cfg, analyzer, tokenSource).WithCloudSpend(service.NewCloudSpendService(spendLoader, tfStateLoader))

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server exited: %v", err)
	}
}
