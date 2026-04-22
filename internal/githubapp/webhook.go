package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/ashtwentyfour/super-eureka/internal/config"
	"github.com/ashtwentyfour/super-eureka/internal/service"
)

type WebhookHandler struct {
	cfg          config.Config
	analyzer     *service.Analyzer
	commenter    *PullRequestCommenter
	pullRequests *PullRequestClient
	checks       *CheckRunPublisher
	cloudspend   *service.CloudSpendService
}

func NewWebhookHandler(cfg config.Config, analyzer *service.Analyzer, tokenSource tokenSource) *WebhookHandler {
	return &WebhookHandler{
		cfg:          cfg,
		analyzer:     analyzer,
		commenter:    NewPullRequestCommenter(tokenSource),
		pullRequests: NewPullRequestClient(tokenSource),
		checks:       NewCheckRunPublisher(tokenSource),
	}
}

func (h *WebhookHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.handleHealth)
	mux.HandleFunc("/healthz", h.handleHealth)
	mux.HandleFunc("/webhooks/github/pricing-estimate/comment", h.handleGitHubCommentWebhook)
	mux.HandleFunc("/webhooks/github/pricing-estimate/json", h.handleGitHubJSONWebhook)
	mux.HandleFunc("/webhooks/github/cloudspend/comment", h.handleGitHubCloudSpendWebhook)
	return mux
}

func (h *WebhookHandler) WithCloudSpend(service *service.CloudSpendService) *WebhookHandler {
	h.cloudspend = service
	return h
}

