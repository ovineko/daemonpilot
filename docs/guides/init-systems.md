# Init Systems

daemonpilot detects the init system at install time and generates its service
manifest. The host must already provide a working manager and the privileges
needed to register and control the service.

## Supported Systems

| Init system  | Typical platforms                     | Auto-restart | Detection priority |
| ------------ | ------------------------------------- | ------------ | ------------------ |
| systemd      | Most Linux distributions              | Yes          | 1 (highest)        |
| launchd      | macOS                                 | Yes          | 2                  |
| OpenRC       | Gentoo, Alpine, Artix                 | Yes[^1]      | 3                  |
| dinit        | Artix Linux, Chimera Linux, eweOS     | Yes          | 4                  |
| Finit        | Embedded systems, custom Linux builds | Yes          | 5                  |
| runit        | Void Linux, Alpine, Artix             | Yes          | 6                  |
| s6           | Embedded systems, s6-overlay          | Yes          | 7                  |
| procd        | OpenWrt                               | Yes          | 8                  |
| BusyBox init | Buildroot, embedded Linux             | No           | 9                  |
| sysvinit     | Legacy Linux, embedded devices        | No           | 10                 |
| crontab      | Any POSIX system (fallback)           | No           | 11 (lowest)        |

Windows uses SCM rather than this POSIX detection sequence. It supports only
system scope and configures automatic startup and restart recovery.

[^1]:
    OpenRC restarts supervised services only when its supervisor support is
    available. daemonpilot looks for the `supervise-daemon` executable in PATH
    and generates a supervised service when found. Existing installations
    created with the old help-command probe need `update-config` to enable
    supervision; without the executable, the script uses background mode.

