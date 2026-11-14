// Package ipc carries runtime state from the watchdog to its worker over
// inherited anonymous pipes.
package ipc

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Client reads dynamic runtime data from the watchdog via IPC pipes.
// It runs in the worker process.
type Client struct {
	requestW  *os.File
	responseR *os.File
	scanner   *bufio.Scanner
	mu        sync.Mutex
}

// NewClient creates a client from explicit file handles.
func NewClient(requestW, responseR *os.File) *Client {
	return &Client{
		requestW:  requestW,
		responseR: responseR,
		scanner:   bufio.NewScanner(responseR),
	}
}

// NewClientFromEnv creates a client using file descriptors specified in the
// DAEMONPILOT_IPC_FD environment variable. The format is "requestFD,responseFD".
func NewClientFromEnv() (*Client, error) {
	val := os.Getenv(EnvIPCFD)
	if val == "" {
		return nil, fmt.Errorf("environment variable %s not set", EnvIPCFD)
	}

	parts := strings.SplitN(val, ",", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid %s format: expected \"requestFD,responseFD\", got %q", EnvIPCFD, val)
	}

	reqFD, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid request FD in %s: %w", EnvIPCFD, err)
	}

	respFD, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid response FD in %s: %w", EnvIPCFD, err)
	}

	requestW := os.NewFile(uintptr(reqFD), "ipc-request-w")
	if requestW == nil {
		return nil, fmt.Errorf("failed to open request FD %d", reqFD)
	}

	responseR := os.NewFile(uintptr(respFD), "ipc-response-r")
	if responseR == nil {
		_ = requestW.Close()
		return nil, fmt.Errorf("failed to open response FD %d", respFD)
	}

	return NewClient(requestW, responseR), nil
}

// Request sends a request to the watchdog and returns the current dynamic
// runtime data. It is safe for concurrent use.
func (c *Client) Request() (*DynamicInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := c.requestW.Write([]byte{MsgTypeRequest}); err != nil {
		return nil, fmt.Errorf("ipc request write: %w", err)
	}

	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("ipc response read: %w", err)
		}
		return nil, errors.New("ipc response: unexpected EOF")
	}

	var info *DynamicInfo
	if err := json.Unmarshal(c.scanner.Bytes(), &info); err != nil {
		return nil, fmt.Errorf("ipc response unmarshal: %w", err)
	}

	if info == nil {
		return nil, errors.New("ipc response: expected an object")
	}
	return info, nil
}

// NotifyReady sends a readiness signal to the watchdog. It is safe for
// concurrent use.
func (c *Client) NotifyReady() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := c.requestW.Write([]byte{MsgTypeReady}); err != nil {
		return fmt.Errorf("ipc ready write: %w", err)
	}
	return nil
}

// CollectLogs acknowledges every log line through finishedThrough and returns
// the next retained batch. Repeating a request with the same cursor returns
// the same unacknowledged lines.
func (c *Client) CollectLogs(finishedThrough uint64) (*LogBatch, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	request := make([]byte, 9)
	request[0] = MsgTypeLogs
	binary.BigEndian.PutUint64(request[1:], finishedThrough)
	if _, err := c.requestW.Write(request); err != nil {
		return nil, fmt.Errorf("ipc log request write: %w", err)
	}

	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("ipc log response read: %w", err)
		}
		return nil, errors.New("ipc log response: unexpected EOF")
	}

	var batch *LogBatch
	if err := json.Unmarshal(c.scanner.Bytes(), &batch); err != nil {
		return nil, fmt.Errorf("ipc log response unmarshal: %w", err)
	}
	if batch == nil {
		return nil, errors.New("ipc log response: expected an object")
	}
	return batch, nil
}

// Close releases the client's pipe handles.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error
	if err := c.requestW.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := c.responseR.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("ipc client close: %v", errs)
	}
	return nil
}
