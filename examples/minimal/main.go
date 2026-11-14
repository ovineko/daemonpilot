// Package main is a minimal daemonpilot service: a watchdog-supervised worker
// with no self-update.
package main

import (
	"context"
	"log"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
)

func main() {
	s, err := daemonpilot.New(daemonpilot.Config{
		Name:        "minimal-agent",
		Description: "A minimal daemonpilot-managed service",
		WorkerFunc:  workerMain,
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
