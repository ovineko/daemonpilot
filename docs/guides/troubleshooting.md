# Troubleshooting

## Diagnosing Init System Detection

If the wrong init system is chosen — crontab instead of BusyBox, say — turn on
trace logging and every detection step is reported:

```bash
sudo env LOG_LEVEL=trace /opt/myapp/myapp install
```

```text
level=DEBUG msg="init detection: starting service manager detection"
level=DEBUG msg="init detection: checking systemd" detector=systemd
level=DEBUG msg="init detection: systemd: stat /run/systemd/system -> no such file or directory"
level=DEBUG msg="init detection: systemd: detected=false reason=systemd directory not found"
level=DEBUG msg="init detection: checking runit" detector=runit
level=DEBUG msg="init detection: runit: lookpath runsvdir -> /usr/bin/runsvdir"
level=DEBUG msg="init detection: runit: stat /var/service -> no such file or directory"
level=DEBUG msg="init detection: runit: detected=false reason=/var/service directory not found"
level=DEBUG msg="init detection: checking busybox" detector=busybox
level=DEBUG msg="init detection: busybox: readlink /proc/1/exe -> /bin/busybox"
level=DEBUG msg="init detection: busybox: detected=true reason=PID 1 is busybox"
level=DEBUG msg="init detection: selected manager=busybox" manager=busybox
```

Each line names the check, its raw result, and the conclusion, so the first
detector that decided wrongly is visible directly. Only `trace` and `debug`
enable the debug trace. Detection warnings remain enabled at other levels.

## BusyBox Not Detected

BusyBox is normally found because `/proc/1/exe` points at a path containing
"busybox". Some images hide that: on nanokvm devices `/proc/1/exe` points to
`/usr/sbin/init`, which _is_ BusyBox but says so nowhere in its path. The
detector falls back to inspecting the binary's contents:

```text
level=DEBUG msg="init detection: busybox: readlink /proc/1/exe -> /usr/sbin/init"
level=DEBUG msg="init detection: busybox: checking path for busybox string -> not found"
level=DEBUG msg="init detection: busybox: resolving symlinks for /usr/sbin/init"
level=DEBUG msg="init detection: busybox: checking binary content of /usr/sbin/init"
level=DEBUG msg="init detection: busybox: found 'BusyBox' in binary content"
level=DEBUG msg="init detection: busybox: detected=true reason=binary content contains BusyBox"
```

If you see `detected=false reason=no busybox indicators found` on a system you
know runs BusyBox, check that the init binary contains the string `BusyBox` near
its start — that is the marker the content check looks for.

Also check the exclusions: the BusyBox detector gives up when it sees systemd,
OpenRC, runit, s6, or procd markers, because those systems ship on top of
BusyBox and should win. The trace log names the exclusion that fired.

## dinit Not Detected

The primary marker is the `/etc/dinit.d` directory. Fallbacks are the `dinitctl`
command and the `/sbin/dinit` binary, both confirmed against `/proc/1/exe`.

```text
level=DEBUG msg="init detection: dinit: checking dinit detection" detector=dinit
level=DEBUG msg="init detection: dinit: found /etc/dinit.d" detected=true
level=DEBUG msg="init detection: selected manager=dinit" manager=dinit
```

Services are written to `/etc/dinit.d/<name>` and controlled with `dinitctl`.

## Finit Not Detected

The primary marker is `/etc/finit.d`, with `/etc/finit.conf` and `/sbin/finit`
as fallbacks.

Finit uses `initctl`, and so does upstart. The detector therefore looks for
Finit-specific files _before_ falling back to the binary check, so an upstart
system is not mistaken for Finit. Configs live in
`/etc/finit.d/available/<name>.conf` and are enabled by symlink from
`/etc/finit.d/enabled/`.

## sysvinit Not Detected

