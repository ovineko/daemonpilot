package updater

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/crypto"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	"github.com/ovineko/daemonpilot/pkg/httpsource"
)

var errVerifyRejected = errors.New("rejected by test verifier")

func mustEdPriv(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// The extension tests cover the four verifier/hash combinations plus the
// negatives: a custom verifier or hash never disables the other check, never
// falls back to Ed25519, and never permits an unsigned apply.

const extensionPayload = "extension-payload"

func extensionInfo(t *testing.T, payload []byte, digest string) *daemonpilot.UpdateInfo {
	t.Helper()
	return &daemonpilot.UpdateInfo{
		Version:         "2.0.0",
		WatchdogVersion: "1",
		Size:            int64(len(payload)),
		SHA256:          digest,
	}
}

// downloadExtension runs one DownloadAndVerify against a mock source and
// returns the staged path — valid for statting both after a success and
// after a rejection — plus the verification error.
func downloadExtension(t *testing.T, payload []byte, signature []byte, info *daemonpilot.UpdateInfo, cfg *daemonpilot.UpdateConfig) (string, error) {
	t.Helper()
	source := &mockUpdateSource{
		downloadUpdateFn: func(context.Context, *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(payload)), signature, nil
		},
	}
	cfg.Source = source
	u := New(cfg, newTestLogger(), t.TempDir(), "myapp")
	staged := daemonpilot.StagedNewPath(filepath.Join(u.installDir, u.binaryName))
	_, err := u.DownloadAndVerify(context.Background(), info)
	return staged, err
}

// Default hash, default signature: the pre-extension contract must keep
// working unchanged, including the SignatureContext bytes.
func TestExtensionDefaultHashDefaultSignature(t *testing.T) {
	payload := []byte(extensionPayload)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const context = "myapp-binary-v1\x00"
	digest := sha256.Sum256(payload)
	signature := ed25519.Sign(priv, crypto.DigestMessage(context, digest[:]))

	t.Run("verifies", func(t *testing.T) {
		path, err := downloadExtension(t, payload, signature, extensionInfo(t, payload, hex.EncodeToString(digest[:])),
			&daemonpilot.UpdateConfig{PublicKey: pub, SignatureContext: context})
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		wantSuffix := ".new" + executableExtension()
		if !strings.HasSuffix(path, wantSuffix) {
			t.Fatalf("staged path = %q, want suffix %q", path, wantSuffix)
		}
	})
	t.Run("wrong context rejected", func(t *testing.T) {
		_, err := downloadExtension(t, payload, signature, extensionInfo(t, payload, hex.EncodeToString(digest[:])),
			&daemonpilot.UpdateConfig{PublicKey: pub, SignatureContext: "other\x00"})
		if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
			t.Fatalf("wrong context must fail signature verification, got %v", err)
		}
	})
	t.Run("wrong digest rejected", func(t *testing.T) {
		_, err := downloadExtension(t, payload, signature, extensionInfo(t, payload, strings.Repeat("ab", 32)),
			&daemonpilot.UpdateConfig{PublicKey: pub, SignatureContext: context})
		if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("wrong digest must fail the hash check, got %v", err)
		}
	})
}

// Custom hash, default signature: Ed25519 over SignatureContext || SHA-512
// digest. No Ed25519-independent requirements appear; only the hasher and
// the manifest digest field change.
func TestExtensionCustomHashDefaultSignature(t *testing.T) {
	payload := []byte(extensionPayload)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha512.Sum512(payload)
	signature := ed25519.Sign(priv, crypto.DigestMessage("", digest[:]))
	info := extensionInfo(t, payload, "")
	info.Digest = hex.EncodeToString(digest[:])

	cfg := &daemonpilot.UpdateConfig{
		PublicKey: pub,
		NewHash:   func() hash.Hash { return sha512.New() },
	}
	if _, err := downloadExtension(t, payload, signature, info, cfg); err != nil {
		t.Fatalf("sha512 digest must verify under built-in Ed25519: %v", err)
	}

	// A sha256 field under a custom hash must not pass silently — even when
	// it holds exactly the configured digest, the alias is incompatible.
	for _, alias := range []string{
		hex.EncodeToString(digest[:])[:2*sha256.Size],
		hex.EncodeToString(digest[:]),
	} {
		bad := *info
		bad.SHA256 = alias
		if _, err := downloadExtension(t, payload, signature, &bad, cfg); err == nil || !strings.Contains(err.Error(), "digest fields disagree") {
			t.Fatalf("sha256 alias %q under a custom hash must be rejected, got %v", alias, err)
		}
	}
}

