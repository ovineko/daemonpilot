// Package main is a daemonpilot service whose update offers are verified by an
// application-supplied ECDSA P-256 verifier instead of the built-in Ed25519
// check. The key travels in the closure; no Ed25519 public key is configured
// and none is required. Everything else — the SignatureContext prefix, the
// release digest, staging, selftest and rollback — behaves exactly as with
// the built-in verifier.
//
// The release side signs the same bytes with any ECDSA tool: sha256 over
// ("myapp-ecdsa-v1\x00" followed by the release digest), ASN.1-encoded.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
	"github.com/ovineko/daemonpilot/pkg/httpsource"
)

func main() {
	key := mustPublicKey()

	source, err := httpsource.New(httpsource.Config{
		ManifestURL: os.Getenv("AGENT_RELEASE_MANIFEST_URL"),
		DownloadURL: os.Getenv("AGENT_RELEASE_DOWNLOAD_URL"),
	})
	if err != nil {
		log.Fatal(err)
	}

	s, err := daemonpilot.New(daemonpilot.Config{
		Name:        "ecdsa-verified-agent",
		Description: "A daemonpilot service verifying updates with application ECDSA",
		WorkerFunc:  workerMain,
		Update: &daemonpilot.UpdateConfig{
			Source:           source,
			SignatureContext: "myapp-ecdsa-v1\x00",
			VerifyMessage: func(message, signature []byte) error {
				hashed := sha256.Sum256(message)
				if !ecdsa.VerifyASN1(key, hashed[:], signature) {
					return errors.New("ecdsa verification failed")
				}
				return nil
			},
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

// mustPublicKey decodes the operator's P-256 point from two hex coordinates.
// Production code loads these from a managed secret store, not the process
// environment of a privileged service.
func mustPublicKey() *ecdsa.PublicKey {
	x := mustCoordinate("AGENT_ECDSA_KEY_X")
	y := mustCoordinate("AGENT_ECDSA_KEY_Y")
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
}

func mustCoordinate(name string) *big.Int {
	raw, err := hex.DecodeString(os.Getenv(name))
	if err != nil || len(raw) == 0 {
		log.Fatalf("%s must be hex-encoded big-endian coordinates: %v", name, err)
	}
	coordinate := new(big.Int).SetBytes(raw)
	if !coordinate.IsUint64() && coordinate.BitLen() > 256 {
		log.Fatalf("%s is not a P-256 coordinate: %v", name, fmt.Errorf("bit length %d", coordinate.BitLen()))
	}
	return coordinate
}

func workerMain(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
