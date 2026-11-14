package watchdog

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ovineko/daemonpilot/internal/ipc"
)

func FuzzLogRingEncoding(f *testing.F) {
	f.Fuzz(func(t *testing.T, line string, capacity uint16, cursor uint64) {
		if len(line) > 1<<16 {
			t.Skip("bounded log payload")
		}
		ring := newLogRing(int(capacity))
		encoded := formatLogLine("INFO", line, []any{"payload", line})
		ring.append(encoded)
		first := ring.collect(0)
		if !reflect.DeepEqual(first, ring.collect(0)) {
			t.Fatal("read acknowledged logs")
		}
		batch := ring.collect(cursor)
		raw, err := json.Marshal(batch)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ipc.LogBatch
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(batch, decoded) {
			t.Fatal("log JSON changed values")
		}
		if ring.bytes > int(capacity) {
			t.Fatal("ring exceeded cap")
		}
		for _, entry := range batch.Entries {
			if entry.Sequence <= cursor {
				t.Fatal("acknowledged entry returned")
			}
		}
	})
}
