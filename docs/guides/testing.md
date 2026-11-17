# Testing

## Layers and commands

Unit tests exercise policy and platform adapters. Filesystem Unix datagram
notification tests skip only when the host denies socket creation or reports
`ENOSYS`; the NUL-prefixed abstract-socket test is Linux-only. Other listen errors
remain failures. Integration journeys build fixture binaries in temporary directories
and exercise real processes and files.
The Docker lifecycle layer adds a Linux PID namespace and two real userlands.
The real-init layer adds installation, container-reboot persistence, and
service-manager restart contracts (see the matrix below).

Use the Go version declared in `go.mod`. Dependencies, including procguard, are
downloaded at the versions pinned in `go.mod` and verified against `go.sum`; no
sibling checkout is required. Keep `GOWORK=off` for all commands on this page to
avoid unrelated parent workspaces. Task commands use the repository-managed
toolchain after `pnpm install` and `pnpm dm init`.

```sh
export GOWORK=off
GOWORK=off go test -count=1 ./...
GOWORK=off go test -count=3 ./test/...
GOWORK=off go vet -tags e2e_docker ./...
GOWORK=off go test -tags e2e_docker -count=1 -v -run 'TestExpectationRegistry|TestNative' ./e2e/docker
pnpm dm exec task -- e2e:docker
```

The task sets `DAEMONPILOT_E2E_REQUIRE=1`. The equivalent direct command is:

```sh
GOWORK=off DAEMONPILOT_E2E_REQUIRE=1 go test -tags e2e_docker -count=1 -v -timeout 20m -run 'TestExpectationRegistry|TestProfiles' ./e2e/docker
```

Without `DAEMONPILOT_E2E_REQUIRE=1`, an unavailable Docker daemon produces an explicit
skip. Build failures, image failures, and assertion failures never become skips.
To isolate a contract, use `-run '^TestProfiles$/^debian$/^update-fast$'` or
`-run '^TestNative$/^update-fast$'` in the same command.

`TestNative` is a Linux companion: it launches the same static binaries and uses
the same assertions. Its HTTP client calls the real release handler through an
in-memory transport, permitting signing, streaming-source parsing, selftest,
retry, and process tests in sandboxes that cannot bind sockets. It does not test
DNS, TCP, separate release-server processes, Docker stop, or namespace PID 1.
Its stop scenario sends SIGTERM directly. Docker proves those additional pieces.

## Harness and profiles

The expectation registry in `e2e/docker/scenarios_test.go` is keyed by scenario ID.
`TestExpectationRegistry` verifies that IDs appear here and their contract
headings exist. Each test prints its contract and mutation recipe. Profiles
provide image, flags, optional entrypoint/arguments, a scenario subset, and a
preparation callback with container exec access. Real-init profiles reuse the
builds, network, volume, control-file and cleanup machinery, with an
image-entrypoint option for PID 1 init systems.

The host drives the `docker` CLI. It cross-builds six immutable static variants
and a release server once per suite (`GOOS=linux`, host `GOARCH`, `CGO_ENABLED=0`).
Each scenario gets an internal user-defined network, a release-server container,
and bind volumes created with `t.TempDir`. Builds are mounted read-only.
Containers use the invoking user's numeric UID/GID
so bind-volume evidence and production state files remain readable by the host.
These profiles target a local Docker daemon with ordinary bind-mount ownership;
rootless UID remapping and remote-daemon filesystem mappings are not covered.
Container names derive from the test name and host PID, with a temporary-directory hash to
avoid collisions between repeated runs. Cleanup removes containers before their
network and temporary volumes, including on failure. No ports are published.

| Profile  | Image                  | Proves                                                                   | Does not prove                                                   |
| -------- | ---------------------- | ------------------------------------------------------------------------ | ---------------------------------------------------------------- |
| `debian` | `debian:bookworm-slim` | Static application lifecycle and updates, normally as PID 1 without init | A real service manager, another kernel, or dynamic glibc linking |
| `alpine` | `alpine:3.21`          | Same contracts in musl userland                                          | Dynamic musl linking or a different Linux kernel                 |

The SIGKILL scenario alone uses `--init` and a shell anchor. The anchor keeps the
namespace alive after the watchdog dies, so the test can distinguish kernel
parent-death behavior from namespace teardown. Cooperative parent monitoring is
disabled for this test. The worker must handle kernel SIGTERM and finish; its
unmonitored grandchild is expected to survive, and the harness explicitly kills
it afterwards. This is Linux's documented weaker guarantee, not whole-tree
protection after an arbitrary supervisor death.

The fixture has `HealthFunc` in every scenario so watchdog replacement also
exercises cancellation of the health goroutine. Controls target a worker PID;
readiness receipt is observed before asserting the next restart uses reset
backoff. State files record build version, resident watchdog version, worker PID, and readiness count. JSON records
include process identity and timestamps. Tests use bounded polling of these
observations; logs are supplemental rejection and diagnostic evidence. Health
timeouts respect their context. Grandchildren install a SIGTERM handler before
publishing their start record.

