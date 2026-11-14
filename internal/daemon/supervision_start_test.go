package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type pendingSupervisorExecutor struct {
	output          string
	failures, calls int
}

func (e *pendingSupervisorExecutor) Run(_ string, _ ...string) ([]byte, error) {
	e.calls++
	if e.calls <= e.failures {
		return []byte(e.output), errors.New("control failed")
	}
	return nil, nil
}

func TestSupervisionStartRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, command, output string
		timeout               time.Duration
		wantCalls             int
		wantErr               bool
	}{
		{"runit scanner delay", "sv", "warning: svc: unable to open supervise/ok: file does not exist", time.Second, 3, false},
		{"s6 scanner delay", "s6-svc", "s6-svc: fatal: unable to control /var/service/svc: supervisor not listening", time.Second, 3, false},
		{"runit permission", "sv", "unable to open supervise/ok: permission denied", time.Second, 1, true},
		{"s6 permission", "s6-svc", "unable to control /var/service/svc: permission denied", time.Second, 1, true},
		{"bounded absence", "sv", "unable to open supervise/ok: file does not exist", 0, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &pendingSupervisorExecutor{output: tc.output, failures: 2}
			err := startSupervisedService(e, tc.timeout, 0, tc.command, "up", "svc")
			if (err != nil) != tc.wantErr || e.calls != tc.wantCalls {
				t.Fatalf("calls=%d error=%v", e.calls, err)
			}
			if err != nil && !strings.Contains(err.Error(), tc.output) {
				t.Fatalf("lost control output: %v", err)
			}
		})
	}
}
