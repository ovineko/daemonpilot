package daemonpilot

import (
	"context"
	"testing"

	"github.com/ovineko/daemonpilot/internal/ipc"
)

func TestCollectWatchdogLogs(t *testing.T) {
	server, pipes, err := ipc.NewServer(func() ipc.DynamicInfo { return ipc.DynamicInfo{} })
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()
	server.SetLogProvider(func(cursor uint64) ipc.LogBatch {
		return ipc.LogBatch{Entries: []ipc.LogEntry{{Sequence: cursor + 1, Line: "watchdog line"}}, Dropped: 2}
	})
	go server.Serve(context.Background())

	resetIPCClient()
	t.Cleanup(resetIPCClient)
	ipcClientFactory = func() (*ipc.Client, error) {
		return ipc.NewClient(pipes.RequestW, pipes.ResponseR), nil
	}
	t.Cleanup(func() { ipcClientFactory = ipc.NewClientFromEnv })

	batch, err := CollectWatchdogLogs(6)
	if err != nil {
		t.Fatalf("CollectWatchdogLogs: %v", err)
	}
	if batch.Dropped != 2 || len(batch.Entries) != 1 || batch.Entries[0].Sequence != 7 {
		t.Fatalf("batch = %#v", batch)
	}
}
