// Command crashfixture is a minimal versioned executable for the updater
// crash-injection tests: the installed file must be startable by ordinary
// execution, answer the daemonpilot selftest subcommand, and the output must
// identify the version that its bytes carry.
package main

import (
	"fmt"
	"os"
)

var version = "v0"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "selftest" {
		fmt.Println(`{"success":true}`)
		return
	}
	fmt.Printf("crashfixture %s\n", version)
}
