package ipc

import (
	"testing"
)

func TestCollectLogsWireRoundTripAndCursor(t *testing.T) {
	server, pipes, err := NewServer(func() DynamicInfo { return DynamicInfo{} })
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

	var cursors []uint64
	server.SetLogProvider(func(cursor uint64) LogBatch {
		cursors = append(cursors, cursor)
		return LogBatch{Entries: []LogEntry{{Sequence: cursor + 1, Line: "line"}}, Dropped: 4}
	})
	ctx := t.Context()
	go server.Serve(ctx)
	client := NewClient(pipes.RequestW, pipes.ResponseR)

	for range 2 {
		batch, err := client.CollectLogs(7)
		if err != nil {
			t.Fatalf("CollectLogs: %v", err)
		}
		if batch.Dropped != 4 || len(batch.Entries) != 1 || batch.Entries[0].Sequence != 8 {
			t.Fatalf("batch = %#v", batch)
		}
	}
	if len(cursors) != 2 || cursors[0] != 7 || cursors[1] != 7 {
		t.Fatalf("cursors = %v, want [7 7]", cursors)
	}
}
