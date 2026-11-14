// Package setup wires the internal watchdog and updater implementations into
// the public API. Import it for its side effects:
//
//	import _ "github.com/ovineko/daemonpilot/pkg/daemonpilot/setup"
package setup

import (
	"github.com/ovineko/daemonpilot/internal/updater"
	"github.com/ovineko/daemonpilot/internal/watchdog"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

func init() {
	daemonpilot.SetWatchdogFactory(func(cfg *daemonpilot.Config) daemonpilot.Runner {
		return watchdog.New(cfg)
	})

	daemonpilot.SetUpdaterFactory(func(cfg *daemonpilot.UpdateConfig, logger daemonpilot.Logger, installDir, binaryName string) daemonpilot.UpdaterFacade {
		return updater.New(cfg, logger, installDir, binaryName)
	})
}
