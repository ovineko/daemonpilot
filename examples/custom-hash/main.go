// Package main is a daemonpilot service whose release digests use SHA-384
// instead of the default SHA-256. The library feeds the staged bytes to the
// application's hasher streaming — the whole binary is never held in
// memory — and the built-in Ed25519 check then signs the SignatureContext
// followed by the 48-byte digest. Release manifests carry that digest in the
// UpdateInfo.Digest field; internal rollback digests stay SHA-256 and are
// unaffected.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/hex"
	"hash"
	"log"
	"os"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
	"github.com/ovineko/daemonpilot/pkg/httpsource"
)

func main() {
	key := mustEd25519Key()

	s, err := daemonpilot.New(daemonpilot.Config{
		Name:        "sha384-digest-agent",
		Description: "A daemonpilot service hashing release digests with SHA-384",
		WorkerFunc:  workerMain,
		Update: &daemonpilot.UpdateConfig{
			Source:           mustSource(),
			PublicKey:        key,
			SignatureContext: "myapp-sha384-v1\x00",
			// One independent hasher per verification; the digest lands in
			// the manifest's "digest" field, the only accepted digest field
			// while NewHash is set — a nonempty "sha256" field rejects the
			// offer even for an explicit sha256.New (see pkg/release for the
			// default SHA-256 release server).
			NewHash: func() hash.Hash { return sha512.New384() },
			// RetryBaseBackoff/RetryMaxBackoff stay configurable as before.
			CheckInterval: 5 * time.Minute,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := s.Run(); err != nil {
		log.Fatal(err)
	}
}

func mustEd25519Key() ed25519.PublicKey {
	seed, err := hex.DecodeString(os.Getenv("AGENT_ED25519_PUBLIC_KEY"))
	if err != nil || len(seed) != ed25519.PublicKeySize {
		log.Fatalf("AGENT_ED25519_PUBLIC_KEY must be %d hex bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(seed)
}

// mustSource builds the streaming HTTP release source; manifests carry the
// SHA-384 digest in the "digest" field.
func mustSource() daemonpilot.UpdateSource {
	source, err := httpsource.New(httpsource.Config{
		ManifestURL: os.Getenv("AGENT_RELEASE_MANIFEST_URL"),
		DownloadURL: os.Getenv("AGENT_RELEASE_DOWNLOAD_URL"),
	})
	if err != nil {
		log.Fatal(err)
	}
	return source
}

func workerMain(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
