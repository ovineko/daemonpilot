// Package httpsource streams signed HTTP artifacts into daemonpilot's update verifier.
package httpsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/ovineko/daemonpilot/internal/updatehttp"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

// Config describes a manifest endpoint and a streaming download endpoint.
type Config struct {
	// Client defaults to a client with a 30-second response-header timeout and no body timeout.
	Client *http.Client
	// ManifestURL expands {os} and {arch}. Empty disables polling; 204 and 404 mean no update.
	ManifestURL string
	// DownloadURL is required and expands path-escaped {version}, {os}, and {arch}.
	DownloadURL string
	// Authorize decorates every outgoing request, including redirects and fallback requests.
	Authorize func(*http.Request) error
	// SignatureHeader defaults to X-Daemonpilot-Signature and carries the
	// hex-encoded signature for the configured verifier (Ed25519 by default;
	// custom VerifyMessage implementations define their own lengths).
	SignatureHeader string
	// SHA256Header defaults to X-Daemonpilot-Sha256; it is optional on the wire.
	SHA256Header string
	// FallbackVersion is tried once after a concrete version returns 404. Empty disables it.
	FallbackVersion string
}

// Source implements daemonpilot.UpdateSource. Construct it with New.
type Source struct {
	cfg    Config
	client *http.Client
}

// StatusError describes a non-2xx response, with at most 512 bytes of body.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body) }

// New validates URL templates and header names and applies defaults.
func New(cfg Config) (*Source, error) {
	if cfg.DownloadURL == "" {
		return nil, errors.New("download URL is required")
	}
	for _, template := range []struct {
		value   string
		version bool
	}{{cfg.ManifestURL, false}, {cfg.DownloadURL, true}} {
		if template.value == "" {
			continue
		}
		if err := validateTemplate(template.value, template.version); err != nil {
			return nil, err
		}
	}
	var err error
	cfg.SignatureHeader, cfg.SHA256Header, err = updatehttp.Headers(cfg.SignatureHeader, cfg.SHA256Header)
	if err != nil {
		return nil, fmt.Errorf("HTTP source headers: %w", err)
	}
	client := http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
	if cfg.Client != nil {
		client = *cfg.Client
	}
	if cfg.Authorize != nil {
		transport := client.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		client.Transport = authorizedTransport{base: transport, authorize: cfg.Authorize}
	}
	return &Source{cfg: cfg, client: &client}, nil
}

func validateTemplate(template string, version bool) error {
	u, err := url.Parse(template)
	if err != nil {
		return fmt.Errorf("parse URL template: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" || u.User != nil {
		return errors.New("URL template requires an absolute HTTP(S) URL without credentials or fragment")
	}
	if strings.ContainsAny(u.Host+u.RawQuery, "{}") {
		return errors.New("URL placeholders must be in the path")
	}
	path := strings.NewReplacer("{os}", "linux", "{arch}", "amd64").Replace(u.Path)
	if version {
		path = strings.ReplaceAll(path, "{version}", "version")
	}
	if strings.ContainsAny(path, "{}") {
		return errors.New("unknown URL template placeholder")
	}
	return nil
}

func expand(template, version string) string {
	return strings.NewReplacer("{os}", url.PathEscape(runtime.GOOS), "{arch}", url.PathEscape(runtime.GOARCH), "{version}", url.PathEscape(version)).Replace(template)
}

type authorizedTransport struct {
	base      http.RoundTripper
	authorize func(*http.Request) error
}

func (t authorizedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if err := t.authorize(req); err != nil {
		return nil, fmt.Errorf("authorize update request: %w", err)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("send update request: %w", err)
	}
	return resp, nil
}

func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &StatusError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
}

// CheckUpdate reports no offer for an empty URL, 204, 404, or the current version.
func (s *Source) CheckUpdate(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error) {
	info, available, err := s.checkManifest(ctx, currentVersion)
	if err != nil {
		return nil, err
	}
	var offer *daemonpilot.UpdateInfo
	if available {
		offer = &info
	}
	return offer, nil
}

// DownloadUpdate returns an unread body and decoded signature. The caller must close it.
// Manifest hash and size take precedence over response headers.
func (s *Source) DownloadUpdate(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
	if info == nil {
		return nil, nil, errors.New("update info is required")
	}
	resp, err := s.get(ctx, expand(s.cfg.DownloadURL, info.Version))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusNotFound && info.Version != "" && s.cfg.FallbackVersion != "" && info.Version != s.cfg.FallbackVersion {
		_ = resp.Body.Close()
		resp, err = s.get(ctx, expand(s.cfg.DownloadURL, s.cfg.FallbackVersion))
		if err != nil {
			return nil, nil, err
		}
	}
	body, signature, err := s.downloadHeaders(resp, info)
	if err != nil {
		_ = resp.Body.Close()
	}
	return body, signature, err
}

func (s *Source) checkManifest(ctx context.Context, currentVersion string) (daemonpilot.UpdateInfo, bool, error) {
	if s.cfg.ManifestURL == "" {
		return daemonpilot.UpdateInfo{}, false, nil
	}
	resp, err := s.get(ctx, expand(s.cfg.ManifestURL, ""))
	if err != nil {
		return daemonpilot.UpdateInfo{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return daemonpilot.UpdateInfo{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return daemonpilot.UpdateInfo{}, false, statusError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return daemonpilot.UpdateInfo{}, false, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) > 1<<20 {
		return daemonpilot.UpdateInfo{}, false, errors.New("manifest exceeds one MiB")
	}
	var info daemonpilot.UpdateInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return daemonpilot.UpdateInfo{}, false, fmt.Errorf("decode manifest: %w", err)
	}
	if info.Version == "" || info.Size < 0 {
		return daemonpilot.UpdateInfo{}, false, errors.New("invalid manifest version or size")
	}
	if info.Version == currentVersion {
		return daemonpilot.UpdateInfo{}, false, nil
	}
	return info, true, nil
}

func (s *Source) downloadHeaders(resp *http.Response, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, statusError(resp)
	}
	// The signature is opaque bytes for the configured verifier: the built-in
	// Ed25519 check validates its own 64-byte size, while VerifyMessage
	// implementations define their own shapes and lengths. Only emptiness is
	// an HTTP-level failure here.
	signature, err := hex.DecodeString(resp.Header.Get(s.cfg.SignatureHeader))
	if err != nil || len(signature) == 0 {
		return nil, nil, fmt.Errorf("missing or invalid %s header", s.cfg.SignatureHeader)
	}
	digest := resp.Header.Get(s.cfg.SHA256Header)
	// The manifest is authoritative for the expected digest: the download
	// header only fills an offer that declares no digest of its own, so it
	// can neither overwrite a manifest value nor create a Digest/SHA256
	// conflict that DownloadAndVerify would have to reject. A custom
	// release-hash manifest (Digest set) never receives a SHA-256 alias from
	// the header — sums of different algorithms are not spellings of one
	// value.
	if info.Digest == "" && info.SHA256 == "" && digest != "" {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size {
			return nil, nil, fmt.Errorf("invalid %s header", s.cfg.SHA256Header)
		}
		info.SHA256 = digest
	}
	if info.Size == 0 && resp.ContentLength > 0 {
		info.Size = resp.ContentLength
	}
	return resp.Body, signature, nil
}

func (s *Source) get(ctx context.Context, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create update request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request update: %w", err)
	}
	return resp, nil
}
