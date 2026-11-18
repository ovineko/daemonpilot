package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

var mode string

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		os.Exit(2)
	}
	switch mode {
	case "run":
		run()
	case "exit0":
		return
	case "exit1":
		os.Exit(1)
	case "exit2":
		os.Exit(2)
	case "count-exit1":
		path := os.Getenv("WATCHDOG_TEST_COUNT_FILE")
		if path == "" {
			os.Exit(2)
		}
		data, _ := os.ReadFile(path)
		count, _ := strconv.Atoi(string(data))
		count++
		if err := os.WriteFile(path, []byte(strconv.Itoa(count)), 0o600); err != nil {
			os.Exit(2)
		}
		if count <= 2 {
			os.Exit(1)
		}
		run()
	case "marker-exit1":
		path := os.Getenv("WATCHDOG_TEST_MARKER")
		if path == "" {
			os.Exit(2)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		_, err = fmt.Fprintln(file, "started")
		_ = file.Close()
		if err != nil {
			os.Exit(2)
		}
		time.Sleep(200 * time.Millisecond)
		os.Exit(1)
	default:
		os.Exit(2)
	}
}

func run() {
	for {
		time.Sleep(time.Hour)
	}
}