`/etc/inittab` is the primary marker. Without it, the detector checks
`/sbin/init` and `/proc/1/exe`, excluding anything that resolves to systemd,
BusyBox, runit, s6, procd, dinit, or Finit.

Enable and disable pick a strategy at runtime — `update-rc.d`, `chkconfig`,
direct `/etc/rc*.d/` symlinks, `S95`/`K95` renaming, or a `.disabled` flag file.
See [Init Systems](init-systems.md#enable-and-disable-without-symlinks).

## The Service Will Not Start

- Read the error itself first. A failing `systemctl`, `launchctl` or init-script
  call carries that command's own output, so the cause is usually in the message
  already:

  ```text
  testapp: starting service: running systemctl: exit status 1: Unit not found.
  ```

  Output longer than 512 bytes is cut off with `(output truncated)`.

- Run `myapp status` next: it reports the init system's own view.
- Check whether the init system can report status at all — on some systems
  `status` is unavailable, and the command says so.
- Run the worker in the foreground (`myapp worker`) to see startup errors that
  the init system swallows.
- On systemd with `NotifyReady: true`, a worker that never calls
  `daemonpilot.NotifyReady()` is considered failed once the start timeout expires.
- For a user-scope install, query the per-user manager rather than the system
  one: `systemctl --user status <name>` and `journalctl --user -u <name>`.

## Install Preflight Failures

`install` probes what it is about to write before it mutates anything, so a
permission problem names the path and a remedy instead of surfacing a raw
`permission denied` from the middle of the sequence.

- **`insufficient privileges for the installation paths`** (`daemonpilot.ErrNeedsPrivileges`)
  — the install directory or the init system's manifest directory did not accept
  a probe file. The message names every probed path, the effective uid, and
  which of them failed; a directory that does not exist yet is probed through its
  nearest existing ancestor, since `install` would create it. For a system-scope
  install the remedy is to re-run with elevated privileges (for example under
  `sudo`), or to set `Config.Scope = daemonpilot.ScopeUser`, which the message spells
  out with the install and log directories that scope would resolve to. A
  user-scope install never elevates: fix the ownership of the paths under your
  home directory. The check probes rather than compares uids, so a
  group-writable `/etc/systemd/system` or `CAP_DAC_OVERRIDE` passes, and a
  read-only `/etc` in a container fails even under root.

- **`the installed binary cannot be executed`** (`daemonpilot.ErrNotExecutable`) —
  after copying the binary, `install` runs it once as `<binary> watchdog-version`
  from its destination. A write probe alone is not enough: `/home` is frequently
  mounted `noexec` on hardened fleets and `~/.local/share` inherits that, which
  yields a green write preflight, a successful install, and then a `203/EXEC`
  restart loop forever, because the generated unit carries `Restart=always` with
  `StartLimitIntervalSec=0`. Either mount the filesystem `exec`, or set
  `Config.InstallDir` to a directory on an exec-capable filesystem. The same
  probe reports a binary built for a different architecture or a missing dynamic
  loader (`is not a valid executable on this platform`). A non-zero exit status
  is _not_ treated as a failure — your program owns its argument parsing — only
  an `EACCES`/`ENOEXEC` from the exec layer itself is.

- **`the service is already installed in the other scope`**
  (`daemonpilot.ErrScopeCollision`) — a service installed in one scope and then in
  the other leaves two supervisors racing over the same binary name. The
  preflight checks the _other_ scope's deterministic manifest path for the
  detected init system —`/etc/systemd/system/<name>.service` against
  `~/.config/systemd/user/<name>.service`, `/Library/LaunchDaemons/com.<name>.plist`
  against `~/Library/LaunchAgents/com.<name>.plist` — and refuses, naming the
  path and the scope it belongs to. Uninstall the other installation first, or
  install under a different `Config.Name`. `InstallDir` is deliberately not
  probed: it may be set explicitly, so it is not deterministic; the manifest path
  is.

- **`lock file … is owned by uid …`** — `run` takes its single-instance lock on
  a file it opens with `O_NOFOLLOW` and refuses one owned by another account, so
  a lock is never taken on a file someone else controls. Running as root the
  file is unlinked and recreated instead of refused, because a group-writable
  system install directory — Homebrew leaves `/usr/local/bin` writable by `admin`
  on Intel Macs — would otherwise let an unprivileged user pre-create it and
  permanently block a root daemon. Two files are exempt from that replacement:
  one that `Config.ServiceUser` owns, since `install` handed it over deliberately
  and recreating it would leave a root-owned file the service account can never
  open again; and one another process is actively holding, which is reported as
  already-running instead — unlinking it would detach the inode that instance is
  locking and let two watchdogs run over the same install directory. As a
  non-root user, remove the file or fix its ownership only after ensuring no
  instance is holding it. Root can repair a symlink through no-follow replacement;
  non-root refuses it. Suspect objects and contention are fatal. See the measured
  host-capability exception below.

### Single-Instance Lock Capability

On Unix, a write-open failure first retries an existing file read-only: `flock`
does not require write access. The file still undergoes ownership, regular-file,
link-count, and inode checks. A read-only filesystem alone does not imply that
locking is unavailable.

For other non-contention failures, daemonpilot tries a unique probe file in the same
directory. If that probe locks successfully, the original path is broken and
`run` fails. If the probe also cannot establish a lock, `run` continues in an
explicitly degraded mode, logging both errors at ERROR and exporting
`RuntimeInfo.LockStatus = LockStatusUnlocked` (`unlocked`). There is no
single-instance guarantee in that mode. Windows reports `LockStatusUnsupported`
(`unsupported`), since daemonpilot has no instance lock there; SCM only serializes
launches made through the service manager.

## Upgrading an Existing `ServiceUser` Install

Re-run `install` after upgrading the binary of a system-scope service that sets
`Config.ServiceUser`. Two files in the root-owned install directory are now
created and handed to the service account by `install`, and only by `install`:

- `<name>.lock`. The supervised process runs unprivileged and may be unable to
  create it in a root-owned directory. Without an existing usable lock, a host
  that cannot create a probe file can run in degraded unlocked mode rather than
  providing single-instance protection. Reinstall to restore a lock owned by the
  service account; do not rely on degraded mode for normal operation.
- `.daemonpilot-<name>.json`. The staged update records the rollback digest there
  before displacing the binary and aborts when it cannot, so a state file the
  service account cannot write is a self-update that fails and undoes itself on
  every poll, logging `failed to record rollback digest, aborting update`.

Re-running `install` is idempotent and fixes both: it chowns the files if they
already exist rather than replacing them, so a live instance's lock is never
detached.

## The Install Directory Must Be Machine-Local

`Config.InstallDir` must live on local storage. A network-mounted home
directory — NFS, SMB, or any shared `$HOME` in a user-scope install — is not
supported:

- The Unix single-instance lock uses `flock`. Network filesystem behavior depends
  on the protocol, server, and mount options; shared storage is not a supported
  cross-host coordination mechanism.
- Both would then write the same `<name>.rollback` copy, the same `<name>.new`
  staging file and the same state file, so each host's self-update would
  overwrite the other's rollback binary and recorded digest. The rollback
  integrity check turns that into a refused rollback rather than an executed
  wrong binary, but the safety net is gone either way.

In user scope on a host with a network home, set `Config.InstallDir` explicitly
to a local path.

## User-Scope Problems

- **`per-user init system manager is unreachable`** — the host runs systemd but
  this session cannot talk to the per-user manager, which is what happens under
  `sudo -u <user>` or in a bare cron job. Detection aborts instead of falling
  through to crontab, which cannot restart a watchdog. Either enable lingering
  (`loginctl enable-linger <user>`) so the manager runs independently of logins,
  or run the command from a real user session (`machinectl shell <user>@`,
  `systemd-run --user`).

- **Install refuses because lingering is disabled** — without lingering, systemd
  tears the per-user manager down at logout and does not start it at boot, so
  the service silently ceases to exist. Run the `loginctl enable-linger <user>`
  command from the error message and install again. `Config.AllowNoLinger`
  installs anyway, for a service that is meant to end with the session;
  `myapp status` then explains an inactive service with a reminder that it was
  installed without lingering.

- **`systemd loads unit <name>.service from …, but it was written to …`** — the
  install wrote the unit where `$HOME` points, but the user manager reads it
  from somewhere else. This is usually a mismatched `$HOME` between the
  installing session and the user manager. Unit paths derive from `$HOME` alone;
  `XDG_CONFIG_HOME` is deliberately ignored, because the user manager does not
  inherit the login shell's environment.

- **`was installed in <scope> scope but this command is configured for <scope>`** —
  the state file records the scope the install used, and lifecycle commands
  refuse to act across scopes. Run the command with the same `Config.Scope` the
  install used.

- **`resolving launchd domain: neither gui/<uid> nor user/<uid> is reachable
  from this session`** — a user-scope install on macOS needs a per-user launchd
  domain, and an SSH-only or `sudo -u` context has neither. Run the command from
  a real login session, or install with `Config.Scope = ScopeSystem`.
  `uninstall` is deliberately exempt: it warns, still tries to unload the agent
  from both per-user domains, and removes the plist, so an install can always be
  cleaned up.

- **A user-scope LaunchAgent disappears after logout** — launchd has no
  equivalent of systemd lingering. An agent in `gui/<uid>` is torn down at
  logout and is not started at boot before a login, which is what `myapp status`
  reports for an inactive user-scope agent. Install with
  `Config.Scope = ScopeSystem` for a service that must survive logout.

- **`crontab @reboot is not universally supported`** — BusyBox `crond` honors
  `@reboot` only when built with `FEATURE_CROND_SPECIAL_TIMES`. A time-based
  `Config.CrontabSchedule` addresses that limitation only if a cron daemon is
  actually running. Containers and WSL images without one need a running manager
  or an external supervisor; changing the schedule alone cannot start a daemon.

- **`status` errors instead of reporting "stopped"** — launchd status names the
  domain explicitly, so a domain you are not allowed to query is reported as an
  error rather than silently read as stopped. Inspecting a root LaunchDaemon as
  an unprivileged user is the usual cause; re-run with `sudo`.

## Updates Are Not Applied

- Verify the signature and hash: verification failures delete the staged
  `.new` file and log the reason.
- Check that the selftest passes — the staged binary is run as
  `<binary> selftest` and rejected if it fails or times out.
- A staged update waits for `StagedUpdateFunc` approval when configured; if it
  never approves, nothing is applied.
- After a staged update, a leftover `.rollback` file means the update is still
  unconfirmed. See [Self-Update](self-update.md#rollback).
- **`rollback binary failed verification`** — the `.rollback` file is not the
  binary this installation displaced. It is refused rather than executed, and
  logged at error level. A `.rollback` left over from an earlier installation is
  the usual cause; `uninstall` now removes it, but a file restored from a backup
  or copied in by hand has no recorded digest and is always refused. See
  [Self-Update](self-update.md#rollback-integrity).

## Orphaned Workers

Cooperating Unix workers monitor their expected parent and cancel application
work when it disappears; Linux also supplies kernel SIGTERM. Check whether
`DAEMONPILOT_DISABLE_PARENT_MONITOR=1` has disabled the cooperative monitor for a
test. A worker must honor cancellation or signals to exit promptly. Unix
descendants can escape process groups, and BSD natural-exit cleanup has limits.
Windows uses kill-on-close Job Objects, with a creation-to-assignment window.
See [Architecture](architecture.md#orphan-prevention) for the bounded guarantees.
