package ipc

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// DataProvider returns the current dynamic runtime data. Called by the server
// each time the worker requests a snapshot.
type DataProvider func() DynamicInfo

// ReadyHandler is called when the worker sends a readiness signal.
type ReadyHandler func()

// LogProvider acknowledges completed lines and returns the next retained batch.
type LogProvider func(finishedThrough uint64) LogBatch

// Server listens for single-byte requests on the request pipe and writes
// JSON-encoded DynamicInfo responses on the response pipe. It runs in the
// watchdog process.
type Server struct {
	requestR     *os.File
	responseW    *os.File
	provider     DataProvider
	readyHandler ReadyHandler
	logProvider  LogProvider
	done         chan struct{}
	closeOnce    sync.Once
}

// PipeFiles holds the file descriptors that must be passed to the worker
// process via exec.Cmd.ExtraFiles.
type PipeFiles struct {
	RequestW  *os.File
	ResponseR *os.File
}

// NewServer creates an IPC server and returns the pipe ends that should be
// passed to the worker process. The caller must pass pipeFiles.RequestW and
// pipeFiles.ResponseR to the child via exec.Cmd.ExtraFiles and close them in
// the parent after Start.
func NewServer(provider DataProvider) (*Server, *PipeFiles, error) {
	requestR, requestW, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("creating request pipe: %w", err)
	}

	responseR, responseW, err := os.Pipe()
	if err != nil {
		_ = requestR.Close()
		_ = requestW.Close()
		return nil, nil, fmt.Errorf("creating response pipe: %w", err)
	}

	s := &Server{
		requestR:     requestR,
		responseW:    responseW,
		provider:     provider,
		readyHandler: func() {},
		done:         make(chan struct{}),
	}

	pf := &PipeFiles{
		RequestW:  requestW,
		ResponseR: responseR,
	}

	return s, pf, nil
}

// SetLogProvider sets the callback used for log collection requests. It must
// be called before Serve.
func (s *Server) SetLogProvider(provider LogProvider) {
	s.logProvider = provider
}

// SetReadyHandler sets the callback invoked when the worker sends a readiness
// signal via IPC. Must be called before Serve.
func (s *Server) SetReadyHandler(h ReadyHandler) {
	if h != nil {
		s.readyHandler = h
	}
}

// Serve processes IPC requests until the request pipe closes or a response write fails.
// Context cancellation closes the response side; Windows waits for the synchronous
// request read to complete before closing its reader.
func (s *Server) Serve(ctx context.Context) {
	defer func() {
		_ = s.requestR.Close()
		close(s.done)
	}()

	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.done:
		}
	}()

	reader := bufio.NewReader(s.requestR)

	for {
		msgType, cursor, err := readRequest(reader)
		if err != nil {
			return
		}

		switch msgType {
		case MsgTypeReady:
			s.readyHandler()
		case MsgTypeRequest:
			info := s.provider()
			if !s.writeResponse(info) {
				return
			}
		case MsgTypeLogs:
			batch := LogBatch{}
			if s.logProvider != nil {
				batch = s.logProvider(cursor)
			}
			if !s.writeResponse(batch) {
				return
			}
		}
	}
}

// Close shuts down the server. On Windows, Serve closes the request reader
// after its synchronous pipe read completes to avoid blocking this caller.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		_ = s.responseW.Close()
		closeRequestReader(s.requestR)
	})
}

// Done returns a channel that is closed when Serve exits.
func (s *Server) Done() <-chan struct{} {
	return s.done
}

func (s *Server) writeResponse(value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return true
	}
	data = append(data, '\n')
	_, err = s.responseW.Write(data)
	return err == nil
}

func readRequest(reader io.Reader) (byte, uint64, error) {
	var kind [1]byte
	if _, err := io.ReadFull(reader, kind[:]); err != nil {
		return 0, 0, fmt.Errorf("read IPC message type: %w", err)
	}
	switch kind[0] {
	case MsgTypeRequest, MsgTypeReady:
		return kind[0], 0, nil
	case MsgTypeLogs:
		var cursor [8]byte
		if _, err := io.ReadFull(reader, cursor[:]); err != nil {
			return 0, 0, fmt.Errorf("read IPC log cursor: %w", err)
		}
		return kind[0], binary.BigEndian.Uint64(cursor[:]), nil
	default:
		return 0, 0, fmt.Errorf("invalid IPC message type: %d", kind[0])
	}
}
