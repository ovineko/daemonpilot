package main

import (
	"os"
	"strconv"
	"time"
)

func main() {
	delay, err := time.ParseDuration(os.Getenv("FIXTURE_DELAY"))
	if err != nil {
		panic(err)
	}
	code, err := strconv.Atoi(os.Getenv("FIXTURE_CODE"))
	if err != nil {
		panic(err)
	}
	time.Sleep(delay)
	os.Exit(code)
}
