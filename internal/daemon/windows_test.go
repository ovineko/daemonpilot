//go:build windows

package daemon

import (
	"testing"

	"golang.org/x/sys/windows/svc/mgr"
)

type fakeWindowsServiceConfig struct {
	config mgr.Config
}

func (f *fakeWindowsServiceConfig) Config() (mgr.Config, error) {
	return f.config, nil
}

func (f *fakeWindowsServiceConfig) UpdateConfig(config mgr.Config) error {
	f.config = config
	return nil
}

func TestUpdateWindowsSCMBinaryPathPreservesStartTypeAcrossRecoveryTransitions(t *testing.T) {
	for _, test := range []struct {
		name      string
		startType uint32
	}{
		{name: "disabled", startType: mgr.StartDisabled},
		{name: "demand start", startType: mgr.StartManual},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeWindowsServiceConfig{config: mgr.Config{
				BinaryPathName: `C:\ProgramData\agent\agent.exe`,
				StartType:      test.startType,
				DisplayName:    "agent service",
			}}
			for _, path := range []string{
				`C:\ProgramData\agent\agent.new.exe`,
				`C:\ProgramData\agent\agent.exe`,
			} {
				if err := updateWindowsSCMBinaryPath(service, path); err != nil {
					t.Fatalf("updateWindowsSCMBinaryPath(%q): %v", path, err)
				}
				if service.config.BinaryPathName != path {
					t.Fatalf("BinaryPathName = %q, want %q", service.config.BinaryPathName, path)
				}
				if service.config.StartType != test.startType {
					t.Fatalf("StartType = %d, want %d", service.config.StartType, test.startType)
				}
				if service.config.DisplayName != "agent service" {
					t.Fatalf("DisplayName = %q, want it preserved", service.config.DisplayName)
				}
			}
		})
	}
}
