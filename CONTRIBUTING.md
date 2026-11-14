# Contributing to daemonpilot

## Development Setup

Use the Go toolchain required by `go.mod`, Node.js and pnpm matching
`package.json`, and Git. Repository tooling and documentation commands run
through datamitsu.

Go dependencies, including `github.com/ovineko/procguard`, are downloaded at the
versions pinned in `go.mod` and verified against `go.sum`. No sibling source
checkout or local replacement is required.

```bash
cd daemonpilot
export GOWORK=off
go mod download
pnpm install --frozen-lockfile
pnpm dm init
```

`GOWORK=off` makes these commands use the published dependencies pinned by this
module rather than an unrelated parent workspace.

## Verification

Run commands from the repository root with the Go module setup above:

```bash
go vet ./...
go test ./...
go test -race ./...
go test -count=3 ./test/...
pnpm dm check
```

The final Go command repeats the real-process integration journeys. Tests build
fixture binaries into temporary directories and synchronize on observable process
or file state with bounded deadlines. A facility-dependent test may skip with an
explicit reason when the host genuinely cannot provide it; a skipped test is not
evidence that the facility works.

For coverage:

```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

Use the managed quality gate rather than invoking separate linters or formatters.
Hooks run `check`; CI runs the read-only `lint` operation. Root `.datamitsuignore`
records tool exclusions, including the pinned Go analyzer's incompatibility with
Go 1.27.2 export data. Builds and `go vet` remain required. Do not bypass hooks or
disable commit signing. See the [testing guide](https://daemonpilot.ovineko.com/guides/testing/)
for Docker init-system contracts, cross-architecture execution, fuzzing, and the
difference between build coverage and exercised platform behavior.

## Documentation

Published pages live in `docs/` and navigation is configured in `zensical.toml`.
Keep examples aligned with the public API and preserve the canonical module
`github.com/ovineko/daemonpilot` and site
<https://daemonpilot.ovineko.com/>. Internal implementation plans belong in
`docs/.plans/`; ordinary directories under `docs/` are published.

```bash
pnpm dm exec task -- docs:build
pnpm dm exec task -- docs:serve
```

The preview listens on port 8000. Review changed pages, navigation, relative links,
and desktop/mobile rendering before submitting documentation changes.

## Project Structure

| Path                       | Responsibility                                                           |
| -------------------------- | ------------------------------------------------------------------------ |
| `pkg/daemonpilot/`         | Public API, config, CLI dispatch, lifecycle handlers, and worker helpers |
| `pkg/daemonpilot/setup/`   | Registration of internal watchdog and updater factories                  |
| `pkg/daemonpilot/metrics/` | Optional Prometheus integration, isolated from the core public package   |
| `pkg/httpsource/`          | Streaming HTTP update source                                             |
| `pkg/release/`             | Release signing and HTTP serving                                         |
| `cmd/daemonpilot-sign/`    | Release-signing CLI                                                      |
| `internal/daemon/`         | Service managers, detection, and generated configuration                 |
| `internal/watchdog/`       | Supervision policy, health checks, updates, and probation                |
| `internal/updater/`        | Download, verification, probes, apply, and rollback                      |
| `internal/ipc/`            | Protocol over inherited anonymous pipes                                  |
| `internal/process/`        | Signal handling, probes, and Windows service integration                 |
| `test/integration/`        | Real-process, cross-package journeys                                     |
| `examples/`                | Embedding and update-source examples                                     |

Process mechanics are provided by procguard. Keep policy in daemonpilot instead of
adding a second process supervisor. See the
[architecture guide](https://daemonpilot.ovineko.com/guides/architecture/) for the design boundaries.

## Guidelines

- Prefer the standard library and keep external dependencies minimal. The core
  already uses procguard and platform support libraries; Prometheus is optional.
- Use table-driven tests and cover success, failure, cancellation, and cleanup.
- Keep test files and processes isolated in temporary directories. Never modify
  an installed service as part of the ordinary test suite.
- Keep platform-specific code in build-tagged files with counterparts for the
  excluded platforms, and distinguish cross-compilation from runtime validation.
- Keep the public API small. `pkg/daemonpilot` must not import the internal watchdog
  or updater, or import the optional metrics package back into the core.
- Write documentation and necessary comments in English. Prefer clear names and
  code over comments that repeat what the code does.
- Update affected documentation and the changelog with permanent behavior changes.

## Commit Messages

Use Conventional Commits, for example:

```text
feat: add readiness support
fix: preserve rollback configuration
docs: clarify install scope
test: cover cancelled downloads
refactor: simplify update state
```

## Version Injection

Build an example with explicit version values when testing version-dependent
behavior:

```bash
go build -ldflags "-X github.com/ovineko/daemonpilot/pkg/daemonpilot.Version=1.0.0 -X github.com/ovineko/daemonpilot/pkg/daemonpilot.WatchdogVersion=1" -o myapp ./examples/minimal
```

`Config.Version` and `Config.WatchdogVersion` override the package variables when
set explicitly. A changed watchdog version selects the staged update path; it is
not a semantic-version comparison.
