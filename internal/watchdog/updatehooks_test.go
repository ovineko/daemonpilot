package watchdog

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/updater"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	"github.com/ovineko/daemonpilot/pkg/release"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

type hookSource struct{ data, signature []byte }

func (s hookSource) CheckUpdate(context.Context, string) (*daemonpilot.UpdateInfo, error) {
	return &daemonpilot.UpdateInfo{Version: "2"}, nil
}

func (s hookSource) DownloadUpdate(context.Context, *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error) {
	return io.NopCloser(bytes.NewReader(s.data)), s.signature, nil
}

func TestPreRestartFunc(t *testing.T) {
	fixture := testsupport.Fixture(t, "update", "./testdata/updatefixture")
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signature, _, _, err := release.SignFile(key, fixture)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("busy")
	for _, tc := range []struct {
		name                                            string
		staged, refuse, selftestFail, cancel, cancelNil bool
	}{
		{name: "fast proceeds"},
		{name: "staged proceeds", staged: true},
		{name: "fast refuses", refuse: true},
		{name: "staged refuses", staged: true, refuse: true},
		{name: "failed selftest", selftestFail: true},
		{name: "cancelled hook", cancel: true},
		{name: "cancelled hook returning nil", cancel: true, cancelNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.InstallDir = t.TempDir()
			cfg.WatchdogVersion = "2"
			if tc.staged {
				cfg.WatchdogVersion = "1"
			}
			current := filepath.Join(cfg.InstallDir, daemonpilot.InstalledBinaryName(cfg.Name))
			if err := os.WriteFile(current, []byte("old binary"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.selftestFail {
				if err := os.WriteFile(filepath.Join(cfg.InstallDir, "fail-selftest"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hookCalls, stagedCalls := 0, 0
			cfg.Update = &daemonpilot.UpdateConfig{
				Source: hookSource{data, signature}, PublicKey: pub, SelftestTimeout: testsupport.Scale(10 * time.Second), RetryMaxAttempts: 6, RetryBaseBackoff: time.Second, RetryMaxBackoff: time.Minute,
				PreRestartFunc: func(ctx context.Context, info *daemonpilot.UpdateInfo) error {
					hookCalls++
					if info.Version != "2" || ctx.Err() != nil {
						t.Error("wrong hook input")
					}

					before, err := os.ReadFile(current)
					if err != nil || string(before) != "old binary" {
						t.Error("swapped before hook")
					}
					if tc.cancel {
						cancel()
						if tc.cancelNil {
							return nil
						}
						return context.Canceled
					}
					if tc.refuse {
						return refusal
					}
					return nil
				},
				StagedUpdateFunc: func(context.Context, *daemonpilot.UpdateInfo, daemonpilot.SelftestResult) (bool, error) {
					stagedCalls++
					if hookCalls != 1 {
						t.Error("staged before pre-restart")
					}
					return true, nil
				},
			}
			w := New(cfg)
			u := updater.New(cfg.Update, cfg.Logger, cfg.InstallDir, cfg.Name)
			err := w.performUpdateCheck(ctx, u)
			wantCalls := 1
			if tc.selftestFail {
				wantCalls = 0
			}
			if hookCalls != wantCalls {
				t.Fatalf("hook calls=%d", hookCalls)
			}
			failed := tc.refuse || tc.selftestFail || tc.cancel
			if tc.staged && !failed {
				if !errors.Is(err, updater.ErrWatchdogRestart) {
					t.Fatal(err)
				}
			} else if (err != nil) != failed {
				t.Fatalf("error=%v", err)
			}
			if tc.refuse && !errors.Is(err, refusal) {
				t.Fatalf("lost refusal: %v", err)
			}
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			wantAttempts := 0
			if tc.refuse || tc.selftestFail {
				wantAttempts = 1
			}
			if w.updateRetry.attempts != wantAttempts {
				t.Fatalf("attempts=%d", w.updateRetry.attempts)
			}
			wantStaged := 0
			if tc.staged && !failed {
				wantStaged = 1
			}
			if stagedCalls != wantStaged {
				t.Fatalf("staged calls=%d", stagedCalls)
			}
			if _, err := os.Stat(daemonpilot.StagedNewPath(current)); !os.IsNotExist(err) {
				t.Fatalf("staging file left: %v", err)
			}

			got, err := os.ReadFile(current)
			if err != nil {
				t.Fatal(err)
			}
			if failed && string(got) != "old binary" {
				t.Fatal("failed attempt replaced current binary")
			}
			if !failed && !bytes.Equal(got, data) {
				t.Fatal("successful attempt did not install binary")
			}
			deferred := strings.Contains(logs.String(), "step=pre_restart outcome=deferred version=2 error=busy")
			if deferred != tc.refuse {
				t.Fatalf("refusal log: %s", logs.String())
			}
			if tc.cancel && strings.Contains(logs.String(), "outcome=deferred") {
				t.Fatal("cancellation logged as refusal")
			}
		})
	}
}

func TestAcceptVersion(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "budgeted", true: "unlimited"}[disabled], func(t *testing.T) {
			cfg := testConfig()
			cfg.InstallDir = t.TempDir()
			source := &retrySource{info: &daemonpilot.UpdateInfo{Version: "old"}}
			var logs bytes.Buffer
			cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			calls := 0
			accept := false
			cfg.Update = &daemonpilot.UpdateConfig{Source: source, DisableRetryBudget: disabled, RetryMaxAttempts: 6, RetryBaseBackoff: time.Second, RetryMaxBackoff: time.Minute, AcceptVersion: func(current, offered string) error {
				calls++
				if current != cfg.Version || offered != source.info.Version {
					t.Error("wrong versions")
				}
				if !accept {
					return errors.New("downgrade")
				}
				return nil
			}}
			w := New(cfg)
			w.updateRetry = updateRetryRecord{version: "failing", attempts: 2, nextAttempt: time.Now().Add(testsupport.Scale(time.Hour))}
			initial := w.updateRetry
			u := updater.New(cfg.Update, cfg.Logger, cfg.InstallDir, cfg.Name)
			for _, version := range []string{"old", "old", "other", "old"} {
				source.info.Version = version
				if err := w.performUpdateCheck(context.Background(), u); err != nil {
					t.Fatal(err)
				}
			}
			if source.downloads != 0 || w.updateRetry != initial || calls != 4 {
				t.Fatalf("downloads=%d calls=%d retry=%+v", source.downloads, calls, w.updateRetry)
			}
			if strings.Count(logs.String(), "outcome=rejected") != 2 || !strings.Contains(logs.String(), "level=WARN") {
				t.Fatal(logs.String())
			}
			accept = true
			if err := w.performUpdateCheck(context.Background(), u); err == nil {
				t.Fatal("accepted offer did not reach failing download")
			}
			if source.downloads != 1 || calls != 5 {
				t.Fatalf("downloads=%d calls=%d", source.downloads, calls)
			}
		})
	}
}
