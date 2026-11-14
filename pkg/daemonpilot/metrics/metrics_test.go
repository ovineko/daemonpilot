package metrics

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

func TestRegisterMetrics(t *testing.T) {
	tests := []struct {
		name    string
		setup   func() *prometheus.Registry
		wantErr bool
	}{
		{
			name:    "registers all metrics with fresh registry",
			setup:   prometheus.NewRegistry,
			wantErr: false,
		},
		{
			name: "fails on duplicate registration",
			setup: func() *prometheus.Registry {
				reg := prometheus.NewRegistry()
				reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{
					Name: "daemonpilot_worker_restarts_total",
					Help: "duplicate",
				}))
				return reg
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := tt.setup()
			m, err := Register(reg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m == nil {
				t.Fatal("expected non-nil Metrics")
			}
			if m.WorkerRestarts == nil {
				t.Error("WorkerRestarts is nil")
			}
			if m.WorkerUptime == nil {
				t.Error("WorkerUptime is nil")
			}
			if m.WatchdogUptime == nil {
				t.Error("WatchdogUptime is nil")
			}
			if m.CurrentBackoff == nil {
				t.Error("CurrentBackoff is nil")
			}
			if m.HealthcheckFailures == nil {
				t.Error("HealthcheckFailures is nil")
			}
			if m.UpdateLastCheck == nil {
				t.Error("UpdateLastCheck is nil")
			}
			if m.VersionInfo == nil {
				t.Error("VersionInfo is nil")
			}
		})
	}
}

func TestRegisterMetrics_AllCollectorsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := Register(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}

	expectedNames := map[string]bool{
		"daemonpilot_worker_restarts_total":       false,
		"daemonpilot_worker_uptime_seconds":       false,
		"daemonpilot_watchdog_uptime_seconds":     false,
		"daemonpilot_current_backoff_seconds":     false,
		"daemonpilot_healthcheck_failures_total":  false,
		"daemonpilot_update_last_check_timestamp": false,
	}

	for _, f := range families {
		if _, ok := expectedNames[f.GetName()]; ok {
			expectedNames[f.GetName()] = true
		}
	}

	for name, found := range expectedNames {
		if !found {
			t.Errorf("metric %q not found in gathered families", name)
		}
	}
}

