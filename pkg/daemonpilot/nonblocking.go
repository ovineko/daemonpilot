package daemonpilot

import (
	"io"
	"sync/atomic"
)

const defaultNonBlockingBuffer = 1024

// NonBlockingWriter decouples callers from a stalled log mirror using one drain
// goroutine. It drops writes when its bounded queue is full. It is process-lived;
// there is no flush or close operation, and destination errors are ignored.
type NonBlockingWriter struct {
	queue   chan []byte
	dropped atomic.Int64
}

// NewNonBlockingWriter starts one drain goroutine. A nonpositive buffer uses 1024
// queued writes; a nil destination discards output. The bound counts writes, not bytes.
func NewNonBlockingWriter(w io.Writer, buffer int) *NonBlockingWriter {
	if buffer <= 0 {
		buffer = defaultNonBlockingBuffer
	}
	if w == nil {
		w = io.Discard
	}
	n := &NonBlockingWriter{queue: make(chan []byte, buffer)}
	go func() {
		for p := range n.queue {
			_, _ = w.Write(p)
		}
	}()
	return n
}

// Write copies p without waiting for the destination, returning len(p), nil even
// when the queue is full. Dropped counts those discarded writes.
func (n *NonBlockingWriter) Write(p []byte) (int, error) {
	copyOfP := append([]byte(nil), p...)
	select {
	case n.queue <- copyOfP:
	default:
		n.dropped.Add(1)
	}
	return len(p), nil
}

// Dropped reports the number of writes discarded because the queue was full.
func (n *NonBlockingWriter) Dropped() int64 { return n.dropped.Load() }
