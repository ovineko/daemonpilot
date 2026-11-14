# Service Management

## Lifecycle

```bash
sudo myapp install      # detect init system, write manifest, enable, start
myapp status            # current state
sudo myapp stop         # stop, stay installed
sudo myapp start        # start again
sudo myapp restart      # stop + start
sudo myapp disable      # stop auto-start on boot
sudo myapp enable       # auto-start on boot again
sudo myapp uninstall    # stop and remove
```

`install` starts the service where the manager supports it and configures boot
startup through that manager's registration or enable mechanism. `uninstall`
requests a stop before removing the manifest. See the platform-specific
[process cleanup limits](architecture.md#worker-process-trees). To keep a service
installed but not running, call `stop` after installing.

Whether `disable` also stops a running service depends on the manager: changing
an active supervision directory or reloading a removed definition can stop it.
Use `stop` when an immediate stop is required.

Operations that the detected init system cannot perform are reported rather than
silently skipped — see [capabilities](init-systems.md#capabilities).

## Installed State

`install` records what it did in a JSON state file inside the install directory:
the init system it detected, the manifest path, the version, and when it was
installed. A staged update additionally records `rollback_digest`, the SHA-256
of the `.rollback` copy — transient, in both scopes, cleared as soon as the copy
is consumed (see [Rollback Integrity](self-update.md#rollback-integrity)). A user-scope install also records the scope, the resolved install
directory, whether lingering was enabled, and — on macOS — the launchd domain
the agent was bootstrapped into (`gui/<uid>` or `user/<uid>`), so later commands
address that same domain instead of whatever the current session would resolve.

Later commands do not detect the init system again. They rebuild the manager
from the recorded one, so they act on the install that actually happened even
from a session that would detect something else — `sudo -u` without a logind
session, for instance. Detection is used only as a fallback for a service with
no state file. A command configured for a different scope than the one recorded
is refused rather than acting on the wrong service.

Without a state file, `validate` reports that the service is not installed
instead of reporting success for work it did not do. `uninstall` falls back to
the detected init system and removes the manifest if one is actually there —
an install that failed after writing the manifest never gets to write a state
file, and those artifacts still have to be removable — and otherwise reports
that the service is not installed. With the crontab manager, an entry that has
already been removed by hand is a warning rather than an error: the binary and
the state file still have to be cleaned up.

`uninstall` also sweeps the working files this service left next to its binary —
`.lock`, `.new`, `.rollback`, `.rollback.invalid`, `.restore` and `.backup`. A leftover
`.rollback` is the reason: after a reinstall, `run` would find the stale copy
from the previous installation and act on it. Only artifacts named after this
service are touched, and the install directory itself is left in place, since it
may be shared. A removal that fails is a warning — the service is already
unregistered by then.

A state file can also exist without an install: the updater writes a minimal one
to record `rollback_digest` for a service registered outside daemonpilot. A state
file that records no init system is read as absent, so `validate` reports the
service as not installed rather than diffing against a manifest path it never
had.

## Detecting Drift

```bash
sudo myapp validate
```

`validate` compares the installed service against what this binary would
generate now. It reports two kinds of problem:

- **Init system changed** — the manifest was written for a different init system
  than the one running now (a distribution upgrade, or an image rebuilt on a
  different base). Uninstall and reinstall; there is no safe in-place fix. The
  comparison is against a fresh detection of the host init system; when
  detection itself fails, the check is skipped with a warning rather than
  reported as a change.
- **Configuration drift** — the manifest no longer matches what the current
  config and patchers produce, whether from a hand edit or a config change in
  code.

!!! note "macOS installs predating the log-path change"

    launchd manifests used to point `StandardOutPath` and `StandardErrorPath`
    at `/tmp/<name>.{out,err}.log`. They now use `/var/log/<name>/` in system
    scope and `~/Library/Logs/<name>/` in user scope, because `/tmp` is
    world-writable and any local user could pre-create those files for a root
    LaunchDaemon to append to. Existing installs therefore report configuration
    drift until `update-config` or a reinstall, and any log shipper watching
    `/tmp` needs repointing.

## Applying Config Changes

```bash
sudo myapp update-config
```

`update-config` rewrites the manifest of an installed service without a
reinstall. It:

1. Regenerates the config, running [config patchers](config-patchers.md).
2. Backs up the current manifest next to it as `.backup`.
3. Stops the service, if the init system supports stopping.
4. Writes the new manifest, and the environment file for init systems that keep
   one (dinit).
5. Starts the service again.

Synchronous manifest-write and start-command failures trigger rollback: the previous manifest and environment file are
restored and the service restarted. If even the rollback fails, the `.backup`
file is deliberately left on disk and named in the error, so the previous
configuration is recoverable by hand.

A successful asynchronous start request is not a worker-health check. A service
that crashes after the manager accepted the request does not trigger CLI rollback.
The dinit and Finit backends reload changed definitions before starting them,
including definitions restored during rollback. Native behavior still depends
on the installed init system; see [testing coverage](testing.md).

Crontab manifests export service environment variables before launching the
command. Existing entries generated before this correction need `update-config`
to propagate those values to the process. Manual start retains the configured
log redirect and detaches standard input/output from the installer.

`update-config` cannot change the init system. That is what `validate` reports
and a reinstall fixes.
