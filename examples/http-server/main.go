// Package main is a daemonpilot-supervised HTTP server with graceful shutdown.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
)

func main() {
	s, err := daemonpilot.New(daemonpilot.Config{
		Name:        "http-server",
		Description: "Simple HTTP server managed by daemonpilot",
		WorkerFunc:  runHTTPServer,
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := s.Run(); err != nil {
		log.Fatal(err)
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// %q escapes newlines in the request-controlled fields, so a crafted
		// path or User-Agent cannot forge extra log lines.
		//nolint:gosec // the %q quoting above is the mitigation gosec is asking for
		log.Printf("Request: %q %q from %q - User-Agent: %q",
			r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
		next.ServeHTTP(w, r)
	})
}

func runHTTPServer(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "Hello from daemonpilot HTTP server!\n")
		_, _ = fmt.Fprintf(w, "Time: %s\n", time.Now().Format(time.RFC3339))
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"healthy","time":"%s"}`, time.Now().Format(time.RFC3339))
	})

	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "pong\n")
	})

	server := &http.Server{
		Addr:              ":8083",
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errChan := make(chan error, 1)
	go func() {
		log.Printf("Starting HTTP server on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Println("Shutting down HTTP server...")
		// context.WithoutCancel keeps the shutdown deadline independent of ctx,
		// which is already cancelled here.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down HTTP server: %w", err)
		}
		return nil
	case err := <-errChan:
		return err
	}
}
