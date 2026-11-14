// Package metrics exports daemonpilot's runtime state as Prometheus collectors.
// It is separate from pkg/daemonpilot on purpose: the Prometheus client and its
// protobuf dependency are megabytes of binary that an application which never
// exports metrics should not carry.
package metrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

// Register registers daemonpilot metrics with the provided Prometheus registry.
// Users manage their own HTTP server and /metrics endpoint; this function
// only creates and registers the collectors.
//
// Registered metrics:
//   - daemonpilot_worker_restarts_total       (counter)
//   - daemonpilot_worker_uptime_seconds       (gauge)
//   - daemonpilot_watchdog_uptime_seconds     (gauge)
//   - daemonpilot_current_backoff_seconds     (gauge)
//   - daemonpilot_healthcheck_failures_total  (counter)
//   - daemonpilot_update_last_check_timestamp (gauge)
//   - daemonpilot_version_info                (gauge with version/watchdog_version/init_system labels)
func Register(registry *prometheus.Registry) (*Metrics, error) {
	m := &Metrics{
		WorkerRestarts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "daemonpilot_worker_restarts_total",
			Help: "Total number of worker process restarts.",
		}),
		WorkerUptime: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "daemonpilot_worker_uptime_seconds",
			Help: "Current worker process uptime in seconds.",
		}),
		WatchdogUptime: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "daemonpilot_watchdog_uptime_seconds",
			Help: "Current watchdog process uptime in seconds.",
		}),
		CurrentBackoff: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "daemonpilot_current_backoff_seconds",
			Help: "Current restart backoff delay in seconds.",
		}),
		HealthcheckFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "daemonpilot_healthcheck_failures_total",
			Help: "Total number of health check failures.",
		}),
		UpdateLastCheck: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "daemonpilot_update_last_check_timestamp",
			Help: "Unix timestamp of the last update check.",
		}),
		VersionInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "daemonpilot_version_info",
			Help: "Version information for the daemonpilot process.",
		}, []string{"version", "watchdog_version", "init_system"}),
	}

	collectors := []prometheus.Collector{
		m.WorkerRestarts,
		m.WorkerUptime,
		m.WatchdogUptime,
		m.CurrentBackoff,
		m.HealthcheckFailures,
		m.UpdateLastCheck,
		m.VersionInfo,
	}

	for _, c := range collectors {
		if err := registry.Register(c); err != nil {
			return nil, fmt.Errorf("registering daemonpilot metrics: %w", err)
		}
	}

	return m, nil
}

// Metrics holds all daemonpilot Prometheus metrics collectors.
// After calling Register, use UpdateFromRuntimeInfo to refresh metric values,
// or call StartUpdater to poll RuntimeInfo automatically.
type Metrics struct {
	WorkerRestarts      prometheus.Counter
	WorkerUptime        prometheus.Gauge
	WatchdogUptime      prometheus.Gauge
	CurrentBackoff      prometheus.Gauge
	HealthcheckFailures prometheus.Counter
	UpdateLastCheck     prometheus.Gauge
	VersionInfo         *prometheus.GaugeVec

	mu                 sync.Mutex
	lastRestartCount   int
	lastHealthFailures int
}

// UpdateFromRuntimeInfo refreshes all metric values from a RuntimeInfo snapshot.
// Counter metrics (WorkerRestarts, HealthcheckFailures) are incremented by the
// delta between the current and previous RuntimeInfo values.
func (m *Metrics) UpdateFromRuntimeInfo(info *daemonpilot.RuntimeInfo) {
	if info == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if delta := info.RestartCount - m.lastRestartCount; delta > 0 {
		m.WorkerRestarts.Add(float64(delta))
		m.lastRestartCount = info.RestartCount
	}

	m.WorkerUptime.Set(info.WorkerUptime.Seconds())
	m.WatchdogUptime.Set(info.WatchdogUptime.Seconds())
	m.CurrentBackoff.Set(info.CurrentBackoff.Seconds())

	if delta := info.LastHealthCheck.ConsecutiveFailures - m.lastHealthFailures; delta > 0 {
		m.HealthcheckFailures.Add(float64(delta))
	}
	m.lastHealthFailures = info.LastHealthCheck.ConsecutiveFailures

	if !info.UpdateStatus.LastCheckTime.IsZero() {
		m.UpdateLastCheck.Set(float64(info.UpdateStatus.LastCheckTime.Unix()))
	}

	m.VersionInfo.With(prometheus.Labels{
		"version":          info.Version,
		"watchdog_version": info.WatchdogVersion,
		"init_system":      info.InitSystem,
	}).Set(1)
}

// RuntimeInfoFunc is a function that returns the current RuntimeInfo snapshot.
type RuntimeInfoFunc func() (*daemonpilot.RuntimeInfo, error)

// StartUpdater starts a background goroutine that periodically polls infoFn
// and updates metrics from the returned RuntimeInfo. The goroutine stops when
// ctx is cancelled. It returns immediately.
func (m *Metrics) StartUpdater(ctx context.Context, interval time.Duration, infoFn RuntimeInfoFunc) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	go m.runUpdater(ctx, interval, infoFn)
}

func (m *Metrics) runUpdater(ctx context.Context, interval time.Duration, infoFn RuntimeInfoFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := infoFn()
			if err != nil {
				continue
			}
			m.UpdateFromRuntimeInfo(info)
		}
	}
}
