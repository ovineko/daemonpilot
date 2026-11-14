# Self-Update

Self-update is opt-in: provide an `UpdateConfig` with an `UpdateSource` and an
ed25519 public key (or a custom `VerifyFunc`). The watchdog polls for offers.
Fast-path updates apply automatically; staged updates require approval through
`StagedUpdateFunc`.

```go
source, err := httpsource.New(httpsource.Config{
	ManifestURL: "https://releases.example.com/manifest/{os}/{arch}",
	DownloadURL: "https://releases.example.com/download/{version}/{os}/{arch}",
})
if err != nil {
	return err
}
s, err := daemonpilot.New(daemonpilot.Config{
	Name:       "myapp",
	WorkerFunc: workerMain,
	Update: &daemonpilot.UpdateConfig{
		Source:        source,
		PublicKey:     publicKey, // ed25519.PublicKey
		CheckInterval: 5 * time.Minute,
	},
})
```

## The Update Source

Your source decides what "a new version" means — an HTTP endpoint, an object
store, a package feed:

```go
type UpdateSource interface {
	// CheckUpdate returns nil, nil when no update is available.
	CheckUpdate(ctx context.Context, currentVersion string) (*UpdateInfo, error)
	DownloadUpdate(ctx context.Context, info *UpdateInfo) (io.ReadCloser, []byte, error)
}
```

`DownloadUpdate` returns the binary and its signature. Use `pkg/httpsource` for streaming downloads with authentication and signature
headers. See [HTTP Releases](http-releases.md) for the matching server, signer,
and a complete configuration. The small
[`examples/http-update-source`](../reference/examples.md) package wraps it.

## What Happens on Each Check

1. **Check** — `CheckUpdate` is asked whether something newer exists.
   `AcceptVersion`, when set, filters the offer before download or retry gating.
2. **Download and verify** — the binary is streamed to `<name>.new` next to the
   installed one, its SHA-256 is compared against `UpdateInfo.SHA256` when the
   source provides one, and the ed25519 signature is verified. A failure at any
   point deletes the staged file and aborts.
3. **Selftest** — the staged binary is executed as `<binary> selftest`, in its
   own process group or Windows job, with `SelftestTimeout` as the limit. It must
   start and return a successful JSON report. Without `SelftestFunc`, the default
   report is successful but performs no application-specific checks.
4. **Coordinate downtime** — after probing the watchdog version, call
   `PreRestartFunc` if configured.
5. **Apply** — fast path or staged path, decided by the watchdog version.

## Application Update Policy

`AcceptVersion func(current, offered string) error` is called on every offer,
even if its retry budget is exhausted. An error skips download without changing
retry state. Each refused version logs once at WARN per resident watchdog
lifetime, with `phase=update`, `outcome=rejected`, `version`, and `error`.
An accepted offer can be attempted later even if that same version was refused
before. Daemonpilot does not impose version ordering; implement the application's
own ordering, including date-based or nonnumeric schemes, in this callback.

`PreRestartFunc func(context.Context, *UpdateInfo) error` runs after download,
signature verification, selftest, and the watchdog-version probe succeed. It runs
on both fast and staged paths, before swapping the binary or restarting, and
before `StagedUpdateFunc`. Use it to drain requests or coordinate downtime.
Returning nil proceeds; returning an error removes the staged file, aborts the
attempt, and charges the retry budget. A refusal logs at WARN with
`phase=update`, `step=pre_restart`, `outcome=deferred`, `version`, and `error`.
Cancellation aborts and cleans up without a refusal log or retry charge. Daemonpilot
adds no deadline: the callback must manage its own timeout and honor cancellation.
Staged approval can still refuse after this callback succeeds, so coordination
must tolerate an attempt that does not ultimately restart.

These callbacks take effect only when the **resident watchdog** runs a binary
that contains them. A fast update replaces the worker while the old watchdog
keeps running. Until the watchdog itself is upgraded or restarted, nodes update
exactly as before; that is the intended direction of degradation. Change the
watchdog contract version when rolling out the new coordination behavior.

## Fast Path and Staged Path

The downloaded binary is asked for its `watchdog-version`. If it matches the
running one, the updater selects the fast path even if watchdog code changed:

