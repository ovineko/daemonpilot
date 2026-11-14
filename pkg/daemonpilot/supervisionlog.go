package daemonpilot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const defaultSupervisionLogMaxBytes = 8 << 20

// SupervisionLogWriter tees Windows service logs to stderr and a rotating file.
// Elsewhere, or if the file cannot be opened, it returns os.Stderr. A nonpositive
// maxBytes uses 8 MiB per generation. The parent directory must exist. Use once
// in the watchdog's run mode; the returned writer lives for the process lifetime.
func SupervisionLogWriter(path string, maxBytes int64) io.Writer {
	if !supervisionFileNeeded() {
		return os.Stderr
	}
	file, err := newRotatingLog(path, maxBytes)
	if err != nil {
		return os.Stderr
	}
	return supervisionMirror{file: file, stderr: os.Stderr}
}

type supervisionMirror struct {
	file   io.Writer
	stderr io.Writer
}

func (m supervisionMirror) Write(p []byte) (int, error) {
	_, _ = m.file.Write(p)
	_, _ = m.stderr.Write(p)
	return len(p), nil
}

type rotatingLog struct {
	mu             sync.Mutex
	path           string
	maxBytes, size int64
	file           *os.File
}

func newRotatingLog(path string, maxBytes int64) (*rotatingLog, error) {
	if maxBytes <= 0 {
		maxBytes = defaultSupervisionLogMaxBytes
	}
	r := &rotatingLog{path: path, maxBytes: maxBytes}
	if err := r.open(); err != nil {
		return nil, err
	}
	if r.size >= r.maxBytes {
		if err := r.rotate(); err != nil {
			_ = r.close()
			return nil, err
		}
	}
	return r, nil
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	written := 0
	for len(p) > 0 {
		if r.file == nil {
			return written, errors.New("supervision log unavailable")
		}
		if r.size >= r.maxBytes {
			if err := r.rotate(); err != nil {
				return written, err
			}
		}
		count := min(int64(len(p)), r.maxBytes-r.size)
		n, err := r.file.Write(p[:count])
		written += n
		r.size += int64(n)
		p = p[n:]
		if err != nil {
			_ = r.close()
			return written, fmt.Errorf("write supervision log: %w", err)
		}
		if n == 0 {
			_ = r.close()
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (r *rotatingLog) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open supervision log: %w", err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("stat supervision log: %w", err)
		}
		return errors.New("supervision log must be a regular file")
	}
	r.file, r.size = f, info.Size()
	return nil
}

func (r *rotatingLog) rotate() error {
	if err := r.close(); err != nil {
		return err
	}
	if err := os.Remove(r.path + ".1"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove previous supervision log: %w", err)
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		return fmt.Errorf("rotate supervision log: %w", err)
	}
	return r.open()
}

func (r *rotatingLog) close() error {
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	if err != nil {
		return fmt.Errorf("close supervision log: %w", err)
	}
	return nil
}
