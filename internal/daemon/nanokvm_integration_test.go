package daemon

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestDetectServiceManager_NanokvmFullChain(t *testing.T) {
	runitMgr := newTestRunitManager(func(m *runitManager) {
		m.lookPathFunc = func(name string) (string, error) {
			if name == "runsvdir" {
				return "/usr/bin/runsvdir", nil
			}
			return "", errors.New("not found")
		}
		m.statFunc = func(path string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		}
	})

	sysvinitMgr := newTestSysvinitManager(func(m *sysvinitManager) {
		m.statFunc = func(path string) (os.FileInfo, error) {
			if path == "/etc/inittab" {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
		m.readlinkFunc = func(path string) (string, error) {
			if path == "/proc/1/exe" {
				return "/usr/sbin/init", nil
			}
			return "", os.ErrNotExist
		}
	})

	withDetectionLogger(func(buf *bytes.Buffer) {
		withDetectors([]detectorEntry{
			{name: "systemd", detect: func() bool { return false }, create: func() ServiceManager { return &systemdManager{} }},
			{name: "launchd", detect: func() bool { return false }, create: func() ServiceManager { return &launchdManager{} }},
			{name: "openrc", detect: func() bool { return false }, create: func() ServiceManager { return &openrcManager{} }},
			{name: "sysvinit", detect: sysvinitMgr.detect, create: func() ServiceManager { return sysvinitMgr }},
			{name: "runit", detect: runitMgr.detect, create: func() ServiceManager { return runitMgr }},
			{name: "s6", detect: func() bool { return false }, create: func() ServiceManager { return &s6Manager{} }},
			{name: "procd", detect: func() bool { return false }, create: func() ServiceManager { return &procdManager{} }},
			{name: "busybox", detect: func() bool { return false }, create: func() ServiceManager { return &busyboxManager{} }},
			{name: "crontab", detect: func() bool { return true }, create: func() ServiceManager { return &crontabManager{} }},
		}, func() {
			mgr, err := DetectServiceManager()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mgr.Name() != "sysvinit" {
				t.Errorf("expected sysvinit manager, got %q", mgr.Name())
			}

			output := buf.String()

			expectedSequence := []string{
				"init detection: starting service manager detection",
				"init detection: checking systemd",
				"init detection: systemd: detected=false",
				"init detection: checking sysvinit",
				"init detection: sysvinit: found /etc/inittab",
				"init detection: selected manager=sysvinit",
			}
			for _, msg := range expectedSequence {
				if !strings.Contains(output, msg) {
					t.Errorf("expected log to contain %q\ngot:\n%s", msg, output)
				}
			}

			if strings.Contains(output, "init detection: checking crontab") {
				t.Error("crontab should not have been checked - sysvinit should win first")
			}
			if strings.Contains(output, "init detection: checking busybox") {
				t.Error("busybox should not have been checked - sysvinit should win first")
			}
		})
	})
}
