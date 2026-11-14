// Package release signs and serves artifacts for daemonpilot's built-in digest verifier.
package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ovineko/daemonpilot/internal/crypto"
	"github.com/ovineko/daemonpilot/internal/updatehttp"
)

// SignDigest signs the SHA-256 digest, not the binary bytes. Like ed25519.Sign,
// it panics if priv is not an ed25519 private key of the correct length.
func SignDigest(priv ed25519.PrivateKey, digest [32]byte) []byte {
	return SignDigestContext(priv, "", digest)
}

// SignDigestContext signs context followed by the SHA-256 digest.
// Use a purpose and version ending in NUL, e.g. "myapp-binary-v1\x00", to
// separate key uses and delimit the context. Empty context matches SignDigest.
// Like ed25519.Sign, it panics for an invalid private-key length.
func SignDigestContext(priv ed25519.PrivateKey, context string, digest [32]byte) []byte {
	return ed25519.Sign(priv, crypto.DigestMessage(context, digest[:]))
}

// SignFile streams a regular file and returns its signature, hex SHA-256, and byte count.
func SignFile(priv ed25519.PrivateKey, path string) (sig []byte, sha256Hex string, size int64, err error) {
	return SignFileContext(priv, "", path)
}

// SignFileContext streams a regular file and signs its digest with SignDigestContext.
// It returns the signature, hex SHA-256, and byte count. Empty context matches SignFile.
func SignFileContext(priv ed25519.PrivateKey, context, path string) (sig []byte, sha256Hex string, size int64, err error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, "", 0, errors.New("invalid ed25519 private key length")
	}
	f, err := openArtifact(path)
	if err != nil {
		return nil, "", 0, err
	}
	defer func() { _ = f.Close() }()
	return signReader(priv, context, f)
}

func openArtifact(path string) (*os.File, error) {
	//nolint:gosec // Artifact paths are selected by the application's resolver or signer.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("stat artifact: %w", err)
		}
		return nil, errors.New("artifact must be a regular file")
	}
	return f, nil
}

func signReader(priv ed25519.PrivateKey, context string, r io.Reader) ([]byte, string, int64, error) {
	h := sha256.New()
	size, err := io.Copy(h, r)
	if err != nil {
		return nil, "", 0, fmt.Errorf("hash artifact: %w", err)
	}
	digest := [sha256.Size]byte(h.Sum(nil))
	return SignDigestContext(priv, context, digest), hex.EncodeToString(digest[:]), size, nil
}

// Artifact identifies an immutable release file and its application/watchdog versions.
type Artifact struct{ Path, Version, WatchdogVersion string }

// HandlerConfig configures signed manifest and download responses.
type HandlerConfig struct {
	Key ed25519.PrivateKey
	// SignatureContext is passed to SignDigestContext for every signature.
	// It is fixed per handler, so the handler's cache need not key on context.
	SignatureContext string
	// Resolve maps an allowed OS/architecture pair to an artifact; (nil, nil) means 404.
	Resolve func(goos, goarch string) (*Artifact, error)
	// Authorize defaults to open access. An error returns 401 without exposing the error.
	Authorize func(*http.Request) error
	// Header names default to X-Daemonpilot-Signature and X-Daemonpilot-Sha256.
	SignatureHeader, SHA256Header string
}

type signedArtifact struct {
	mtime             time.Time
	size              int64
	signature, digest string
}

type handler struct {
	cfg   HandlerConfig
	mu    sync.Mutex
	cache map[string]signedArtifact
}

