package daemonpilot

import "errors"

// errNotRegularFile marks the type refusal in openRegularNoFollow, as opposed to
// an open that simply failed. The distinction is load-bearing on the rollback
// path: a file that is not a lone regular file is a verdict about what sits at
// the path, while an open that failed on fd exhaustion or I/O says nothing about
// it — and only the former may quarantine the copy.
var errNotRegularFile = errors.New("not a regular file")
