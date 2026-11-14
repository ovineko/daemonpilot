package daemonpilot

import (
	"encoding/json"
	"testing"
)

func TestVersionDefaults(t *testing.T) {
	if Version != "dev" {
		t.Errorf("expected Version default 'dev', got %q", Version)
	}
	if WatchdogVersion != "1" {
		t.Errorf("expected WatchdogVersion default '1', got %q", WatchdogVersion)
	}
}

func TestConfigZeroValue(t *testing.T) {
	var cfg Config
	if cfg.Name != "" {
		t.Error("expected empty Name")
	}
	if cfg.Update != nil {
		t.Error("expected nil Update")
	}
	if cfg.WorkerFunc != nil {
		t.Error("expected nil WorkerFunc")
	}
	if cfg.SelftestFunc != nil {
		t.Error("expected nil SelftestFunc")
	}
	if cfg.GracefulTimeout != 0 {
		t.Error("expected zero GracefulTimeout")
	}
	if cfg.Logger != nil {
		t.Error("expected nil Logger")
	}
	if cfg.ConfigPatchers != nil {
		t.Error("expected nil ConfigPatchers")
	}
	if cfg.ServiceEnvironment != nil {
		t.Error("expected nil ServiceEnvironment")
	}
	if cfg.ServiceArguments != nil {
		t.Error("expected nil ServiceArguments")
	}
	if cfg.ServiceUser != "" {
		t.Error("expected empty ServiceUser")
	}
	if cfg.ServiceGroup != "" {
		t.Error("expected empty ServiceGroup")
	}
	if cfg.SystemdDirectives != nil {
		t.Error("expected nil SystemdDirectives")
	}
	if cfg.LaunchdKeys != nil {
		t.Error("expected nil LaunchdKeys")
	}
	if cfg.OpenRCDirectives != nil {
		t.Error("expected nil OpenRCDirectives")
	}
	if cfg.CrontabSchedule != "" {
		t.Error("expected empty CrontabSchedule")
	}
}

func TestConfigWithEmptyPatchers(t *testing.T) {
	cfg := Config{
		Name:           "test-service",
		ConfigPatchers: []ConfigPatcher{},
	}
	if cfg.ConfigPatchers == nil {
		t.Error("expected non-nil empty ConfigPatchers slice")
	}
	if len(cfg.ConfigPatchers) != 0 {
		t.Errorf("expected 0 patchers, got %d", len(cfg.ConfigPatchers))
	}
}

func TestConfigWithPatchers(t *testing.T) {
	noop := func(path string, content string, info PatchInfo) (string, error) {
		return content, nil
	}
	cfg := Config{
		Name:           "test-service",
		ConfigPatchers: []ConfigPatcher{noop},
	}
	if len(cfg.ConfigPatchers) != 1 {
		t.Errorf("expected 1 patcher, got %d", len(cfg.ConfigPatchers))
	}
	result, err := cfg.ConfigPatchers[0]("test.conf", "content", PatchInfo{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if result != "content" {
		t.Errorf("expected 'content', got %q", result)
	}
}

func TestUpdateConfigZeroValue(t *testing.T) {
	var cfg UpdateConfig
	if cfg.Source != nil {
		t.Error("expected nil Source")
	}
	if cfg.PublicKey != nil {
		t.Error("expected nil PublicKey")
	}
	if cfg.CheckInterval != 0 {
		t.Error("expected zero CheckInterval")
	}
	if cfg.StagedUpdateFunc != nil {
		t.Error("expected nil StagedUpdateFunc")
	}
	if cfg.RollbackConfirmFunc != nil {
		t.Error("expected nil RollbackConfirmFunc")
	}
}

func TestUpdateInfoJSONRoundTrip(t *testing.T) {
	info := UpdateInfo{
		Version:         "1.5.0",
		WatchdogVersion: "2",
		Arch:            "linux/amd64",
		Size:            12345678,
		SHA256:          "abc123def456",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded UpdateInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded != info {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", decoded, info)
	}
}

func TestUpdateInfoJSONFields(t *testing.T) {
	info := UpdateInfo{
		Version:         "1.0.0",
		WatchdogVersion: "1",
		Arch:            "darwin/arm64",
		Size:            9999,
		SHA256:          "deadbeef",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	expectedKeys := []string{"version", "watchdog_version", "arch", "size", "sha256"}
	for _, key := range expectedKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON key %q", key)
		}
	}
}

func TestSelftestResultJSONRoundTrip(t *testing.T) {
	result := SelftestResult{
		OK: true,
		Checks: map[string]CheckResult{
			"startup":      {OK: true},
			"connectivity": {OK: true, Message: "connected", LatencyMS: 45},
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded SelftestResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.OK != result.OK {
		t.Errorf("OK mismatch: got %v, want %v", decoded.OK, result.OK)
	}
	if len(decoded.Checks) != len(result.Checks) {
		t.Errorf("Checks count mismatch: got %d, want %d", len(decoded.Checks), len(result.Checks))
	}
	if !decoded.Checks["startup"].OK {
		t.Error("startup check should be OK")
	}
	if !decoded.Checks["connectivity"].OK {
		t.Error("connectivity check should be OK")
	}
}

func TestSelftestResultJSONFieldNames(t *testing.T) {
	result := SelftestResult{
		OK: false,
		Checks: map[string]CheckResult{
			"test": {OK: false, Message: "failed"},
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	if _, ok := raw["success"]; !ok {
		t.Error("expected JSON key 'success' (not 'ok')")
	}
	if _, ok := raw["checks"]; !ok {
		t.Error("expected JSON key 'checks'")
	}
}

func TestCheckResultOmitEmpty(t *testing.T) {
	result := CheckResult{OK: true}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	if _, ok := raw["message"]; ok {
		t.Error("empty message should be omitted from JSON")
	}
	if _, ok := raw["latency_ms"]; ok {
		t.Error("zero latency should be omitted from JSON")
	}
}

func TestSelftestResultFailure(t *testing.T) {
	result := SelftestResult{
		OK: false,
		Checks: map[string]CheckResult{
			"startup": {OK: true},
			"crypto":  {OK: false, Message: "key mismatch"},
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded SelftestResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.OK {
		t.Error("expected OK to be false")
	}
	if decoded.Checks["crypto"].Message != "key mismatch" {
		t.Errorf("expected message 'key mismatch', got %q", decoded.Checks["crypto"].Message)
	}
}

func TestSelftestResultEmptyChecks(t *testing.T) {
	result := SelftestResult{OK: true}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded SelftestResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !decoded.OK {
		t.Error("expected OK to be true")
	}
}