The release server uses `pkg/release.NewHandler`. Each scenario generates an
ed25519 key and an unrelated wrong key; private seeds remain in temporary files.
Offers select immutable artifacts. Counts distinguish manifests from downloads,
so policy rejection and retry exhaustion require continued successful discovery,
not merely a quiet server. Failed tests print lifecycle logs, events, and counts.

## Container behavior

Absence of a running init does not guarantee an empty detected name: the
detectors also inspect installed tools and `/sbin/init`. Alpine can select
BusyBox or crontab even though daemonpilot is PID 1. Both report no automatic
watchdog restart. The fixture enables `VerboseLogging` so embedded-host filtering
does not suppress readiness or retry evidence. The suite asserts the relevant
capability (`CanAutoRestart=false`) and records the detected name.

When init detection cannot select a manager, `CanInitRestartWatchdog` remains
false. A fast update replaces only the worker. A staged update starts a new
`run --exec-through` watchdog; the original PID 1 remains as its proxy and retains
the instance lock. The new watchdog has a different PID and start record.

Exit 101 takes a different route: `handleLegacyExitCode` returns `ErrExecRestart`,
and `handleRun` calls `execSelf`. On Unix this invokes `syscall.Exec` directly,
retaining the PID and loading the current disk image. The new application startup
timestamp changes; the kernel process creation timestamp does not. The test
checks this direct-exec behavior.
Exit 100 and normal SIGTERM stop return successfully from `Run`, so this fixture
exits 0; the worker's reserved code is a request, not the container exit code.

Exec-through children commit or roll back using the same worker stability policy
as the ordinary watchdog; the proxy retains only its repeated-child-crash safety
net. Native proxy tests force this path independently of host init detection.
The 30-second readiness soak is fixed in the library; the Docker scenarios use
the shorter 3-second `StableThreshold` fallback and a 9-second `RollbackTimeout`.

## Scenario contracts

