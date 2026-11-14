// Package daemonpilot provides a framework for building self-managing, long-running
// Go services. It handles daemonization (registering with OS init systems),
// watchdog monitoring (parent-child process model with auto-restart and backoff),
// and secure self-update (ed25519 signature verification, pre-flight selftest,
// and automatic rollback).
//
// A daemonpilot-based application runs as a single binary with multiple modes
// selected by CLI subcommand:
//
//	myapp install          # register in the OS init system
//	myapp uninstall        # unregister from the OS init system
//	myapp run              # watchdog mode (parent process)
//	myapp worker           # worker mode (child process, spawned by watchdog)
//	myapp selftest         # self-test mode (for update verification)
//	myapp watchdog-version # print watchdog version
//	myapp version          # print full version information
//
// Basic usage:
//
//	func main() {
//	    s, err := daemonpilot.New(daemonpilot.Config{
//	        Name:        "myapp",
//	        Description: "My long-running service",
//	        WorkerFunc: func(ctx context.Context) error {
//	            <-ctx.Done()
//	            return nil
//	        },
//	    })
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//	    if err := s.Run(); err != nil {
//	        log.Fatal(err)
//	    }
//	}
//
// The watchdog process monitors the worker, restarting it with exponential
// backoff on failure. Self-update support is enabled by providing an
// [UpdateConfig] with an [UpdateSource] implementation and an ed25519 public key.
package daemonpilot
