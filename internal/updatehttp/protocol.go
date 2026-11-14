// Package updatehttp holds the shared signed-release HTTP protocol.
package updatehttp

import (
	"errors"
	"fmt"
	"strings"
)

// Default HTTP headers carry the artifact signature and SHA-256 digest.
const (
	SignatureHeader = "X-Daemonpilot-Signature"
	SHA256Header    = "X-Daemonpilot-Sha256"
)

// Headers defaults and validates HTTP field names.
func Headers(signature, digest string) (string, string, error) {
	if signature == "" {
		signature = SignatureHeader
	}
	if digest == "" {
		digest = SHA256Header
	}
	for _, name := range []string{signature, digest} {
		for _, c := range name {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
				return "", "", fmt.Errorf("invalid header name %q", name)
			}
		}
	}
	if strings.EqualFold(signature, digest) {
		return "", "", errors.New("signature and SHA256 headers must differ")
	}
	return signature, digest, nil
}
