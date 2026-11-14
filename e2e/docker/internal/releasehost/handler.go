//go:build e2e_docker

// Package releasehost exposes the real release handler and test-owned request evidence.
package releasehost

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"
	"github.com/ovineko/daemonpilot/pkg/release"
)

// Offer selects an immutable artifact and which generated key signs it.
type Offer struct {
	ID       string `json:"ID"`
	WrongKey bool   `json:"WrongKey"`
}

// Counts records completed manifest and download requests by release selector.
type Counts struct{ Manifests, Downloads map[string]int }

// Transport runs the HTTP contract without sockets in restricted native sandboxes.
type Transport struct{ Handler http.Handler }

// RoundTrip adapts an HTTP handler to the client's transport interface.
func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.Handler.ServeHTTP(w, r)
	return w.Result(), nil
}

// New uses the same handler for native HTTP and the isolated server container.
func New(dir, artifacts string, key, wrongKey ed25519.PrivateKey) (http.Handler, error) {
	var mu sync.Mutex
	counts := Counts{Manifests: map[string]int{}, Downloads: map[string]int{}}
	var selected *release.Artifact
	resolve := func(string, string) (*release.Artifact, error) {
		return selected, nil
	}
	handlers := make([]http.Handler, 2)
	for i, k := range []ed25519.PrivateKey{key, wrongKey} {
		h, err := release.NewHandler(release.HandlerConfig{Key: k, Resolve: resolve})
		if err != nil {
			return nil, fmt.Errorf("release handler: %w", err)
		}
		handlers[i] = h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		data, err := os.ReadFile(filepath.Join(dir, "offer.json")) //nolint:gosec // Test-owned selector.
		var offer Offer
		if err != nil || json.Unmarshal(data, &offer) != nil {
			http.Error(w, "invalid offer", http.StatusInternalServerError)
			return
		}
		selected = nil
		if variant, ok := wire.FindVariant(offer.ID); ok {
			selected = &release.Artifact{Path: filepath.Join(artifacts, variant.ID), Version: variant.Version, WatchdogVersion: variant.WatchdogVersion}
		}
		index := 0
		if offer.WrongKey {
			index = 1
		}
		handlers[index].ServeHTTP(w, r)
		if strings.Contains(r.URL.Path, "/manifest/") {
			counts.Manifests[offer.ID]++
		}
		if strings.Contains(r.URL.Path, "/download/") {
			counts.Downloads[offer.ID]++
		}
		if err := wire.WriteJSON(filepath.Join(dir, "counts.json"), counts); err != nil {
			panic(err)
		}
	}), nil
}
