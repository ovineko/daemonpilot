//go:build e2e_docker

// Command relserver serves signed fixture releases inside the test network.
package main

import (
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/releasehost"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("dir", "/releases", "test control directory")
	artifacts := flag.String("artifacts", "/artifacts", "immutable builds")
	listen := flag.String("listen", ":8080", "HTTP address")
	flag.Parse()
	keys := make([]ed25519.PrivateKey, 2)
	for i, name := range []string{"key", "wrong-key"} {
		seed, err := os.ReadFile(filepath.Join(*dir, name)) //nolint:gosec // Test-generated keys in the isolated volume.
		if err != nil {
			return fmt.Errorf("read signing seed: %w", err)
		}
		if len(seed) != ed25519.SeedSize {
			return errors.New("invalid signing seed length")
		}
		keys[i] = ed25519.NewKeyFromSeed(seed)
	}
	h, err := releasehost.New(*dir, *artifacts, keys[0], keys[1])
	if err != nil {
		return fmt.Errorf("create release server: %w", err)
	}
	s := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 3 * time.Second}
	if err := s.ListenAndServe(); err != nil {
		return fmt.Errorf("serve releases: %w", err)
	}
	return nil
}
