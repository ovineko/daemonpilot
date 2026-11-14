# daemonpilot

A Go library for self-managing, long-running services: register your executable
with the host's service manager, supervise a worker process, and verify and
selftest signed updates. Watchdog-changing updates support approval and rollback.

## Get Started

See the [installation requirements](https://daemonpilot.ovineko.com/get-started/installation/)
and [Quick Start](https://daemonpilot.ovineko.com/get-started/quick-start/)
for dependency setup and a complete application. The public API is
`github.com/ovineko/daemonpilot/pkg/daemonpilot`; import
`github.com/ovineko/daemonpilot/pkg/daemonpilot/setup` for its default implementations.

For a source checkout and local verification, see [CONTRIBUTING.md](CONTRIBUTING.md).

## Documentation

Full documentation: **<https://daemonpilot.ovineko.com/>**

- [Quick Start](https://daemonpilot.ovineko.com/get-started/quick-start/)
- [Installation and install scopes](https://daemonpilot.ovineko.com/get-started/installation/)
- [Architecture](https://daemonpilot.ovineko.com/guides/architecture/)
- [Init Systems](https://daemonpilot.ovineko.com/guides/init-systems/)
- [Self-Update](https://daemonpilot.ovineko.com/guides/self-update/)
- [CLI Commands](https://daemonpilot.ovineko.com/reference/cli/)
- [Configuration](https://daemonpilot.ovineko.com/reference/configuration/)

## License

MIT. See [LICENSE](LICENSE).
