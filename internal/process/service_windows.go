//go:build windows

package process

import (
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
)

// stopGraceOnShutdown bounds how long a Stop request waits for the supervised work
// to unwind before the handler reports Stopped anyway. The SCM kills a service that
// takes too long, and reporting Stopped late is worse than reporting it early: the
// process is going away either way, and a handler still sitting in Execute is what
// produces the "service did not respond in a timely fashion" dialog.
const stopGraceOnShutdown = 20 * time.Second

var (
	serviceStopOnce sync.Once
	serviceStopCh   = make(chan struct{})
)

// signalServiceStop releases everything waiting on the service stop channel. Once
// only: the SCM may send Stop and Shutdown in quick succession, and closing a closed
// channel panics inside a handler nothing would report.
func signalServiceStop() {
	serviceStopOnce.Do(func() { close(serviceStopCh) })
}

// serviceStopChan is what SetupSignalHandler waits on in addition to signals. A
// Windows service receives NO signals at all — SIGTERM does not exist and the
// console Ctrl handlers never fire for a service — so without this the SCM's Stop
// would be delivered to a handler that had no way to reach the running work, and
// the process would be terminated hard instead of unwinding.
func serviceStopChan() <-chan struct{} { return serviceStopCh }

// RunAsService connects SCM control requests to body when launched as a service.
// Console launches run directly because they have no SCM status handle.
func RunAsService(name string, body func() error) error {
	if !IsWindowsService() {
		return body()
	}
	h := &serviceHandler{body: body}
	if runErr := svc.Run(name, h); runErr != nil {
		return runErr
	}
	return h.err
}

// IsWindowsService reports whether SCM launched this process, using the same
// detection as RunAsService. A detection error falls back to console behavior.
func IsWindowsService() bool {
	isService, err := svc.IsWindowsService()
	return err == nil && isService
}

type serviceHandler struct {
	body func() error
	err  error
}

// Execute is the SCM's side of the contract: report StartPending immediately, then
// Running, then answer control requests until the work finishes or a stop arrives.
func (h *serviceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}

	done := make(chan error, 1)
	go func() { done <- h.body() }()

	const accepted = svc.AcceptStop | svc.AcceptShutdown
	s <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case h.err = <-done:
			// The work ended on its own. A non-zero code is what makes the SCM
			// apply its recovery actions, so an error here must not be reported as
			// a clean stop — that is the difference between a node that restarts
			// and one that silently stays down.
			s <- svc.Status{State: svc.StopPending}
			if h.err != nil {
				return false, 1
			}
			return false, 0

		case req := <-r:
			switch req.Cmd {
			case svc.Interrogate:
				// Answering with the CURRENT status, not a fabricated one: this is
				// the SCM asking "are you still there", and echoing a made-up
				// Running would mask a wedged service.
				s <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				signalServiceStop()
				select {
				case h.err = <-done:
				case <-time.After(stopGraceOnShutdown):
				}
				return false, 0
			default:
				// Anything else is not accepted, so the SCM should not be sending
				// it; ignoring it is right, and reporting it as an error is not.
			}
		}
	}
}
