package daemonpilot

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRuntimeModeConstants(t *testing.T) {
	tests := []struct {
		mode RuntimeMode
		want string
	}{
		{ModeWatchdog, "watchdog"},
		{ModeWorker, "worker"},
		{ModeExecThrough, "exec-through"},
		{ModeStandalone, "standalone"},
	}
	for _, tt := range tests {
		if string(tt.mode) != tt.want {
			t.Errorf("got %q, want %q", tt.mode, tt.want)
		}
	}
}

func TestRuntimeInfoZeroValue(t *testing.T) {
	var info RuntimeInfo
	if info.Mode != "" {
		t.Error("expected empty Mode")
	}
	if info.IsExecThrough {
		t.Error("expected false IsExecThrough")
	}
	if info.Version != "" {
		t.Error("expected empty Version")
	}
	if info.WatchdogVersion != "" {
		t.Error("expected empty WatchdogVersion")
	}
	if info.BinaryPath != "" {
		t.Error("expected empty BinaryPath")
	}
	if info.WatchdogPID != 0 {
		t.Error("expected zero WatchdogPID")
	}
	if info.WorkerPID != 0 {
		t.Error("expected zero WorkerPID")
	}
	if info.WatchdogUptime != 0 {
		t.Error("expected zero WatchdogUptime")
	}
	if info.WorkerUptime != 0 {
		t.Error("expected zero WorkerUptime")
	}
	if info.InitSystem != "" {
		t.Error("expected empty InitSystem")
	}
	if info.CanAutoRestart {
		t.Error("expected false CanAutoRestart")
	}
	if info.RestartCount != 0 {
		t.Error("expected zero RestartCount")
	}
	if info.CurrentBackoff != 0 {
		t.Error("expected zero CurrentBackoff")
	}
}

func TestRuntimeInfoCreation(t *testing.T) {
	info := RuntimeInfo{
		Mode:            ModeWorker,
		IsExecThrough:   false,
		Version:         "1.2.3",
		WatchdogVersion: "2",
		BinaryPath:      "/opt/myapp/myapp",
		WatchdogPID:     1234,
		WorkerPID:       5678,
		WatchdogUptime:  10 * time.Minute,
		WorkerUptime:    5 * time.Minute,
		InitSystem:      "systemd",
		CanAutoRestart:  true,
		RestartCount:    3,
		CurrentBackoff:  4 * time.Second,
		LastHealthCheck: HealthCheckInfo{
			Timestamp:           time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
			ConsecutiveFailures: 0,
		},
		UpdateStatus: UpdateStatusInfo{
			LastCheckTime: time.Date(2026, 3, 20, 11, 55, 0, 0, time.UTC),
			Result:        "up-to-date",
			CurrentStatus: "idle",
		},
	}

	if info.Mode != ModeWorker {
		t.Errorf("Mode = %q, want %q", info.Mode, ModeWorker)
	}
	if info.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", info.Version, "1.2.3")
	}
	if info.WatchdogPID != 1234 {
		t.Errorf("WatchdogPID = %d, want %d", info.WatchdogPID, 1234)
	}
	if info.RestartCount != 3 {
		t.Errorf("RestartCount = %d, want %d", info.RestartCount, 3)
	}
	if !info.CanAutoRestart {
		t.Error("expected CanAutoRestart to be true")
	}
}

