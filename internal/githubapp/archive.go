package githubapp

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type ArchiveFetcher struct {
	tokenSource tokenSource
	client      *http.Client
	baseURL     string
}

func NewArchiveFetcher(source tokenSource) *ArchiveFetcher {
	return &ArchiveFetcher{
		tokenSource: source,
		client:      http.DefaultClient,
		baseURL:     githubAPIBaseURL,
	}
}

func (f *ArchiveFetcher) FetchRepositoryRef(ctx context.Context, owner, repo, ref, destRoot string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/zipball/%s", f.baseURL, owner, repo, ref)
	token, err := f.tokenSource.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve github installation token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build github request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download ref archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("download ref archive: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	tmpFile, err := os.CreateTemp(destRoot, "github-archive-*.zip")
	if err != nil {
		return "", fmt.Errorf("create temp archive: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return "", fmt.Errorf("write temp archive: %w", err)
	}

	destDir, err := os.MkdirTemp(destRoot, "repo-*")
	if err != nil {
		return "", fmt.Errorf("create temp repo dir: %w", err)
	}

	if err := unzip(tmpFile.Name(), destDir); err != nil {
		return "", err
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		return "", fmt.Errorf("read unzipped repo directory: %w", err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return "", fmt.Errorf("unexpected github archive layout in %s", destDir)
	}

	return filepath.Join(destDir, entries[0].Name()), nil
}

func unzip(srcZip, destDir string) error {
	reader, err := zip.OpenReader(srcZip)
	if err != nil {
		return fmt.Errorf("open zip archive: %w", err)
	}
	defer reader.Close()

	for _, file := range reader.File {
		targetPath := filepath.Join(destDir, file.Name)
		if !strings.HasPrefix(filepath.Clean(targetPath), filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid archive path: %s", file.Name)
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, file.Mode()); err != nil {
				return fmt.Errorf("create directory %s: %w", targetPath, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("create parent directory %s: %w", filepath.Dir(targetPath), err)
		}

		rc, err := file.Open()
		if err != nil {
			return fmt.Errorf("open archive entry %s: %w", file.Name, err)
		}

		dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, file.Mode())
		if err != nil {
			rc.Close()
			return fmt.Errorf("create file %s: %w", targetPath, err)
		}

		if _, err := io.Copy(dst, rc); err != nil {
			rc.Close()
			dst.Close()
			return fmt.Errorf("write file %s: %w", targetPath, err)
		}

		rc.Close()
		dst.Close()
	}

	return nil
}
