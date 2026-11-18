package main

import (
	"os"
	"time"
)

var version string

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" || version == "" {
		os.Exit(2)
	}
	versionPath := os.Getenv("FIXTURE_VERSION_FILE")
	releasePath := os.Getenv("FIXTURE_RELEASE_FILE")
	if versionPath == "" || releasePath == "" {
		os.Exit(2)
	}
	if err := os.WriteFile(versionPath, []byte(version), 0o600); err != nil {
		os.Exit(2)
	}
	if version == "v2" {
		for {
			if _, err := os.Stat(releasePath); err == nil {
				_ = os.WriteFile(versionPath+".done", []byte("done"), 0o600)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for {
		time.Sleep(time.Hour)
	}
}
