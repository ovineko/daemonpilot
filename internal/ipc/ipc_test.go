package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestDynamicInfoJSONRoundTrip(t *testing.T) {
	info := DynamicInfo{
		WatchdogPID:        1234,
		WorkerPID:          5678,
		WatchdogStartTime:  time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC),
		WorkerStartTime:    time.Date(2026, 3, 20, 10, 5, 0, 0, time.UTC),
		RestartCount:       3,
		CurrentBackoff:     4 * time.Second,
		HealthTimestamp:    time.Date(2026, 3, 20, 10, 10, 0, 0, time.UTC),
		HealthError:        "connection refused",
		HealthFailures:     2,
		UpdateLastCheck:    time.Date(2026, 3, 20, 10, 8, 0, 0, time.UTC),
		UpdateResult:       "available",
		UpdateAvailableVer: "2.0.0",
		UpdateStatus:       "downloading",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded DynamicInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.WatchdogPID != info.WatchdogPID {
		t.Errorf("WatchdogPID = %d, want %d", decoded.WatchdogPID, info.WatchdogPID)
	}
	if decoded.WorkerPID != info.WorkerPID {
		t.Errorf("WorkerPID = %d, want %d", decoded.WorkerPID, info.WorkerPID)
	}
	if !decoded.WatchdogStartTime.Equal(info.WatchdogStartTime) {
		t.Errorf("WatchdogStartTime = %v, want %v", decoded.WatchdogStartTime, info.WatchdogStartTime)
	}
	if !decoded.WorkerStartTime.Equal(info.WorkerStartTime) {
		t.Errorf("WorkerStartTime = %v, want %v", decoded.WorkerStartTime, info.WorkerStartTime)
	}
	if decoded.RestartCount != info.RestartCount {
		t.Errorf("RestartCount = %d, want %d", decoded.RestartCount, info.RestartCount)
	}
	if decoded.CurrentBackoff != info.CurrentBackoff {
		t.Errorf("CurrentBackoff = %v, want %v", decoded.CurrentBackoff, info.CurrentBackoff)
	}
	if decoded.HealthError != info.HealthError {
		t.Errorf("HealthError = %q, want %q", decoded.HealthError, info.HealthError)
	}
	if decoded.HealthFailures != info.HealthFailures {
		t.Errorf("HealthFailures = %d, want %d", decoded.HealthFailures, info.HealthFailures)
	}
	if decoded.UpdateResult != info.UpdateResult {
		t.Errorf("UpdateResult = %q, want %q", decoded.UpdateResult, info.UpdateResult)
	}
	if decoded.UpdateAvailableVer != info.UpdateAvailableVer {
		t.Errorf("UpdateAvailableVer = %q, want %q", decoded.UpdateAvailableVer, info.UpdateAvailableVer)
	}
	if decoded.UpdateStatus != info.UpdateStatus {
		t.Errorf("UpdateStatus = %q, want %q", decoded.UpdateStatus, info.UpdateStatus)
	}
}

func TestDynamicInfoZeroValue(t *testing.T) {
	var info DynamicInfo

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal zero value: %v", err)
	}

	var decoded DynamicInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal zero value: %v", err)
	}

	if decoded.WatchdogPID != 0 {
		t.Errorf("WatchdogPID = %d, want 0", decoded.WatchdogPID)
	}
	if decoded.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0", decoded.RestartCount)
	}
}

func TestServerClientRequestResponse(t *testing.T) {
	expected := DynamicInfo{
		WatchdogPID:       100,
		WorkerPID:         200,
		WatchdogStartTime: time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC),
		WorkerStartTime:   time.Date(2026, 3, 20, 10, 1, 0, 0, time.UTC),
		RestartCount:      5,
		CurrentBackoff:    8 * time.Second,
	}

	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return expected
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)

	got, err := client.Request()
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	if got.WatchdogPID != expected.WatchdogPID {
		t.Errorf("WatchdogPID = %d, want %d", got.WatchdogPID, expected.WatchdogPID)
	}
	if got.WorkerPID != expected.WorkerPID {
		t.Errorf("WorkerPID = %d, want %d", got.WorkerPID, expected.WorkerPID)
	}
	if got.RestartCount != expected.RestartCount {
		t.Errorf("RestartCount = %d, want %d", got.RestartCount, expected.RestartCount)
	}
	if got.CurrentBackoff != expected.CurrentBackoff {
		t.Errorf("CurrentBackoff = %v, want %v", got.CurrentBackoff, expected.CurrentBackoff)
	}

	cancel()
	_ = client.Close()
}

