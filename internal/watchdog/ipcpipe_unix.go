//go:build !windows

package watchdog

// inheritedFDIPCSupported reports whether the watchdog can hand the worker its
// IPC pipe ends as inherited file descriptors.
//
// True everywhere but Windows: the transport is exec.Cmd.ExtraFiles plus an env
// var naming the descriptor NUMBERS (3 and 4), which is a POSIX construct.
const inheritedFDIPCSupported = true