// Custom signature, default hash: an ECDSA P-256 verifier over the digest
// message replaces Ed25519, with no public key in the configuration at all.
func TestExtensionCustomVerifierDefaultHash(t *testing.T) {
	payload := []byte(extensionPayload)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	message := crypto.DigestMessage("ecdsa-v1\x00", digest[:])
	hashed := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, key, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	verifier := func(message, signature []byte) error {
		hashed := sha256.Sum256(message)
		if !ecdsa.VerifyASN1(&key.PublicKey, hashed[:], signature) {
			return errVerifyRejected
		}
		return nil
	}

	t.Run("verifies without an Ed25519 key", func(t *testing.T) {
		cfg := &daemonpilot.UpdateConfig{Source: &mockUpdateSource{}, VerifyMessage: verifier, SignatureContext: "ecdsa-v1\x00"}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("custom verifier must not require a public key: %v", err)
		}
		if _, err := downloadExtension(t, payload, signature, extensionInfo(t, payload, hex.EncodeToString(digest[:])), cfg); err != nil {
			t.Fatalf("ecdsa verifier: %v", err)
		}
	})
	t.Run("callback failure never falls back", func(t *testing.T) {
		rejecting := func(message, signature []byte) error { return errVerifyRejected }
		cfg := &daemonpilot.UpdateConfig{VerifyMessage: rejecting}
		path, err := downloadExtension(t, payload, ed25519.Sign(mustEdPriv(t), crypto.DigestMessage("", digest[:])), extensionInfo(t, payload, hex.EncodeToString(digest[:])), cfg)
		if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
			t.Fatalf("rejected signature must abort, got %v", err)
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatal("failed verification must remove the staged file")
		}
	})
	t.Run("malformed signature rejected without panic", func(t *testing.T) {
		cfg := &daemonpilot.UpdateConfig{VerifyMessage: verifier, SignatureContext: "ecdsa-v1\x00"}
		if _, err := downloadExtension(t, payload, []byte{0x30, 0x00}, extensionInfo(t, payload, hex.EncodeToString(digest[:])), cfg); err == nil {
			t.Fatal("malformed signature must be rejected")
		}
	})
}

// Custom hash and custom signature together, end to end through one
// DownloadAndVerify: SHA-384 digest plus an ECDSA verifier over that digest.
func TestExtensionCustomHashCustomVerifier(t *testing.T) {
	payload := []byte(extensionPayload)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha512.Sum384(payload)
	message := crypto.DigestMessage("p384-v1\x00", digest[:])
	hashed := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, key, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	info := extensionInfo(t, payload, "")
	info.Digest = hex.EncodeToString(digest[:])
	cfg := &daemonpilot.UpdateConfig{
		SignatureContext: "p384-v1\x00",
		VerifyMessage: func(message, signature []byte) error {
			hashed := sha256.Sum256(message)
			if !ecdsa.VerifyASN1(&key.PublicKey, hashed[:], signature) {
				return errVerifyRejected
			}
			return nil
		},
		NewHash: func() hash.Hash { return sha512.New384() },
	}
	if _, err := downloadExtension(t, payload, signature, info, cfg); err != nil {
		t.Fatalf("sha384+ecdsa: %v", err)
	}
}