func TestServerClientMultipleRequests(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	server, pipeFiles, err := NewServer(func() DynamicInfo {
		mu.Lock()
		callCount++
		count := callCount
		mu.Unlock()
		return DynamicInfo{
			RestartCount: count,
		}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)

	for i := 1; i <= 5; i++ {
		got, err := client.Request()
		if err != nil {
			t.Fatalf("Request %d: %v", i, err)
		}
		if got.RestartCount != i {
			t.Errorf("Request %d: RestartCount = %d, want %d", i, got.RestartCount, i)
		}
	}

	cancel()
	_ = client.Close()
}

func TestServerClientDynamicData(t *testing.T) {
	var current DynamicInfo
	var mu sync.Mutex

	server, pipeFiles, err := NewServer(func() DynamicInfo {
		mu.Lock()
		defer mu.Unlock()
		return current
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)

	got, err := client.Request()
	if err != nil {
		t.Fatalf("Request 1: %v", err)
	}
	if got.RestartCount != 0 {
		t.Errorf("initial RestartCount = %d, want 0", got.RestartCount)
	}

	mu.Lock()
	current.RestartCount = 10
	current.HealthError = "timeout"
	mu.Unlock()

	got, err = client.Request()
	if err != nil {
		t.Fatalf("Request 2: %v", err)
	}
	if got.RestartCount != 10 {
		t.Errorf("updated RestartCount = %d, want 10", got.RestartCount)
	}
	if got.HealthError != "timeout" {
		t.Errorf("HealthError = %q, want %q", got.HealthError, "timeout")
	}

	cancel()
	_ = client.Close()
}

func TestServerClientConcurrentRequests(t *testing.T) {
	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return DynamicInfo{
			WatchdogPID:  42,
			RestartCount: 7,
		}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)

	var wg sync.WaitGroup
	errCh := make(chan error, 10)

	for range 10 {
		wg.Go(func() {
			got, err := client.Request()
			if err != nil {
				errCh <- err
				return
			}
			if got.WatchdogPID != 42 {
				errCh <- fmt.Errorf("WatchdogPID = %d, want 42", got.WatchdogPID)
			}
		})
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent request error: %v", err)
	}

	cancel()
	_ = client.Close()
}

func TestServerClose(t *testing.T) {
	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return DynamicInfo{}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	_ = pipeFiles.RequestW.Close()
	_ = pipeFiles.ResponseR.Close()

	server.Close()
	server.Close()
}

func TestServerServeDoneChannel(t *testing.T) {
	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return DynamicInfo{}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	_ = pipeFiles.RequestW.Close()
	_ = pipeFiles.ResponseR.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	go server.Serve(ctx)

	select {
	case <-server.Done():
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("Serve did not exit after context cancellation")
	}
}

func TestClientRequestAfterServerClose(t *testing.T) {
	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return DynamicInfo{WatchdogPID: 1}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Serve(ctx)

	got, err := client.Request()
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if got.WatchdogPID != 1 {
		t.Errorf("WatchdogPID = %d, want 1", got.WatchdogPID)
	}

	cancel()
	closeDone := make(chan struct{})
	go func() {
		server.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("Server.Close blocked on the pending request read")
	}

	requestDone := make(chan error, 1)
	go func() {
		_, err := client.Request()
		requestDone <- err
	}()
	select {
	case err := <-requestDone:
		if err == nil {
			t.Error("expected error after server close")
		}
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("post-close request did not return")
	}

	select {
	case <-server.Done():
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("Serve did not exit after the post-close request")
	}
}

func TestNewClientFromEnvMissing(t *testing.T) {
	t.Setenv(EnvIPCFD, "")
	_, err := NewClientFromEnv()
	if err == nil {
		t.Error("expected error when env var is empty")
	}
}

func TestNewClientFromEnvInvalidFormat(t *testing.T) {
	tests := []struct {
		name string
		val  string
	}{
		{"single value", "3"},
		{"non-numeric", "abc,def"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvIPCFD, tt.val)
			_, err := NewClientFromEnv()
			if err == nil {
				t.Error("expected error for invalid format")
			}
		})
	}
}

func TestDynamicInfoJSONFieldNames(t *testing.T) {
	info := DynamicInfo{
		WatchdogPID:  1,
		WorkerPID:    2,
		RestartCount: 3,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	requiredKeys := []string{
		"watchdog_pid", "worker_pid", "watchdog_start_time",
		"worker_start_time", "restart_count", "current_backoff",
	}
	for _, key := range requiredKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON key %q", key)
		}
	}
}

