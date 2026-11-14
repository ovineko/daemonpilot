//go:build windows

package daemonpilot

import "context"

func startStackDumpHandler(context.Context, string, func(string, error)) bool { return false }
