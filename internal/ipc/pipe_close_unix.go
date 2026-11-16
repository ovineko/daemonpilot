//go:build !windows

package ipc

import "os"

func closeRequestReader(file *os.File) {
	_ = file.Close()
}
