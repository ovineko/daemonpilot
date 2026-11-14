//go:build e2e_docker && windows

package main

import (
	"errors"

	"github.com/ovineko/daemonpilot/e2e/docker/internal/wire"
)

func grandchild(wire.Config) error { return errors.New("Docker fixture requires Unix signals") }
