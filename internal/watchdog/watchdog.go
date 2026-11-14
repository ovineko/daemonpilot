// Package watchdog combines process supervision with daemonpilot health and update policy.
package watchdog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ovineko/daemonpilot/internal/ipc"
	"github.com/ovineko/daemonpilot/internal/updater"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
	"github.com/ovineko/procguard"
)

// SpecFunc describes a fresh worker launch.
type SpecFunc func(ctx context.Context) (procguard.Spec, error)

// UpdateCheckFunc performs one self-update check cycle.
type UpdateCheckFunc func(ctx context.Context) error

// Watchdog supervises one worker process for the lifetime of a Run call.
type Watchdog struct {
	config           *daemonpilot.Config
	logger           daemonpilot.Logger
	backoff          *procguard.Backoff
	shuttingDown     atomic.Bool
	needsExecRestart atomic.Bool
	inProxyMode      atomic.Bool
	workerReady      atomic.Bool
	workerEverReady  atomic.Bool
	onProbation      atomic.Bool
	workerMu         sync.Mutex
	runCancel        context.CancelFunc
	workerDone       chan struct{}
	childMu          sync.Mutex
	child            *procguard.Child
	supervisor       *procguard.Supervisor
	spawnMu          sync.Mutex
	specFunc         SpecFunc
	updateCheckFn    UpdateCheckFunc
	applyUpdateFn    func(context.Context, *updater.Updater, *daemonpilot.UpdateInfo) error
	updateRetry      updateRetryRecord
	// updateBackoff schedules retry delays for the most recent failing
	// offered version. It shares the arithmetic with worker backoff but keeps
	// its own counter, budget and reset rules.
	updateBackoff    *procguard.Backoff
	rejectedVersions map[string]struct{}
	startTime        time.Time
	ipcServer        *ipc.Server
	healthFailures   atomic.Int32
	logRing          *logRing
}

type lifecycleLogger struct {
	logger daemonpilot.Logger
	attrs  []any
}

func (l lifecycleLogger) Debug(msg string, args ...any) { l.logger.Debug(msg, l.args(args)...) }
func (l lifecycleLogger) Info(msg string, args ...any)  { l.logger.Info(msg, l.args(args)...) }
func (l lifecycleLogger) Warn(msg string, args ...any)  { l.logger.Warn(msg, l.args(args)...) }
func (l lifecycleLogger) Error(msg string, args ...any) { l.logger.Error(msg, l.args(args)...) }

func (l lifecycleLogger) args(args []any) []any {
	combined := make([]any, 0, len(l.attrs)+len(args))
	combined = append(combined, l.attrs...)
	return append(combined, args...)
}

// New returns a watchdog for config, defaulting to silent operation when the
// config carries no logger.
func New(config *daemonpilot.Config) *Watchdog {
	logger := config.Logger
	if logger == nil {
		logger = daemonpilot.NoopLogger()
	}
	var ring *logRing
	if config.CollectWatchdogLogs {
		ring = newLogRing(config.WatchdogLogBufferBytes)
		logger = captureLogger{logger: logger, ring: ring}
	}
	logger = lifecycleLogger{logger: logger, attrs: []any{
		"phase", "run", "service_name", config.Name, "install_dir", config.InstallDir,
	}}
	w := &Watchdog{
		config:  config,
		logger:  logger,
		backoff: procguard.NewBackoff(config.MaxRestartDelay, config.StableThreshold),
		logRing: ring,
	}
	if config.Update != nil {
		w.updateBackoff = procguard.NewBackoffWithOptions(procguard.BackoffOptions{
			Base:    config.Update.RetryBaseBackoff,
			Maximum: config.Update.RetryMaxBackoff,
		})
	}
	w.specFunc = w.defaultSpec
	w.supervisor, _ = procguard.NewSupervisor(procguard.Options{
		Spec:    w.workerSpec,
		Restart: procguard.RestartPolicy{Mode: procguard.RestartAlways, PreventExitCodes: []int{daemonpilot.ExitPermanent, daemonpilot.ExitRespawnWatchdog}},
		Backoff: w.backoff,
	})
	return w
}

// SetSpecFunc replaces the worker launch factory.
func (w *Watchdog) SetSpecFunc(fn SpecFunc) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.specFunc = fn
}

// IsShuttingDown reports whether a shutdown has been requested. Restart logic
// checks this so a worker exit during shutdown is not treated as a crash.
func (w *Watchdog) IsShuttingDown() bool {
	return w.shuttingDown.Load()
}

// Shutdown marks the watchdog as stopping and cancels the current worker. It
// is safe to call before a worker has been spawned: the flag is set first, so a
// spawn racing this call still observes the shutdown.
func (w *Watchdog) Shutdown() {
	w.shuttingDown.Store(true)
	w.supervisor.Stop()
}

// WorkerState returns an authoritative snapshot of worker supervision.
func (w *Watchdog) WorkerState() procguard.State { return w.supervisor.State() }

// InProxyMode reports whether this watchdog handed supervision to an
// exec-through child and now only forwards signals to it.
func (w *Watchdog) InProxyMode() bool {
	return w.inProxyMode.Load()
}

