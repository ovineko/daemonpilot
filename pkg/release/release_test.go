package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/updater"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	"github.com/ovineko/daemonpilot/pkg/httpsource"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestReleaseRoundTrip(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("release binary contents")
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, binary, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, context string }{
		{"legacy", ""},
		{"context", "myapp-binary-v1\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig, digest, size, err := SignFileContext(key, tc.context, path)
			hash := sha256.Sum256(binary)
			if err != nil || size != int64(len(binary)) || digest != hex.EncodeToString(hash[:]) || !bytes.Equal(sig, SignDigestContext(key, tc.context, hash)) {
				t.Fatalf("SignFile: %s %d %v", digest, size, err)
			}
			h, err := NewHandler(HandlerConfig{Key: key, SignatureContext: tc.context, Resolve: func(goos, goarch string) (*Artifact, error) {
				if goos != runtime.GOOS || goarch != runtime.GOARCH {
					t.Errorf("platform %s/%s", goos, goarch)
				}
				return &Artifact{Path: path, Version: "2", WatchdogVersion: "3"}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			for _, network := range []bool{false, true} {
				t.Run(map[bool]string{false: "in memory", true: "HTTP"}[network], func(t *testing.T) {
					base := "https://release.test"
					client := &http.Client{Transport: handlerTransport{handler: h}}
					if network {
						listener, err := net.Listen("tcp", "127.0.0.1:0")
						if err != nil {
							t.Skipf("host cannot bind HTTP test listener: %v", err)
						}
						server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: h, ReadHeaderTimeout: time.Second}}
						server.Start()
						defer server.Close()
						base, client = server.URL, server.Client()
					}
					source, err := httpsource.New(httpsource.Config{Client: client, ManifestURL: base + "/manifest/{os}/{arch}", DownloadURL: base + "/download/{version}/{os}/{arch}"})
					if err != nil {
						t.Fatal(err)
					}
					info, err := source.CheckUpdate(context.Background(), "1")
					if err != nil || info == nil {
						t.Fatalf("manifest: %+v %v", info, err)
					}
					if info.Version != "2" || info.WatchdogVersion != "3" || info.SHA256 != digest || info.Size != size {
						t.Fatalf("manifest: %+v", info)
					}
					info.Version = "requested-old-version"
					u := updater.New(&daemonpilot.UpdateConfig{Source: source, PublicKey: pub, SignatureContext: tc.context}, slog.New(slog.DiscardHandler), t.TempDir(), "service")
					staged, err := u.DownloadAndVerify(context.Background(), info)
					if err != nil {
						t.Fatal(err)
					}

					got, err := os.ReadFile(staged)
					if err != nil || !bytes.Equal(got, binary) {
						t.Fatalf("staged: %q %v", got, err)
					}
				})
			}
		})
	}
}

func TestSignContext(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("artifact")
	digest := sha256.Sum256(data)
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, context := range []string{"", "myapp-binary-v1\x00"} {
		t.Run(context, func(t *testing.T) {
			want := ed25519.Sign(key, append([]byte(context), digest[:]...))
			if got := SignDigestContext(key, context, digest); !bytes.Equal(got, want) {
				t.Fatal("digest signature differs from exact context-prefixed message")
			}
			sig, hash, size, err := SignFileContext(key, context, path)
			if err != nil || !bytes.Equal(sig, want) || hash != hex.EncodeToString(digest[:]) || size != int64(len(data)) {
				t.Fatalf("file signature = %x, hash = %s, size = %d, error = %v", sig, hash, size, err)
			}
		})
	}
	sig, _, _, err := SignFile(key, path)
	if err != nil || !ed25519.Verify(pub, digest[:], sig) || !bytes.Equal(sig, SignDigest(key, digest)) {
		t.Fatalf("legacy signature changed: %v", err)
	}
}

func TestHandlerRoutes(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path      string
		denied, missing, broken bool
		status                  int
	}{
		{name: "manifest", method: "GET", path: "/prefix/manifest/linux/amd64", status: 200},
		{name: "head manifest", method: "HEAD", path: "/manifest/linux/amd64", status: 200},
		{name: "head download", method: "HEAD", path: "/download/any/linux/amd64", status: 200},
		{name: "escaped version", method: "GET", path: "/download/a%2Fb/linux/amd64", status: 200},
		{name: "traversal", method: "GET", path: "/manifest/../amd64", status: 404},
		{name: "encoded traversal", method: "GET", path: "/manifest/%2e%2e/amd64", status: 404},
		{name: "unknown arch", method: "GET", path: "/manifest/linux/nope", status: 404},
		{name: "method", method: "POST", path: "/manifest/linux/amd64", status: 405},
		{name: "authorization", method: "GET", path: "/manifest/linux/amd64", denied: true, status: 401},
		{name: "absent", method: "GET", path: "/manifest/linux/amd64", missing: true, status: 404},
		{name: "resolver error", method: "GET", path: "/manifest/linux/amd64", broken: true, status: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewHandler(HandlerConfig{Key: key, Resolve: func(string, string) (*Artifact, error) {
				if tc.broken {
					return nil, errors.New("private error")
				}
				if tc.missing {
					return nil, nil
				}
				return &Artifact{Path: path, Version: "1"}, nil
			}, Authorize: func(*http.Request) error {
				if tc.denied {
					return errors.New("secret")
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if tc.method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD returned body")
			}
			if tc.name == "head download" && (w.Header().Get("Content-Length") != "4" || w.Header().Get("X-Daemonpilot-Signature") == "") {
				t.Fatal(w.Header())
			}
		})
	}
}

func TestSignatureCache(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &handler{cfg: HandlerConfig{Key: key}, cache: make(map[string]signedArtifact)}
	f, err := openArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	first, err := h.sign(path, f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	cached, err := h.sign(path, f)
	if err != nil || cached != first {
		t.Fatalf("cache: %+v %v", cached, err)
	}
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil || pos != 1 {
		t.Fatal("cache hit read the file")
	}
	_ = f.Close()
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(testsupport.Scale(time.Second))
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	next, err := openArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	changed, err := h.sign(path, next)
	if err != nil || changed.digest == first.digest {
		t.Fatalf("cache not refreshed: %+v %v", changed, err)
	}
}

type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, r)
	return recorder.Result(), nil
}
