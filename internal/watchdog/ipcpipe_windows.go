//go:build windows

package watchdog

// exec.Cmd.ExtraFiles is unsupported on Windows. The descriptor-number IPC
// protocol cannot be used with Windows handles.
const inheritedFDIPCSupported = false
