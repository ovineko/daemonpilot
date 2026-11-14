//go:build !windows

package integration

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/process"
	"github.com/ovineko/procguard"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestOrphanPrevention_SIGKILL(t *testing.T) {
	if os.Getenv("BE_WATCHDOG") == "1" {
		runAsWatchdog()
		return
	}
	if os.Getenv("BE_WORKER") == "1" {
		runAsWorker()
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testsupport.Scale(30*time.Second))
	defer cancel()

	watchdogCmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOrphanPrevention_SIGKILL$")
	watchdogCmd.Env = append(os.Environ(), "BE_WATCHDOG=1")

	watchdogStdout, err := watchdogCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to create stdout pipe: %v", err)
	}

	watchdogCmd.Stderr = os.Stderr

	if err := watchdogCmd.Start(); err != nil {
		t.Fatalf("failed to start watchdog: %v", err)
	}

	watchdogPID := watchdogCmd.Process.Pid
	t.Logf("watchdog started with PID %d", watchdogPID)

	workerPID, err := readWorkerPID(watchdogStdout)
	if err != nil {
		t.Fatalf("failed to read worker PID from watchdog: %v", err)
	}
	t.Logf("worker started with PID %d", workerPID)

	if !processExists(workerPID) {
		t.Fatalf("worker process %d does not exist after spawn", workerPID)
	}

	if err := syscall.Kill(watchdogPID, syscall.SIGKILL); err != nil {
		t.Fatalf("failed to SIGKILL watchdog: %v", err)
	}
	t.Logf("sent SIGKILL to watchdog PID %d", watchdogPID)

	_ = watchdogCmd.Wait()

	deadline := time.After(testsupport.Scale(6 * time.Second))
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			_ = syscall.Kill(workerPID, syscall.SIGKILL)
			t.Fatalf("worker process %d still alive after 6 seconds - orphan prevention failed", workerPID)
		case <-ticker.C:
			if !processExists(workerPID) {
				t.Logf("worker process %d exited (orphan prevention works)", workerPID)
				return
			}
		}
	}
}

func runAsWatchdog() {
	workerCmd := exec.Command(os.Args[0], "-test.run=^TestOrphanPrevention_SIGKILL$")
	workerCmd.Env = append(os.Environ(),
		"BE_WATCHDOG=",
		"BE_WORKER=1",
		fmt.Sprintf("DAEMONPILOT_PARENT_PID=%d", os.Getpid()),
	)
	workerCmd.Stderr = os.Stderr

	if err := workerCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "watchdog: failed to start worker: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("WORKER_PID=%d\n", workerCmd.Process.Pid)

	// Block until killed
	select {}
}

func runAsWorker() {
	parentPIDStr := os.Getenv("DAEMONPILOT_PARENT_PID")
	parentPID, err := strconv.Atoi(parentPIDStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: invalid DAEMONPILOT_PARENT_PID %q: %v\n", parentPIDStr, err)
		os.Exit(1)
	}

	signalCtx, signalCancel := process.SetupSignalHandler(context.Background())
	defer signalCancel()
	ctx, cancel, err := procguard.WatchParent(signalCtx, parentPID, time.Second)
	if err != nil {
		panic(err)
	}
	<-ctx.Done()
	cancel()
}

func readWorkerPID(r io.Reader) (int, error) {
	scanner := bufio.NewScanner(r)
	deadline := time.After(testsupport.Scale(5 * time.Second))
	resultCh := make(chan string, 1)

	go func() {
		for scanner.Scan() {
			text := scanner.Text()
			if strings.HasPrefix(text, "WORKER_PID=") {
				resultCh <- text
				return
			}
		}
		close(resultCh)
	}()

	select {
	case <-deadline:
		return 0, errors.New("timeout waiting for WORKER_PID from watchdog")
	case line, ok := <-resultCh:
		if !ok || !strings.HasPrefix(line, "WORKER_PID=") {
			return 0, errors.New("watchdog exited without printing WORKER_PID")
		}
		pidStr := strings.TrimPrefix(line, "WORKER_PID=")
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			return 0, fmt.Errorf("invalid worker PID %q: %w", pidStr, err)
		}
		return pid, nil
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
