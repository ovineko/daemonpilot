package httpsource

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

func TestManifest(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "network"}[network], func(t *testing.T) { testManifest(t, network) })
	}
}

func testManifest(t *testing.T, network bool) {
	t.Helper()
	for _, tc := range []struct {
		name           string
		status         int
		body, current  string
		offer, wantErr bool
	}{
		{"offer", 200, `{"version":"2","watchdog_version":"3","size":9,"sha256":"abc"}`, "1", true, false},
		{"equal", 200, `{"version":"2"}`, "2", false, false},
		{"empty", 204, "", "1", false, false},
		{"missing", 404, "", "1", false, false},
		{"bad JSON", 200, "{", "1", false, true},
		{"bad version", 200, `{}`, "1", false, true},
		{"server error", 500, "failure", "1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newHTTPTestServer(t, network, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/manifest/"+runtime.GOOS+"/"+runtime.GOARCH {
					t.Errorf("path: %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.close()
			s := newTestSource(t, Config{Client: server.client, ManifestURL: server.URL + "/manifest/{os}/{arch}", DownloadURL: server.URL + "/download/{version}/{os}/{arch}"})
			info, err := s.CheckUpdate(context.Background(), tc.current)
			if (err != nil) != tc.wantErr || (info != nil) != tc.offer {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			if info != nil && (info.WatchdogVersion != "3" || info.Size != 9 || info.SHA256 != "abc") {
				t.Fatalf("info=%+v", info)
			}
		})
	}
	s := newTestSource(t, Config{DownloadURL: "https://example.com/download/{version}"})
	if info, err := s.CheckUpdate(context.Background(), "1"); info != nil || err != nil {
		t.Fatalf("push only: %v %v", info, err)
	}
}

func newTestSource(t *testing.T, cfg Config) *Source {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDownload(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "network"}[network], func(t *testing.T) { testDownload(t, network) })
	}
}

func testDownload(t *testing.T, network bool) {
	t.Helper()
	for _, tc := range []struct {
		name, signature, digest string
		status                  int
		preserve, wantErr       bool
	}{
		{"headers", strings.Repeat("ab", 64), strings.Repeat("cd", 32), 200, false, false},
		{"preserve manifest", strings.Repeat("ab", 64), "invalid ignored header", 200, true, false},
		{"optional hash", strings.Repeat("ab", 64), "", 200, false, false},
		{"missing signature", "", "", 200, false, true},
		{"bad signature", "zz", "", 200, false, true},
		{"bad hash", strings.Repeat("ab", 64), "zz", 200, false, true},
		{"status", "", "", 503, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newHTTPTestServer(t, network, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != "/download/a%2Fb/"+runtime.GOOS+"/"+runtime.GOARCH {
					t.Errorf("path: %s", r.URL.EscapedPath())
				}
				w.Header().Set("Content-Length", "4")
				w.Header().Set("Sig", tc.signature)
				w.Header().Set("Hash", tc.digest)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "body")
			}))
			defer server.close()
			s := newTestSource(t, Config{Client: server.client, DownloadURL: server.URL + "/download/{version}/{os}/{arch}", SignatureHeader: "Sig", SHA256Header: "Hash"})
			info := &daemonpilot.UpdateInfo{Version: "a/b"}
			if tc.preserve {
				info.Size = 100
				info.SHA256 = "manifest"
			}
			body, sig, err := s.DownloadUpdate(context.Background(), info)
			if body != nil {
				defer func() { _ = body.Close() }()
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("error: %v", err)
			}
			if err != nil {
				return
			}
			if hex.EncodeToString(sig) != tc.signature {
				t.Fatal("signature changed")
			}
			if tc.preserve {
				if info.Size != 100 || info.SHA256 != "manifest" {
					t.Fatalf("overwrote manifest: %+v", info)
				}
			} else if info.Size != 4 || info.SHA256 != tc.digest {
				t.Fatalf("headers: %+v", info)
			}
		})
	}
}

func TestFallbackAndAuthorization(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "network"}[network], func(t *testing.T) { testFallbackAndAuthorization(t, network) })
	}
}

