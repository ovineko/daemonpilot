// daemonpilot-sign generates hex-seed keys and signs release binary digests.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/ovineko/daemonpilot/pkg/release"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: daemonpilot-sign keygen|sign|pubkey -key <file> [binary]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	keyPath := flags.String("key", "signing.key", "hex-seed private key file")
	var signatureContext string
	if args[0] == "sign" {
		flags.StringVar(&signatureContext, "context", "", `signature context with Go-style escapes (e.g. myapp-binary-v1\x00)`)
	}
	if err := flags.Parse(args[1:]); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	switch args[0] {
	case "keygen":
		if flags.NArg() != 0 {
			return errors.New("usage: daemonpilot-sign keygen [-key <file>]")
		}
		return keygen(*keyPath, out)
	case "sign", "pubkey":
		wanted := 0
		if args[0] == "sign" {
			wanted = 1
		}
		if flags.NArg() != wanted {
			return errors.New("usage: daemonpilot-sign sign -key <file> [-context <string>] <binary> | pubkey -key <file>")
		}
		key, err := readKey(*keyPath)
		if err != nil {
			return err
		}
		if args[0] == "pubkey" {
			return printPublicKey(out, key)
		}
		signatureContext, err = strconv.Unquote(`"` + signatureContext + `"`)
		if err != nil {
			return fmt.Errorf("parse context: %w", err)
		}
		sig, digest, _, err := release.SignFileContext(key, signatureContext, flags.Arg(0))
		if err != nil {
			return fmt.Errorf("sign file: %w", err)
		}
		if _, err := fmt.Fprintf(out, "%s\n%s\n", hex.EncodeToString(sig), digest); err != nil {
			return fmt.Errorf("print signature: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func keygen(path string, out io.Writer) error {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	//nolint:gosec // The command's caller selects the private key destination.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private key: %w", err)
	}
	_, writeErr := fmt.Fprintln(f, hex.EncodeToString(key.Seed()))
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	return printPublicKey(out, key)
}

func readKey(path string) (ed25519.PrivateKey, error) {
	//nolint:gosec // The command's caller selects the private key source.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("private key must contain a 32-byte hex ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func printPublicKey(out io.Writer, key ed25519.PrivateKey) error {
	publicKey, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("unexpected public key type")
	}
	if _, err := fmt.Fprintln(out, hex.EncodeToString(publicKey)); err != nil {
		return fmt.Errorf("print public key: %w", err)
	}
	return nil
}
