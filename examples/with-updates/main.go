// Package main is a daemonpilot service with signed self-update enabled.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"log"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
)

// Replace with your actual public key (generated with ed25519.GenerateKey).
const publicKeyHex = "0000000000000000000000000000000000000000000000000000000000000000"

func main() {
	pubKey, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		log.Fatalf("invalid public key: %v", err)
	}

	s, err := daemonpilot.New(daemonpilot.Config{
		Name:        "updatable-agent",
		Description: "A daemonpilot service with self-update support",
		WorkerFunc:  workerMain,
		SelftestFunc: func(ctx context.Context) daemonpilot.SelftestResult {
			return daemonpilot.SelftestResult{
				OK: true,
				Checks: map[string]daemonpilot.CheckResult{
					"config": {OK: true, Message: "configuration valid"},
				},
			}
		},
		Update: &daemonpilot.UpdateConfig{
			Source:        &MyUpdateSource{BaseURL: "https://updates.example.com"},
			PublicKey:     ed25519.PublicKey(pubKey),
			CheckInterval: 5 * time.Minute,
			StagedUpdateFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo, report daemonpilot.SelftestResult) (bool, error) {
				log.Printf("staged update available: %s -> %s", daemonpilot.Version, info.Version)
				return true, nil
			},
			RollbackConfirmFunc: func(ctx context.Context) (bool, error) {
				log.Println("confirming watchdog update succeeded")
				return true, nil
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := s.Run(); err != nil {
		log.Fatal(err)
	}
}

func workerMain(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	log.Println("worker started")
	for {
		select {
		case <-ctx.Done():
			log.Println("worker shutting down")
			return nil
		case <-ticker.C:
			log.Println("worker tick")
		}
	}
}
