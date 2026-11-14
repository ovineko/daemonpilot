package ipc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func FuzzIPCDecode(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte, logs bool) {
		if len(data) > 1<<17 {
			t.Skip("bounded IPC response")
		}
		request, err := os.CreateTemp(t.TempDir(), "request")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = request.Close() }()
		client := &Client{requestW: request, scanner: bufio.NewScanner(bytes.NewReader(data))}
		line, _, _ := bytes.Cut(data, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if logs {
			got, err := client.CollectLogs(0)
			var want *LogBatch
			valid := json.Unmarshal(line, &want) == nil && want != nil && len(line) < bufio.MaxScanTokenSize
			if valid != (err == nil) {
				t.Fatalf("log response acceptance: %q: %v", data, err)
			}
			if err == nil && !reflect.DeepEqual(got, want) {
				t.Fatal("log response changed")
			}
		} else {
			got, err := client.Request()
			var want *DynamicInfo
			valid := json.Unmarshal(line, &want) == nil && want != nil && len(line) < bufio.MaxScanTokenSize
			if valid != (err == nil) {
				t.Fatalf("response acceptance: %q: %v", data, err)
			}
			if err == nil && !reflect.DeepEqual(got, want) {
				t.Fatal("response changed")
			}
		}
	})
}

func FuzzIPCProtocol(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		reader := bytes.NewReader(data)
		kind, cursor, err := readRequest(reader)
		valid := len(data) > 0 && (data[0] == MsgTypeRequest || data[0] == MsgTypeReady || data[0] == MsgTypeLogs && len(data) >= 9)
		if valid != (err == nil) {
			t.Fatalf("invalid frame acceptance %x: %v", data, err)
		}
		if !valid {
			return
		}
		consumed := 1
		if kind == MsgTypeLogs {
			consumed = 9
			if cursor != binary.BigEndian.Uint64(data[1:9]) {
				t.Fatal("cursor changed")
			}
		}
		if kind != data[0] || reader.Len() != len(data)-consumed {
			t.Fatal("frame boundaries changed")
		}
	})
}