// The built-in chain keeps its requirements: nil callbacks with no usable
// key must refuse the configuration, and a truncated download must fail the
// size check independently of signatures.
func TestExtensionDefaultsStayEnforced(t *testing.T) {
	cfg := &daemonpilot.UpdateConfig{Source: &mockUpdateSource{}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "PublicKey is required") {
		t.Fatalf("unsigned apply must stay impossible, got %v", err)
	}
	both := &daemonpilot.UpdateConfig{Source: &mockUpdateSource{}, VerifyFunc: func(string, []byte) error { return nil }, VerifyMessage: func([]byte, []byte) error { return nil }}
	if err := both.Validate(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("two verification points must be rejected, got %v", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(extensionPayload)
	digest := sha256.Sum256(payload)
	signature := ed25519.Sign(priv, crypto.DigestMessage("", digest[:]))
	info := extensionInfo(t, payload, hex.EncodeToString(digest[:]))
	info.Size = int64(len(payload)) + 1 // truncated download
	truncating := &mockUpdateSource{
		downloadUpdateFn: func(context.Context, *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			return io.NopCloser(bytes.NewReader(payload)), signature, nil
		},
	}
	cfg2 := &daemonpilot.UpdateConfig{PublicKey: pub, Source: truncating}
	u := New(cfg2, newTestLogger(), t.TempDir(), "myapp")
	if _, err := u.DownloadAndVerify(context.Background(), info); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("truncated download must fail the size check, got %v", err)
	}
}

// The header-vs-manifest policy regression, end to end at the updater
// boundary: a custom-hash offer that carries only the SHA-256 alias must
// fail with the configuration hint, while the same offer carrying the
// configured digest must verify.
func TestExtensionCustomHashManifestAliasHint(t *testing.T) {
	payload := []byte(extensionPayload)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest384 := sha512.Sum384(payload)
	digest256 := sha256.Sum256(payload)
	signature := ed25519.Sign(priv, crypto.DigestMessage("", digest384[:]))

	info := extensionInfo(t, payload, "")
	info.Digest = hex.EncodeToString(digest384[:])
	cfg := &daemonpilot.UpdateConfig{
		PublicKey: pub,
		NewHash:   func() hash.Hash { return sha512.New384() },
	}
	if _, err := downloadExtension(t, payload, signature, info, cfg); err != nil {
		t.Fatalf("custom digest must verify: %v", err)
	}

	aliasOnly := extensionInfo(t, payload, hex.EncodeToString(digest256[:]))
	if _, err := downloadExtension(t, payload, signature, aliasOnly, cfg); err == nil ||
		!strings.Contains(err.Error(), "provide the digest field") {
		t.Fatalf("sha256 alias under a custom hash must fail with the config hint, got %v", err)
	}
}

// TestDigestFieldPolicy pins the manifest digest-field policy for every
// shape under the default hash and a configured hash: which field is
// compared, which combinations are rejected before the download is even
// judged, and that a skipped digest comparison never skips the signature.
func TestDigestFieldPolicy(t *testing.T) {
	payload := []byte(extensionPayload)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest256 := sha256.Sum256(payload)
	digest384 := sha512.Sum384(payload)
	other384 := sha512.Sum384([]byte("other-payload"))
	hex256 := hex.EncodeToString(digest256[:])
	hex384 := hex.EncodeToString(digest384[:])
	sign256 := ed25519.Sign(priv, crypto.DigestMessage("", digest256[:]))
	sign384 := ed25519.Sign(priv, crypto.DigestMessage("", digest384[:]))
	garbage := make([]byte, ed25519.SignatureSize)
	newSHA384 := func() hash.Hash { return sha512.New384() }
	newSHA256 := func() hash.Hash { return sha256.New() }

	for _, tc := range []struct {
		name      string
		digest    string
		sha256    string
		newHash   func() hash.Hash
		signature []byte
		wantErr   string
	}{
		{"default sha256 field verifies", "", hex256, nil, sign256, ""},
		{"default digest field verifies the sha256", hex256, "", nil, sign256, ""},
		{"default both equal verifies case-insensitively", strings.ToUpper(hex256), hex256, nil, sign256, ""},
		{"default both conflicting reject", hex256, strings.Repeat("cd", 32), nil, sign256, "digest fields disagree"},
		{"default both non-hex reject", "not-hex", hex256, nil, sign256, "not valid hex"},
		{"default both wrong length reject even when equal", hex256[:62], hex256[:62], nil, sign256, "want 64"},
		{"default non-hex sha256 field reject", "", "not-hex", nil, sign256, "not valid hex"},
		{"default short sha256 field reject", "", hex256[:62], nil, sign256, "want 64"},
		{"default non-hex digest field reject", "not-hex", "", nil, sign256, "not valid hex"},
		{"custom both equal reject", hex384, hex384, newSHA384, sign384, "digest fields disagree"},
		{"custom genuine sha256 alias reject", "", hex256, newSHA384, sign256, "provide the digest field"},
		{"explicit sha256.New reads the digest field", hex256, "", newSHA256, sign256, ""},
		{"explicit sha256.New still rejects the sha256 alias", "", hex256, newSHA256, sign256, "digest fields disagree"},
		{"custom digest of wrong length reject", hex256, "", newSHA384, sign384, "want 96"},
		{"custom non-hex digest reject", "not-hex", "", newSHA384, sign384, "not valid hex"},
		{"custom digest mismatch reject", hex.EncodeToString(other384[:]), "", newSHA384, sign384, "digest mismatch"},
		{"both empty default skips digest but keeps signature", "", "", nil, garbage, "signature verification failed"},
		{"both empty custom skips digest but keeps signature", "", "", newSHA384, garbage, "signature verification failed"},
		{"both empty custom verifies on signature alone", "", "", newSHA384, sign384, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &daemonpilot.UpdateInfo{
				Version:         "2.0.0",
				WatchdogVersion: "1",
				Size:            int64(len(payload)),
				Digest:          tc.digest,
				SHA256:          tc.sha256,
			}
			path, err := downloadExtension(t, payload, tc.signature, info,
				&daemonpilot.UpdateConfig{PublicKey: pub, NewHash: tc.newHash})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("verify: %v", err)
				}
				if _, statErr := os.Stat(path); statErr != nil {
					t.Fatalf("verified offer must keep the staged file: %v", statErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatal("rejected offer must remove the staged file")
			}
		})
	}
}

