package daemonpilot

import (
	"context"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/ipc"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestNotifyReadySendsSignal(t *testing.T) {
	readyCh := make(chan struct{}, 1)

	srv, pipes, err := ipc.NewServer(func() ipc.DynamicInfo {
		return ipc.DynamicInfo{}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetReadyHandler(func() {
		readyCh <- struct{}{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	t.Cleanup(func() {
		cancel()
		<-srv.Done()
	})

	origFactory := ipcClientFactory
	resetIPCClient()
	t.Cleanup(func() {
		ipcClientFactory = origFactory
		resetIPCClient()
	})
	ipcClientFactory = func() (*ipc.Client, error) {
		return ipc.NewClient(pipes.RequestW, pipes.ResponseR), nil
	}

	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}

	select {
	case <-readyCh:
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("watchdog did not receive ready signal")
	}
}

func TestNotifyReadyNoIPCReturnsNil(t *testing.T) {
	setupNoIPC(t)

	err := NotifyReady()
	if err != nil {
		t.Fatalf("expected nil error without IPC, got: %v", err)
	}
}

func TestNotifyReadyFollowedByGetRuntimeInfo(t *testing.T) {
	readyCh := make(chan struct{}, 1)

	srv, pipes, err := ipc.NewServer(func() ipc.DynamicInfo {
		return ipc.DynamicInfo{WatchdogPID: 99, RestartCount: 5}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetReadyHandler(func() {
		readyCh <- struct{}{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	t.Cleanup(func() {
		cancel()
		<-srv.Done()
	})

	setupEnv(t, map[string]string{
		EnvMode:    "worker",
		EnvVersion: "1.0.0",
	})

	origFactory := ipcClientFactory
	resetIPCClient()
	t.Cleanup(func() {
		ipcClientFactory = origFactory
		resetIPCClient()
	})
	ipcClientFactory = func() (*ipc.Client, error) {
		return ipc.NewClient(pipes.RequestW, pipes.ResponseR), nil
	}

	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}

	select {
	case <-readyCh:
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("watchdog did not receive ready signal")
	}

	info, err := GetRuntimeInfo()
	if err != nil {
		t.Fatalf("GetRuntimeInfo: %v", err)
	}
	if info.WatchdogPID != 99 {
		t.Errorf("WatchdogPID = %d, want 99", info.WatchdogPID)
	}
	if info.RestartCount != 5 {
		t.Errorf("RestartCount = %d, want 5", info.RestartCount)
	}
}