func (h *WebhookHandler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *WebhookHandler) handleGitHubCommentWebhook(w http.ResponseWriter, r *http.Request) {
	payload, ok := h.parseWebhook(w, r)
	if !ok {
		return
	}

	report, err := h.analyzer.AnalyzePullRequest(r.Context(), payload.ToAnalysisRequest())
	if err != nil {
		log.Printf("analysis failed for PR #%d: %v", payload.Number, err)
		http.Error(w, fmt.Sprintf("analysis failed: %v", err), http.StatusInternalServerError)
		return
	}

	owner, repo := payload.RepositoryParts()
	comment, err := h.commenter.CreateComment(r.Context(), owner, repo, payload.Number, report.Markdown())
	if err != nil {
		log.Printf("comment failed for PR #%d: %v", payload.Number, err)
		http.Error(w, fmt.Sprintf("comment failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "comment_posted",
		"comment": comment,
		"report":  toAnalyzeResponse(report),
	})
}

func (h *WebhookHandler) handleGitHubJSONWebhook(w http.ResponseWriter, r *http.Request) {
	payload, ok := h.parseWebhook(w, r)
	if !ok {
		return
	}

	report, err := h.analyzer.AnalyzePullRequest(r.Context(), payload.ToAnalysisRequest())
	if err != nil {
		log.Printf("analysis failed for PR #%d: %v", payload.Number, err)
		http.Error(w, fmt.Sprintf("analysis failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(toAnalyzeResponse(report))
}

func (h *WebhookHandler) handleGitHubCloudSpendWebhook(w http.ResponseWriter, r *http.Request) {
	payload, ok := h.parseIssueCommentWebhook(w, r)
	if !ok {
		return
	}
	if h.cloudspend == nil {
		http.Error(w, "cloud spend service not configured", http.StatusInternalServerError)
		return
	}

	workspace, matched := parseCloudSpendCommand(payload.Comment.Body)
	if !matched {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ignored comment"))
		return
	}

	owner, repo := payload.RepositoryParts()
	pr, err := h.pullRequests.GetPullRequest(r.Context(), owner, repo, payload.Issue.Number)
	if err != nil {
		log.Printf("load pull request failed for PR #%d: %v", payload.Issue.Number, err)
		http.Error(w, fmt.Sprintf("load pull request failed: %v", err), http.StatusInternalServerError)
		return
	}

	analysis, err := h.analyzer.LoadPullRequestAnalysis(r.Context(), pr.ToAnalysisRequest(payload.Repository.FullName))
	if err != nil {
		log.Printf("cloud spend analysis failed for PR #%d: %v", payload.Issue.Number, err)
		http.Error(w, fmt.Sprintf("cloud spend analysis failed: %v", err), http.StatusInternalServerError)
		return
	}

	spendReport, err := h.cloudspend.BuildReport(r.Context(), analysis, workspace)
	if err != nil {
		log.Printf("cloud spend report failed for PR #%d: %v", payload.Issue.Number, err)
		http.Error(w, fmt.Sprintf("cloud spend report failed: %v", err), http.StatusInternalServerError)
		return
	}

	check, err := h.checks.UpsertCompletedCheckRun(
		r.Context(),
		owner,
		repo,
		pr.Head.SHA,
		fmt.Sprintf("cloudspend (%s)", workspace),
		fmt.Sprintf("Cloud spend for workspace %s", workspace),
		spendReport.SummaryMarkdown(),
		spendReport.Markdown(),
	)
	if err != nil {
		log.Printf("cloud spend check failed for PR #%d: %v", payload.Issue.Number, err)
		http.Error(w, fmt.Sprintf("cloud spend check failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "check_posted",
		"workspace": workspace,
		"check":     check,
		"report":    spendReport.Markdown(),
	})
}

func (h *WebhookHandler) parseWebhook(w http.ResponseWriter, r *http.Request) (PullRequestEvent, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return PullRequestEvent{}, false
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil {
		http.Error(w, "unable to read request body", http.StatusBadRequest)
		return PullRequestEvent{}, false
	}

	if !validateSignature(r.Header.Get("X-Hub-Signature-256"), body, h.cfg.WebhookSecret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return PullRequestEvent{}, false
	}

	if r.Header.Get("X-GitHub-Event") != "pull_request" {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ignored event"))
		return PullRequestEvent{}, false
	}

	var payload PullRequestEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return PullRequestEvent{}, false
	}

	if !payload.IsInterestingAction() {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ignored action"))
		return PullRequestEvent{}, false
	}

	return payload, true
}

func validateSignature(signature string, body []byte, secret string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.TrimPrefix(signature, prefix)))
}

func parseCloudSpendCommand(body string) (string, bool) {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "cloudspend") {
		return "", false
	}
	fields := strings.Fields(body)
	if len(fields) != 3 || fields[1] != "-w" || fields[2] == "" {
		return "", false
	}
	return fields[2], true
}

type PullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Title string `json:"title"`
		HTML  string `json:"html_url"`
		Head  struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				Name     string `json:"name"`
				FullName string `json:"full_name"`
				Owner    struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type IssueCommentEvent struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
	Issue struct {
		Number      int `json:"number"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		Body string `json:"body"`
	} `json:"comment"`
}

func (e PullRequestEvent) IsInterestingAction() bool {
	switch e.Action {
	case "opened", "reopened", "synchronize", "edited":
		return true
	default:
		return false
	}
}

func (e PullRequestEvent) ToAnalysisRequest() service.AnalysisRequest {
	return service.AnalysisRequest{
		RepositoryFullName: e.Repository.FullName,
		PullRequestNumber:  e.Number,
		PullRequestTitle:   e.PullRequest.Title,
		PullRequestURL:     e.PullRequest.HTML,
		HeadOwner:          e.PullRequest.Head.Repo.Owner.Login,
		HeadRepo:           e.PullRequest.Head.Repo.Name,
		HeadRef:            e.PullRequest.Head.Ref,
		HeadSHA:            e.PullRequest.Head.SHA,
		BaseRef:            e.PullRequest.Base.Ref,
		BaseRepoFullName:   e.PullRequest.Base.Repo.FullName,
	}
}

func (e PullRequestEvent) RepositoryParts() (string, string) {
	fullName := e.Repository.FullName
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return e.PullRequest.Head.Repo.Owner.Login, e.PullRequest.Head.Repo.Name
}

func (e IssueCommentEvent) RepositoryParts() (string, string) {
	fullName := e.Repository.FullName
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return e.Repository.Owner.Login, e.Repository.Name
}

func (h *WebhookHandler) parseIssueCommentWebhook(w http.ResponseWriter, r *http.Request) (IssueCommentEvent, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return IssueCommentEvent{}, false
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil {
		http.Error(w, "unable to read request body", http.StatusBadRequest)
		return IssueCommentEvent{}, false
	}

	if !validateSignature(r.Header.Get("X-Hub-Signature-256"), body, h.cfg.WebhookSecret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return IssueCommentEvent{}, false
	}

	if r.Header.Get("X-GitHub-Event") != "issue_comment" {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ignored event"))
		return IssueCommentEvent{}, false
	}

	var payload IssueCommentEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return IssueCommentEvent{}, false
	}
	if payload.Action != "created" {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ignored action"))
		return IssueCommentEvent{}, false
	}
	if payload.Issue.PullRequest == nil {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ignored non-pr comment"))
		return IssueCommentEvent{}, false
	}

	return payload, true
}
