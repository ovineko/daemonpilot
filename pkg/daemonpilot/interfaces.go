package daemonpilot

import (
	"context"
	"io"
)

// UpdateSource is the interface for fetching updates.
// Users implement this for their specific infrastructure (HTTP server, GitHub Releases, etc.).
type UpdateSource interface {
	// CheckUpdate checks if a new version is available.
	// Returns nil, nil if no update is available.
	CheckUpdate(ctx context.Context, currentVersion string) (*UpdateInfo, error)

	// DownloadUpdate downloads the binary and its signature.
	// The caller is responsible for closing the returned ReadCloser.
	DownloadUpdate(ctx context.Context, info *UpdateInfo) (binary io.ReadCloser, signature []byte, err error)
}