func TestRuntimeInfoJSONRoundTrip(t *testing.T) {
	info := RuntimeInfo{
		Mode:            ModeExecThrough,
		IsExecThrough:   true,
		Version:         "2.0.0",
		WatchdogVersion: "3",
		BinaryPath:      "/usr/local/bin/myapp",
		WatchdogPID:     100,
		WorkerPID:       200,
		WatchdogUptime:  time.Hour,
		WorkerUptime:    30 * time.Minute,
		InitSystem:      "busybox",
		CanAutoRestart:  false,
		RestartCount:    5,
		CurrentBackoff:  8 * time.Second,
		LastHealthCheck: HealthCheckInfo{
			Timestamp:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Error:               "connection refused",
			ConsecutiveFailures: 2,
		},
		UpdateStatus: UpdateStatusInfo{
			LastCheckTime:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Result:           "available",
			AvailableVersion: "2.1.0",
			CurrentStatus:    "downloading",
		},
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded RuntimeInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Mode != info.Mode {
		t.Errorf("Mode = %q, want %q", decoded.Mode, info.Mode)
	}
	if decoded.IsExecThrough != info.IsExecThrough {
		t.Errorf("IsExecThrough = %v, want %v", decoded.IsExecThrough, info.IsExecThrough)
	}
	if decoded.Version != info.Version {
		t.Errorf("Version = %q, want %q", decoded.Version, info.Version)
	}
	if decoded.WatchdogVersion != info.WatchdogVersion {
		t.Errorf("WatchdogVersion = %q, want %q", decoded.WatchdogVersion, info.WatchdogVersion)
	}
	if decoded.BinaryPath != info.BinaryPath {
		t.Errorf("BinaryPath = %q, want %q", decoded.BinaryPath, info.BinaryPath)
	}
	if decoded.WatchdogPID != info.WatchdogPID {
		t.Errorf("WatchdogPID = %d, want %d", decoded.WatchdogPID, info.WatchdogPID)
	}
	if decoded.WorkerPID != info.WorkerPID {
		t.Errorf("WorkerPID = %d, want %d", decoded.WorkerPID, info.WorkerPID)
	}
	if decoded.InitSystem != info.InitSystem {
		t.Errorf("InitSystem = %q, want %q", decoded.InitSystem, info.InitSystem)
	}
	if decoded.CanAutoRestart != info.CanAutoRestart {
		t.Errorf("CanAutoRestart = %v, want %v", decoded.CanAutoRestart, info.CanAutoRestart)
	}
	if decoded.RestartCount != info.RestartCount {
		t.Errorf("RestartCount = %d, want %d", decoded.RestartCount, info.RestartCount)
	}
	if decoded.LastHealthCheck.Error != info.LastHealthCheck.Error {
		t.Errorf("LastHealthCheck.Error = %q, want %q", decoded.LastHealthCheck.Error, info.LastHealthCheck.Error)
	}
	if decoded.LastHealthCheck.ConsecutiveFailures != info.LastHealthCheck.ConsecutiveFailures {
		t.Errorf("ConsecutiveFailures = %d, want %d", decoded.LastHealthCheck.ConsecutiveFailures, info.LastHealthCheck.ConsecutiveFailures)
	}
	if decoded.UpdateStatus.AvailableVersion != info.UpdateStatus.AvailableVersion {
		t.Errorf("AvailableVersion = %q, want %q", decoded.UpdateStatus.AvailableVersion, info.UpdateStatus.AvailableVersion)
	}
	if decoded.UpdateStatus.CurrentStatus != info.UpdateStatus.CurrentStatus {
		t.Errorf("CurrentStatus = %q, want %q", decoded.UpdateStatus.CurrentStatus, info.UpdateStatus.CurrentStatus)
	}
}

func TestRuntimeInfoJSONFieldNames(t *testing.T) {
	info := RuntimeInfo{
		Mode:           ModeWatchdog,
		Version:        "1.0.0",
		InitSystem:     "systemd",
		CanAutoRestart: true,
		WatchdogPID:    42,
		RestartCount:   1,
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
		"mode", "is_exec_through", "version", "watchdog_version",
		"binary_path", "init_system", "can_auto_restart", "is_embedded",
	}
	for _, key := range requiredKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON key %q", key)
		}
	}
}

func TestRuntimeInfoJSONOmitEmpty(t *testing.T) {
	info := RuntimeInfo{
		Mode:       ModeWorker,
		Version:    "1.0.0",
		InitSystem: "systemd",
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
		"watchdog_pid", "worker_pid", "watchdog_uptime", "worker_uptime",
		"restart_count", "current_backoff",
	}
	for _, key := range omittedKeys {
		if _, ok := raw[key]; ok {
			t.Errorf("expected key %q to be omitted when zero", key)
		}
	}
}

func TestHealthCheckInfoZeroValue(t *testing.T) {
	var hc HealthCheckInfo
	if !hc.Timestamp.IsZero() {
		t.Error("expected zero Timestamp")
	}
	if hc.Error != "" {
		t.Error("expected empty Error")
	}
	if hc.ConsecutiveFailures != 0 {
		t.Error("expected zero ConsecutiveFailures")
	}
}

