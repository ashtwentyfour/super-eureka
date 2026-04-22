package terraform

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type StateLoaderImpl struct{}

func NewStateLoader(context.Context) (*StateLoaderImpl, error) { return &StateLoaderImpl{}, nil }

func (l *StateLoaderImpl) Load(ctx context.Context, backend S3Backend) (*StateSummary, error) {
	return l.loadKey(ctx, backend, backend.Key)
}

func (l *StateLoaderImpl) LoadForWorkspace(ctx context.Context, backend S3Backend, workspace string) (*StateSummary, error) {
	if workspace == "" || workspace == "default" {
		return l.Load(ctx, backend)
	}
	key := backend.Key
	prefix := backend.WorkspaceKeyPrefix
	if prefix == "" {
		prefix = "env:"
	}
	if key == "" {
		return nil, fmt.Errorf("incomplete s3 backend config")
	}
	return l.loadKey(ctx, backend, fmt.Sprintf("%s/%s/%s", prefix, workspace, key))
}

func (l *StateLoaderImpl) loadKey(ctx context.Context, backend S3Backend, key string) (*StateSummary, error) {
	if backend.Bucket == "" || backend.Key == "" {
		return nil, fmt.Errorf("incomplete s3 backend config")
	}

	loadOptions := []func(*config.LoadOptions) error{}
	if backend.Region != "" {
		loadOptions = append(loadOptions, config.WithRegion(backend.Region))
	}

	cfg, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(cfg)
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &backend.Bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, fmt.Errorf("get remote state from s3: %w", err)
	}
	defer resp.Body.Close()

	var state StateSummary
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, fmt.Errorf("decode terraform state: %w", err)
	}
	return &state, nil
}
