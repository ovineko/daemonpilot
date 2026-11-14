//go:build windows

package daemon

// platformDetectors puts the SCM first on Windows. It is the only entry that can
// ever match there: every unix detector below it already refuses on GOOS, and the
// crontab fallback has no `crontab` to find — so without this the detection walks
// the whole list and answers "no supported service manager detected", which is
// precisely what `install` reported before this backend existed.
func platformDetectors() []detectorEntry {
	return []detectorEntry{
		{
			name:   windowsInitSystemName,
			detect: func() bool { return true },
			create: func() ServiceManager { return newWindowsManager() },
		},
	}
}
