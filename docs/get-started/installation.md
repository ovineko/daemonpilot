# Installation

## Requirements

- Go 1.26.0 or newer
- Linux, macOS, or Windows for native service integration. Windows uses the
  Service Control Manager (SCM) and requires Administrator access for installation.

## Add the Module

Add daemonpilot to your application's module:

```bash
go get github.com/ovineko/daemonpilot/pkg/daemonpilot
```

Go downloads procguard automatically at the version pinned by daemonpilot. No
local procguard checkout, workspace, or `replace` directive is required.

Two imports are needed in a service binary:

```go
import (
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"

	// Registers the watchdog and updater implementations with the public API.
	_ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
)
```

The blank import wires the internal watchdog and updater into `pkg/daemonpilot`.
Without it, `daemonpilot.New` reports that no watchdog factory is configured.

## Version Injection

Two versions travel with the binary, and both are injected at build time:

| Variable          | Meaning                                                                |
| ----------------- | ---------------------------------------------------------------------- |
| `Version`         | Your application version. Changing it alone allows a fast-path update. |
| `WatchdogVersion` | The supervision logic version. Changing it forces a staged update.     |

```bash
go build -ldflags "\
  -X github.com/ovineko/daemonpilot/pkg/daemonpilot.Version=1.5.0 \
  -X github.com/ovineko/daemonpilot/pkg/daemonpilot.WatchdogVersion=2" \
  -o myapp
```

