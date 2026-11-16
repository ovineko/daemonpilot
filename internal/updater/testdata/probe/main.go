// Command probe is a portable executable used to test updater subprocess probes.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "selftest":
		mode := os.Getenv("DAEMONPILOT_TEST_PROBE")
		switch mode {
		case "invalid-json":
			fmt.Print("not json at all")
		case "failure-result":
			fmt.Print(`{"success":false,"checks":{"api":{"ok":false,"message":"unreachable"}}}`)
		case "nonzero":
			os.Exit(1)
		case "delay":
			time.Sleep(30 * time.Second)
		default:
			fmt.Print(`{"success":true,"checks":{"db":{"ok":true,"message":"connected"}}}`)
		}
	case "watchdog-version":
		switch os.Getenv("DAEMONPILOT_TEST_PROBE") {
		case "nonzero":
			os.Exit(1)
		case "delay":
			time.Sleep(30 * time.Second)
		case "empty":
		case "whitespace":
			fmt.Print("  3  \n")
		default:
			fmt.Print("2\n")
		}
	case "worker":
		for {
			time.Sleep(time.Hour)
		}
	default:
		fmt.Println(os.Getenv("DAEMONPILOT_TEST_PROBE"))
	}
}