// NewHandler serves GET and HEAD under any prefix at /manifest/{os}/{arch}
// and /download/{version}/{os}/{arch}. Downloads serve whatever Resolve returns,
// regardless of the requested version. Publish immutable files by atomic replacement.
func NewHandler(cfg HandlerConfig) (http.Handler, error) {
	if len(cfg.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid ed25519 private key length")
	}
	if cfg.Resolve == nil {
		return nil, errors.New("artifact resolver is required")
	}
	var err error
	cfg.SignatureHeader, cfg.SHA256Header, err = updatehttp.Headers(cfg.SignatureHeader, cfg.SHA256Header)
	if err != nil {
		return nil, fmt.Errorf("release headers: %w", err)
	}
	cfg.Key = append(ed25519.PrivateKey(nil), cfg.Key...)
	return &handler{cfg: cfg, cache: make(map[string]signedArtifact)}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Authorize != nil && h.cfg.Authorize(r) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kind, goos, goarch := route(r.URL.EscapedPath())
	if kind == "" {
		http.NotFound(w, r)
		return
	}
	artifact, err := h.cfg.Resolve(goos, goarch)
	if err != nil {
		http.Error(w, "resolve artifact failed", http.StatusInternalServerError)
		return
	}
	if artifact == nil {
		http.NotFound(w, r)
		return
	}
	f, err := openArtifact(artifact.Path)
	if err != nil {
		http.Error(w, "open artifact failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	signed, err := h.sign(artifact.Path, f)
	if err != nil {
		http.Error(w, "sign artifact failed", http.StatusInternalServerError)
		return
	}
	if kind == "manifest" {
		w.Header().Set("Content-Type", "application/json")
		body, err := json.Marshal(struct {
			Version         string `json:"version"`
			WatchdogVersion string `json:"watchdog_version"`
			Size            int64  `json:"size"`
			SHA256          string `json:"sha256"`
		}{artifact.Version, artifact.WatchdogVersion, signed.size, signed.digest})
		if err != nil {
			http.Error(w, "encode manifest failed", http.StatusInternalServerError)
			return
		}
		body = append(body, '\n')
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}
	w.Header().Set(h.cfg.SignatureHeader, signed.signature)
	w.Header().Set(h.cfg.SHA256Header, signed.digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", signed.mtime, f)
}

func route(path string) (kind, goos, goarch string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 3 {
		return "", "", ""
	}
	goos, goarch = parts[len(parts)-2], parts[len(parts)-1]
	if !allowedOS(goos) || !allowedArch(goarch) {
		return "", "", ""
	}
	if parts[len(parts)-3] == "manifest" {
		return "manifest", goos, goarch
	}
	if len(parts) >= 4 && parts[len(parts)-4] == "download" {
		version, err := url.PathUnescape(parts[len(parts)-3])
		if err == nil && version != "" {
			return "download", goos, goarch
		}
	}
	return "", "", ""
}

func allowedOS(value string) bool {
	switch value {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "js", "linux", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows":
		return true
	default:
		return false
	}
}

func allowedArch(value string) bool {
	switch value {
	case "386", "amd64", "arm", "arm64", "loong64", "mips", "mipsle", "mips64", "mips64le", "ppc64", "ppc64le", "riscv64", "s390x", "wasm":
		return true
	default:
		return false
	}
}

func (h *handler) sign(path string, f *os.File) (signedArtifact, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	info, err := f.Stat()
	if err != nil {
		return signedArtifact{}, fmt.Errorf("stat artifact: %w", err)
	}
	if cached, ok := h.cache[path]; ok && cached.size == info.Size() && cached.mtime.Equal(info.ModTime()) {
		return cached, nil
	}
	sig, digest, size, err := signReader(h.cfg.Key, h.cfg.SignatureContext, f)
	if err != nil {
		return signedArtifact{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return signedArtifact{}, fmt.Errorf("rewind artifact: %w", err)
	}
	after, err := f.Stat()
	if err != nil {
		return signedArtifact{}, fmt.Errorf("stat signed artifact: %w", err)
	}
	if size != info.Size() || !after.ModTime().Equal(info.ModTime()) || after.Size() != size {
		return signedArtifact{}, errors.New("artifact changed while signing")
	}
	result := signedArtifact{mtime: info.ModTime(), size: size, signature: hex.EncodeToString(sig), digest: digest}
	h.cache[path] = result
	return result, nil
}
