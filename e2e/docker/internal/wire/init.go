//go:build e2e_docker

package wire

import "github.com/ovineko/daemonpilot/pkg/daemonpilot"

// Capabilities records the production manager's advertised operations.
type Capabilities struct {
	Enable          bool `json:"Enable"`
	Start           bool `json:"Start"`
	Status          bool `json:"Status"`
	RestartWatchdog bool `json:"RestartWatchdog"`
}

// InitObservation is read inside the container, including root-owned state.
type InitObservation struct {
	Name             string                 `json:"Name"`
	PID1             string                 `json:"PID1"`
	PID1Command      string                 `json:"PID1Command"`
	Manifest         string                 `json:"Manifest"`
	Content          string                 `json:"Content"`
	ManifestContent  string                 `json:"ManifestContent"`
	Status           string                 `json:"Status"`
	StatusError      string                 `json:"StatusError"`
	UID              int                    `json:"UID"`
	Capabilities     Capabilities           `json:"Capabilities"`
	State            *daemonpilot.StateData `json:"State"`
	StateError       string                 `json:"StateError"`
	Processes        []int                  `json:"Processes"`
	Files            map[string]bool        `json:"Files"`
	BinaryDigest     string                 `json:"BinaryDigest"`
	RollbackVerified bool                   `json:"RollbackVerified"`
}