func TestUpdateStatusInfoZeroValue(t *testing.T) {
	var us UpdateStatusInfo
	if !us.LastCheckTime.IsZero() {
		t.Error("expected zero LastCheckTime")
	}
	if us.Result != "" {
		t.Error("expected empty Result")
	}
	if us.AvailableVersion != "" {
		t.Error("expected empty AvailableVersion")
	}
	if us.CurrentStatus != "" {
		t.Error("expected empty CurrentStatus")
	}
}

func TestHealthCheckInfoJSONRoundTrip(t *testing.T) {
	hc := HealthCheckInfo{
		Timestamp:           time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Error:               "timeout",
		ConsecutiveFailures: 3,
	}

	data, err := json.Marshal(hc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded HealthCheckInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !decoded.Timestamp.Equal(hc.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", decoded.Timestamp, hc.Timestamp)
	}
	if decoded.Error != hc.Error {
		t.Errorf("Error = %q, want %q", decoded.Error, hc.Error)
	}
	if decoded.ConsecutiveFailures != hc.ConsecutiveFailures {
		t.Errorf("ConsecutiveFailures = %d, want %d", decoded.ConsecutiveFailures, hc.ConsecutiveFailures)
	}
}

func TestUpdateStatusInfoJSONRoundTrip(t *testing.T) {
	us := UpdateStatusInfo{
		LastCheckTime:    time.Date(2026, 3, 20, 11, 0, 0, 0, time.UTC),
		Result:           "available",
		AvailableVersion: "2.0.0",
		CurrentStatus:    "downloading",
	}

	data, err := json.Marshal(us)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded UpdateStatusInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !decoded.LastCheckTime.Equal(us.LastCheckTime) {
		t.Errorf("LastCheckTime = %v, want %v", decoded.LastCheckTime, us.LastCheckTime)
	}
	if decoded.Result != us.Result {
		t.Errorf("Result = %q, want %q", decoded.Result, us.Result)
	}
	if decoded.AvailableVersion != us.AvailableVersion {
		t.Errorf("AvailableVersion = %q, want %q", decoded.AvailableVersion, us.AvailableVersion)
	}
	if decoded.CurrentStatus != us.CurrentStatus {
		t.Errorf("CurrentStatus = %q, want %q", decoded.CurrentStatus, us.CurrentStatus)
	}
}

func TestReadStaticEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want RuntimeInfo
	}{
		{
			name: "all env vars set",
			env: map[string]string{
				EnvMode:            "worker",
				EnvVersion:         "1.2.3",
				EnvWatchdogVersion: "2",
				EnvBinaryPath:      "/opt/myapp/myapp",
				EnvInitSystem:      "systemd",
				EnvCanAutoRestart:  "true",
				EnvIsExecThrough:   "false",
			},
			want: RuntimeInfo{
				Mode:            ModeWorker,
				Version:         "1.2.3",
				WatchdogVersion: "2",
				BinaryPath:      "/opt/myapp/myapp",
				InitSystem:      "systemd",
				CanAutoRestart:  true,
				IsExecThrough:   false,
			},
		},
		{
			name: "exec-through mode",
			env: map[string]string{
				EnvMode:            "exec-through",
				EnvVersion:         "2.0.0",
				EnvWatchdogVersion: "3",
				EnvBinaryPath:      "/usr/local/bin/myapp",
				EnvInitSystem:      "crontab",
				EnvCanAutoRestart:  "false",
				EnvIsExecThrough:   "true",
			},
			want: RuntimeInfo{
				Mode:            ModeExecThrough,
				Version:         "2.0.0",
				WatchdogVersion: "3",
				BinaryPath:      "/usr/local/bin/myapp",
				InitSystem:      "crontab",
				CanAutoRestart:  false,
				IsExecThrough:   true,
			},
		},
		{
			name: "no env vars set",
			env:  map[string]string{},
			want: RuntimeInfo{},
		},
		{
			name: "partial env vars",
			env: map[string]string{
				EnvMode:    "worker",
				EnvVersion: "1.0.0",
			},
			want: RuntimeInfo{
				Mode:    ModeWorker,
				Version: "1.0.0",
			},
		},
		{
			name: "invalid boolean values default to false",
			env: map[string]string{
				EnvCanAutoRestart: "yes",
				EnvIsExecThrough:  "1",
			},
			want: RuntimeInfo{
				CanAutoRestart: false,
				IsExecThrough:  false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origLookup := envLookup
			defer func() { envLookup = origLookup }()

			envLookup = func(key string) string {
				return tt.env[key]
			}

			got := ReadStaticEnv()

			if got.Mode != tt.want.Mode {
				t.Errorf("Mode = %q, want %q", got.Mode, tt.want.Mode)
			}
			if got.Version != tt.want.Version {
				t.Errorf("Version = %q, want %q", got.Version, tt.want.Version)
			}
			if got.WatchdogVersion != tt.want.WatchdogVersion {
				t.Errorf("WatchdogVersion = %q, want %q", got.WatchdogVersion, tt.want.WatchdogVersion)
			}
			if got.BinaryPath != tt.want.BinaryPath {
				t.Errorf("BinaryPath = %q, want %q", got.BinaryPath, tt.want.BinaryPath)
			}
			if got.InitSystem != tt.want.InitSystem {
				t.Errorf("InitSystem = %q, want %q", got.InitSystem, tt.want.InitSystem)
			}
			if got.CanAutoRestart != tt.want.CanAutoRestart {
				t.Errorf("CanAutoRestart = %v, want %v", got.CanAutoRestart, tt.want.CanAutoRestart)
			}
			if got.IsExecThrough != tt.want.IsExecThrough {
				t.Errorf("IsExecThrough = %v, want %v", got.IsExecThrough, tt.want.IsExecThrough)
			}
		})
	}
}