- **Fast path** — the new binary replaces the old one and the next worker
  respawn picks it up. The watchdog keeps running; the worker restart skips
  backoff. Downtime is one worker restart.
- **Staged path** — the watchdog versions differ, so the watchdog process must
  be replaced. `StagedUpdateFunc` must approve it; a nil callback rejects all
  staged offers. The callback can refuse an update mid-transaction. Once
  approved, the previous binary is kept as `<name>.rollback`, and then:
  - when init can restart the watchdog, `Run` requests `ErrExecRestart` and
    `handleRun` replaces the process directly with `syscall.Exec` on Unix;
  - on systems that do not, [exec-through](architecture.md#exec-through) runs
    the new binary as a child instead.

On Windows, direct re-exec is unsupported. A staged replacement, rollback, or
`RespawnWatchdog` request ends the run with an error rather than loading the
binary in place. An installed SCM service relies on its configured recovery to
restart; console `run` needs an external restart. Do not assume Unix PID-preserving
re-exec behavior on Windows.

## Rolling Out Watchdog Code

A same-`WatchdogVersion` update takes the fast path: it swaps the binary and
restarts only the worker. The resident watchdog keeps its old in-memory code,
possibly for weeks. A worker can call `daemonpilot.RespawnWatchdog()` (exit 101)
after flushing its logs to roll newer watchdog code onto running hosts without
changing `WatchdogVersion`. Only request this when the resident watchdog is
known to understand that exit code. On Unix, exit 101 always requests a direct
`syscall.Exec` of the installed binary, preserving the watchdog PID regardless
of init capability. It does not select exec-through; a staged update selects
exec-through when init cannot restart the watchdog.

`daemonpilot.ResidentWatchdogStale(ownWatchdogVersion)` detects a non-empty
`DAEMONPILOT_WATCHDOG_VERSION` differing from the worker's version. Empty or unset
means an older watchdog that cannot understand exit 101, so it returns false.
`Config.RespawnStaleWatchdog` (default `false`) performs this check before
`WorkerFunc`, logs both versions at INFO, and exits 101 on a mismatch. Equal
versions cannot reveal different in-memory code; that case needs an explicit
application decision to call `RespawnWatchdog`.

Changing `WatchdogVersion` selects the staged path, which the **resident, old
watchdog** executes. Change it only once every resident watchdog is known to
handle that path correctly. Updating the file cannot repair the code already
executing that transition.

## Retry Budget

The watchdog retains one in-memory record for the most recent failing offered
version: version, attempt count, and next attempt time. It is lost on watchdog
restart and survives worker restarts. Discovery still runs every check interval
(except during staged probation); only work after an offer is gated. A different
version resets the record and is tried immediately, even if the old one exhausted
its budget. A successful update clears the record. No offer, discovery errors,
and cancelled operations leave the existing failure count unchanged.

`RetryMaxAttempts` defaults to 6 total failed attempts. Failures after an offer
(download, verification, selftest including an unsuccessful report, version
probe, or apply) delay another attempt by `RetryBaseBackoff` (30 seconds), doubling
up to `RetryMaxBackoff` (15 minutes). Attempts happen on the first poll at or after
the deadline. Exhaustion logs one WARN with `phase=update`, `outcome=gave_up`, and
`version`; later offers of that version are skipped. Cancellation and a watchdog
restart request do not consume attempts. Staged rejection (including a nil
`StagedUpdateFunc` or `false, nil`) returns an error and consumes an attempt.

Set `DisableRetryBudget: true` to preserve unrestricted retry behavior on every
check. Negative attempts or backoff values are rejected; zero selects defaults.
A base delay larger than the maximum is capped at the maximum immediately.

## Rollback

With `StagedRollbackOnUnstable` enabled, every new watchdog, including an
exec-through child, keeps the staged update on probation. Update checks pause
while the `.rollback` copy remains provisional. The supervising watchdog commits
once the worker is running and either:

- has signalled readiness and has at least 30 seconds of continuous uptime; or
- has run continuously for `StableThreshold` (also the path used without IPC on Windows).

Readiness alone never commits a crash-looping worker. Commit removes `.rollback`
and clears its recorded digest while the new watchdog keeps running. If the
worker does not prove stable within `RollbackTimeout`, the watchdog restores the
verified previous binary and restarts it. An ordinary watchdog requests direct
re-exec; an exec-through child stops its entire run and exits **0**, which tells
the proxy to reset backoff and immediately launch the on-disk binary.

The child owns worker probation while it runs. The proxy's max-backoff rollback
is a safety net for repeated watchdog start failures or crashes before the child
can resolve probation. The proxy waits for the child to exit before touching the
copy, so it cannot race the child's commit, restore, or quarantine. A successful
restore consumes the copy and clears the digest; quarantine moves an invalid
copy aside. The proxy therefore cannot restore a copy the child already consumed.
Atomic replacement keeps the installed binary whole if the child crashes during
restore; a crash before consumption can leave the verified copy for the next
owner to resolve. These file operations are not a multi-file transaction.

The proxy never commits based on child uptime: a long-lived watchdog might have
an unstable worker. If the proxy restores the previous binary and that watchdog
also repeatedly fails, `OnCriticalFailure` fires.

Without `StagedRollbackOnUnstable`, the legacy startup confirmation applies:
`RollbackConfirmFunc` approves or denies the update before worker supervision.
Approval deletes the copy and clears the digest; denial or `RollbackTimeout`
restores the previous binary. Without a callback, startup confirmation is
automatic.

### Rollback Integrity

A rollback restores a binary and executes it, so the `.rollback` file is as
sensitive as the update itself. It is bound to the update that created it:

- When the staged update makes the rollback copy, its SHA-256 is recorded in the
  state file as `rollback_digest`.
- Before any rollback restores it — in the updater and in the watchdog's
  staged-rollback path alike — the file is hashed again and the digests must
  match. A mismatch is logged at error level and the rollback is refused; an
  unverified binary is never executed.
- Restore opens a regular rollback file, copies it into `<name>.restore`, hashes
  the bytes while copying, and replaces the installed binary only after the
  digest matches. Unix opens with `O_NOFOLLOW`; Windows checks the file type
  through `Lstat` before opening and does not offer the same atomic no-follow
  guarantee. A mismatch removes the restore staging file. Hashing while copying
  avoids trusting a separate earlier read of mutable contents. This is not a
  security boundary against a writer of the install directory, including the
  restore staging path. `uninstall` sweeps a `.restore` file left by interruption.
- A `.rollback` file with **no** recorded digest is unusable, and
  `CheckRollbackFile` reports it as such. A rollback copy nobody recorded is one
  nobody created through the verified update path.
- The digest is cleared as soon as the rollback copy is consumed or confirmed
  away, so a later file at the same path cannot inherit it.
- A file that fails verification is **quarantined**: it is renamed to
  `<name>.rollback.invalid` so you can still inspect it, and the recorded digest
  is cleared. Leaving it in place would be worse than removing it — with
  `StagedRollbackOnUnstable`, the mere presence of `<name>.rollback` re-arms
  probation at every start, so a file that can never be restored would burn a
  full `RollbackTimeout` window and re-fire `OnCriticalFailure` forever.
  `uninstall` sweeps `.rollback.invalid` along with the other artifacts.
  An unreadable copy or state file is refused without quarantine, so a transient
  read failure does not destroy a valid rollback. Quarantine is reserved for a
  missing/mismatched digest or a non-regular rollback object.

This uses only the SHA-256 the updater already computes: there is no new key
material and no new signature format, and the ed25519 trust anchor is unchanged.

Be precise about what it buys you. The digest binds the `.rollback` file to the
binary the update actually displaced, which defeats symlink substitution and a
stale copy left by a previous installation — the case where a reinstall would
otherwise find, and act on, a rollback belonging to a service that no longer
exists. It is **not** a defence against an attacker who can already write the
install directory: the digest lives in the state file in that same directory, so
whoever can plant `<name>.rollback` can rewrite `rollback_digest` to match. In
user scope, where the watchdog, the worker and any other code running as that
user share a uid, protect the install directory itself — the digest raises the
bar, it does not replace filesystem permissions.

Before staging, the updater removes any existing `<name>.new` path without
following a symlink, then creates a fresh file with `O_EXCL`. A file planted
between those operations causes creation to fail rather than redirecting the
write. The install directory is created when missing. The staged file is synced
before replacement and the containing directory is synced afterward on a
best-effort basis. These steps improve crash durability but cannot guarantee
recovery across every filesystem or power-loss scenario. Every rename stays
inside the install directory; there is no cross-device fallback.

## Signing Releases

By default, updates use ed25519 signatures over the SHA-256 digest of the binary;
an optional signature context is prepended as described below.
`pkg/release.SignFile` hashes the file as a stream and signs that digest, matching
the built-in verifier. From the daemonpilot source checkout, the `daemonpilot-sign`
CLI stores a private key as a hex
32-byte seed followed by a newline:

```bash
go run ./cmd/daemonpilot-sign keygen -key signing.key
go run ./cmd/daemonpilot-sign sign -key signing.key ./myapp
go run ./cmd/daemonpilot-sign pubkey -key signing.key
```

`keygen` prints the public key as hex; `sign` prints the signature hex and SHA-256
hex on separate lines. See [HTTP Releases](http-releases.md) for key handling,
server caching, and the matching HTTP source. Keep the private key out of the
repository and CI logs.

### Domain-separated signatures

Set `UpdateConfig.SignatureContext` when the signing key also serves other
purposes. The built-in check verifies `[]byte(context) || SHA256(binary)` using
ordinary ed25519. Choose a context naming the application, purpose, and version,
ending in a NUL byte, such as `"myapp-binary-v1\x00"`. Distinct purpose tags
prevent cross-purpose signature replay; the NUL terminates the tag before the
digest. Each use of a shared key must have its own distinct domain.

```go
update := &daemonpilot.UpdateConfig{
    Source: source,
    PublicKey: publicKey,
    SignatureContext: "myapp-binary-v1\x00",
}
```

Sign with the exact same context using `release.SignFileContext`,
`release.SignDigestContext`, or `release.HandlerConfig.SignatureContext`:

```bash
go run ./cmd/daemonpilot-sign sign -key signing.key -context 'myapp-binary-v1\x00' ./myapp
```

The CLI interprets Go-style escapes by wrapping the flag value in double quotes
and calling `strconv.Unquote`; single quotes in the shell preserve `\x00` for
the CLI. Go API strings are used as supplied, without additional unquoting.

The default empty context preserves every existing bare-digest signature byte
for byte. A context-signed artifact fails verification with an empty or different
context; there is no fallback. Coordinate signer and client configuration when
enabling it. `VerifyFunc`, when set, replaces the built-in signature check
entirely and receives the staged path and original signature; `SignatureContext`
does not apply to that callback.

## Custom Verification and Release Hashes

Two independent extension points keep the defaults while allowing replacement:

- **`VerifyMessage func(message, signature []byte) error`** replaces the
  built-in Ed25519 check at the message level. It receives the exact bytes
  the library would sign — `SignatureContext` followed by the release digest —
  and the raw signature; returning nil activates the update, any error aborts
  it with no fallback to Ed25519 or unsigned apply. Keys travel in the
  closure, so `PublicKey` is not required for custom verifiers (the built-in
  chain still demands one), and setting both `VerifyFunc` and `VerifyMessage`
  is a configuration error. See `examples/custom-verifier`.
- **`NewHash func() hash.Hash`** selects the release-digest algorithm. One
  independent hasher runs per verification over the streamed bytes; the
  built-in Ed25519 check then signs the context followed by that digest.
  Manifests carry the result in the `Digest` field. Setting `NewHash` —
  explicitly, including `sha256.New` — rejects any nonempty `sha256` field
  regardless of its value, so the alias can never be reinterpreted as the
  configured digest. Under the default hash both fields mean SHA-256: each
  must be valid hex of the digest size, and both present must agree. Internal
  rollback digests remain SHA-256 regardless. See `examples/custom-hash`.

Neither point disables anything else: digest checks, signature checks, size
limits, cancellation, staging, selftest and apply/rollback rules all stay in
force. Algorithm choice and key custody are the application's responsibility;
the built-in signing CLI remains Ed25519/SHA-256.

## Configuration

See the [UpdateConfig reference](../reference/configuration.md#updateconfig) for
every field and its default.
