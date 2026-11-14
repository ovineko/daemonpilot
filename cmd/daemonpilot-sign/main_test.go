package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSigningCommands(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	binary := filepath.Join(dir, "binary")
	var out bytes.Buffer
	if err := run([]string{"keygen", "-key", keyPath}, &out); err != nil {
		t.Fatal(err)
	}
	publicHex := strings.TrimSpace(out.String())
	public, err := hex.DecodeString(publicHex)
	if err != nil || len(public) != ed25519.PublicKeySize {
		t.Fatal("invalid public key")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}

	seed, err := os.ReadFile(keyPath)
	if err != nil || len(strings.TrimSpace(string(seed))) != 64 {
		t.Fatal("not hex seed")
	}
	if err := run([]string{"keygen", "-key", keyPath}, &out); err == nil {
		t.Fatal("overwrote key")
	}
	out.Reset()
	if err := run([]string{"pubkey", "-key", keyPath}, &out); err != nil || strings.TrimSpace(out.String()) != publicHex {
		t.Fatalf("pubkey: %s %v", &out, err)
	}
	data := []byte("binary contents")
	if err := os.WriteFile(binary, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"sign", "-key", keyPath, binary}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(out.String())
	if len(lines) != 2 {
		t.Fatal(out.String())
	}
	sig, err := hex.DecodeString(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if lines[1] != hex.EncodeToString(digest[:]) || !ed25519.Verify(public, digest[:], sig) {
		t.Fatal("incorrect signature or hash")
	}
	legacyOutput := out.String()
	for _, tc := range []struct {
		name, flag, context string
		wantError           bool
	}{
		{name: "empty"},
		{name: "plain", flag: "myapp-binary-v1", context: "myapp-binary-v1"},
		{name: "NUL", flag: `myapp-binary-v1\x00`, context: "myapp-binary-v1\x00"},
		{name: "Go escapes", flag: `\u0061\n\t\\\"`, context: "a\n\t\\\""},
		{name: "invalid escape", flag: `\q`, wantError: true},
		{name: "incomplete escape", flag: `\x0`, wantError: true},
		{name: "unescaped quote", flag: `"`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			err := run([]string{"sign", "-key", keyPath, "-context", tc.flag, binary}, &output)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "parse context") || output.Len() != 0 {
					t.Fatalf("invalid context: output = %q, error = %v", output.String(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Fields(output.String())
			if len(lines) != 2 {
				t.Fatal(output.String())
			}
			sig, err := hex.DecodeString(lines[0])
			if err != nil || lines[1] != hex.EncodeToString(digest[:]) || !ed25519.Verify(public, append([]byte(tc.context), digest[:]...), sig) {
				t.Fatal("incorrect context signature or hash")
			}
			if tc.context == "" && output.String() != legacyOutput {
				t.Fatal("empty context changed legacy output")
			}
		})
	}
	for _, args := range [][]string{nil, {"unknown"}, {"sign"}, {"pubkey", "extra"}, {"sign", "-unknown"}, {"pubkey", "-key", binary}, {"pubkey", "-context", "ignored"}, {"keygen", "-context", "ignored"}} {
		if err := run(args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