// ChildPID returns the exec-through child's PID, or zero when absent.
func (w *Watchdog) ChildPID() int {
	w.childMu.Lock()
	defer w.childMu.Unlock()
	if w.child == nil {
		return 0
	}
	return w.child.PID()
}

// Run supervises the worker until ctx is cancelled or a fatal error occurs,
// concurrently driving update checks, health checks, and staged-update
// probation. It returns daemonpilot.ErrExecRestart when the caller must re-exec
// the replaced binary.
func (w *Watchdog) Run(ctx context.Context) error {
	w.startTime = time.Now()
	w.logger.Info("watchdog starting",
		"step", "supervise", "outcome", "started",
		"version", w.config.Version,
		"watchdog_version", w.config.WatchdogVersion,
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w.runCancel = cancel

	go func() {
		<-ctx.Done()
		w.Shutdown()
	}()

	var wg sync.WaitGroup

	// Staged-update probation: if this watchdog started with a .rollback file
	// present and the app opted into StagedRollbackOnUnstable, resolve the
	// provisional staged update by worker stability rather than at startup.
	// Arm onProbation BEFORE starting the update checker so the goroutine-start
	// happens-before edge orders the Store ahead of the checker's initial
	// performUpdateCheck guard (which skips new updates while on probation) —
	// otherwise the checker could apply an update that overwrites the .rollback
	// net this probation depends on.
	probationPath, onProbation := w.stagedProbationPath()
	if onProbation {
		w.onProbation.Store(true)
	}

	w.workerDone = make(chan struct{})
	wg.Go(func() {
		defer close(w.workerDone)
		w.manageWorker(ctx)
	})

	if w.config.Update != nil {
		wg.Go(func() {
			w.checkUpdates(ctx)
		})
	}

	if w.config.HealthFunc != nil {
		wg.Go(func() {
			w.runHealthChecks(ctx)
		})
	}

	if onProbation {
		wg.Go(func() {
			w.runProbation(ctx, probationPath)
		})
	}

	wg.Wait()

	if w.ipcServer != nil {
		w.ipcServer.Close()
	}

	w.logger.Info("watchdog stopped", "step", "supervise", "outcome", "succeeded")

	if w.needsExecRestart.Load() {
		return daemonpilot.ErrExecRestart
	}

	return nil
}

// RunExecThroughChildExported exposes the exec-through child loop to tests in
// other packages.
func (w *Watchdog) RunExecThroughChildExported(ctx context.Context) {
	w.runExecThroughChild(ctx)
}

// HealthFailures returns the number of consecutive failed health checks.
func (w *Watchdog) HealthFailures() int {
	return int(w.healthFailures.Load())
}

func childSpec(path string, args ...string) procguard.Spec {
	return procguard.Spec{Path: path, Args: args, Env: filterEnv(os.Environ(), "DAEMONPILOT_"), Stdout: os.Stdout, Stderr: os.Stderr}
}

func filterEnv(env []string, prefix string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func (w *Watchdog) defaultSpec(ctx context.Context) (procguard.Spec, error) {
	spec := childSpec(os.Args[0], daemonpilot.DaemonpilotArgs(w.config.CommandPrefix, "worker")...)
	spec.GracePeriod = w.config.GracefulTimeout
	if w.config.WorkerCgroupMemoryMax != 0 || w.config.WorkerCgroupParent != "" {
		// Explicit, fail-closed request: procguard refuses the launch when
		// the host cannot provide the delegated cgroup or the limit.
		spec.Cgroup = &procguard.CgroupConfig{
			Parent:    w.config.WorkerCgroupParent,
			MemoryMax: w.config.WorkerCgroupMemoryMax,
		}
	}
	// The worker runs without IPC where the pipe ends cannot be inherited. That
	// path already exists for a failed NewServer below, and the design anticipates
	// it: a staged update with no readiness signal commits on sustained uptime
	// instead (see the probation loop). What is lost is the worker collecting the
	// watchdog's logs and the explicit readiness signal — said out loud, because a
	// platform quietly running a different supervision contract is worse than a
	// slower one.
	var (
		server    *ipc.Server
		pipeFiles *ipc.PipeFiles
		err       error
	)
	if inheritedFDIPCSupported {
		server, pipeFiles, err = ipc.NewServer(w.ipcDataProvider)
		if err != nil {
			w.logger.Warn("failed to create IPC server, worker will run without IPC",
				"error", err.Error())
		}
	} else {
		w.logger.Info("watchdog-worker IPC is unavailable on this platform; the worker will not collect watchdog logs and readiness is inferred from uptime",
			"reason", "inherited file descriptors are not supported")
	}
	if server != nil {
		if w.logRing != nil {
			server.SetLogProvider(w.logRing.collect)
		}
		server.SetReadyHandler(func() {
			w.logger.Info("worker signaled readiness")
			w.workerReady.Store(true)
			w.workerEverReady.Store(true)
			w.supervisor.MarkStable()
		})
	}

	w.workerMu.Lock()
	currentVersion := w.config.Version
	w.workerMu.Unlock()

	spec.Env = append(spec.Env,
		fmt.Sprintf("%s=%s", daemonpilot.EnvMode, daemonpilot.ModeWorker),
		fmt.Sprintf("%s=%s", daemonpilot.EnvVersion, currentVersion),
		fmt.Sprintf("%s=%s", daemonpilot.EnvWatchdogVersion, w.config.WatchdogVersion),
		fmt.Sprintf("%s=%s", daemonpilot.EnvBinaryPath, os.Args[0]),
		fmt.Sprintf("%s=%s", daemonpilot.EnvInitSystem, w.config.InitSystemName),
		fmt.Sprintf("%s=%v", daemonpilot.EnvCanAutoRestart, w.config.CanInitRestartWatchdog),
		fmt.Sprintf("%s=%v", daemonpilot.EnvIsExecThrough, w.config.ExecThrough),
		fmt.Sprintf("%s=%s", daemonpilot.EnvLockStatus, w.config.LockStatus),
		fmt.Sprintf("%s=%d", daemonpilot.EnvParentPID, os.Getpid()),
	)

	if v := os.Getenv(daemonpilot.EnvDisableParentMonitor); v != "" {
		spec.Env = append(spec.Env,
			fmt.Sprintf("%s=%s", daemonpilot.EnvDisableParentMonitor, v))
	}

	if w.config.MaxMemoryBytes > 0 {
		spec.Env = append(spec.Env,
			fmt.Sprintf("%s=%d", daemonpilot.EnvMaxMemoryBytes, w.config.MaxMemoryBytes))
	}

	if w.config.OOMScoreAdj != nil {
		spec.Env = append(spec.Env,
			fmt.Sprintf("%s=%d", daemonpilot.EnvOOMScoreAdj, *w.config.OOMScoreAdj))
	}

	if pipeFiles != nil {
		spec.ExtraFiles = append(spec.ExtraFiles, pipeFiles.RequestW, pipeFiles.ResponseR)
		baseExtraFD := 3 + len(spec.ExtraFiles) - 2
		spec.Env = append(spec.Env,
			fmt.Sprintf("%s=%d,%d", ipc.EnvIPCFD, baseExtraFD, baseExtraFD+1))
	}

	spec.AfterStart = func(_ int, startErr error) {
		if pipeFiles != nil {
			_ = pipeFiles.RequestW.Close()
			_ = pipeFiles.ResponseR.Close()
		}
		if startErr != nil {
			if server != nil {
				server.Close()
			}
			return
		}
		if w.ipcServer != nil {
			w.ipcServer.Close()
		}
		w.ipcServer = server
		if server != nil {
			go server.Serve(ctx)
		}
	}
	return spec, nil
}

func (w *Watchdog) ipcDataProvider() ipc.DynamicInfo {
	state := w.supervisor.State()
	return ipc.DynamicInfo{
		WatchdogPID: os.Getpid(), WorkerPID: state.PID,
		WatchdogStartTime: w.startTime, WorkerStartTime: state.StartedAt,
		RestartCount: state.Restarts, CurrentBackoff: w.backoff.Current(),
		HealthFailures: int(w.healthFailures.Load()),
	}
}

// stopRun stops the whole watchdog (worker manager, update checker, health
// checker) by cancelling the Run-level context. Used when the worker's exit
// code demands the watchdog itself stop rather than restart the worker.
func (w *Watchdog) stopRun() {
	w.shuttingDown.Store(true)
	if w.runCancel != nil {
		w.runCancel()
	}
}

func (w *Watchdog) workerSpec(ctx context.Context) (procguard.Spec, error) {
	w.logger.Info("spawning worker", "step", "spawn_worker", "outcome", "started")
	w.workerReady.Store(false)
	w.spawnMu.Lock()
	fn := w.specFunc
	w.spawnMu.Unlock()
	spec, err := fn(ctx)
	if err != nil {
		return spec, fmt.Errorf("failed to spawn worker: %w", err)
	}
	return spec, nil
}

// handleLegacyExitCode honors the stable worker exit-code contract
// (ExitPermanent, ExitRespawnWatchdog) and reports whether the run should stop.
// It is kept permanently as the rollback safety net: a worker built against the
// legacy root API — or rolled back to such a build — must never end up in a
// restart loop under this watchdog.
func (w *Watchdog) handleLegacyExitCode(exitCode int) bool {
	switch exitCode {
	case daemonpilot.ExitPermanent:
		w.logger.Info("worker requested permanent stop (legacy exit code)", "step", "restart_decision", "outcome", "stopped",
			"exit_code", exitCode)
		w.stopRun()
		return true
	case daemonpilot.ExitRespawnWatchdog:
		w.logger.Info("worker requested watchdog respawn from on-disk binary (legacy exit code)", "step", "restart_decision", "outcome", "respawn_requested",
			"exit_code", exitCode)
		w.needsExecRestart.Store(true)
		w.stopRun()
		return true
	}
	return false
}

func (w *Watchdog) manageWorker(ctx context.Context) {
	if w.IsShuttingDown() {
		w.supervisor.Stop()
	}
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		w.logWorkerEvents()
	}()
	outcome := w.supervisor.Run(ctx)
	<-logged
	if outcome.Reason == procguard.Completed && outcome.Last != nil {
		w.handleLegacyExitCode(outcome.Last.Code)
	}
}

func (w *Watchdog) logWorkerEvents() {
	startFailed := false
	for event := range w.supervisor.Events() {
		switch event.Kind {
		case procguard.EventStarted:
			startFailed = false
			w.logger.Info("worker spawned", "pid", event.PID)
		case procguard.EventStartFailed:
			startFailed = true
			w.logger.Error("failed to spawn worker", "error", event.Err.Error())
		case procguard.EventExited:
			exit := event.Exit
			w.logger.Info("worker exited", "step", "supervise_worker", "outcome", "exited", "exit_code", exit.Code, "uptime", exit.Runtime())
			if err := exitError(*exit); err != nil && exit.Code != 0 {
				w.logger.Warn("worker exited with error", "error", err.Error())
			}
			if w.workerReady.Load() {
				w.logger.Info("worker had signaled readiness, resetting backoff")
			}
			if w.backoff.Stable(exit.Runtime()) {
				w.logger.Info("worker was stable, resetting backoff", "uptime", exit.Runtime(), "threshold", w.config.StableThreshold)
			}
		case procguard.EventRestartScheduled:
			if startFailed {
				w.logger.Info("retrying worker spawn after backoff", "delay", event.Delay, "failures", w.backoff.Failures())
			} else if event.Delay > 0 {
				w.logger.Info("restarting worker after backoff", "step", "restart_decision", "outcome", "restart_scheduled", "delay", event.Delay, "failures", w.backoff.Failures())
			}
		case procguard.EventStopped:
		}
	}
}

func exitError(exit procguard.Exit) error {
	if exit.Err != nil {
		return exit.Err
	}
	if exit.Signal != nil {
		return fmt.Errorf("signal: %s", exit.Signal)
	}
	if exit.Code != 0 {
		return fmt.Errorf("exit status %d", exit.Code)
	}
	return nil
}

func (w *Watchdog) checkUpdates(ctx context.Context) {
	w.logger.Info("update checker started",
		"interval", w.config.Update.CheckInterval,
	)

	checkFn := w.updateCheckFn
	if checkFn == nil {
		u := updater.New(w.config.Update, w.logger, w.config.InstallDir, w.config.Name)
		checkFn = func(ctx context.Context) error {
			return w.performUpdateCheck(ctx, u)
		}
	}

	ticker := time.NewTicker(w.config.Update.CheckInterval)
	defer ticker.Stop()

	initialCheck := make(chan struct{}, 1)
	initialCheck <- struct{}{}

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("update checker stopped")
			return
		case <-initialCheck:
		case <-ticker.C:
		}
		if err := checkFn(ctx); err != nil {
			if errors.Is(err, updater.ErrWatchdogRestart) {
				// stopRun, NOT Shutdown — this cost a node in the field.
				//
				// Shutdown cancels only the WORKER's context. Run then blocks in
				// wg.Wait() on every other goroutine it started, and
				// runHealthChecks returns only on the RUN context. So on any
				// application that supplies a HealthFunc, this hung forever: the
				// worker was stopped and reaped ("worker exited" in the journal),
				// "watchdog stopped" was never reached, and the exec this whole
				// path exists to perform never happened. What is left is a live
				// process holding the instance lock with no worker and no
				// supervised children — the data plane down, nothing restarting,
				// and no way back except a hand-run restart on the box.
				//
				// The other two callers that set needsExecRestart (the
				// ExitRespawnWatchdog branch and rollbackStaged) already use
				// stopRun. This was the odd one out, and it is the only one a
				// staged update can reach — which is why it survived: the staged
				// path runs only when the embedding application's watchdog
				// version changes, so until one did, nothing ever executed it.
				if w.config.ExecThrough {
					w.logger.Info("exec-through child received staged update, exiting for root to restart")
					w.stopRun()
					return
				}
				if w.config.CanInitRestartWatchdog {
					w.logger.Info("init system can restart watchdog, exiting for restart via exec")
					w.needsExecRestart.Store(true)
					w.stopRun()
					return
				}
				w.logger.Info("init system cannot restart watchdog, using exec-through")
				w.stopWorkerForExecThrough()
				w.runExecThroughChild(ctx)
				return
			}
			w.logger.Error("update check failed", "error", err.Error())
		}
	}
}