Change `WatchdogVersion` only after every resident watchdog is known to handle
the staged path correctly: that path runs in the old watchdog. To roll out new
watchdog code while keeping the same version, a worker can request a restart
with `daemonpilot.RespawnWatchdog()` after flushing its logs. See
[Self-Update](../guides/self-update.md#rolling-out-watchdog-code).

## Cross-Compilation

The library has no cgo dependencies, so the usual `GOOS`/`GOARCH` matrix works:

```bash
GOOS=linux  GOARCH=amd64          go build -o myapp-linux-amd64
GOOS=linux  GOARCH=arm64          go build -o myapp-linux-arm64
GOOS=linux  GOARCH=arm   GOARM=7  go build -o myapp-linux-armv7
GOOS=linux  GOARCH=riscv64        go build -o myapp-linux-riscv64
GOOS=darwin GOARCH=arm64          go build -o myapp-darwin-arm64
GOOS=freebsd GOARCH=amd64         go build -o myapp-freebsd-amd64
GOOS=openbsd GOARCH=amd64         go build -o myapp-openbsd-amd64
GOOS=windows GOARCH=amd64         go build -o myapp-windows-amd64.exe
```

For release builds, add the version-injection flags from the preceding section.

CI cross-compiles linux, darwin, freebsd, openbsd and windows on amd64 and arm64.
Compilation does not establish native runtime support on every target:
freebsd and openbsd have no native init-system provider, so
detection falls through to the crontab provider, which installs an `@reboot`
entry instead of a native service. Windows supports machine-wide SCM services:
`install`, `uninstall`, `start`, `stop`, `restart`, `enable`, `disable`, `status`,
`validate`, and `update-config`. The service runs as LocalSystem, starts
automatically, reports SCM lifecycle status, and has restart recovery configured.
Console `run` and `worker` also work.

Windows user scope, per-service environment settings, and POSIX `ServiceUser` /
`ServiceGroup` identities are rejected. Use arguments or an application config
file for configuration. procguard assigns workers and exec-through children to
kill-on-close Job Objects; watchdog death kills the assigned tree. Assignment
follows process creation, leaving a small initial window without job protection.
Windows has no watchdog-worker IPC: runtime information is static-only,
`NotifyReady` cannot notify the watchdog, and watchdog log collection is unavailable.
Probation therefore relies on sustained worker uptime. SIGUSR1 stack dumps and
`RLIMIT_AS` memory limits are unavailable on Windows. There is no daemonpilot
single-instance lock on Windows; SCM serializes its service, but a manual `run`
can race that service over the same install directory.

## Install Scope

`Config.Scope` decides who owns the installation. The zero value is
`daemonpilot.ScopeSystem`, so existing configurations keep behaving exactly as
before.

| Scope                   | Owner             | Privileges                                       | systemd unit                            | launchd job                               |
| ----------------------- | ----------------- | ------------------------------------------------ | --------------------------------------- | ----------------------------------------- |
| `ScopeSystem` (default) | The machine       | write/control access to system paths and manager | `/etc/systemd/system/<name>.service`    | `/Library/LaunchDaemons/com.<name>.plist` |
| `ScopeUser`             | The invoking user | none beyond the account                          | `~/.config/systemd/user/<name>.service` | `~/Library/LaunchAgents/com.<name>.plist` |

```go
cfg := daemonpilot.Config{
	Name:  "myapp",
	Scope: daemonpilot.ScopeUser,
	// ...
}
```

Scope comes from `Config.Scope` and nothing else. There is no `--user` flag, no
`DAEMONPILOT_SCOPE` environment variable and no detection from the effective uid:
`daemonpilot.New` computes `InstallDir` before any flag parsing happens, and a
scope that changes between `install` and `uninstall` would leave the state
file, the lock and the updater pointing at different directories. An
application that wants a user-facing flag parses it itself and sets
`Config.Scope` before calling `New`.

A given `Config.Name` may exist in **one scope only**. Before writing anything,
`install` stats the other scope's manifest path — `/etc/systemd/system/<name>.service`
against `~/.config/systemd/user/<name>.service`, `/Library/LaunchDaemons/com.<name>.plist`
against `~/Library/LaunchAgents/com.<name>.plist` — and refuses with
`daemonpilot.ErrScopeCollision` rather than leave two supervisors racing over one
binary. The check errs towards missing a collision rather than inventing one: it
resolves the user-scope path from the invoking process's `$HOME`, which is
root's under `sudo`, so a system-scope install run that way will not see a
user-scope installation belonging to another account. The crontab manager is
exempt, since both of its scopes edit the same crontab.

### Paths

User-scope defaults are derived from `$HOME` and only from `$HOME`:

| Platform | Default `InstallDir` for `ScopeUser`   |
| -------- | -------------------------------------- |
| Linux    | `~/.local/share/<name>`                |
| macOS    | `~/Library/Application Support/<name>` |

Init systems that log to files write under `Config.LogDir`, which follows the
same rule:

| Platform | Default `LogDir` for `ScopeUser` |
| -------- | -------------------------------- |
| Linux    | `~/.local/state/<name>`          |
| macOS    | `~/Library/Logs/<name>`          |

In system scope `LogDir` stays empty and each init system keeps its own
historical location. Setting `Config.LogDir` explicitly overrides both.

`XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `XDG_STATE_HOME` and friends are
deliberately ignored. The systemd user manager does not inherit the login
shell's environment, so a unit written under an `XDG_CONFIG_HOME` override
would be invisible to `systemctl --user`. If `$HOME` is unset, `SetDefaults`
returns `daemonpilot.ErrNoHomeDir` rather than silently producing a relative path.

`ServiceUser` and `ServiceGroup` are rejected in user scope: they require root,
and a user-scope service already runs as the invoking user.

### Init-System Support

systemd, launchd and crontab support user scope — see
[Init Systems](../guides/init-systems.md#install-scopes). With `ScopeUser`, detection skips managers that cannot serve that scope and
can select crontab when available, with a warning about the changed supervision
model. It fails if no compatible manager is available. An unreachable systemd
user manager is an explicit error instead of a fallback.

### macOS LaunchAgents

A user-scope install on macOS writes a LaunchAgent to
`~/Library/LaunchAgents/com.<name>.plist` and bootstraps it into a per-user
domain. The domain is resolved rather than assumed: daemonpilot tries
`gui/<uid>` first and falls back to `user/<uid>`, because
`launchctl bootstrap gui/<uid>` fails outright on a headless Mac or a CI runner
with no graphical session. The domain that actually worked is pinned in the
state file and reused by `uninstall`, `status`, `start` and `stop`, so a later
session with different login state cannot target a different domain and leave
an orphaned agent behind.

**launchd has no equivalent of systemd lingering.** A LaunchAgent in
`gui/<uid>` is torn down at logout and does not start at boot before a
graphical login — there is no per-user switch that changes this, so there is
nothing for daemonpilot to check or to tell you to enable. If the service must
run without anyone logged in, install it machine-wide with `ScopeSystem`.

Logs land in `~/Library/Logs/<name>/<name>.{out,err}.log`.

### crontab

The crontab manager was always per-user — it calls `crontab -l` and
`crontab <file>` and never `crontab -u`, so it edits the invoking user's
crontab in either scope. In user scope the entry redirects output to
`~/.local/state/<name>/<name>.log` — `~/Library/Logs/<name>/<name>.log` on
macOS — instead of `/var/log/<name>.log`, and the directory is created before
the entry is installed.

The default schedule is `@reboot`, and two caveats apply wherever crontab is
the primary manager rather than a fallback — Alpine, OpenWrt and Buildroot in
user scope:

- BusyBox `crond` honours `@reboot` only when it was built with
  `FEATURE_CROND_SPECIAL_TIMES`. Where it was not, the entry is installed and
  silently never fires. daemonpilot emits a warning naming this at install
  time; set `Config.CrontabSchedule` to a real time specification (for example
  `*/5 * * * *`, which the Unix instance lock normally keeps from starting a
  second copy) if you cannot rely on `@reboot`. See the
  [lock capability caveat](../guides/troubleshooting.md#single-instance-lock-capability).
- Most containers and WSL run no cron daemon at all, so nothing ever reads the
  crontab. Installing succeeds; the service simply never starts.

### Lingering

A systemd user manager is normally started at login and torn down at logout, so
a user service does not survive logout and does not start at boot. daemonpilot
therefore refuses a user-scope install when lingering is off, and names the
remedy:

```bash
loginctl enable-linger <user>
```

daemonpilot never runs that command for you, and never runs `disable-linger` on
uninstall: lingering is per-user, not per-service, so toggling it would affect
every other service that account owns. Set `Config.AllowNoLinger` to install
anyway — the install logs a warning and records the state, and the service
lives only as long as the session.

## Install Location

In system scope, the service is installed to `/opt/<name>` on Linux and
`/usr/local/bin` on macOS by default. Override it with `Config.InstallDir`. The install
directory also holds the state file, the single-instance `.lock` file, the
staged `.new` binary during an update, and the `.rollback` copy of the previous
binary.

Two properties of that directory are load-bearing:

- It must be on an **exec-capable** filesystem. `install` runs the copied binary
  once from its destination and fails with `daemonpilot.ErrNotExecutable` when the
  exec layer refuses it, because a `noexec` mount — `/home` is frequently one on
  hardened fleets, and `~/.local/share` inherits it — passes every write probe
  and then produces a `203/EXEC` restart loop forever.
- It must be **machine-local**. The Unix single-instance lock uses `flock`;
  locking behavior on shared filesystems depends on the server and mount options.
  A shared install directory is unsupported even when locking appears to work.

When `Config.ServiceUser` is set, `install` also creates `<name>.lock` and hands
it to that account while the install directory stays root-owned. Contention and
suspect lock paths are fatal; only a measured host inability to establish any
lock permits a logged degraded run without single-instance protection. See
[lock capability](../guides/troubleshooting.md#single-instance-lock-capability).
The state file is handed over as well: a staged update must record its rollback
digest before displacing the binary and aborts if it cannot write that state.