func TestDynamicInfoJSONOmitEmpty(t *testing.T) {
	info := DynamicInfo{
		WatchdogPID: 1,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	omittedKeys := []string{
		"health_error", "health_failures",
		"update_result", "update_available_ver", "update_status",
	}
	for _, key := range omittedKeys {
		if _, ok := raw[key]; ok {
			t.Errorf("expected key %q to be omitted when zero", key)
		}
	}
}

func TestServerClientHealthData(t *testing.T) {
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	expected := DynamicInfo{
		HealthTimestamp: ts,
		HealthError:     "connection timeout",
		HealthFailures:  3,
	}

	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return expected
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)

	got, err := client.Request()
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	if !got.HealthTimestamp.Equal(ts) {
		t.Errorf("HealthTimestamp = %v, want %v", got.HealthTimestamp, ts)
	}
	if got.HealthError != "connection timeout" {
		t.Errorf("HealthError = %q, want %q", got.HealthError, "connection timeout")
	}
	if got.HealthFailures != 3 {
		t.Errorf("HealthFailures = %d, want 3", got.HealthFailures)
	}

	cancel()
	_ = client.Close()
}

func TestNotifyReadySignal(t *testing.T) {
	readyCh := make(chan struct{}, 1)

	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return DynamicInfo{WatchdogPID: 1}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	server.SetReadyHandler(func() {
		readyCh <- struct{}{}
	})

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)

	if err := client.NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}

	select {
	case <-readyCh:
	case <-time.After(testsupport.Scale(2 * time.Second)):
		t.Fatal("ready handler was not called")
	}

	cancel()
	_ = client.Close()
}

func TestNotifyReadyDoesNotAffectDataRequests(t *testing.T) {
	readyCount := 0
	var mu sync.Mutex

	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return DynamicInfo{WatchdogPID: 42, RestartCount: 3}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	server.SetReadyHandler(func() {
		mu.Lock()
		readyCount++
		mu.Unlock()
	})

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)

	if err := client.NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}

	got, err := client.Request()
	if err != nil {
		t.Fatalf("Request after NotifyReady: %v", err)
	}
	if got.WatchdogPID != 42 {
		t.Errorf("WatchdogPID = %d, want 42", got.WatchdogPID)
	}
	if got.RestartCount != 3 {
		t.Errorf("RestartCount = %d, want 3", got.RestartCount)
	}

	if err := client.NotifyReady(); err != nil {
		t.Fatalf("second NotifyReady: %v", err)
	}

	// small delay for server to process
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if readyCount != 2 {
		t.Errorf("ready handler called %d times, want 2", readyCount)
	}
	mu.Unlock()

	cancel()
	_ = client.Close()
}

func TestSetReadyHandlerNil(t *testing.T) {
	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return DynamicInfo{}
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	server.SetReadyHandler(nil)

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)

	if err := client.NotifyReady(); err != nil {
		t.Fatalf("NotifyReady with nil handler: %v", err)
	}

	got, err := client.Request()
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if got.WatchdogPID != 0 {
		t.Errorf("WatchdogPID = %d, want 0", got.WatchdogPID)
	}

	cancel()
	_ = client.Close()
}

func TestServerClientUpdateData(t *testing.T) {
	ts := time.Date(2026, 3, 20, 11, 0, 0, 0, time.UTC)
	expected := DynamicInfo{
		UpdateLastCheck:    ts,
		UpdateResult:       "available",
		UpdateAvailableVer: "3.0.0",
		UpdateStatus:       "downloading",
	}

	server, pipeFiles, err := NewServer(func() DynamicInfo {
		return expected
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := NewClient(pipeFiles.RequestW, pipeFiles.ResponseR)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Serve(ctx)

	got, err := client.Request()
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	if !got.UpdateLastCheck.Equal(ts) {
		t.Errorf("UpdateLastCheck = %v, want %v", got.UpdateLastCheck, ts)
	}
	if got.UpdateResult != "available" {
		t.Errorf("UpdateResult = %q, want %q", got.UpdateResult, "available")
	}
	if got.UpdateAvailableVer != "3.0.0" {
		t.Errorf("UpdateAvailableVer = %q, want %q", got.UpdateAvailableVer, "3.0.0")
	}
	if got.UpdateStatus != "downloading" {
		t.Errorf("UpdateStatus = %q, want %q", got.UpdateStatus, "downloading")
	}

	cancel()
	_ = client.Close()
}