func (w *Watchdog) installedBinaryPath() string {
	return installedBinaryPathFor(w.config, runtime.GOOS)
}

func (w *Watchdog) rollbackBinaryPath() string {
	return rollbackBinaryPathFor(w.config, runtime.GOOS)
}

func installedBinaryPathFor(config *daemonpilot.Config, goos string) string {
	return filepath.Join(config.InstallDir, daemonpilot.InstalledBinaryNameFor(config.Name, goos))
}

func rollbackBinaryPathFor(config *daemonpilot.Config, goos string) string {
	return installedBinaryPathFor(config, goos) + ".rollback"
}

type updateRetryRecord struct {
	version     string
	attempts    int
	nextAttempt time.Time
}

func (w *Watchdog) performUpdateCheck(ctx context.Context, u *updater.Updater) error {
	// Do not apply a new update while a staged update is still on probation:
	// a second staged update mid-probation would overwrite the .rollback net.
	if w.onProbation.Load() {
		return nil
	}

	info, err := u.CheckForUpdate(ctx, w.config.Version)
	if err != nil {
		return fmt.Errorf("check for update: %w", err)
	}
	if info == nil {
		return nil
	}

	if accept := w.config.Update.AcceptVersion; accept != nil {
		if err := accept(w.config.Version, info.Version); err != nil {
			if _, logged := w.rejectedVersions[info.Version]; !logged {
				if w.rejectedVersions == nil {
					w.rejectedVersions = make(map[string]struct{})
				}
				w.rejectedVersions[info.Version] = struct{}{}
				w.logger.Warn("update version rejected", "phase", "update", "outcome", "rejected", "version", info.Version, "error", err)
			}
			return nil
		}
	}

	if w.config.Update.DisableRetryBudget {
		return w.applyOfferedUpdate(ctx, u, info)
	}
	if info.Version != w.updateRetry.version {
		w.updateRetry = updateRetryRecord{version: info.Version}
		w.updateBackoff.Reset()
	}
	if w.updateRetry.attempts >= w.config.Update.RetryMaxAttempts || time.Now().Before(w.updateRetry.nextAttempt) {
		return nil
	}
	apply := w.applyUpdateFn
	if apply == nil {
		apply = w.applyOfferedUpdate
	}
	err = apply(ctx, u, info)
	if err == nil || errors.Is(err, updater.ErrWatchdogRestart) {
		w.updateRetry = updateRetryRecord{}
		w.updateBackoff.Reset()
	} else if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		w.updateRetry.attempts++
		w.updateRetry.nextAttempt = time.Now().Add(w.updateBackoff.Next())
		if w.updateRetry.attempts == w.config.Update.RetryMaxAttempts {
			w.logger.Warn("update retry budget exhausted", "phase", "update", "outcome", "gave_up", "version", info.Version)
		}
	}
	return err
}