func testFallbackAndAuthorization(t *testing.T, network bool) {
	t.Helper()
	for _, tc := range []struct {
		name, version         string
		fallbackStatus, calls int
	}{
		{"success", "1", 200, 2}, {"once", "1", 404, 2}, {"already fallback", "latest", 404, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, authCalls := 0, 0
			server := newHTTPTestServer(t, network, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer token" {
					t.Error("missing authorization")
				}
				status := 404
				if r.URL.Path == "/latest" {
					status = tc.fallbackStatus
				}
				w.Header().Set("X-Daemonpilot-Signature", strings.Repeat("ab", 64))
				w.WriteHeader(status)
			}))
			defer server.close()
			s := newTestSource(t, Config{Client: server.client, DownloadURL: server.URL + "/{version}", FallbackVersion: "latest", Authorize: func(r *http.Request) error { authCalls++; r.Header.Set("Authorization", "Bearer token"); return nil }})
			body, _, err := s.DownloadUpdate(context.Background(), &daemonpilot.UpdateInfo{Version: tc.version})
			if body != nil {
				_ = body.Close()
			}
			if (err != nil) != (tc.fallbackStatus != 200) || calls != tc.calls || authCalls != tc.calls {
				t.Fatalf("calls=%d auth=%d err=%v", calls, authCalls, err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	reads  int
	closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	b.reads++
	return copy(p, strings.Repeat("x", len(p))), nil
}
func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestStreamingAndErrorClosure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		signature string
		wantErr   bool
	}{
		{"stream", 200, strings.Repeat("ab", 64), false}, {"missing signature", 200, "", true}, {"status", 500, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &trackedBody{}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"X-Daemonpilot-Signature": []string{tc.signature}}, Body: b}, nil
			})}
			s := newTestSource(t, Config{Client: client, DownloadURL: "https://example.com/{version}"})
			body, _, err := s.DownloadUpdate(context.Background(), &daemonpilot.UpdateInfo{Version: "1"})
			if (err != nil) != tc.wantErr || b.closed != tc.wantErr {
				t.Fatalf("closed=%v err=%v", b.closed, err)
			}
			if !tc.wantErr {
				if b.reads != 0 {
					t.Fatal("eager read")
				}
				_ = body.Close()
			}
			if tc.status == 500 {
				var status *StatusError
				if !errors.As(err, &status) || status.StatusCode != http.StatusInternalServerError || len(status.Body) != 512 {
					t.Fatalf("status: %v", err)
				}
			}
		})
	}
}

func TestConfigAndAuthFailure(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{DownloadURL: "file:///binary"},
		{DownloadURL: "https://example.com/{unknown}"},
		{DownloadURL: "https://example.com/{version}", ManifestURL: "https://example.com/{version}"},
		{DownloadURL: "https://example.com", SignatureHeader: "bad\nheader"},
		{DownloadURL: "https://example.com", SignatureHeader: "same", SHA256Header: "Same"},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	denied := errors.New("no token")
	s := newTestSource(t, Config{DownloadURL: "https://example.com/{version}", Authorize: func(*http.Request) error { return denied }})
	if _, _, err := s.DownloadUpdate(context.Background(), &daemonpilot.UpdateInfo{}); !errors.Is(err, denied) {
		t.Fatalf("auth: %v", err)
	}
}

type testEndpoint struct {
	URL    string
	client *http.Client
	close  func()
}

func newHTTPTestServer(t *testing.T, network bool, h http.Handler) testEndpoint {
	t.Helper()
	if !network {
		return testEndpoint{URL: "https://test.example", close: func() {}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, r)
			return recorder.Result(), nil
		})}}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("host cannot bind HTTP test listener: %v", err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: h, ReadHeaderTimeout: time.Second}}
	server.Start()
	return testEndpoint{URL: server.URL, client: server.Client(), close: server.Close}
}

// The manifest is authoritative for the expected digest: the download header
// only fills a silent offer, never overwrites a manifest value and never
// injects a SHA-256 alias next to a custom-hash Digest.
func TestDownloadHeaderDigestPolicy(t *testing.T) {
	validSHA256 := strings.Repeat("cd", 32)
	otherSHA256 := strings.Repeat("ef", 32)
	sha384 := strings.Repeat("38", 48)

	for _, tc := range []struct {
		name                   string
		manifestDigest, manSHA string
		headerSHA              string
		wantDigest, wantSHA256 string
		wantErr                bool
	}{
		{"silent offer filled from header", "", "", validSHA256, "", validSHA256, false},
		{"custom digest ignores the header", sha384, "", validSHA256, sha384, "", false},
		{"manifest sha256 kept over header", "", validSHA256, otherSHA256, "", validSHA256, false},
		{"short header refused", "", "", strings.Repeat("cd", 8), "", "", true},
		{"non-hex header refused", "", "", "zz", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newHTTPTestServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() == "/manifest" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"version":"2.0.0","digest":%q,"sha256":%q,"size":4}`, tc.manifestDigest, tc.manSHA)
					return
				}
				w.Header().Set("Content-Length", "4")
				w.Header().Set("Sig", strings.Repeat("ab", 64))
				w.Header().Set("Hash", tc.headerSHA)
				_, _ = io.WriteString(w, "body")
			}))
			defer server.close()
			s := newTestSource(t, Config{Client: server.client, ManifestURL: server.URL + "/manifest", DownloadURL: server.URL + "/dl", SignatureHeader: "Sig", SHA256Header: "Hash"})
			info, err := s.CheckUpdate(context.Background(), "1.0.0")
			if err != nil {
				t.Fatalf("check update: %v", err)
			}
			body, _, err := s.DownloadUpdate(context.Background(), info)
			if body != nil {
				defer func() { _ = body.Close() }()
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("error: %v", err)
			}
			if err != nil {
				return
			}
			if info.Digest != tc.wantDigest || info.SHA256 != tc.wantSHA256 {
				t.Fatalf("digest=%q sha256=%q, want digest=%q sha256=%q", info.Digest, info.SHA256, tc.wantDigest, tc.wantSHA256)
			}
		})
	}
}