"Auto-restart" means the init system starts the watchdog again after it exits.
Systems that do not are the reason [exec-through](architecture.md#exec-through)
exists — they cannot be relied on to bring a replaced binary back.

## Detection Order

Detectors run in the priority order above and the first match wins. Priority
matters because these systems overlap: a Buildroot image ships the BusyBox
binary _and_ an `/etc/inittab`, an Alpine system may have both OpenRC and
BusyBox, and almost every system has `crontab`. Higher-priority detectors also
exclude their neighbours explicitly — the BusyBox detector, for instance, gives
up as soon as it sees systemd, OpenRC, runit, s6, or procd markers.

Detection is diagnosable. Set `LOG_LEVEL=trace` (or `debug`) and every check is
logged with its result — see [Troubleshooting](troubleshooting.md).

## Capabilities

Not every init system supports every operation, so managers advertise what they
can do and the CLI checks before acting:

| Capability             | Meaning                                              |
| ---------------------- | ---------------------------------------------------- |
| `SupportsEnable()`     | `enable` and `disable` control auto-start on boot    |
| `SupportsStart()`      | `start` and `stop` control the running service       |
| `SupportsStatus()`     | `status` can report the real service state           |
| `SupportsScope(s)`     | The init system can register a service in scope `s`  |
| `CanRestartWatchdog()` | The init system restarts the watchdog after it exits |

launchd, for example, has no separate enable step: boot behaviour is a plist key
(`RunAtLoad`), so `enable` reports that the operation does not apply rather than
pretending to succeed.

## Install Scopes

`Config.Scope` selects a machine-wide install (`ScopeSystem`, the default) or a
per-user one (`ScopeUser`). Managers advertise what they can register through
`SupportsScope(scope)`, and detection skips any manager that cannot serve the
requested scope.

| Init system  | System scope | User scope                        |
| ------------ | ------------ | --------------------------------- |
| systemd      | Yes          | Yes (`systemctl --user`)          |
| launchd      | Yes          | Yes (LaunchAgent, `gui`/`user`)   |
| crontab      | Yes          | Yes (the invoking user's crontab) |
| dinit        | Yes          | No                                |
| runit        | Yes          | No                                |
| s6           | Yes          | No                                |
| OpenRC       | Yes          | No — no user runlevels            |
| Finit        | Yes          | No — no user `initctl` FIFO       |
| sysvinit     | Yes          | No — no user `/etc/rc*.d`         |
| BusyBox init | Yes          | No — no user `/etc/rc*.d`         |
| procd        | Yes          | No — no per-user instance         |

Under `ScopeUser`, detection skips managers without user-scope support and can
select crontab when available; otherwise it fails with `ErrScopeNotSupported`.
The fallback warns because it changes the supervision model. daemonpilot does
not bootstrap a per-user dinit, runit, or s6 supervisor.

A systemd user install writes the unit to `~/.config/systemd/user/<name>.service`,
uses `WantedBy=default.target`, omits `User=`/`Group=` (systemd refuses to load
a user unit carrying them) and omits the `network-online.target` ordering, which
does not exist in the user manager. Every `systemctl` call carries `--user` and
is given `XDG_RUNTIME_DIR` in its environment.

If the host runs systemd but its per-user manager is unreachable — no
`$XDG_RUNTIME_DIR/systemd/private` and no `/run/user/<uid>/systemd/private`,
typical of `sudo -u` without a login session — detection aborts with a message
naming the remedies (`loginctl enable-linger <user>`, `machinectl shell`,
`systemd-run --user`) instead of quietly selecting crontab and swapping the
supervision model.

A launchd user install writes `~/Library/LaunchAgents/com.<name>.plist` and
bootstraps it into a resolved per-user domain — `gui/<uid>` when a graphical
session exists, otherwise `user/<uid>`. The resolved domain is pinned in the
state file, so every later `launchctl` call targets the domain the agent was
actually bootstrapped into. launchd has no equivalent of systemd lingering: an
agent in `gui/<uid>` is torn down at logout and does not start before graphical
login, and no per-user setting changes that.

A crontab user install is the same `crontab -l` / `crontab <file>` pair used in
system scope — that manager never called `crontab -u` and so always edited the
invoking user's crontab. Only the output redirect changes, from
`/var/log/<name>.log` to `~/.local/state/<name>/<name>.log`
(`~/Library/Logs/<name>/<name>.log` on macOS). On Alpine, OpenWrt
and Buildroot crontab is the primary user-scope manager rather than a fallback,
which makes its `@reboot` default worth checking: BusyBox `crond` honours
`@reboot` only when built with `FEATURE_CROND_SPECIAL_TIMES`, and containers
and WSL usually run no cron daemon at all. Install warns about this and points
at `Config.CrontabSchedule`.

Lifecycle commands after install do not re-detect. The scope, install directory
and init system chosen at install time are recorded in the state file and
reused, so `uninstall` over a different kind of session still targets the
service that was actually installed. A command whose configured scope disagrees
with the recorded one is refused — except `run`, which logs the divergence at
error level and continues, because the generated unit restarts it unconditionally.

## Where Files Land

| Init system  | Service manifest (system scope)           | Service manifest (user scope)             | Control command |
| ------------ | ----------------------------------------- | ----------------------------------------- | --------------- |
| systemd      | `/etc/systemd/system/<name>.service`      | `~/.config/systemd/user/<name>.service`   | `systemctl`     |
| launchd      | `/Library/LaunchDaemons/com.<name>.plist` | `~/Library/LaunchAgents/com.<name>.plist` | `launchctl`     |
| OpenRC       | `/etc/init.d/<name>`                      | —                                         | `rc-service`    |
| dinit        | `/etc/dinit.d/<name>`                     | —                                         | `dinitctl`      |
| Finit        | `/etc/finit.d/available/<name>.conf`      | —                                         | `initctl`       |
| sysvinit     | `/etc/init.d/<name>`                      | —                                         | `service`       |
| runit        | `/etc/sv/<name>/run`                      | —                                         | `sv`            |
| s6           | `/etc/s6/sv/<name>/run`                   | —                                         | `s6-svc`        |
| procd        | `/etc/init.d/<name>`                      | —                                         | `service`       |
| BusyBox init | `/etc/init.d/S95<name>`                   | —                                         | the script      |
| crontab      | the user crontab, marked with a comment   | the same crontab; only the log path moves | `crontab`       |

A `—` means the manager does not support user scope, so `install` with
`Config.Scope = ScopeUser` on that host either falls through to crontab or
fails with `ErrScopeNotSupported`.

## Enable and Disable Without Symlinks

Classic System V systems enable a service by symlinking it into `/etc/rc*.d/`.
Minimal systems often have neither those directories nor `update-rc.d` and
`chkconfig`. daemonpilot picks a strategy at runtime, in this order:

1. `update-rc.d` (Debian, Ubuntu)
2. `chkconfig` (RHEL, CentOS)
3. Direct symlinks in `/etc/rc*.d/`
4. Renaming the script between `S95<name>` and `K95<name>`
5. A `/etc/init.d/<name>.disabled` flag file, checked by the generated script

The flag-file fallback is what makes `disable` meaningful on BusyBox images that
support none of the above.

## BusyBox and the Respawn Loop

BusyBox init never restarts a service that dies. A watchdog killed by the OOM
killer would therefore stay dead until the next reboot, which is exactly the
failure mode embedded devices cannot afford. The generated init script wraps the
daemon in a small shell respawn loop: it restarts the daemon on an abnormal
exit, and stops when the daemon exits cleanly (an intentional stop) or when the
`stop` command creates the stop-flag file. The PID file holds the daemon's PID,
not the loop's, so `stop` and `status` behave normally.

Dinit environment files use literal `NAME=VALUE` lines. Finit uses an
`env:/etc/finit.d/available/<name>.env` modifier with a service-specific file.
Both managers reload changed definitions before restarting after `update-config`,
including restored environment files after a failed apply. Environment values
containing newlines, carriage returns or NUL are rejected by these backends.
Run `update-config` on existing installations to regenerate corrected files.

SysVinit and BusyBox start scripts detach their background supervision subshell
from the CLI's descriptors. runit and s6 wait up to ten seconds for the service
scanner to make a newly registered supervisor controllable; other start errors
are returned immediately.