func TestEnvVarConstants(t *testing.T) {
	expected := map[string]string{
		"DAEMONPILOT_MODE":                   EnvMode,
		"DAEMONPILOT_VERSION":                EnvVersion,
		"DAEMONPILOT_WATCHDOG_VERSION":       EnvWatchdogVersion,
		"DAEMONPILOT_BINARY_PATH":            EnvBinaryPath,
		"DAEMONPILOT_INIT_SYSTEM":            EnvInitSystem,
		"DAEMONPILOT_CAN_AUTO_RESTART":       EnvCanAutoRestart,
		"DAEMONPILOT_IS_EXEC_THROUGH":        EnvIsExecThrough,
		"DAEMONPILOT_PARENT_PID":             EnvParentPID,
		"DAEMONPILOT_DISABLE_PARENT_MONITOR": EnvDisableParentMonitor,
		"DAEMONPILOT_LOCK_STATUS":            EnvLockStatus,
	}
	for want, got := range expected {
		if got != want {
			t.Errorf("constant value = %q, want %q", got, want)
		}
	}
}

func TestRuntimeInfoIsEmbeddedField(t *testing.T) {
	tests := []struct {
		initSystem   string
		wantEmbedded bool
	}{
		{"busybox", true},
		{"procd", true},
		{"s6", true},
		{"runit", true},
		{"systemd", false},
		{"launchd", false},
		{"openrc", false},
		{"crontab", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.initSystem, func(t *testing.T) {
			origLookup := envLookup
			defer func() { envLookup = origLookup }()

			envLookup = func(key string) string {
				if key == EnvInitSystem {
					return tt.initSystem
				}
				return ""
			}

			info := ReadStaticEnv()
			if info.IsEmbedded != tt.wantEmbedded {
				t.Errorf("IsEmbedded = %v, want %v for init system %q", info.IsEmbedded, tt.wantEmbedded, tt.initSystem)
			}
		})
	}
}

func TestRuntimeInfoIsEmbeddedInJSON(t *testing.T) {
	info := RuntimeInfo{
		Mode:       ModeWorker,
		InitSystem: "busybox",
		IsEmbedded: true,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if val, ok := raw["is_embedded"]; !ok {
		t.Error("missing is_embedded in JSON")
	} else if val != true {
		t.Errorf("is_embedded = %v, want true", val)
	}
}

func TestRuntimeInfoExecThroughMode(t *testing.T) {
	info := RuntimeInfo{
		Mode:           ModeExecThrough,
		IsExecThrough:  true,
		Version:        "1.0.0",
		InitSystem:     "crontab",
		CanAutoRestart: false,
	}

	if info.Mode != ModeExecThrough {
		t.Errorf("Mode = %q, want %q", info.Mode, ModeExecThrough)
	}
	if !info.IsExecThrough {
		t.Error("expected IsExecThrough to be true")
	}
	if info.CanAutoRestart {
		t.Error("expected CanAutoRestart to be false for crontab")
	}
}