// The digest fields are judged as downloaded, not as offered: a source may
// rewrite the offer inside DownloadUpdate, and a sha256 field appearing only
// there rejects the offer even when it holds exactly the configured digest.
func TestDigestPolicyRejectsSHA256IntroducedDuringDownload(t *testing.T) {
	payload := []byte(extensionPayload)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest384 := sha512.Sum384(payload)
	signature := ed25519.Sign(priv, crypto.DigestMessage("", digest384[:]))
	source := &mockUpdateSource{
		downloadUpdateFn: func(_ context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
			info.SHA256 = hex.EncodeToString(digest384[:])
			return io.NopCloser(bytes.NewReader(payload)), signature, nil
		},
	}
	u := New(&daemonpilot.UpdateConfig{
		Source:    source,
		PublicKey: pub,
		NewHash:   func() hash.Hash { return sha512.New384() },
	}, newTestLogger(), t.TempDir(), "myapp")

	info := &daemonpilot.UpdateInfo{
		Version:         "2.0.0",
		WatchdogVersion: "1",
		Size:            int64(len(payload)),
		Digest:          hex.EncodeToString(digest384[:]),
	}
	staged := daemonpilot.StagedNewPath(filepath.Join(u.installDir, u.binaryName))
	_, err = u.DownloadAndVerify(context.Background(), info)
	if err == nil || !strings.Contains(err.Error(), "digest fields disagree") {
		t.Fatalf("sha256 introduced during download must reject the offer, got %v", err)
	}
	if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
		t.Fatal("rejected offer must remove the staged file")
	}
}

// serveReleaseE2E hosts one release: a JSON manifest with the digest and a
// download endpoint carrying the hex signature in the default header. The
// header value is passed through verbatim, so tests can serve deliberately
// malformed encodings.
func serveReleaseE2E(t *testing.T, manifest, signatureHex string, payload []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(manifest))
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Daemonpilot-Signature", signatureHex)
		_, _ = w.Write(payload)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// The full HTTP path with a custom verifier: a release server hands out a
