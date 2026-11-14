package daemonpilot

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/daemonpilot/internal/testsupport"
)

type stalledLogWriter struct {
	entered  chan struct{}
	release  chan struct{}
	received chan []byte
	once     sync.Once
}

func (w *stalledLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	w.received <- append([]byte(nil), p...)
	return len(p), nil
}

func TestNonBlockingWriter(t *testing.T) {
	for _, buffer := range []int{1, 0, -1} {
		t.Run(strconv.Itoa(buffer), func(t *testing.T) {
			sink := &stalledLogWriter{entered: make(chan struct{}), release: make(chan struct{}), received: make(chan []byte, 2048)}
			defer close(sink.release)
			w := NewNonBlockingWriter(sink, buffer)
			p := []byte("original")
			if n, err := w.Write(p); err != nil || n != len(p) {
				t.Fatalf("Write: %d %v", n, err)
			}
			p[0] = 'X'
			select {
			case <-sink.entered:
			case <-time.After(testsupport.Scale(time.Second)):
				t.Fatal("drain never started")
			}
			capacity := buffer
			if capacity <= 0 {
				capacity = 1024
			}
			done := make(chan struct{})
			go func() {
				for range capacity + 10 {
					_, _ = w.Write([]byte("line"))
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(testsupport.Scale(time.Second)):
				t.Fatal("Write blocked")
			}
			if w.Dropped() != 10 {
				t.Fatalf("Dropped = %d", w.Dropped())
			}
			// Release the sink separately so the copied first write can be inspected.
			select {
			case sink.release <- struct{}{}:
			case <-time.After(testsupport.Scale(time.Second)):
				t.Fatal("sink did not receive release")
			}
			select {
			case got := <-sink.received:
				if string(got) != "original" {
					t.Fatalf("buffer reused: %q", got)
				}
			case <-time.After(testsupport.Scale(time.Second)):
				t.Fatal("no data")
			}
		})
	}
}

func TestRotatingLog(t *testing.T) {
	for _, tc := range []struct{ name, initial, write, current, previous string }{
		{"append", "ab", "cd", "abcd", ""},
		{"rotate at open", "abcde", "f", "f", "abcde"},
		{"cross cap", "abc", "def", "f", "abcde"},
		{"multiple generations", "", "abcdefghijkl", "kl", "fghij"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "supervision.log")
			if err := os.WriteFile(path, []byte(tc.initial), 0o600); err != nil {
				t.Fatal(err)
			}
			w, err := newRotatingLog(path, 5)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.close() }()
			if n, err := w.Write([]byte(tc.write)); err != nil || n != len(tc.write) {
				t.Fatalf("Write: %d %v", n, err)
			}

			data, err := os.ReadFile(path)
			if err != nil || string(data) != tc.current {
				t.Fatalf("current %q: %v", data, err)
			}

			data, err = os.ReadFile(path + ".1")
			if tc.previous == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("unexpected previous: %q %v", data, err)
				}
			} else if err != nil || string(data) != tc.previous {
				t.Fatalf("previous %q: %v", data, err)
			}
		})
	}
}

func TestSupervisionMirrorKeepsFileWhenStderrFails(t *testing.T) {
	var file bytes.Buffer
	m := supervisionMirror{file: &file, stderr: failingLogWriter{}}
	if _, err := m.Write([]byte("diagnostic")); err != nil || file.String() != "diagnostic" {
		t.Fatalf("%q: %v", file.String(), err)
	}
}

type failingLogWriter struct{}

func (failingLogWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestSupervisionLogFallback(t *testing.T) {
	if got := SupervisionLogWriter(filepath.Join(t.TempDir(), "missing", "log"), 5); got != os.Stderr {
		t.Fatal("expected stderr")
	}
}

func TestRotatingLogFailureAndConcurrency(t *testing.T) {
	t.Run("failed rotation remains bounded", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "log")
		if err := os.Mkdir(path+".1", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path+".1", "keep"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := newRotatingLog(path, 4)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = w.close() }()
		if n, err := w.Write([]byte("12345678")); err == nil || n != 4 {
			t.Fatalf("write=%d %v", n, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() != 4 {
			t.Fatalf("log grew after failed rotation: %v %v", info, err)
		}
		if _, err := newRotatingLog(path, 4); err == nil {
			t.Fatal("open did not report rotation failure")
		}
	})
	t.Run("concurrent writes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "log")
		w, err := newRotatingLog(path, 101)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = w.close() }()
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				for range 10 {
					if _, err := w.Write(bytes.Repeat([]byte("x"), 20)); err != nil {
						t.Error(err)
					}
				}
			})
		}
		wg.Wait()
		for _, name := range []string{path, path + ".1"} {
			info, err := os.Stat(name)
			if err != nil || info.Size() > 101 {
				t.Fatalf("unbounded log: %v %v", info, err)
			}
		}
	})
}
