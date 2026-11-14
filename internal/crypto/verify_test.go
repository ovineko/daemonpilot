package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
)

func TestDigestMessage(t *testing.T) {
	digest := sha256.Sum256([]byte("artifact"))
	for _, context := range []string{"", "myapp-binary-v1\x00", "another-purpose-v2\x00"} {
		t.Run(context, func(t *testing.T) {
			got := DigestMessage(context, digest[:])
			want := append([]byte(context), digest[:]...)
			if !bytes.Equal(got, want) {
				t.Fatalf("message = %x, want %x", got, want)
			}
		})
	}
}

func TestVerifySignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	data := []byte("hello world")
	hash := sha256.Sum256(data)
	sig := ed25519.Sign(priv, hash[:])

	tests := []struct {
		name      string
		pubKey    ed25519.PublicKey
		data      []byte
		signature []byte
		want      bool
	}{
		{"valid signature", pub, hash[:], sig, true},
		{"wrong data", pub, []byte("wrong"), sig, false},
		{"wrong signature", pub, hash[:], make([]byte, ed25519.SignatureSize), false},
		{"wrong key size", []byte("short"), hash[:], sig, false},
		{"wrong signature size", pub, hash[:], []byte("short"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VerifySignature(tt.pubKey, tt.data, tt.signature)
			if got != tt.want {
				t.Errorf("VerifySignature() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("wrong key", func(t *testing.T) {
		pub2, _, _ := ed25519.GenerateKey(nil)
		got := VerifySignature(pub2, hash[:], sig)
		if got {
			t.Error("VerifySignature() should fail with wrong key")
		}
	})
}
