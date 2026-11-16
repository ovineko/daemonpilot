//go:build windows

package ipc

import "os"

func closeRequestReader(file *os.File) {
	// Closing a Windows pipe reader during a synchronous read can block; Serve closes it after the read ends.
}