func (w *Watchdog) applyOfferedUpdate(ctx context.Context, u *updater.Updater, info *daemonpilot.UpdateInfo) error {
	newPath, err := u.DownloadAndVerify(ctx, info)
	if err != nil {
		return fmt.Errorf("download and verify: %w", err)
	}

	// Return the transient download/verify buffers (verifying the signature reads
	// the whole staged binary into memory) to the OS BEFORE forking the selftest
	// and version-probe subprocesses, so peak RSS during an update stays close to
	// 1x the binary. On tiny (RISC-V SBC) nodes with little RAM and no swap the
	// old ~2x-buffer-plus-two-forks spike could OOM-kill the agent mid-update.
	debug.FreeOSMemory()

	selftestResult, err := u.RunSelftest(ctx, newPath, w.config.Update.SelftestTimeout)
	if err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("selftest: %w", err)
	}
	if !selftestResult.OK {
		_ = os.Remove(newPath)
		w.logger.Warn("selftest failed, update rejected")
		if !w.config.Update.DisableRetryBudget {
			return errors.New("selftest failed, update rejected")
		}
		return nil
	}

	newWatchdogVersion, err := u.GetBinaryWatchdogVersion(ctx, newPath)
	if err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("get watchdog version: %w", err)
	}

	if err := ctx.Err(); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("pre restart: %w", err)
	}
	if prepare := w.config.Update.PreRestartFunc; prepare != nil {
		err := prepare(ctx, info)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			_ = os.Remove(newPath)
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				w.logger.Warn("update deferred before restart", "phase", "update", "step", "pre_restart", "outcome", "deferred", "version", info.Version, "error", err)
			}
			return fmt.Errorf("pre restart: %w", err)
		}
	}

	if newWatchdogVersion == w.config.WatchdogVersion {
		w.logger.Info("watchdog version unchanged, applying fast-path update")
		if err := u.ApplyFastUpdate(ctx, newPath); err != nil {
			return fmt.Errorf("fast update: %w", err)
		}
		w.workerMu.Lock()
		w.config.Version = info.Version
		w.workerMu.Unlock()
		w.logger.Info("worker restarting after update, skipping backoff")
		w.supervisor.Restart(true)
		return nil
	}

	w.logger.Info("watchdog version changed, applying staged-path update",
		"old", w.config.WatchdogVersion,
		"new", newWatchdogVersion,
	)
	if err := u.ApplyStagedUpdate(ctx, newPath, info, selftestResult); err != nil {
		return fmt.Errorf("applying staged update: %w", err)
	}
	return nil
}

