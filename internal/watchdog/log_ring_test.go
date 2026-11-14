package watchdog

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/ipc"
	"github.com/ovineko/daemonpilot/pkg/daemonpilot"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

func TestLogRingEvictionSequenceAndDrops(t *testing.T) {
	ring := newLogRing(6)
	ring.append("aa")
	ring.append("bbb")
	ring.append("cc")

	batch := ring.collect(0)
	if batch.Dropped != 1 {
		t.Fatalf("Dropped = %d, want 1", batch.Dropped)
	}
	if len(batch.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(batch.Entries))
	}
	if batch.Entries[0].Sequence != 2 || batch.Entries[1].Sequence != 3 {
		t.Fatalf("sequences = %d,%d, want 2,3", batch.Entries[0].Sequence, batch.Entries[1].Sequence)
	}
}

func TestLogRingCursorKeepsUnacknowledgedLines(t *testing.T) {
	ring := newLogRing(100)
	for _, line := range []string{"one", "two", "three"} {
		ring.append(line)
	}

	first := ring.collect(1)
	second := ring.collect(1)
	if len(first.Entries) != 2 || len(second.Entries) != 2 {
		t.Fatalf("entry counts = %d,%d, want 2,2", len(first.Entries), len(second.Entries))
	}
	if first.Entries[0].Sequence != 2 || second.Entries[0].Sequence != 2 {
		t.Fatalf("first sequences = %d,%d, want 2,2", first.Entries[0].Sequence, second.Entries[0].Sequence)
	}
}

func TestLogRingAppendWithoutCollectorDoesNotBlock(t *testing.T) {
	ring := newLogRing(64)
	done := make(chan struct{})
	go func() {
		for range 10000 {
			ring.append("a log line")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testsupport.Scale(time.Second)):
		t.Fatal("append blocked without a collector")
	}
	if ring.collect(0).Dropped == 0 {
		t.Fatal("full ring did not report evictions")
	}
}

func TestWatchdogLogCaptureOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := &daemonpilot.Config{
			Name:                   "test",
			Logger:                 daemonpilot.NoopLogger(),
			CollectWatchdogLogs:    enabled,
			WatchdogLogBufferBytes: 1024,
		}
		w := New(cfg)
		w.logger.Info("visible only when retained")
		if enabled && len(w.logRing.collect(0).Entries) != 1 {
			t.Fatal("enabled capture did not retain the log")
		}
		if !enabled && w.logRing != nil {
			t.Fatal("disabled capture allocated a ring")
		}
	}
}

func TestLogRingAppendDoesNotDependOnBlockedIPCWriter(t *testing.T) {
	ring := newLogRing(1024)
	server, pipes, err := ipc.NewServer(func() ipc.DynamicInfo { return ipc.DynamicInfo{} })
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()
	defer func() {
		if err := pipes.RequestW.Close(); err != nil {
			t.Errorf("close RequestW: %v", err)
		}
	}()
	defer func() {
		if err := pipes.ResponseR.Close(); err != nil {
			t.Errorf("close ResponseR: %v", err)
		}
	}()
	server.SetLogProvider(func(uint64) ipc.LogBatch {
		entries := make([]ipc.LogEntry, 10000)
		for i := range entries {
			entries[i] = ipc.LogEntry{Sequence: uint64(i + 1), Line: "a line large enough to fill the pipe before the client reads it"}
		}
		return ipc.LogBatch{Entries: entries}
	})
	go server.Serve(t.Context())
	request := make([]byte, 9)
	request[0] = ipc.MsgTypeLogs
	binary.BigEndian.PutUint64(request[1:], 0)
	if _, err := pipes.RequestW.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		ring.append("still independent")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testsupport.Scale(time.Second)):
		t.Fatal("append depended on an unresponsive client")
	}
}

func TestLogRingSurvivesWorkerRestart(t *testing.T) {
	ring := newLogRing(1024)
	ring.append("before first worker")
	first := ring.collect(0)
	if len(first.Entries) != 1 {
		t.Fatalf("first worker received %d entries, want 1", len(first.Entries))
	}
	ring.collect(first.Entries[0].Sequence)
	ring.append("between workers")

	second := ring.collect(0)
	if len(second.Entries) != 1 || second.Entries[0].Sequence != 2 {
		t.Fatalf("next worker batch = %#v, want sequence 2 only", second)
	}
}
