package watchdog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/updater"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

type retrySource struct {
	info              *daemonpilot.UpdateInfo
	checkErr          error
	checks, downloads int
}

func (s *retrySource) CheckUpdate(context.Context, string) (*daemonpilot.UpdateInfo, error) {
	s.checks++
	return s.info, s.checkErr
}

func (s *retrySource) DownloadUpdate(context.Context, *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
	s.downloads++
	return nil, nil, errors.New("broken offer")
}

func TestUpdateRetryBudget(t *testing.T) {
	failure := errors.New("apply failure")
	now := time.Now()
	for _, tc := range []struct {
		name                        string
		record                      updateRetryRecord
		version                     string
		applyErr, checkErr          error
		canceled, disabled, noOffer bool
		wantCalls, wantAttempts     int
		wantVersion                 string
	}{
		{name: "first failure", version: "2", applyErr: failure, wantCalls: 1, wantAttempts: 1, wantVersion: "2"},
		{name: "waiting", version: "2", record: updateRetryRecord{"2", 1, now.Add(time.Hour)}, wantAttempts: 1, wantVersion: "2"},
		{name: "exhausted", version: "2", record: updateRetryRecord{"2", 3, now.Add(-time.Hour)}, wantAttempts: 3, wantVersion: "2"},
		{name: "new version", version: "3", record: updateRetryRecord{"2", 3, now.Add(time.Hour)}, applyErr: failure, wantCalls: 1, wantAttempts: 1, wantVersion: "3"},
		{name: "retry due", version: "2", record: updateRetryRecord{"2", 1, now.Add(-time.Hour)}, applyErr: failure, wantCalls: 1, wantAttempts: 2, wantVersion: "2"},
		{name: "success clears", version: "2", record: updateRetryRecord{"2", 1, now.Add(-time.Hour)}, wantCalls: 1},
		{name: "restart clears", version: "2", record: updateRetryRecord{"2", 1, now.Add(-time.Hour)}, applyErr: updater.ErrWatchdogRestart, wantCalls: 1},
		{name: "cancellation", version: "2", record: updateRetryRecord{"2", 1, now.Add(-time.Hour)}, applyErr: context.Canceled, wantCalls: 1, wantAttempts: 1, wantVersion: "2"},
		{name: "canceled context", version: "2", applyErr: failure, canceled: true, wantCalls: 1, wantVersion: "2"},
		{name: "check failure untouched", version: "3", record: updateRetryRecord{"2", 1, now}, checkErr: failure, wantAttempts: 1, wantVersion: "2"},
		{name: "no offer untouched", noOffer: true, record: updateRetryRecord{"2", 1, now}, wantAttempts: 1, wantVersion: "2"},
		{name: "disabled bypasses", version: "2", disabled: true, record: updateRetryRecord{"2", 3, now.Add(time.Hour)}, wantCalls: 1, wantAttempts: 3, wantVersion: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.InstallDir = t.TempDir()
			source := &retrySource{info: &daemonpilot.UpdateInfo{Version: tc.version}, checkErr: tc.checkErr}
			if tc.noOffer {
				source.info = nil
			}
			cfg.Update = &daemonpilot.UpdateConfig{Source: source, RetryMaxAttempts: 3, RetryBaseBackoff: time.Second, RetryMaxBackoff: time.Minute, DisableRetryBudget: tc.disabled}
			w := New(cfg)
			w.updateRetry = tc.record
			calls := 0
			w.applyUpdateFn = func(context.Context, *updater.Updater, *daemonpilot.UpdateInfo) error { calls++; return tc.applyErr }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			before := time.Now()
			err := w.performUpdateCheck(ctx, updater.New(cfg.Update, cfg.Logger, cfg.InstallDir, cfg.Name))
			if tc.checkErr != nil && !errors.Is(err, tc.checkErr) {
				t.Fatalf("check error lost: %v", err)
			}
			if calls > 0 && !errors.Is(err, tc.applyErr) {
				t.Fatalf("apply error lost: %v", err)
			}
			if tc.disabled {
				calls = source.downloads
				if err == nil {
					t.Fatal("disabled failure disappeared")
				}
			}
			if calls != tc.wantCalls || source.checks != 1 || w.updateRetry.attempts != tc.wantAttempts || w.updateRetry.version != tc.wantVersion {
				t.Fatalf("calls=%d checks=%d record=%+v", calls, source.checks, w.updateRetry)
			}
			if tc.wantAttempts == tc.record.attempts && tc.wantVersion == tc.record.version && w.updateRetry != tc.record {
				t.Fatalf("retry record changed without a failure: %+v, want %+v", w.updateRetry, tc.record)
			}
			if tc.wantAttempts > tc.record.attempts && !tc.disabled {
				delay := w.updateBackoff.Current()
				if delay <= 0 {
					t.Fatalf("failed attempt must schedule a delay: %+v", w.updateRetry)
				}
				if w.updateRetry.nextAttempt.Before(before.Add(delay)) || w.updateRetry.nextAttempt.After(time.Now().Add(delay)) {
					t.Fatalf("wrong next attempt: %v", w.updateRetry.nextAttempt)
				}
			}
		})
	}
}

func TestRetryExhaustionWarnsOnceAndStillChecksOffers(t *testing.T) {
	cfg := testConfig()
	cfg.InstallDir = t.TempDir()
	source := &retrySource{info: &daemonpilot.UpdateInfo{Version: "2"}}
	cfg.Update = &daemonpilot.UpdateConfig{Source: source, RetryMaxAttempts: 2, RetryBaseBackoff: time.Second, RetryMaxBackoff: time.Minute}
	var logs bytes.Buffer
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	w := New(cfg)
	u := updater.New(cfg.Update, cfg.Logger, cfg.InstallDir, cfg.Name)
	for range 5 {
		_ = w.performUpdateCheck(context.Background(), u)
		w.updateRetry.nextAttempt = time.Time{}
	}
	if source.checks != 5 || source.downloads != 2 {
		t.Fatalf("checks=%d downloads=%d", source.checks, source.downloads)
	}
	if strings.Count(logs.String(), "outcome=gave_up") != 1 || !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "version=2") {
		t.Fatal(logs.String())
	}
	source.info = &daemonpilot.UpdateInfo{Version: "3"}
	_ = w.performUpdateCheck(context.Background(), u)
	if source.downloads != 3 || w.updateRetry.attempts != 1 {
		t.Fatalf("new offer was delayed: %+v", w.updateRetry)
	}
}

func TestUpdateRetryBackoff(t *testing.T) {
	// The shared procguard arithmetic is tested there; this pins the wiring:
	// the watchdog's retry schedule uses the configured base and cap, with
	// its own counter independent of worker backoff.
	cfg := testConfig()
	cfg.Update = &daemonpilot.UpdateConfig{
		RetryBaseBackoff: 30 * time.Second,
		RetryMaxBackoff:  15 * time.Minute,
	}
	w := New(cfg)
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}
	for i, d := range want {
		if got := w.updateBackoff.Next(); got != d {
			t.Fatalf("attempt %d delay = %v, want %v", i+1, got, d)
		}
	}
	for !w.updateBackoff.Saturated() {
		w.updateBackoff.Next()
	}
	if got := w.updateBackoff.Next(); got != 15*time.Minute {
		t.Fatalf("capped delay = %v, want the maximum", got)
	}
	w.updateBackoff.Reset()
	if got := w.updateBackoff.Next(); got != 30*time.Second {
		t.Fatalf("delay after reset = %v, want the base", got)
	}
}