func (w *Watchdog) stopWorkerForExecThrough() {
	w.Shutdown()
	if w.workerDone != nil {
		<-w.workerDone
	}
	w.logger.Info("stopped worker for exec-through update")
}

// The child owns worker probation while it runs. The proxy only restores after
// failed starts or repeated child crashes, and waits for child.Wait before
// touching .rollback. Restore consumes the verified copy; quarantine moves an
// invalid copy away. Neither can race a live child's commit or restore.
func (w *Watchdog) runExecThroughChild(ctx context.Context) {
	w.inProxyMode.Store(true)
	w.logger.Info("entering proxy mode for exec-through child")

	backoff := procguard.NewBackoff(w.config.MaxRestartDelay, w.config.StableThreshold)

	rollbackPath := w.rollbackBinaryPath()
	rolledBack := false

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("proxy mode shutting down")
			return
		default:
		}

		binaryPath := w.installedBinaryPath()
		args := daemonpilot.DaemonpilotArgs(w.config.CommandPrefix, "run", "--exec-through")
		args = append(args, w.config.ServiceArguments...)

		spec := childSpec(binaryPath, args...)
		spec.GracePeriod = w.config.GracefulTimeout
		spec.Env = append(spec.Env, "DAEMONPILOT_EXEC_THROUGH=1")
		if v := os.Getenv(daemonpilot.EnvDisableParentMonitor); v != "" {
			spec.Env = append(spec.Env,
				fmt.Sprintf("%s=%s", daemonpilot.EnvDisableParentMonitor, v))
		}

		child, err := procguard.Start(ctx, spec)
		if err != nil {
			w.logger.Error("failed to start exec-through child",
				"error", err.Error(),
			)

			backoff.Next()
			if w.handleExecThroughMaxBackoff(backoff, rolledBack, rollbackPath) {
				rolledBack = true
			}
			delay := backoff.Current()
			w.logger.Info("retrying exec-through child after backoff",
				"delay", delay,
				"failures", backoff.Failures(),
			)

			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}

		w.childMu.Lock()
		w.child = child
		w.childMu.Unlock()

		w.logger.Info("exec-through child started",
			"child_pid", child.PID(),
		)

		exit := child.Wait()
		waitErr := exitError(exit)
		uptime := exit.Runtime()

		w.childMu.Lock()
		w.child = nil
		w.childMu.Unlock()

		if ctx.Err() != nil {
			w.logger.Info("proxy mode shutting down after signal")
			return
		}

		exitCode := exit.Code

		if exitCode == 0 {
			w.logger.Info("exec-through child exited cleanly (update or rollback), restarting immediately")
			backoff.Reset()
			continue
		}

		w.logger.Warn("exec-through child exited unexpectedly",
			"exit_code", exitCode,
			"uptime", uptime,
		)
		if waitErr != nil {
			w.logger.Warn("exec-through child exit error",
				"error", waitErr.Error(),
			)
		}

		if backoff.Stable(uptime) {
			w.logger.Info("exec-through child was stable, resetting backoff",
				"uptime", uptime,
			)
			backoff.Reset()
			rolledBack = false
		} else {
			backoff.Next()
			if w.handleExecThroughMaxBackoff(backoff, rolledBack, rollbackPath) {
				rolledBack = true
			}
		}

		delay := backoff.Current()
		w.logger.Info("restarting exec-through child after backoff",
			"delay", delay,
			"failures", backoff.Failures(),
		)

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (w *Watchdog) handleExecThroughMaxBackoff(backoff *procguard.Backoff, rolledBack bool, rollbackPath string) bool {
	if !backoff.Saturated() {
		return false
	}

	if rolledBack {
		err := errors.New("exec-through: both new and old binaries failing repeatedly")
		w.logger.Error("critical failure: rollback binary also failing",
			"error", err.Error(),
		)
		if w.config.OnCriticalFailure != nil {
			w.config.OnCriticalFailure(err)
		}
		return false
	}

	binaryPath := w.installedBinaryPath()
	if _, err := os.Lstat(rollbackPath); err != nil {
		w.logger.Warn("no rollback file available for restore", "path", rollbackPath)
		return false
	}
	w.logger.Info("child crashing repeatedly, restoring rollback binary",
		"from", rollbackPath,
		"to", binaryPath,
	)
	restored, err := w.restoreRollbackBinary(rollbackPath, binaryPath)
	if !restored {
		if errors.Is(err, daemonpilot.ErrRollbackUnverified) {
			w.logger.Error("refusing to restore an unverified rollback binary",
				"path", rollbackPath, "error", err.Error())
			// Move it aside as the other two rollback paths do. Left in place it is
			// re-hashed and refused at every max-backoff cycle, forever, over a file
			// that can never be restored.
			w.quarantineRollbackFile(rollbackPath)
			return false
		}
		w.logger.Error("failed to restore rollback binary", "error", err.Error())
		return false
	}
	if err != nil {
		w.logger.Warn("restored the rollback binary but failed to clean up the copy",
			"path", rollbackPath, "error", err.Error())
	}

	backoff.Reset()
	w.logger.Info("rollback binary restored, resetting backoff")
	return true
}

