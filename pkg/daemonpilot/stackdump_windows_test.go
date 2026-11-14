//go:build windows

package daemonpilot

import (
	"context"
	"testing"
)

func TestStartStackDumpHandlerUnsupported(t *testing.T) {
	if StartStackDumpHandler(context.Background(), t.TempDir(), func(string, error) { t.Error("callback on unsupported platform") }) {
		t.Fatal("SIGUSR1 handler reported supported on Windows")
	}
}
