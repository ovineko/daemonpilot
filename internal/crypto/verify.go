// Package crypto verifies ed25519 signatures over update artifacts.
package crypto

import (
	"crypto/ed25519"
)

// DigestMessage prefixes a release digest with the exact context bytes.
// An empty context preserves the legacy bare-digest message. The digest is
// whatever byte string the configured release hash produced: the built-in
// tools use SHA-256, but the message shape does not depend on the digest
// size. Signing and verification must construct identical bytes.
func DigestMessage(context string, digest []byte) []byte {
	message := make([]byte, 0, len(context)+len(digest))
	message = append(message, context...)
	return append(message, digest...)
}

// VerifySignature reports whether signature is a valid ed25519 signature of
// data under publicKey. Malformed keys and signatures are rejected rather than
// passed to ed25519.Verify, which panics on a wrong key size.
func VerifySignature(publicKey ed25519.PublicKey, data []byte, signature []byte) bool {
	if len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	if len(signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(publicKey, data, signature)
}