func TestMetrics_UpdateFromRuntimeInfo(t *testing.T) {
	tests := []struct {
		name string
		info *daemonpilot.RuntimeInfo
	}{
		{
			name: "nil info is no-op",
			info: nil,
		},
		{
			name: "populates all fields",
			info: &daemonpilot.RuntimeInfo{
				Version:         "1.2.3",
				WatchdogVersion: "4",
				InitSystem:      "systemd",
				WorkerUptime:    5 * time.Minute,
				WatchdogUptime:  10 * time.Minute,
				CurrentBackoff:  2 * time.Second,
				RestartCount:    3,
				LastHealthCheck: daemonpilot.HealthCheckInfo{
					ConsecutiveFailures: 1,
				},
				UpdateStatus: daemonpilot.UpdateStatusInfo{
					LastCheckTime: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
				},
			},
		},
		{
			name: "zero update time skips timestamp",
			info: &daemonpilot.RuntimeInfo{
				Version:         "0.1.0",
				WatchdogVersion: "1",
				InitSystem:      "busybox",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m, err := Register(reg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			m.UpdateFromRuntimeInfo(tt.info)

			if tt.info == nil {
				return
			}

			workerUp := getGaugeValue(t, m.WorkerUptime)
			if workerUp != tt.info.WorkerUptime.Seconds() {
				t.Errorf("WorkerUptime = %v, want %v", workerUp, tt.info.WorkerUptime.Seconds())
			}

			watchdogUp := getGaugeValue(t, m.WatchdogUptime)
			if watchdogUp != tt.info.WatchdogUptime.Seconds() {
				t.Errorf("WatchdogUptime = %v, want %v", watchdogUp, tt.info.WatchdogUptime.Seconds())
			}

			backoff := getGaugeValue(t, m.CurrentBackoff)
			if backoff != tt.info.CurrentBackoff.Seconds() {
				t.Errorf("CurrentBackoff = %v, want %v", backoff, tt.info.CurrentBackoff.Seconds())
			}

			if !tt.info.UpdateStatus.LastCheckTime.IsZero() {
				lastCheck := getGaugeValue(t, m.UpdateLastCheck)
				want := float64(tt.info.UpdateStatus.LastCheckTime.Unix())
				if lastCheck != want {
					t.Errorf("UpdateLastCheck = %v, want %v", lastCheck, want)
				}
			}
		})
	}
}

func TestMetrics_UpdateFromRuntimeInfo_CounterDeltas(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := Register(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m.UpdateFromRuntimeInfo(&daemonpilot.RuntimeInfo{
		RestartCount: 3,
		LastHealthCheck: daemonpilot.HealthCheckInfo{
			ConsecutiveFailures: 2,
		},
		Version:         "1.0.0",
		WatchdogVersion: "1",
	})

	restarts := getCounterValue(t, m.WorkerRestarts)
	if restarts != 3 {
		t.Errorf("WorkerRestarts after first update = %v, want 3", restarts)
	}
	healthFails := getCounterValue(t, m.HealthcheckFailures)
	if healthFails != 2 {
		t.Errorf("HealthcheckFailures after first update = %v, want 2", healthFails)
	}

	m.UpdateFromRuntimeInfo(&daemonpilot.RuntimeInfo{
		RestartCount: 5,
		LastHealthCheck: daemonpilot.HealthCheckInfo{
			ConsecutiveFailures: 4,
		},
		Version:         "1.0.0",
		WatchdogVersion: "1",
	})

	restarts = getCounterValue(t, m.WorkerRestarts)
	if restarts != 5 {
		t.Errorf("WorkerRestarts after second update = %v, want 5", restarts)
	}
	healthFails = getCounterValue(t, m.HealthcheckFailures)
	if healthFails != 4 {
		t.Errorf("HealthcheckFailures after second update = %v, want 4", healthFails)
	}

	// Same values should not increment counters
	m.UpdateFromRuntimeInfo(&daemonpilot.RuntimeInfo{
		RestartCount: 5,
		LastHealthCheck: daemonpilot.HealthCheckInfo{
			ConsecutiveFailures: 4,
		},
		Version:         "1.0.0",
		WatchdogVersion: "1",
	})

	restarts = getCounterValue(t, m.WorkerRestarts)
	if restarts != 5 {
		t.Errorf("WorkerRestarts after no-change update = %v, want 5", restarts)
	}
	healthFails = getCounterValue(t, m.HealthcheckFailures)
	if healthFails != 4 {
		t.Errorf("HealthcheckFailures after no-change update = %v, want 4", healthFails)
	}
}

func TestMetrics_UpdateFromRuntimeInfo_HealthFailuresReset(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := Register(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m.UpdateFromRuntimeInfo(&daemonpilot.RuntimeInfo{
		LastHealthCheck: daemonpilot.HealthCheckInfo{ConsecutiveFailures: 3},
		Version:         "1.0.0",
		WatchdogVersion: "1",
	})

	healthFails := getCounterValue(t, m.HealthcheckFailures)
	if healthFails != 3 {
		t.Errorf("HealthcheckFailures = %v, want 3", healthFails)
	}

	// Failures reset to 0 (health recovered), then rise again
	m.UpdateFromRuntimeInfo(&daemonpilot.RuntimeInfo{
		LastHealthCheck: daemonpilot.HealthCheckInfo{ConsecutiveFailures: 0},
		Version:         "1.0.0",
		WatchdogVersion: "1",
	})
	// Counter should not change (no positive delta)
	healthFails = getCounterValue(t, m.HealthcheckFailures)
	if healthFails != 3 {
		t.Errorf("HealthcheckFailures after reset = %v, want 3", healthFails)
	}

	// New failures after reset
	m.UpdateFromRuntimeInfo(&daemonpilot.RuntimeInfo{
		LastHealthCheck: daemonpilot.HealthCheckInfo{ConsecutiveFailures: 2},
		Version:         "1.0.0",
		WatchdogVersion: "1",
	})
	healthFails = getCounterValue(t, m.HealthcheckFailures)
	if healthFails != 5 {
		t.Errorf("HealthcheckFailures after new failures = %v, want 5", healthFails)
	}
}

func TestMetrics_UpdateFromRuntimeInfo_VersionInfoLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := Register(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m.UpdateFromRuntimeInfo(&daemonpilot.RuntimeInfo{
		Version:         "2.0.0",
		WatchdogVersion: "3",
		InitSystem:      "runit",
	})

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}

	var found bool
	for _, f := range families {
		if f.GetName() != "daemonpilot_version_info" {
			continue
		}
		for _, metric := range f.GetMetric() {
			labels := make(map[string]string)
			for _, lp := range metric.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["version"] == "2.0.0" && labels["watchdog_version"] == "3" && labels["init_system"] == "runit" {
				found = true
				if metric.GetGauge().GetValue() != 1 {
					t.Errorf("version_info gauge value = %v, want 1", metric.GetGauge().GetValue())
				}
			}
		}
	}
	if !found {
		t.Error("daemonpilot_version_info metric with expected labels not found")
	}
}

func TestMetrics_StartUpdater(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := Register(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var callCount atomic.Int64
	infoFn := func() (*daemonpilot.RuntimeInfo, error) {
		n := int(callCount.Add(1))
		return &daemonpilot.RuntimeInfo{
			Version:         "1.0.0",
			WatchdogVersion: "1",
			InitSystem:      "systemd",
			RestartCount:    n,
			WorkerUptime:    time.Duration(n) * time.Second,
			WatchdogUptime:  time.Duration(n) * 2 * time.Second,
			CurrentBackoff:  time.Duration(n) * 100 * time.Millisecond,
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.StartUpdater(ctx, 10*time.Millisecond, infoFn)

	time.Sleep(80 * time.Millisecond)
	cancel()

	time.Sleep(20 * time.Millisecond)

	if callCount.Load() < 2 {
		t.Errorf("infoFn called %d times, want at least 2", callCount.Load())
	}

	restarts := getCounterValue(t, m.WorkerRestarts)
	if restarts < 2 {
		t.Errorf("WorkerRestarts = %v, want at least 2", restarts)
	}
}

func TestMetrics_StartUpdater_StopsOnCancel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := Register(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var callCount atomic.Int64
	infoFn := func() (*daemonpilot.RuntimeInfo, error) {
		callCount.Add(1)
		return &daemonpilot.RuntimeInfo{
			Version:         "1.0.0",
			WatchdogVersion: "1",
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.StartUpdater(ctx, 10*time.Millisecond, infoFn)

	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(30 * time.Millisecond)

	countAtCancel := callCount.Load()
	time.Sleep(50 * time.Millisecond)

	if callCount.Load() != countAtCancel {
		t.Errorf("infoFn still being called after cancel: %d vs %d", callCount.Load(), countAtCancel)
	}
}

func TestMetrics_StartUpdater_SkipsOnError(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := Register(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var callCount atomic.Int64
	infoFn := func() (*daemonpilot.RuntimeInfo, error) {
		callCount.Add(1)
		return nil, context.DeadlineExceeded
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.StartUpdater(ctx, 10*time.Millisecond, infoFn)

	time.Sleep(50 * time.Millisecond)
	cancel()

	if callCount.Load() < 2 {
		t.Errorf("infoFn called %d times, want at least 2", callCount.Load())
	}

	restarts := getCounterValue(t, m.WorkerRestarts)
	if restarts != 0 {
		t.Errorf("WorkerRestarts = %v, want 0 (all errors)", restarts)
	}
}

func getGaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("failed to write gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func getCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("failed to write counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