| ID                                   | Expectation                                                                            | Contract                                                     |
| ------------------------------------ | -------------------------------------------------------------------------------------- | ------------------------------------------------------------ |
| `lifecycle-crash-restart`            | exit 3 restarts distinct workers after 1s then 2s backoff                              | [Guide](architecture.md#the-restart-loop)                    |
| `lifecycle-readiness-resets-backoff` | readiness resets an accumulated backoff; next crash waits 1s                           | [Guide](readiness.md#what-it-changes)                        |
| `lifecycle-health-restart`           | three failed checks trigger restart; dump precedes replacement                         | [Guide](health-checks.md#how-it-runs)                        |
| `lifecycle-exit-permanent`           | exit 100 stops watchdog with code 0 and exactly one worker launch                      | [Guide](watchdog-worker-protocol.md#exit-codes-are-requests) |
| `lifecycle-exit-respawn`             | exit 101 loads the disk image, retaining Unix PID but changing watchdog start time     | [Guide](watchdog-worker-protocol.md#exit-codes-are-requests) |
| `lifecycle-docker-stop`              | SIGTERM completes worker and grandchild cleanup inside grace, exit 0                   | [Guide](architecture.md#worker-process-trees)                |
| `lifecycle-watchdog-sigkill`         | kernel parent-death SIGTERM stops the worker; an unmonitored grandchild survives       | [Guide](architecture.md#orphan-prevention)                   |
| `update-fast`                        | v2 worker, same resident watchdog, restart resets accumulated backoff                  | [Guide](self-update.md#fast-path-and-staged-path)            |
| `update-staged`                      | new watchdog runs v2 and commits the verified rollback after stability                 | [Guide](self-update.md#rollback)                             |
| `update-staged-rollback`             | unstable worker fails probation; verified v1 is restored within the rollback window    | [Guide](self-update.md#rollback-integrity)                   |
| `update-bad-signature`               | wrong signing key is rejected, v1 survives, three failures exhaust budget              | [Guide](http-releases.md#signing-and-keys)                   |
| `update-selftest-fails`              | unsuccessful selftest rejects the build and leaves v1 running                          | [Guide](self-update.md#retry-budget)                         |
| `update-retry-budget`                | exactly three failed downloads despite further polls; a newer good offer still applies | [Guide](self-update.md#retry-budget)                         |
| `update-accept-version`              | policy is exercised across repeated offers with zero downloads                         | [Guide](self-update.md#application-update-policy)            |

## Negative controls

Unless stated otherwise, the mutations below target
`internal/watchdog/watchdog.go`. Signature verification is in
`internal/updater/updater.go`; signal cancellation is in
`internal/process/signals.go`. For the ordinary-crash control, change
`RestartAlways` to `RestartNever` in `New`. For the graceful-stop control, remove
`cancel()` from `case <-sigCh:` while leaving signal registration intact. For
watchdog SIGKILL, ignore SIGTERM as specified in the scenario table.

Apply one minimal mutation at a time to non-test code, rebuild by rerunning the
scenario, require an assertion failure, then restore the source byte-for-byte
before the next control. Back up sources outside the checkout and compare the
restored file with the backup. A compile failure, missing Docker, skip, or
pre-existing red baseline does not count as a caught mutation. The suite always
rebuilds its fixture so a stale binary cannot hide a mutation.

| Scenario                             | Minimal non-test mutation                                               | Required failure                            |
| ------------------------------------ | ----------------------------------------------------------------------- | ------------------------------------------- |
| `lifecycle-crash-restart`            | Change RestartAlways to RestartNever in watchdog.New                    | No replacement worker after exit 3          |
| `lifecycle-readiness-resets-backoff` | Remove supervisor.MarkStable in the ready handler                       | Next crash retains accumulated backoff      |
| `lifecycle-health-restart`           | Remove supervisor.Restart(false) in runHealthChecks                     | No replacement after failed health checks   |
| `lifecycle-exit-permanent`           | Remove ExitPermanent from PreventExitCodes                              | Watchdog does not stop after exit 100       |
| `lifecycle-exit-respawn`             | Remove needsExecRestart.Store(true) from the exit-101 branch            | Watchdog does not load the disk image       |
| `lifecycle-docker-stop`              | Remove cancel() in process.SetupSignalHandler's signal case             | Cleanup does not finish inside grace        |
| `lifecycle-watchdog-sigkill`         | Ignore SIGTERM in process.SetupSignalHandler                            | Worker survives watchdog death              |
| `update-fast`                        | Change supervisor.Restart(true) to Restart(false) in applyOfferedUpdate | Accumulated backoff is not reset            |
| `update-staged`                      | Remove probation commit in runProbation                                 | Rollback copy/digest remain after stability |
| `update-staged-rollback`             | Return without restore from rollbackStaged                              | Verified v1 is not restored and run         |
| `update-bad-signature`               | Bypass VerifySignature failure in updater.DownloadAndVerify             | Wrong-key build is not rejected             |
| `update-selftest-fails`              | Bypass the !selftestResult.OK branch in applyOfferedUpdate              | Failed-selftest build replaces v1           |
| `update-retry-budget`                | Remove the attempt/time gate in performUpdateCheck                      | Failed downloads exceed the budget          |
| `update-accept-version`              | Bypass AcceptVersion in performUpdateCheck                              | Rejected offer causes a download            |

Run Docker controls in both profiles. For native controls, use the same ID under
`TestNative`. Host detection may select direct exec, so exec-through controls also
require the dedicated proxy tests:

```sh
GOWORK=off go test -count=1 -v -run '^TestExecThroughProbation$' ./internal/watchdog
```

| Scenarios                                 | Minimal non-test mutation                                     | Required failure                                  |
| ----------------------------------------- | ------------------------------------------------------------- | ------------------------------------------------- |
| `update-staged`, `update-staged-rollback` | Re-add the `ExecThrough` exclusion in `stagedProbationPath`   | No commit or restored worker within the deadline  |
| `update-staged-rollback`                  | Skip `stopRun` in the exec-through branch of `rollbackStaged` | The old binary is restored on disk but never runs |

The native proxy tests use real helper watchdog and worker processes, verify
commit while the child remains alive, require the restored binary to execute,
and check copy consumption and digest cleanup. A readiness signal followed by
worker crashes must still roll back. A separate stable-child-exit test ensures
the proxy preserves unresolved worker probation. Readiness-soak tests cover both
watchdog modes. These bounded probation tests are not a long-running soak test,
and native results do not prove PID namespace behavior.

## Continuous integration

`.github/workflows/e2e.yml` runs the Linux lifecycle Docker profiles and full real-init
matrix on manual dispatch and a weekly schedule. Docker absence, build failures, image
failures, and assertion failures are not skipped.

The regular `.github/workflows/ci.yml` runs build, vet, unit tests, race tests where
supported, and `go test -count=3 ./test/...` on Linux amd64/arm64, macOS 15 Intel/arm64,
and Windows 2025 amd64 plus Windows 11 arm64. Native service E2E runs on macOS and
Windows; Windows runner privileges are required for SCM installation. Race detection
runs on Windows amd64, not Windows arm64.
Windows runs the Windows service-manifest tests and native SCM lifecycle journey. POSIX
init-manager adapter tests and host-detection tests are constrained to non-Windows hosts;
portable parser, scope, and shared API tests still run on Windows.

`.github/workflows/platform-e2e.yml` runs weekly and on manual dispatch.
`.github/workflows/ci.yml` calls it on every pull request and for manual CI dispatch.
It executes all untagged Go test packages inside Linux containers on amd64, arm64, riscv64,
arm, and 386, and in BSD VM guests on
FreeBSD/OpenBSD/NetBSD amd64/arm64 plus DragonFly amd64. BSD bundles are cross-built on
Linux and execute in native BSD guests; this does not provide a BSD init-system provider
or real-init service journey. Cross-compilation alone does not establish runtime behavior.

## Real init matrix

`pnpm dm exec task -- e2e:docker:init` runs `TestInitProfiles`;
`pnpm dm exec task -- e2e:docker` runs only the lifecycle/update profiles.

```sh
export GOWORK=off
DAEMONPILOT_E2E_REQUIRE=1 go test -tags e2e_docker -count=1 -v -timeout 60m -run 'TestInitRegistry|TestInitProfiles' ./e2e/docker
# One profile and one contract:
DAEMONPILOT_E2E_REQUIRE=1 go test -tags e2e_docker -count=1 -v -timeout 15m -run '^TestInitProfiles$/^systemd$/^init-persistence$' ./e2e/docker
# Docker-free checks:
go test -tags e2e_docker -count=1 -v -run 'TestInitRegistry|TestInitNative' ./e2e/docker
```

Each profile has a self-contained Dockerfile under `e2e/docker/images/`. The
runner copies it into a temporary build context and builds once per selected
profile per run, with tag `daemonpilot-e2e/<init>:<sha256-of-Dockerfile>`.
All base images carry explicit tags. Packages resolve from those distributions'
repositories; these are not digest-pinned reproducible package snapshots. The
s6 profile uses distribution packages, with no unverified release tarball.
Images remain in the local Docker cache; containers, networks, build directories,
and volumes are temporary and cleaned up.

The fixture runs `install` and every lifecycle command as container root. Its
config path and environment marker are explicitly written into the real service
manifest. The HTTP release server runs separately as the host UID. Root-owned
production state is inspected inside the container; fixture evidence files are
readable by the host. No root-owned production state is chmodded for the test.

Every journey asserts both the selected backend and PID 1's executable and
arguments. `E/S/T/R` below mean `SupportsEnable`, `SupportsStart`,
`SupportsStatus`, and `CanRestartWatchdog`. All ten Linux backends currently
advertise E/S/T; the runner also has explicit unsupported-verb assertions rather
than silently omitting a verb. OpenRC's R is measured by executable lookup for `supervise-daemon`
in its image and must be true for this profile.

| Profile / pinned base                     | PID 1; expected E/S/T/R                                | What the profile asserts                                                                                   | What it does not prove / gaps                                                                                                                 |
| ----------------------------------------- | ------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `systemd` / `debian:bookworm-slim`        | systemd; Y/Y/Y/Y                                       | All nine contracts, including SIGKILL recovery and rejected unit apply                                     | Host cgroup delegation is required; no host boot or user scope                                                                                |
| `openrc` / `alpine:3.21`                  | BusyBox init invoking OpenRC default runlevel; Y/Y/Y/Y | All nine contracts; OpenRC supervise-daemon owns watchdog recovery                                         | OpenRC versions without supervision; physical network setup                                                                                   |
| `sysvinit` / `debian:bookworm-slim`       | SysV `/sbin/init`; Y/Y/Y/N                             | Lifecycle, reboot persistence, staged exec-through, uninstall, config apply/rollback                       | No init watchdog restart claim; Debian update-rc.d strategy only                                                                              |
| `runit` / `debian:bookworm-slim`          | runit and runsvdir; Y/Y/Y/Y                            | Lifecycle, persistence, SIGKILL, staged update, uninstall, config environment                              | Synchronous failed-apply rollback gap: sv up accepts a request before the run script succeeds                                                 |
| `s6` / `alpine:3.21`                      | s6-svscan; Y/Y/Y/Y                                     | Same as runit, with real s6 supervision                                                                    | Uses the backend's scan-directory integration; no complete s6-rc database or s6-overlay boot contract; asynchronous failed-apply rollback gap |
| `dinit` / `alpine:3.21`                   | dinit in container mode; Y/Y/Y/Y                       | Lifecycle, persistence, SIGKILL, staged update, uninstall, environment-file apply                          | No hardware/system setup; Invalid service-type reload exercises synchronous config rollback                                                   |
| `finit` / `debian:trixie-slim`            | Finit; Y/Y/Y/Y                                         | Lifecycle, persistence, SIGKILL, staged update, uninstall, environment apply                               | Reload acknowledges a rescan, without guaranteed synchronous rejection of a bad service stanza; failed-apply rollback gap                     |
| `procd` / `openwrt/rootfs:x86-64-24.10.8` | procd; Y/Y/Y/Y                                         | Lifecycle, persistence, SIGKILL, staged update, uninstall, config environment                              | x86-64 only; no router hardware, flash, network reconfiguration, or synchronous failed-start rejection                                        |
| `busybox` / `alpine:3.21`                 | BusyBox init with rcS; Y/Y/Y/N                         | Lifecycle, K/S enable persistence, staged exec-through, uninstall, config apply/rollback                   | No init restart claim; the generated shell respawn loop is distinct; flag-file strategy is not covered by this image                          |
| `crontab` / `debian:bookworm-slim`        | foreground cron; Y/Y/Y/N                               | Lifecycle, real cron @reboot across Docker restart, staged exec-through, uninstall, rejected crontab apply | No init supervision; no BusyBox cron special-times claim                                                                                      |

Finit is packaged in [Debian trixie](https://packages.debian.org/trixie/finit).
Dinit is packaged in [Alpine 3.21](https://pkgs.alpinelinux.org/package/v3.21/community/aarch64/dinit).
The OpenWrt project describes its [rootfs images as experimental](https://github.com/openwrt/docker/blob/main/README.md#rootfs-experimental).

No profile uses `--privileged` or adds capabilities. Systemd requests
`--tmpfs /run --tmpfs /run/lock --cgroupns=host`, a read/write bind of
`/sys/fs/cgroup`, and `container=docker`. The systemd profile needs write access to the **host cgroup tree**;
systemd scopes itself under the container's own cgroup. This is broader access
than the other profiles, although it is not `--privileged`. A private cgroup
namespace combined with the host bind mount failed on the operator's cgroup v2
host; host namespace plus this writable mount was verified manually.
The init containers allocate a console TTY, and systemd logs to that console so
early manager failures reach `docker logs`.
A failed boot with an observed mount/cgroup permission or
read-only-filesystem error becomes an explicit facility-gap skip containing the
exact diagnostic. No privileged retry is attempted. Unknown boot failures,
missing packages, build errors, detection mismatches and service assertion
failures remain failures. A procd run on non-amd64 is an explicit architecture
gap. No extra capability or privileged fallback is attempted.

All profiles share the Docker host's kernel. Reboot means `docker restart` of
the same container, retaining its writable layer and bind volumes, with a new
container start timestamp. It does not boot firmware or a new kernel. Rootless
Docker, remote bind mounts, user scopes, SELinux labeling and dynamic linking
are not covered. Disabled/uninstalled absence is checked repeatedly for ten
seconds after boot readiness, with a process scan rather than an old worker
PID that might have been reused.

### Contracts and negative controls

Every step emits JSON to verbose test output and `init-evidence.jsonl` in its
temporary volume: CLI exit/output, expected contract and mutation, detected
capabilities, PID 1, live process IDs, worker snapshots, installed state,
manifest contents, binary digests, artifact presence, and reboot generations.
Failures additionally print the first failing step, last complete command output,
latest worker snapshot, manifest, init-native status, `ps`, service log files,
fixture journal, release counts, container state and timestamped container logs.
Exited containers are inspected and logged immediately, before removal.
Finit readiness requires runlevel 2; merely accepting `initctl runlevel` in
bootstrap runlevel S is insufficient. Save `go test -v` output to retain evidence after
cleanup. Each scenario uses a fresh container and freshly installed v1.

| ID                      | Required evidence                                                                                            | Named production mutation                                                                               | Companion / host requirement                                                         |
| ----------------------- | ------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------ |
| `init-lifecycle`        | Manifest/state, status, distinct worker after restart, no processes after stop                               | `stop-noop`: omit mgr.Stop in handleStop                                                                | TestHandleStop; real verbs require Docker                                            |
| `init-persistence`      | No service after disable/reboot; fresh worker after enable/reboot                                            | `enable-noop`: omit BusyBox rename or sysvinit update-rc.d; `missing-wantedby`: remove systemd WantedBy | Reboot requires Docker                                                               |
| `init-watchdog-sigkill` | Different live watchdog and worker; old worker gone; no CLI start                                            | `restart-policy`: remove systemd Restart=always or OpenRC supervisor=supervise-daemon                   | Docker; skip where R is false                                                        |
| `init-staged-commit`    | Healthy v2, expected supervision identity, copy/digest consumed, v2 bytes retained                           | `no-commit`: omit probation commit                                                                      | TestNative/update-staged; real init requires Docker                                  |
| `init-staged-rollback`  | Unstable worker launched, restored live v1, original digest, copy/digest consumed                            | `no-restore`: omit rollback restore                                                                     | TestNative/update-staged-rollback; real init requires Docker                         |
| `init-uninstall`        | Registration, manifest, processes, state, binary and artifacts absent across reboot; unrelated file retained | `leave-manifest`: omit manifest removal; `leave-artifacts`: omit removeInstallArtifacts                 | TestHandleUninstall; registration/reboot require Docker                              |
| `init-update-config`    | Replacement worker observes changed environment                                                              | `old-config`: write oldContent; `cron-env-cd`: remove cron environment export                           | TestHandleUpdateConfig, TestInitNativeCrontabEnvironment; real apply requires Docker |
| `init-config-rollback`  | Rejected config, original manifest/environment restored, backup removed                                      | `no-config-restore`: omit restore; `keep-config-backup`: omit backup removal                            | TestHandleUpdateConfig; Docker with listed asynchronous gaps                         |
| `init-manifest-values`  | Exact worker environment, watchdog arguments and working directory                                           | Restore literal-path Finit wrapper, unquoted environment values, or repeated procd environment setters  | Shell/property companions; real init requires Docker                                 |

Detection has an additional `wrong-order` control: move crontab before the real
manager in defaultDetectors. Every Docker journey must reject the wrong backend;
TestDefaultDetectorsOrder can exercise the ordering contract natively.
`wrong-capability`: return false from systemd SupportsStart; both
TestInitNativeCapabilities and every systemd journey must reject it.
`cron-attached-start`: restore the old crontab Start code that strips the log
redirect and backgrounds the command without detaching descriptors; TestCrontabStartDetachesAndKeepsLog must reject the resulting
inherited-output hang. That test substitutes cron lookup only and uses a real
shell/service process; it does not prove a running cron daemon.

Use the same one-mutation-at-a-time, backup/restore/byte-comparison procedure as
the lifecycle controls. A compile error or skipped baseline is not evidence
that a control works. Native controls test the named behavior, but do not prove
that a real init daemon accepted or rejected a manifest.

### Interpreting init behavior

On restart-capable Unix hosts, the staged path returns `ErrExecRestart` and
`handleRun` calls `syscall.Exec`. It retains the watchdog PID; it is **not an
exit followed by an init restart**. The staged contracts assert that behavior.
The independent SIGKILL contract tests init restart. Backends with R=false use
exec-through, even though BusyBox and sysvinit scripts also contain a shell
respawn loop for abnormal exits.

Config rollback covers synchronous command failures. Accepted asynchronous starts
can fail beyond the CLI rollback boundary; the matrix lists those gaps. Dinit
rejects an invalid service type synchronously. Finit reload acknowledges a rescan
and does not guarantee synchronous rejection of an invalid stanza. Systemd's
rollback injection adds a second `ExecStart` to a `Type=simple` unit, which is
invalid under its [ExecStart contract](https://www.freedesktop.org/software/systemd/man/systemd.service.html#ExecStart=).
A nonexistent executable alone is not a reliable synchronous rejection.

Finit readiness requires runlevel 2 rather than bootstrap runlevel S. Dinit and
Finit reload changed definitions and environment files before starting updated
services. OpenRC restart capability depends on finding `supervise-daemon` through
`PATH`; its help command requires service context and is not a capability probe.
Runit uses waiting `sv -w 30 stop`, not asynchronous `sv down`, so uninstall can
remove lock artifacts only after the watchdog exits.

Generated-shell regressions execute real shells to catch inherited-output hangs,
lost environment exports, and descriptor detachment errors. Scanner-registration
tests check bounded missing-supervisor retries. These local checks complement,
rather than replace, real cron, runit, s6 and init-daemon journeys.

## Portable binaries and cross-architecture execution

```sh
sh scripts/crossarch.sh docker
# Equivalent:
pnpm dm exec task -- test:crossarch
```

The runner compiles every untagged package with tests using `go test -c`, and
bundles the lifecycle, process, update, probe, exec-through v2/v3, watchdog
worker-mode, and crash fixtures with `CGO_ENABLED=0`. It runs linux/amd64, arm64,
riscv64, arm/v7, and 386 in architecture-specific Debian image digests pinned in
the script, with Docker `--init` and an explicit `--platform`. Guests need neither
Go nor a checkout. Each package runs
from its own bundled directory with its fuzz corpus. The script reports each
architecture and continues after a test failure. It does not install binfmt;
foreign architectures require interpreters already registered by the operator.
Docker bind mounts require a local daemon.

Build a Linux bundle on a machine with Go, then transfer it to a Linux target
without Go or Docker:

```sh
sh scripts/crossarch.sh pack daemonpilot-tests.tar.gz
# Transfer the archive, then on the target:
mkdir daemonpilot-tests
tar -xzf daemonpilot-tests.tar.gz -C daemonpilot-tests
DAEMONPILOT_TEST_TIME_SCALE=8 sh daemonpilot-tests/run.sh riscv64
```

Omit the architecture argument to detect the native architecture. Use `arm` for
the arm/v7 bundle.
`DAEMONPILOT_TEST_FIXTURES` supplies the required fixtures, including `lifecycle`,
`process`, `update`, `probe`, `execfixture-v2`, and `execfixture-v3` (`.exe` on
Windows) when running individual test binaries. Each test copies its fixture into
its own temporary directory. Ordinary `go test` builds fixtures there when no
directory is supplied.
The watchdog argument probe and process-group probe re-execute the test binary.
No `ps` is required. The packed runner is Linux-only, not a BSD or Windows bundle.
The scripts respect the caller's normal Go cache configuration.

`internal/testsupport.Scale` reads `DAEMONPILOT_TEST_TIME_SCALE`, a positive integer
that defaults to 1. Invalid values and duration overflow fail explicitly. The
Docker runner sets 8 on emulated architectures and 1 for native execution,
including 386 on amd64. Wait deadlines and observation windows scale; polling
frequencies and arithmetic/validation test inputs retain their meaning. The
bundle disables the process-wide Go test timeout; each operation remains bounded.
A caller can add an outer timeout.

The runner declares `DAEMONPILOT_TEST_EMULATED=1` for foreign architectures. Only
that explicit declaration skips the two address-space-limit tests: qemu-user's
own mappings make `RLIMIT_AS` unsuitable for measuring guest memory limits.
Existing host-facility and race-detector exclusions still apply. Emulation does
not certify a different kernel or replace the real-init Docker matrix.

## Fuzzing manifests and decoders

```sh
pnpm dm exec task -- fuzz
sh scripts/fuzz.sh 60s
# Shorter local campaign:
pnpm dm exec task -- fuzz FUZZTIME=10s
```

The default budget is 60 seconds **per target**, defined in `scripts/fuzz.sh`.
The script discovers targets and runs each independently with two workers unless
`GOMAXPROCS` is supplied. It continues after failures and returns a nonzero status
if any campaign failed. Seeds and minimized failures live in each package's
`testdata/fuzz/` and replay during ordinary `go test`.

`FuzzRenderSystemd`, `FuzzRenderLaunchd`, `FuzzRenderOpenRC`,
`FuzzRenderSysvinit`, `FuzzRenderBusybox`, `FuzzRenderRunit`, `FuzzRenderS6`,
`FuzzRenderDinit`, `FuzzRenderFinit`, `FuzzRenderProcd`, `FuzzRenderCrontab`,
and `FuzzRenderWindows` cover arbitrary arguments, environment values, paths,
and mutations of names, descriptions, identities, log directories and environment
keys. NUL is rejected at rendering boundaries. XML and Windows text reject invalid
UTF-8, XML rejects forbidden characters, Dinit/Finit/Windows reject embedded
line breaks, and cron additionally refuses its newline/percent command hazards.
Systemd settings that cannot survive whitespace trimming or line continuation
are rejected instead of silently changed.

Shell tests run real `/bin/sh -n` and execute the generated text with a re-executed
Go test probe. Its JSON uses byte slices so invalid UTF-8 cannot hide byte changes.
Tests compare arguments, two environment variables, and the working directory.
Init-specific entry points are supplied by small shell adapters; sysvinit's `su`
branch and both OpenRC launch modes have explicit regressions. Runtime PID paths
are redirected into the test directory. These adapters test the emitted shell;
the actual init daemons remain covered by the Docker matrix.

Systemd command/environment words pass an independent C-escape and expansion
parser, and accepted fuzz inputs also run through `systemd-analyze verify` when
available, using an existing executable and directory. When it is unavailable,
the parser remains active. Its grammar follows
[systemd.syntax](https://www.freedesktop.org/software/systemd/man/latest/systemd.syntax.html)
and [systemd.service](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html).
The plist is decoded with `encoding/xml`. Windows manifests are decoded and their
command lines pass an independent Windows argv parser. Dinit uses a reference
parser for its [documented quoting and substitution rules](https://davmac.org/projects/dinit/man-pages-html/dinit-service.5.html).
Finit checks its [service syntax](https://finit-project.github.io/config/services/)
and the quoting subset consumed by
[its wordexp-based execution and environment loader](https://github.com/finit-project/finit/blob/master/src/service.c).
Working-directory wrappers are executed too. These checks do not substitute for
running the target init version.

Other targets exercise `LoadState`, `Config.Validate`, `DaemonpilotArgs`, both IPC
response decoders and request framing, HTTP manifest decoding, release routing,
crontab parsing, and watchdog log encoding/cursors/capacity. Unknown JSON fields
remain compatible; malformed JSON, null state/IPC objects, trailing manifest
content, malformed IPC frames, and invalid crontab schedules are rejected.
Versions are opaque strings: argument construction preserves them without parsing
or normalizing their contents. Log capture normalizes invalid UTF-8 before the
ring's JSON transport so retained and transmitted text agree.

The `init-manifest-values` scenario installs environment values containing
quotes, dollar signs, backticks, backslashes, whitespace and Unicode, then requires
an exact observation from a replacement worker. It also records the replacement
watchdog's actual arguments and working directory. Non-cron profiles carry a
literal systemd percent specifier. Its negative controls restore the old Finit
working-directory wrapper, unquoted Finit environment values, or split procd's
environment table across repeated `set_param` calls. Run this scenario through
`pnpm dm exec task -- e2e:docker:init`; local shell/property tests
do not establish a Docker negative-control result.

### Finit command grammar

Finit's supported line format has no working-directory modifier. Its
[service parser](https://raw.githubusercontent.com/finit-project/finit/master/src/service.c)
first splits the description at `--` followed by a space, splits arguments on spaces, reconstructs
quoted tokens, then expands each reconstructed argument separately. The wrapper
uses `cd "$1"&&shift&&exec "$@"`, with directory and executable passed as separate
arguments, preserving the supervised PID. The reference grammar models those
stages; local shell tests check the resulting arguments and working directory.
The [packaged manual](https://manpages.debian.org/trixie/finit/finit.conf.5.en.html)
describes the line format and wrapper approach.

Finit executable paths must be absolute unquoted shell-safe tokens, and working
directories must be absolute. Command values reject
line breaks, tabs, vertical whitespace, `#`, embedded `--` followed by a space, and spaces combined
with quotes, backslashes, repeated spaces, or leading/trailing spaces. Single
spaces in otherwise plain arguments and directory names are supported, as are
empty arguments and literal `--`. Environment-file values retain separate quoted
word-expansion handling.

`init-manifest-values` updates a running installation with empty, spaced, quoted
and literal shell-marker arguments. It checks the replacement watchdog's actual
argv and working directory and the replacement worker's environment after
`update-config`. Inputs remain subject to each backend's documented grammar;
passing a selected corpus does not establish arbitrary shell quoting.

For a production negative control, replace Finit's parameterized working-directory
wrapper with one that embeds a quoted literal directory in the shell script.
Both `TestFinitWorkingDirectory` and the real Finit `init-manifest-values` scenario
must reject the resulting argument or directory mismatch. Changing a test's
reference parser is not a production mutation and does not establish production
fault detection.

## Validation scope

Run the relevant matrix for the revision being reviewed. Retain command output,
revision, host/kernel, tool versions, skips and facility diagnostics. Passing
results on one host or an earlier revision do not establish the current result
or another platform's guarantees. Finit is part of the real-init matrix and uses
the same required assertions as other restart-capable profiles, subject to its
explicit asynchronous config-rejection gap.

Validate long-running soak, systematic mutation/coverage, Windows Job Object behavior,
and update fault injection beyond the named journeys separately. Native Windows SCM
and macOS launchd service journeys run in the regular CI matrix; native BSD unit and
integration binaries run in guest VMs. Cross-compilation and Linux emulation do not
replace those native executions.

| Check                    | Command / procedure                                                                                   | Host requirements and limits                                                           |
| ------------------------ | ----------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| Unit and integration     | `GOWORK=off go test -count=1 ./...` and `GOWORK=off go test -count=3 ./test/...`                      | Go toolchain, downloaded modules, process execution and writable temporary directories |
| Race detection           | `GOWORK=off go test -race ./...`                                                                      | Supported native OS/architecture and C toolchain; Windows arm64 is not race-enabled    |
| Native service E2E       | `DAEMONPILOT_E2E_REQUIRE=1 GOWORK=off go test -tags e2e_native -count=1 -v -timeout 20m ./e2e/native` | Windows SCM requires admin; macOS launchd requires an addressable user domain          |
| Linux lifecycle Docker   | `pnpm dm exec task -- e2e:docker`                                                                     | Local Linux Docker, image access and bind-mount ownership                              |
| Linux real-init Docker   | `pnpm dm exec task -- e2e:docker:init`                                                                | Same, plus writable host cgroup tree for systemd; amd64 for procd                      |
| Five Linux architectures | `GOWORK=off sh scripts/crossarch.sh docker`                                                           | Local Docker and registered binfmt interpreters for foreign architectures              |
| BSD guest runtime suites | Manual/weekly `.github/workflows/platform-e2e.yml` jobs                                               | VM guest runtime; arm64 guests are emulated with scaled wait budgets                   |
| Fuzz campaign            | `GOWORK=off sh scripts/fuzz.sh 60s`                                                                   | Go fuzz-supported host; budget is per target                                           |
| Negative controls        | Passing baseline, one production mutation, assertion failure, byte-identical restoration              | Host needed by selected scenario; compilation errors and skips never count             |

Registry checks keep scenario IDs and guide anchors synchronized; they do not
validate runtime semantics. Prefer observable process, file, argument and
environment assertions over checks that merely search production source text.