// probationPollInterval is how often runProbation re-checks worker stability.
const probationPollInterval = 2 * time.Second

// probationReadySoak is the minimum continuous worker uptime required to commit
// a staged update even after the worker signalled readiness — so a worker that
// heartbeats once and then crash-loops does not commit and destroy the rollback
// net on the strength of a single data point.
const probationReadySoak = 30 * time.Second

// restoreRollbackBinary matches the rollback copy against the digest the staged
// update recorded when it created it, and installs it at targetPath. The
// install directory is writable by the service's own uid in user scope, so the
// file's presence says nothing about where it came from; only the digest does.
//
// Verification and restore are one step on purpose: the copy is read through a
// single descriptor, so nothing can be substituted at the rollback path between
// the digest check and the bytes that land at targetPath. It reports whether the
// restore happened — true with a non-nil error means the binary is in place and
// only the spent copy or its digest could not be cleaned up.
func (w *Watchdog) restoreRollbackBinary(rollbackPath, targetPath string) (bool, error) {
	//nolint:wrapcheck // the daemonpilot error and its context are the message the caller logs
	return daemonpilot.RestoreRollbackBinary(w.config.InstallDir, w.config.Name, rollbackPath, targetPath)
}

// quarantineRollbackFile moves a rollback copy that failed verification aside,
// so its presence stops re-arming probation at every start. Best effort: a file
// that cannot be moved is still never restored, it only keeps costing a window.
func (w *Watchdog) quarantineRollbackFile(rollbackPath string) {
	quarantinePath, err := daemonpilot.QuarantineRollbackFile(w.config.InstallDir, w.config.Name, rollbackPath)
	switch {
	case quarantinePath == "":
		w.logger.Warn("failed to quarantine the unverifiable rollback binary",
			"path", rollbackPath, "error", err.Error())
	case err != nil:
		// The move is what matters; a digest left behind only ever refuses a file.
		w.logger.Warn("moved the unverifiable rollback binary aside, but could not clear its recorded digest",
			"from", rollbackPath, "to", quarantinePath, "error", err.Error())
	default:
		w.logger.Warn("moved the unverifiable rollback binary aside for inspection",
			"from", rollbackPath, "to", quarantinePath)
	}
}

