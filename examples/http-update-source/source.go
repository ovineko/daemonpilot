// Package httpupdate demonstrates the production HTTP update source.
package httpupdate

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/ovineko/daemonpilot/pkg/httpsource"
)

// New configures a source compatible with release.NewHandler.
func New(baseURL, bearerToken string) (*httpsource.Source, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	source, err := httpsource.New(httpsource.Config{
		ManifestURL: baseURL + "/manifest/{os}/{arch}",
		DownloadURL: baseURL + "/download/{version}/{os}/{arch}",
		Authorize: func(req *http.Request) error {
			if bearerToken != "" {
				req.Header.Set("Authorization", "Bearer "+bearerToken)
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure HTTP source: %w", err)
	}
	return source, nil
}
