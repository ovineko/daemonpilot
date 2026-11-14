package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

// MyUpdateSource fetches updates from an HTTP server.
// See examples/http-update-source for a more complete implementation.
type MyUpdateSource struct {
	BaseURL string
}

func (s *MyUpdateSource) CheckUpdate(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
	arch := runtime.GOOS + "/" + runtime.GOARCH
	url := fmt.Sprintf("%s/check?version=%s&arch=%s", s.BaseURL, currentVersion, arch)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building check request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checking for update: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil //nolint:nilnil // daemonpilot's UpdateSource contract: no update available
	}

	var info daemonpilot.UpdateInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decoding update info: %w", err)
	}
	return &info, nil
}

func (s *MyUpdateSource) DownloadUpdate(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
	arch := runtime.GOOS + "/" + runtime.GOARCH
	binaryURL := fmt.Sprintf("%s/download/%s/%s", s.BaseURL, info.Version, arch)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, binaryURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("building download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("downloading binary: %w", err)
	}

	sigReq, err := http.NewRequestWithContext(ctx, http.MethodGet, binaryURL+".sig", nil)
	if err != nil {
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("building signature request: %w", err)
	}

	sigResp, err := http.DefaultClient.Do(sigReq)
	if err != nil {
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("downloading signature: %w", err)
	}
	defer func() { _ = sigResp.Body.Close() }()

	sig, err := io.ReadAll(sigResp.Body)
	if err != nil {
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("reading signature: %w", err)
	}

	return resp.Body, sig, nil
}
