package daemonpilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// StartStackDumpHandler arms SIGUSR1 to write all goroutine stacks to
// <dir>/goroutine-dump-<unix seconds>.txt (0600) and stderr, then calls onDump,
// which may be nil. It is the worker-side counterpart of Config.DumpWorkerOnHealthFailure.
// It stops when ctx is done and returns false without starting on platforms without SIGUSR1.
// The caller must create dir before a dump is requested.
func StartStackDumpHandler(ctx context.Context, dir string, onDump func(path string, err error)) bool {
	return startStackDumpHandler(ctx, dir, onDump)
}

func writeStackDump(dir string) (string, error) {
	buf := make([]byte, 64*1024)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, len(buf)*2)
	}
	path := filepath.Join(dir, "goroutine-dump-"+strconv.FormatInt(time.Now().Unix(), 10)+".txt")
	fileErr := writeStackDumpFile(path, buf)
	_, stderrErr := os.Stderr.Write(buf)
	if err := errors.Join(fileErr, stderrErr); err != nil {
		return path, fmt.Errorf("writing goroutine dump: %w", err)
	}
	return path, nil
}

func writeStackDumpFile(path string, buf []byte) error {
	//nolint:gosec // the caller selects the diagnostic directory
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening goroutine dump: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("setting dump permissions: %w", err)
	}
	_, writeErr := f.Write(buf)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return fmt.Errorf("saving goroutine dump: %w", err)
	}
	return nil
}
