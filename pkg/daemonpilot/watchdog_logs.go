package daemonpilot

import "fmt"

// WatchdogLogEntry is one line retained by the watchdog logger.
type WatchdogLogEntry struct {
	Sequence uint64 `json:"sequence"`
	Line     string `json:"line"`
}

// WatchdogLogBatch contains the retained lines following the acknowledged
// cursor. Dropped is the cumulative number evicted by the watchdog's byte cap.
type WatchdogLogBatch struct {
	Entries []WatchdogLogEntry `json:"entries"`
	Dropped uint64             `json:"dropped"`
}

// CollectWatchdogLogs tells the watchdog that the caller has finished with
// every line through finishedThrough and returns the next retained batch.
// Callers must advance the cursor only after they no longer need those lines.
func CollectWatchdogLogs(finishedThrough uint64) (*WatchdogLogBatch, error) {
	client, err := getOrCreateIPCClient()
	if err != nil {
		return nil, err
	}
	batch, err := client.CollectLogs(finishedThrough)
	if err != nil {
		return nil, fmt.Errorf("collect watchdog logs: %w", err)
	}
	result := &WatchdogLogBatch{Dropped: batch.Dropped}
	result.Entries = make([]WatchdogLogEntry, len(batch.Entries))
	for i, entry := range batch.Entries {
		result.Entries[i] = WatchdogLogEntry{Sequence: entry.Sequence, Line: entry.Line}
	}
	return result, nil
}
