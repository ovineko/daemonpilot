# Config Patchers

Config patchers modify the generated init-system manifest before it is written,
so a project can add directives the library does not model without forking it.

```go
s, err := daemonpilot.New(daemonpilot.Config{
	Name:        "myapp",
	Description: "My service",
	WorkerFunc:  workerMain,
	ConfigPatchers: []daemonpilot.ConfigPatcher{
		func(path, content string, info daemonpilot.PatchInfo) (string, error) {
			if info.InitSystem == "systemd" {
				content += "\n[Service]\nProtectSystem=strict\n"
			}
			return content, nil
		},
	},
})
```

Patchers run in order, each receiving the output of the previous one, during
`install`, `validate`, and `update-config`. A patcher error prevents the generated
manifest from being written. It does not promise the entire CLI operation made
no changes: installation may already have copied the binary. `update-config`
generates and patches content before backing up or stopping the service.

## PatchInfo

| Field         | Description                                            |
| ------------- | ------------------------------------------------------ |
| `OS`          | `runtime.GOOS` of the machine being installed on       |
| `Arch`        | `runtime.GOARCH`                                       |
| `InitSystem`  | Detected init system: `systemd`, `launchd`, `procd`, … |
| `ServiceName` | The service name from `Config.Name`                    |
| `FilePath`    | Destination path of the manifest being generated       |

Always branch on `info.InitSystem`. A patcher that appends `[Service]` to every
manifest produces a broken plist on macOS and a broken shell script on BusyBox.

## Prefer the Typed Fields

Common customizations already have dedicated config fields. Prefer them and
check their platform support in the [configuration reference](../reference/configuration.md):

| Need                     | Use                           |
| ------------------------ | ----------------------------- |
| Environment variables    | `ServiceEnvironment`          |
| Extra CLI arguments      | `ServiceArguments`            |
| Run as a user or group   | `ServiceUser`, `ServiceGroup` |
| systemd `[Service]` keys | `SystemdDirectives`           |
| launchd plist keys       | `LaunchdKeys`                 |
| OpenRC script variables  | `OpenRCDirectives`            |
| crontab schedule         | `CrontabSchedule`             |

Reach for a patcher when the manifest needs something none of these express.

## Changing a Live Service

Patchers affect generated content, so changing one changes what a fresh install
produces. To apply the new output to an already-installed service, run
`update-config` — it backs up the current manifest, rewrites it, restarts the
service, and rolls back synchronous write/start failures. A later application
crash does not trigger that CLI rollback. See
[Service Management](service-management.md).
