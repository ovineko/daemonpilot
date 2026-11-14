package watchdog

import (
	"fmt"
	"strings"
	"sync"

	"github.com/ovineko/daemonpilot/internal/ipc"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"
)

const maxLogBatchEntries = 128

type logRing struct {
	mu       sync.Mutex
	capacity int
	bytes    int
	next     uint64
	dropped  uint64
	entries  []ipc.LogEntry
}

func newLogRing(capacity int) *logRing {
	return &logRing{capacity: capacity, next: 1}
}

func (r *logRing) append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry := ipc.LogEntry{Sequence: r.next, Line: line}
	r.next++
	if len(line) > r.capacity {
		r.dropped += uint64(len(r.entries) + 1)
		r.entries = nil
		r.bytes = 0
		return
	}
	for r.bytes+len(line) > r.capacity && len(r.entries) > 0 {
		r.bytes -= len(r.entries[0].Line)
		r.entries = r.entries[1:]
		r.dropped++
	}
	r.entries = append(r.entries, entry)
	r.bytes += len(line)
}

func (r *logRing) collect(finishedThrough uint64) ipc.LogBatch {
	r.mu.Lock()
	defer r.mu.Unlock()

	first := 0
	for first < len(r.entries) && r.entries[first].Sequence <= finishedThrough {
		r.bytes -= len(r.entries[first].Line)
		first++
	}
	if first > 0 {
		r.entries = append([]ipc.LogEntry(nil), r.entries[first:]...)
	}
	count := min(len(r.entries), maxLogBatchEntries)
	entries := append([]ipc.LogEntry(nil), r.entries[:count]...)
	return ipc.LogBatch{Entries: entries, Dropped: r.dropped}
}

type captureLogger struct {
	logger daemonpilot.Logger
	ring   *logRing
}

func (l captureLogger) Debug(msg string, args ...any) {
	l.logger.Debug(msg, args...)
	l.log("DEBUG", msg, args...)
}

func (l captureLogger) Info(msg string, args ...any) {
	l.logger.Info(msg, args...)
	l.log("INFO", msg, args...)
}

func (l captureLogger) Warn(msg string, args ...any) {
	l.logger.Warn(msg, args...)
	l.log("WARN", msg, args...)
}

func (l captureLogger) Error(msg string, args ...any) {
	l.logger.Error(msg, args...)
	l.log("ERROR", msg, args...)
}

func (l captureLogger) log(level, msg string, args ...any) {
	l.ring.append(formatLogLine(level, msg, args))
}

func formatLogLine(level, msg string, args []any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "level=%s msg=%q", level, msg)
	for len(args) >= 2 {
		fmt.Fprintf(&b, " %v=%v", args[0], args[1])
		args = args[2:]
	}
	if len(args) == 1 {
		fmt.Fprintf(&b, " !BADKEY=%v", args[0])
	}
	return strings.ToValidUTF8(b.String(), "\ufffd")
}
