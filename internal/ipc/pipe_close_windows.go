//go:build windows

package ipc

import "os"

func closeRequestReader(file *os.File) {
	// Closing a Windows pipe reader can wait for its synchronous read, so do not block the caller.
	go func() { _ = file.Close() }()
}
