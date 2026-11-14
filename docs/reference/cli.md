# CLI Commands

`Daemonpilot.Run()` dispatches these commands from the executable's arguments.
Operations that change a system-scope install need sufficient filesystem and
service-manager permissions; examples below use `sudo` on Unix. On Windows,
manage SCM services from an elevated terminal. A user-scope install is selected
by `Config.Scope = daemonpilot.ScopeUser`, not by a command-line flag. See
[Install Scope](../get-started/installation.md#install-scope).

## Commands

| Command            | Behavior                                                                                                                                                              |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `install`          | Detect a compatible manager, preflight permissions and executable storage, copy the binary, write its service definition, enable auto-start and start where supported |
| `uninstall`        | Stop and unregister the service, then remove its binary, saved state, and managed working artifacts; leave the install directory in place                             |
| `status`           | Print human-readable manager status; not a worker health probe                                                                                                        |
| `start`            | Request service start                                                                                                                                                 |
| `stop`             | Request service stop                                                                                                                                                  |
| `restart`          | Stop and then start the service                                                                                                                                       |
| `enable`           | Enable auto-start                                                                                                                                                     |
| `disable`          | Disable auto-start; effects on a currently running service depend on the manager                                                                                      |
| `run`              | Run the watchdog, normally invoked by the service manager                                                                                                             |
| `worker`           | Run `WorkerFunc`, normally invoked by the watchdog                                                                                                                    |
| `selftest`         | Run `SelftestFunc` and print its `SelftestResult` as JSON                                                                                                             |
| `validate`         | Compare the installed definition against this binary's generated configuration and check host init-system drift                                                       |
| `update-config`    | Rewrite an installed definition with patchers applied, restarting where supported and rolling back synchronous failures                                               |
| `watchdog-version` | Print the bare watchdog-contract version                                                                                                                              |
| `version`          | Print application version, watchdog version, and Go runtime version as JSON                                                                                           |

Unsupported manager operations report errors rather than pretending to work.
`install` reports unsupported start/enable capabilities and skips those steps
where necessary; not every manager provides a full lifecycle. See the
[capability table](../guides/init-systems.md#capabilities).

There is no generic `--scope`, `--version`, `--help`, or `--install-dir` flag:
configuration belongs to the embedding application. Missing or unknown commands
print usage and return an error. With an existing application CLI, route commands
through `RunMode(mode, args...)` and configure
[`CommandPrefix`](configuration.md#embedding-in-a-cli-that-already-has-a-run-command)
for all re-executions.

## Lifecycle Examples

```bash
sudo ./myapp install
./myapp status
sudo ./myapp stop
sudo ./myapp start
sudo ./myapp restart
sudo ./myapp disable
sudo ./myapp enable
sudo ./myapp validate
sudo ./myapp update-config
sudo ./myapp uninstall
```

For a binary configured for user scope, omit `sudo`; do not switch accounts
between installation and lifecycle commands. Scope divergence from saved state
is refused. Later commands bind to the manager recorded at install rather than
silently selecting another manager.

Installation preflight can report `ErrNeedsPrivileges` or `ErrScopeCollision`
before copying, or `ErrNotExecutable` after the executable probe. See
[Troubleshooting](../guides/troubleshooting.md). Uninstall normally reports
`ErrNotInstalled` when no registration is present. The crontab manager warns if
its entry was already removed and still cleans up the remaining service artifacts.
Artifact-removal failures after unregistration warn; they do not restore a service
that has already been removed. Details are in
[Service Management](../guides/service-management.md).

## Supervision and Probe Modes

`./myapp worker` runs application work in the foreground without watchdog
supervision, which is useful for diagnosing startup errors. `./myapp run` starts
the watchdog directly. Neither command installs a service.

`run --exec-through` is reserved for internal
[exec-through](../guides/architecture.md#exec-through) launches; do not use it as
a way to bypass the normal instance lock.

`selftest` prints JSON; a failing check is represented by `success: false`, not
by a command error. The updater reads that field and rejects the release.
Nil `SelftestFunc` produces a passing result without application-specific checks.
Keep stdout free of unrelated output so update probes can parse it.

```json
{ "success": true, "checks": { "config": { "ok": true } } }
```

`version` prints a single JSON object, for example:

```json
{ "version": "1.5.0", "watchdog_version": "2", "go_version": "go1.25.5" }
```

The updater runs `watchdog-version` against the verified downloaded executable
to select the fast or staged path. Install also invokes it as an executable
probe: only execution-layer failures such as `EACCES` from a noexec mount or
`ENOEXEC` from an incompatible executable fail that probe; a nonzero program
exit is allowed because the embedding application owns argument parsing.

## Environment Variables

| Variable                             | Purpose                                                                                                     |
| ------------------------------------ | ----------------------------------------------------------------------------------------------------------- |
| `LOG_LEVEL`                          | `trace` or `debug` enables init-detection debug traces; detection warnings remain enabled at other levels   |
| `DAEMONPILOT_DISABLE_PARENT_MONITOR` | `1` disables cooperative parent monitoring for tests, not parent-side process groups or Windows Job Objects |

Other `DAEMONPILOT_` variables carry launch metadata and internal transport state.
Use `daemonpilot.GetRuntimeInfo()` rather than manufacturing them. Dynamic watchdog
queries use Unix IPC; Windows exposes static runtime information. See
[Observability](../guides/observability.md).

## Release Signing CLI

`cmd/daemonpilot-sign` is a separate executable for release tooling:

| Command                                                                 | Output                                                               |
| ----------------------------------------------------------------------- | -------------------------------------------------------------------- |
| `daemonpilot-sign keygen [-key signing.key]`                            | Creates a mode-0600 hex-seed private key file; prints public key hex |
| `daemonpilot-sign sign [-key signing.key] [-context <string>] <binary>` | Signature hex and SHA-256 hex, one per line                          |
| `daemonpilot-sign pubkey [-key signing.key]`                            | Public key hex                                                       |

Flags must precede the binary argument. The default key path is `signing.key`.
`keygen` refuses to overwrite an existing file. Private keys contain a 32-byte
Ed25519 seed encoded as 64 hexadecimal characters followed by a newline.
`-context` accepts Go-style escapes, for example `'myapp-binary-v1\x00'`; the
signer and verifier must use the identical decoded context. An empty context
signs the bare SHA-256 digest. See [HTTP Releases](../guides/http-releases.md).