// clearRollbackDigest drops the recorded digest once the rollback copy it
// describes is gone. Best effort: a stale digest only ever refuses a file.
func (w *Watchdog) clearRollbackDigest() {
	if err := daemonpilot.ClearRollbackDigest(w.config.InstallDir, w.config.Name); err != nil {
		w.logger.Warn("failed to clear the recorded rollback digest", "error", err.Error())
	}
}

// stagedProbationPath reports the .rollback path and true when this watchdog
// should treat its start as a provisional staged update awaiting worker
// stability: the app opted into StagedRollbackOnUnstable and a .rollback file
// left by ApplyStagedUpdate is present on disk.
func (w *Watchdog) stagedProbationPath() (string, bool) {
	if w.config.Update == nil || !w.config.Update.StagedRollbackOnUnstable {
		return "", false
	}
	rollbackPath := w.rollbackBinaryPath()
	// Lstat, like CheckRollbackFile: a planted symlink is armed and then
	// refused by digest verification and quarantined, not silently ignored.
	if _, err := os.Lstat(rollbackPath); err != nil {
		return "", false
	}
	return rollbackPath, true
}

// runProbation resolves a provisional staged update. It commits (deletes the
// .rollback net) once the worker signals readiness and survives probationReadySoak,
// or runs continuously for StableThreshold. Both ordinary and exec-through
// watchdogs use this policy. If RollbackTimeout expires first, restore the
// previous binary and restart it directly or through the waiting proxy.
func (w *Watchdog) runProbation(ctx context.Context, rollbackPath string) {
	window := w.config.Update.RollbackTimeout
	if window <= w.config.StableThreshold {
		// Not fatal: readiness plus the soak can still commit, but without a
		// readiness signal the uptime fallback can never win the race, so
		// every staged update would roll back a healthy binary.
		w.logger.Warn("staged probation window does not exceed the stability threshold; "+
			"without worker readiness the uptime fallback cannot commit and every staged update rolls back",
			"window", window, "stable_threshold", w.config.StableThreshold)
	}
	w.logger.Info("staged update on probation; awaiting worker stability",
		"window", window, "stable_threshold", w.config.StableThreshold)

	deadline := time.NewTimer(window)
	defer deadline.Stop()
	ticker := time.NewTicker(probationPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Shutdown mid-probation (e.g. SIGTERM): leave .rollback in place so
			// the next start re-evaluates the provisional update.
			return
		case <-deadline.C:
			w.logger.Warn("staged update did not stabilize within window; rolling back",
				"window", window)
			w.rollbackStaged(rollbackPath)
			return
		case <-ticker.C:
			// Commit when the worker has proven healthy: it reached readiness
			// AND sustained a short soak, or — with no readiness signal — it ran
			// continuously for the full StableThreshold.
			ready := w.workerEverReady.Load() && w.workerStableFor(probationReadySoak)
			if ready || w.workerStableFor(w.config.StableThreshold) {
				w.logger.Info("staged update stabilized; committing")
				if err := os.Remove(rollbackPath); err != nil && !os.IsNotExist(err) {
					w.logger.Warn("failed to delete rollback file after commit", "error", err.Error())
				} else {
					w.clearRollbackDigest()
				}
				w.onProbation.Store(false)
				return
			}
		}
	}
}

