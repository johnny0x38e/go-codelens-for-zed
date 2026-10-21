package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

const jsonRPCVersion = "2.0"

type responseStream struct {
	mu     sync.Mutex
	out    io.Writer
	trace  io.Writer
	err    error
	closed bool
}

type activeRequest struct {
	cancel context.CancelFunc
}

type activeRequests struct {
	mu       sync.Mutex
	requests map[string]*activeRequest
}

func traceMethod(trace io.Writer, direction, method string) {
	if trace == nil {
		return
	}
	_, _ = fmt.Fprintf(trace, "%s %s\n", direction, method) //nolint:errcheck // Diagnostic tracing is best effort.
}

func (s *responseStream) received(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	traceMethod(s.trace, "-->", method)
}

func (s *responseStream) write(method string, message any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.err != nil {
		return
	}
	if err := writeFrame(s.out, message); err != nil {
		s.err = err
		return
	}
	traceMethod(s.trace, "<--", method)
}

func (s *responseStream) operationError(kind, method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.trace == nil {
		return
	}
	//nolint:errcheck // Diagnostic tracing is best effort.
	_, _ = fmt.Fprintf(s.trace, "%s %s error: %v\n", kind, method, err)
}

func (s *responseStream) firstError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *responseStream) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (r *activeRequests) start(parent context.Context, id json.RawMessage) (context.Context, *activeRequest) {
	ctx, cancel := context.WithCancel(parent)
	active := &activeRequest{cancel: cancel}
	key := string(id)

	r.mu.Lock()
	if previous := r.requests[key]; previous != nil {
		previous.cancel()
	}
	r.requests[key] = active
	r.mu.Unlock()

	return ctx, active
}

func (r *activeRequests) finish(id json.RawMessage, active *activeRequest) {
	key := string(id)
	r.mu.Lock()
	if r.requests[key] == active {
		delete(r.requests, key)
	}
	r.mu.Unlock()
	active.cancel()
}

func (r *activeRequests) cancel(params json.RawMessage) {
	var input cancelRequestParams
	if json.Unmarshal(params, &input) != nil || len(input.ID) == 0 {
		return
	}

	r.mu.Lock()
	active := r.requests[string(input.ID)]
	r.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

func (r *activeRequests) cancelAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, active := range r.requests {
		active.cancel()
	}
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}

		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		contentLength, err = strconv.Atoi(strings.TrimSpace(value))
		if err != nil || contentLength < 0 {
			return nil, fmt.Errorf("invalid Content-Length header %q", value)
		}
	}
	if contentLength < 0 {
		return nil, errors.New("missing Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("read JSON-RPC body: %w", err)
	}

	return body, nil
}

func writeFrame(out io.Writer, message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode JSON-RPC response: %w", err)
	}
	if _, err := fmt.Fprintf(out, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return fmt.Errorf("write JSON-RPC header: %w", err)
	}
	if _, err := out.Write(body); err != nil {
		return fmt.Errorf("write JSON-RPC body: %w", err)
	}

	return nil
}
