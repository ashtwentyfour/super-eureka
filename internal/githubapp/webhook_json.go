package githubapp

import "github.com/ashtwentyfour/super-eureka/internal/service"

type AnalyzeResponse struct {
	GeneratedAt        string                `json:"generated_at"`
	PullRequestNumber  int                   `json:"pull_request_number"`
	PullRequestTitle   string                `json:"pull_request_title"`
	PullRequestURL     string                `json:"pull_request_url"`
	RepositoryFullName string                `json:"repository_full_name"`
	SourceBranch       string                `json:"source_branch"`
	SourceCommit       string                `json:"source_commit"`
	RemoteState        *RemoteStateResponse  `json:"remote_state,omitempty"`
	Resources          []ResourceResponse    `json:"resources"`
	Costs              []serviceCostResponse `json:"costs"`
	Environments       []EnvironmentResponse `json:"environments,omitempty"`
	Markdown           string                `json:"markdown"`
}

type RemoteStateResponse struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Region string `json:"region"`
}

type ResourceResponse struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	File    string `json:"file"`
}

type serviceCostResponse struct {
	ResourceAddress string   `json:"resource_address"`
	MonthlyUSD      float64  `json:"monthly_usd"`
	Basis           string   `json:"basis"`
	Assumptions     []string `json:"assumptions,omitempty"`
	Notes           string   `json:"notes"`
}

type EnvironmentResponse struct {
	Name       string                `json:"name"`
	TFVarsFile string                `json:"tfvars_file"`
	Resources  []ResourceResponse    `json:"resources"`
	Costs      []serviceCostResponse `json:"costs"`
	TotalUSD   float64               `json:"total_usd"`
}

func toAnalyzeResponse(report service.Report) AnalyzeResponse {
	resp := AnalyzeResponse{
		GeneratedAt:        report.GeneratedAt.Format("2006-01-02T15:04:05Z07:00"),
		PullRequestNumber:  report.Request.PullRequestNumber,
		PullRequestTitle:   report.Request.PullRequestTitle,
		PullRequestURL:     report.Request.PullRequestURL,
		RepositoryFullName: report.Request.RepositoryFullName,
		SourceBranch:       report.Request.HeadRef,
		SourceCommit:       report.Request.HeadSHA,
		Markdown:           report.Markdown(),
	}

	if report.Analysis != nil && report.Analysis.Backend != nil {
		resp.RemoteState = &RemoteStateResponse{
			Bucket: report.Analysis.Backend.Bucket,
			Key:    report.Analysis.Backend.Key,
			Region: report.Analysis.Backend.Region,
		}
	}

	if report.Analysis != nil {
		resp.Resources = make([]ResourceResponse, 0, len(report.Analysis.Resources))
		for _, resource := range report.Analysis.Resources {
			resp.Resources = append(resp.Resources, ResourceResponse{
				Address: resource.Address(),
				Type:    resource.Type,
				File:    resource.File,
			})
		}
	}

	resp.Costs = make([]serviceCostResponse, 0, len(report.Costs))
	for _, cost := range report.Costs {
		resp.Costs = append(resp.Costs, serviceCostResponse{
			ResourceAddress: cost.ResourceAddress,
			MonthlyUSD:      cost.MonthlyUSD,
			Basis:           cost.Basis,
			Assumptions:     cost.Assumptions,
			Notes:           cost.Notes,
		})
	}

	resp.Environments = make([]EnvironmentResponse, 0, len(report.Environments))
	for _, env := range report.Environments {
		item := EnvironmentResponse{
			Name:       env.Name,
			TFVarsFile: env.TFVarsFile,
			Resources:  make([]ResourceResponse, 0, len(env.Resources)),
			Costs:      make([]serviceCostResponse, 0, len(env.Costs)),
		}
		for _, resource := range env.Resources {
			item.Resources = append(item.Resources, ResourceResponse{
				Address: resource.Address(),
				Type:    resource.Type,
				File:    resource.File,
			})
		}
		for _, cost := range env.Costs {
			item.TotalUSD += cost.MonthlyUSD
			item.Costs = append(item.Costs, serviceCostResponse{
				ResourceAddress: cost.ResourceAddress,
				MonthlyUSD:      cost.MonthlyUSD,
				Basis:           cost.Basis,
				Assumptions:     cost.Assumptions,
				Notes:           cost.Notes,
			})
		}
		resp.Environments = append(resp.Environments, item)
	}

	return resp
}
