package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(1)
	}
	switch os.Args[1] {
	case "watchdog-version":
		fmt.Println("2")
	case "selftest":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(1)
		}
		_, err = os.Stat(filepath.Join(filepath.Dir(executable), "fail-selftest"))
		fmt.Printf("{\"success\":%t}\n", os.IsNotExist(err))
	default:
		os.Exit(1)
	}
}
