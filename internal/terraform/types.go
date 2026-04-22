package terraform

import "strconv"

type Analysis struct {
	Backend      *S3Backend
	State        *StateSummary
	Resources    []Resource
	Environments []EnvironmentAnalysis
}

type EnvironmentAnalysis struct {
	Name       string
	TFVarsFile string
	Resources  []Resource
}

type S3Backend struct {
	Bucket             string
	Key                string
	Region             string
	WorkspaceKeyPrefix string
}

type StateSummary struct {
	Version   int             `json:"version"`
	Serial    int             `json:"serial"`
	Lineage   string          `json:"lineage"`
	Resources []StateResource `json:"resources"`
}

type StateResource struct {
	Module    string `json:"module"`
	Mode      string `json:"mode"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Instances []struct {
		Attributes map[string]any `json:"attributes"`
	} `json:"instances"`
}

type Resource struct {
	Type        string
	Name        string
	File        string
	Environment string
	InstanceKey string
	Attributes  map[string]Value
}

func (r Resource) Address() string {
	address := r.Type + "." + r.Name
	if r.InstanceKey == "" {
		return address
	}
	if _, err := strconv.Atoi(r.InstanceKey); err == nil {
		return address + "[" + r.InstanceKey + "]"
	}
	return address + "[\"" + r.InstanceKey + "\"]"
}

type Value struct {
	Raw     string
	Literal any
}

type EstimatedCost struct {
	ResourceAddress string
	MonthlyUSD      float64
	Basis           string
	Notes           string
	Assumptions     []string
}