// manifest with the release digest and an ECDSA ASN.1 signature whose length
// is not the Ed25519 size — the HTTP layer must pass it through untouched,
// the custom verifier accepts it, and the update activates.
func TestExtensionHTTPE2ECustomVerifierActivates(t *testing.T) {
	payload := []byte("http-e2e-payload")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	message := crypto.DigestMessage("e2e-v1\x00", digest[:])
	hashed := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, key, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	if len(signature) == ed25519.SignatureSize {
		t.Fatal("fixture must use a non-Ed25519 signature length")
	}

	manifest := fmt.Sprintf(`{"version":"2.0.0","watchdog_version":"1","size":%d,"digest":%q}`,
		len(payload), hex.EncodeToString(digest[:]))
	server := serveReleaseE2E(t, manifest, hex.EncodeToString(signature), payload)

	source, err := httpsource.New(httpsource.Config{
		ManifestURL: server.URL + "/manifest",
		DownloadURL: server.URL + "/download",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &daemonpilot.UpdateConfig{
		Source:           source,
		SignatureContext: "e2e-v1\x00",
		VerifyMessage: func(message, signature []byte) error {
			hashed := sha256.Sum256(message)
			if !ecdsa.VerifyASN1(&key.PublicKey, hashed[:], signature) {
				return errors.New("ecdsa verification failed")
			}
			return nil
		},
	}
	u := New(cfg, newTestLogger(), t.TempDir(), "myapp")

	info, err := source.CheckUpdate(context.Background(), "1.0.0")
	if err != nil || info == nil {
		t.Fatalf("check update: %v, %v", info, err)
	}
	staged, err := u.DownloadAndVerify(context.Background(), info)
	if err != nil {
		t.Fatalf("download and verify: %v", err)
	}

	// Activate: the verified release replaces the installed binary.
	installed := filepath.Join(u.installDir, u.binaryName)
	if err := os.WriteFile(installed, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := u.ApplyFastUpdate(context.Background(), staged); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := os.ReadFile(installed)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("activated binary = %q, %v", got, err)
	}
}

// The combined ECDSA + SHA-384 scenario over real HTTP, with a versioned
// executable: the release is downloaded through httpsource, its SHA-384
// digest is checked, the ECDSA verifier accepts the message, the selftest
// passes, the update activates and the installed path — executed
// independently — reports the new version. A release signed by the wrong
// key must leave the installed v1 runnable.
func TestExtensionHTTPE2EVersionedRunAndReject(t *testing.T) {
	dir := t.TempDir()
	v1 := buildCrashFixture(t, dir, "v1")
	v2 := buildCrashFixture(t, dir, "v2")
	v2Bytes, err := os.ReadFile(v2)
	if err != nil {
		t.Fatal(err)
	}

	verifierKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	digest := sha512.Sum384(v2Bytes)
	message := crypto.DigestMessage("combined-v1\x00", digest[:])
	hashed := sha256.Sum256(message)
	rightSignature, err := ecdsa.SignASN1(rand.Reader, verifierKey, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	wrongSignature, err := ecdsa.SignASN1(rand.Reader, wrongKey, hashed[:])
	if err != nil {
		t.Fatal(err)
	}

	newCfg := func(manifest, signatureHex string) *daemonpilot.UpdateConfig {
		server := serveReleaseE2E(t, manifest, signatureHex, v2Bytes)
		t.Cleanup(server.Close)
		source, err := httpsource.New(httpsource.Config{
			ManifestURL: server.URL + "/manifest",
			DownloadURL: server.URL + "/download",
		})
		if err != nil {
			t.Fatal(err)
		}
		return &daemonpilot.UpdateConfig{
			Source:           source,
			SignatureContext: "combined-v1\x00",
			VerifyMessage: func(message, signature []byte) error {
				hashed := sha256.Sum256(message)
				if !ecdsa.VerifyASN1(&verifierKey.PublicKey, hashed[:], signature) {
					return errors.New("ecdsa verification failed")
				}
				return nil
			},
			NewHash: func() hash.Hash { return sha512.New384() },
		}
	}

	install := func(t *testing.T, which, target string) {
		t.Helper()
		data, err := os.ReadFile(which)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runInstalled := func(t *testing.T, path string) string {
		t.Helper()
		out, err := exec.Command(path).CombinedOutput()
		if err != nil {
			t.Fatalf("installed path must run: %v\n%s", err, out)
		}
		return string(out)
	}

	t.Run("activate", func(t *testing.T) {
		manifest := fmt.Sprintf(`{"version":"2.0.0","watchdog_version":"1","size":%d,"digest":%q}`,
			len(v2Bytes), hex.EncodeToString(digest[:]))
		u := New(newCfg(manifest, hex.EncodeToString(rightSignature)), newTestLogger(), t.TempDir(), "myapp")
		info, err := u.config.Source.CheckUpdate(context.Background(), "1.0.0")
		if err != nil || info == nil {
			t.Fatalf("check update: %v, %v", info, err)
		}
		staged, err := u.DownloadAndVerify(context.Background(), info)
		if err != nil {
			t.Fatalf("download and verify: %v", err)
		}
		selftest, err := u.RunSelftest(context.Background(), staged, 30*time.Second)
		if err != nil || selftest == nil || !selftest.OK {
			t.Fatalf("selftest: %+v, %v", selftest, err)
		}
		installed := filepath.Join(u.installDir, daemonpilot.InstalledBinaryName("myapp"))
		install(t, v1, installed)
		if err := u.ApplyFastUpdate(context.Background(), staged); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if got := runInstalled(t, installed); !strings.Contains(got, "crashfixture v2") {
			t.Fatalf("activated binary reports %q, want v2", got)
		}
	})

	t.Run("wrong signature keeps v1 runnable", func(t *testing.T) {
		manifest := fmt.Sprintf(`{"version":"2.0.0","watchdog_version":"1","size":%d,"digest":%q}`,
			len(v2Bytes), hex.EncodeToString(digest[:]))
		u := New(newCfg(manifest, hex.EncodeToString(wrongSignature)), newTestLogger(), t.TempDir(), "myapp")
		installed := filepath.Join(u.installDir, daemonpilot.InstalledBinaryName("myapp"))
		install(t, v1, installed)

		info, err := u.config.Source.CheckUpdate(context.Background(), "1.0.0")
		if err != nil || info == nil {
			t.Fatalf("check update: %v, %v", info, err)
		}
		if _, err := u.DownloadAndVerify(context.Background(), info); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
			t.Fatalf("wrong-key release must fail verification, got %v", err)
		}
		if got := runInstalled(t, installed); !strings.Contains(got, "crashfixture v1") {
			t.Fatalf("rejected release must leave v1 runnable, got %q", got)
		}
	})
}

// The sha256 field means SHA-256 and only SHA-256: a release configured
// under NewHash whose digest hides in the sha256 alias is incompatible with
// the configuration even when the value itself is the correct digest of the
// bytes and the signature is valid for it. The offer must be rejected before
// apply, the staging file removed, and the installed version left runnable.
func TestExtensionHTTPE2ECustomHashRejectsSHA256AliasField(t *testing.T) {
	dir := t.TempDir()
	v1 := buildCrashFixture(t, dir, "v1")
	v2 := buildCrashFixture(t, dir, "v2")
	v2Bytes, err := os.ReadFile(v2)
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha512.Sum384(v2Bytes)
	signature := ed25519.Sign(priv, crypto.DigestMessage("alias-v1\x00", digest[:]))

	manifest := fmt.Sprintf(`{"version":"2.0.0","watchdog_version":"1","size":%d,"sha256":%q}`,
		len(v2Bytes), hex.EncodeToString(digest[:]))
	server := serveReleaseE2E(t, manifest, hex.EncodeToString(signature), v2Bytes)
	source, err := httpsource.New(httpsource.Config{
		ManifestURL: server.URL + "/manifest",
		DownloadURL: server.URL + "/download",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &daemonpilot.UpdateConfig{
		Source:           source,
		PublicKey:        pub,
		SignatureContext: "alias-v1\x00",
		NewHash:          func() hash.Hash { return sha512.New384() },
	}
	u := New(cfg, newTestLogger(), t.TempDir(), "myapp")
	installed := filepath.Join(u.installDir, daemonpilot.InstalledBinaryName("myapp"))
	v1Bytes, err := os.ReadFile(v1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, v1Bytes, 0o755); err != nil {
		t.Fatal(err)
	}

	info, err := source.CheckUpdate(context.Background(), "1.0.0")
	if err != nil || info == nil {
		t.Fatalf("check update: %v, %v", info, err)
	}
	if _, err := u.DownloadAndVerify(context.Background(), info); err == nil ||
		!strings.Contains(err.Error(), "digest fields disagree") {
		t.Fatalf("a sha256 field under a configured hash must be rejected regardless of its value, got %v", err)
	}
	staged := daemonpilot.StagedNewPath(installed)
	if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
		t.Fatal("rejected release must leave no staged file")
	}
	out, err := exec.Command(installed).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "crashfixture v1") {
		t.Fatalf("rejected release must leave the installed v1 runnable: %v %s", err, out)
	}
}

// Transport-level negatives on the real HTTP path: bad hex encoding, a
// wrong-key signature and an oversized artifact must each fail before or at
// verification, and the built-in Ed25519 chain must still reject a
// wrong-length signature even though the transport no longer length-checks.
func TestExtensionHTTPE2ENegatives(t *testing.T) {
	payload := []byte("http-e2e-payload")
	rightPub, rightPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// A different key pair signs the wrong-key case; the verifier only holds
	// the right pair's public half.
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	message := crypto.DigestMessage("e2e-v1\x00", digest[:])
	rightSig := ed25519.Sign(rightPriv, message)
	otherSig := ed25519.Sign(otherPriv, message)

	for _, tc := range []struct {
		name            string
		signatureHex    string
		manifestSize    int
		builtInVerifier bool
		wantErr         string
	}{
		{"non-hex signature header", "not-hex", len(payload), false, "missing or invalid"},
		{"wrong-key signature", hex.EncodeToString(otherSig), len(payload), false, "signature verification failed"},
		{"oversized declared artifact", hex.EncodeToString(rightSig), len(payload) + 1, false, "size mismatch"},
		{"short signature under built-in chain", "01020304", len(payload), true, "signature verification failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := fmt.Sprintf(`{"version":"2.0.0","watchdog_version":"1","size":%d,"sha256":%q}`,
				tc.manifestSize, hex.EncodeToString(digest[:]))
			server := serveReleaseE2E(t, manifest, tc.signatureHex, payload)

			var cfg *daemonpilot.UpdateConfig
			if tc.builtInVerifier {
				cfg = &daemonpilot.UpdateConfig{PublicKey: rightPub, SignatureContext: "e2e-v1\x00"}
			} else {
				cfg = &daemonpilot.UpdateConfig{
					SignatureContext: "e2e-v1\x00",
					VerifyMessage: func(message, signature []byte) error {
						if !ed25519.Verify(rightPub, message, signature) {
							return errors.New("signature verification failed")
						}
						return nil
					},
				}
			}
			source, err := httpsource.New(httpsource.Config{
				ManifestURL: server.URL + "/manifest",
				DownloadURL: server.URL + "/download",
			})
			if err != nil {
				t.Fatal(err)
			}
			cfg.Source = source
			u := New(cfg, newTestLogger(), t.TempDir(), "myapp")

			info, err := source.CheckUpdate(context.Background(), "1.0.0")
			if err != nil || info == nil {
				t.Fatalf("check update: %v, %v", info, err)
			}
			staged, err := u.DownloadAndVerify(context.Background(), info)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
				t.Fatal("failed verification must remove the staged file")
			}
		})
	}
}