// workerStableFor reports whether the worker is currently running and has been
// up continuously for at least d. A crash-looping worker never satisfies this
// because each launch has its own StartedAt and leader liveness excludes cleanup.
func (w *Watchdog) workerStableFor(d time.Duration) bool {
	state := w.supervisor.State()
	return state.Running && time.Since(state.StartedAt) >= d
}

// rollbackStaged restores the previous binary saved as .rollback and asks the
// caller to restart it (the proxy for an exec-through child). If the copy is
// missing or the restore fails, it stays on the new binary and escalates via
// OnCriticalFailure.
func (w *Watchdog) rollbackStaged(rollbackPath string) {
	// Keep update checks gated until restore and shutdown finish, or a failed
	// restore has left the current binary in charge again.
	defer w.onProbation.Store(false)
	currentPath := w.installedBinaryPath()

	// Lstat, not Stat: a dangling symlink planted at the rollback path must
	// reach VerifyRollbackBinary, which refuses it and quarantines it — a
	// Stat here would follow the link, report the leaf missing, and leave
	// the symlink to re-arm probation at every start.
	if _, err := os.Lstat(rollbackPath); err != nil {
		w.logger.Error("staged rollback: no rollback binary to restore; staying on new binary",
			"error", err.Error())
		if w.config.OnCriticalFailure != nil {
			w.config.OnCriticalFailure(fmt.Errorf("staged rollback: rollback binary missing at %s: %w", rollbackPath, err))
		}
		return
	}

	restored, err := w.restoreRollbackBinary(rollbackPath, currentPath)
	if !restored {
		if errors.Is(err, daemonpilot.ErrRollbackUnverified) {
			w.logger.Error("staged rollback: rollback binary failed verification; staying on new binary",
				"path", rollbackPath, "error", err.Error())
			w.quarantineRollbackFile(rollbackPath)
			if w.config.OnCriticalFailure != nil {
				w.config.OnCriticalFailure(fmt.Errorf("staged rollback: %w", err))
			}
			return
		}
		w.logger.Error("staged rollback: failed to restore previous binary; staying on new binary",
			"error", err.Error())
		if w.config.OnCriticalFailure != nil {
			w.config.OnCriticalFailure(fmt.Errorf("staged rollback restore failed: %w", err))
		}
		return
	}
	if err != nil {
		w.logger.Warn("staged rollback: restored the previous binary but failed to clean up the copy",
			"path", rollbackPath, "error", err.Error())
	}

	if w.config.ExecThrough {
		// Return nil from Run so the child exits 0: the proxy treats this as
		// chain collapse, resets backoff and immediately loads the disk image.
		w.logger.Info("staged rollback: restored previous binary, exiting for proxy restart")
		w.stopRun()
		return
	}

	w.logger.Info("staged rollback: restored previous binary, re-executing")
	w.needsExecRestart.Store(true)
	w.stopRun()
}

func (w *Watchdog) runHealthChecks(ctx context.Context) {
	w.logger.Info("health check goroutine started",
		"interval", w.config.HealthInterval,
		"failure_threshold", w.config.HealthFailures,
	)

	ticker := time.NewTicker(w.config.HealthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("health check goroutine stopped")
			return
		case <-ticker.C:
			if w.inProxyMode.Load() {
				continue
			}

			hasWorker := w.supervisor.State().Running
			if !hasWorker {
				continue
			}

			checkCtx, checkCancel := context.WithTimeout(ctx, w.config.HealthInterval)
			err := w.config.HealthFunc(checkCtx)
			checkCancel()

			if ctx.Err() != nil {
				return
			}

			if err != nil {
				failures := w.healthFailures.Add(1)
				w.logger.Warn("health check failed",
					"error", err.Error(),
					"consecutive_failures", int(failures),
					"threshold", w.config.HealthFailures,
				)

				// Capture the worker's own view BEFORE the restart destroys it. Opt-in,
				// because SIGUSR1 kills a process that doesn't handle it; only on the first
				// failure, so a persistent failure doesn't spam dumps.
				if failures == 1 && w.config.DumpWorkerOnHealthFailure {
					if w.supervisor.State().Running {
						if derr := dumpProcessDiagnostics(w.supervisor); derr != nil {
							w.logger.Warn("failed to signal worker for diagnostics", "error", derr.Error())
						} else {
							w.logger.Info("asked worker to dump diagnostics (SIGUSR1) before any restart")
						}
					}
				}

				if int(failures) >= w.config.HealthFailures {
					w.logger.Error("health check failure threshold reached, restarting worker",
						"failures", int(failures),
					)
					w.logger.Info("worker restarting after health check failure, resetting health counter")
					w.healthFailures.Store(0)
					w.supervisor.Restart(false)
				}
			} else {
				if prev := w.healthFailures.Swap(0); prev > 0 {
					w.logger.Info("health check passed, failure counter reset",
						"previous_failures", int(prev),
					)
				}
			}
		}
	}
}
