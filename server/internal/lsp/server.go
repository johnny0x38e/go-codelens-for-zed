package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/johnny/go-implementation-lens-for-zed/server/internal/analysis"
)

const jsonRPCVersion = "2.0"

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *responseError  `json:"error,omitempty"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type serverState struct {
	workspaceRoots []string
	documents      *documentStore
	cacheMu        sync.RWMutex
	snapshots      map[string]cachedSnapshot
}

type cachedSnapshot struct {
	generation uint64
	snapshot   analysis.Snapshot
}

type codeLens struct {
	Range   analysis.Range `json:"range"`
	Command command        `json:"command"`
}

type command struct {
	Title     string `json:"title"`
	Command   string `json:"command"`
	Arguments []any  `json:"arguments,omitempty"`
}

// Serve handles Language Server Protocol messages until the client sends exit
// or the input stream reaches EOF.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return ServeWithTrace(ctx, in, out, io.Discard)
}

// ServeWithTrace is Serve with optional request/response method tracing. The
// trace stream is diagnostic only: write failures never interrupt LSP traffic.
func ServeWithTrace(ctx context.Context, in io.Reader, out io.Writer, trace io.Writer) error {
	reader := bufio.NewReader(in)
	state := newServerState()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		body, err := readFrame(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		var message request
		if err := json.Unmarshal(body, &message); err != nil {
			return fmt.Errorf("decode JSON-RPC message: %w", err)
		}
		traceMethod(trace, "-->", message.Method)
		if message.Method == "exit" {
			return nil
		}
		if len(message.ID) == 0 {
			state.handleNotification(message.Method, message.Params)
			continue
		}

		var reply response
		switch message.Method {
		case "initialize":
			state.initialize(message.Params)
			reply = response{
				JSONRPC: jsonRPCVersion,
				ID:      message.ID,
				Result: map[string]any{
					"capabilities": map[string]any{
						"textDocumentSync": 1,
						"codeLensProvider": map[string]any{
							"resolveProvider": false,
						},
					},
					"serverInfo": map[string]any{
						"name":    "go-implementation-lens-ls",
						"version": "0.1.0",
					},
				},
			}
		case "textDocument/codeLens":
			result, err := state.semanticCodeLenses(ctx, message.Params)
			if err != nil {
				reply = response{
					JSONRPC: jsonRPCVersion,
					ID:      message.ID,
					Error: &responseError{
						Code:    -32602,
						Message: err.Error(),
					},
				}
			} else {
				reply = response{
					JSONRPC: jsonRPCVersion,
					ID:      message.ID,
					Result:  result,
				}
			}
		case "shutdown":
			reply = response{
				JSONRPC: jsonRPCVersion,
				ID:      message.ID,
				Result:  json.RawMessage("null"),
			}
		default:
			reply = response{
				JSONRPC: jsonRPCVersion,
				ID:      message.ID,
				Error: &responseError{
					Code:    -32601,
					Message: "method not found: " + message.Method,
				},
			}
		}

		if err := writeFrame(out, reply); err != nil {
			return err
		}
		traceMethod(trace, "<--", message.Method)
	}
}

func newServerState() *serverState {
	return &serverState{
		documents: newDocumentStore(),
		snapshots: make(map[string]cachedSnapshot),
	}
}

func traceMethod(trace io.Writer, direction, method string) {
	if trace == nil {
		return
	}
	_, _ = fmt.Fprintf(trace, "%s %s\n", direction, method)
}

func (s *serverState) initialize(params json.RawMessage) {
	var input struct {
		RootURI          string `json:"rootUri"`
		WorkspaceFolders []struct {
			URI string `json:"uri"`
		} `json:"workspaceFolders"`
	}
	if err := json.Unmarshal(params, &input); err != nil {
		return
	}

	seen := make(map[string]struct{})
	addRoot := func(rawURI string) {
		path, err := analysis.URIToPath(rawURI)
		if err != nil {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		s.workspaceRoots = append(s.workspaceRoots, path)
	}
	for _, folder := range input.WorkspaceFolders {
		addRoot(folder.URI)
	}
	addRoot(input.RootURI)
}

func (s *serverState) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "textDocument/didOpen":
		var input struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(params, &input) == nil {
			s.documents.Open(input.TextDocument.URI, input.TextDocument.Text)
		}
	case "textDocument/didChange":
		var input struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if json.Unmarshal(params, &input) == nil && len(input.ContentChanges) > 0 {
			s.documents.Change(input.TextDocument.URI, input.ContentChanges[len(input.ContentChanges)-1].Text)
		}
	case "textDocument/didClose":
		var input struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(params, &input) == nil {
			s.documents.Close(input.TextDocument.URI)
		}
	case "textDocument/didSave", "workspace/didChangeWatchedFiles":
		s.documents.Invalidate()
	}
}

func (s *serverState) semanticCodeLenses(ctx context.Context, params json.RawMessage) ([]codeLens, error) {
	var input struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(params, &input); err != nil {
		return nil, fmt.Errorf("invalid textDocument/codeLens params: %w", err)
	}
	if input.TextDocument.URI == "" {
		return nil, errors.New("textDocument/codeLens params must include textDocument.uri")
	}

	filename, err := analysis.URIToPath(input.TextDocument.URI)
	if err != nil {
		return nil, fmt.Errorf("invalid textDocument/codeLens document URI: %w", err)
	}
	moduleDir, err := analysis.FindModuleRoot(filename, s.workspaceRootFor(filename))
	if errors.Is(err, analysis.ErrNoModule) {
		return []codeLens{}, nil
	}
	if err != nil {
		return []codeLens{}, nil
	}
	snapshot, err := s.snapshotForModule(ctx, moduleDir)
	if err != nil {
		return []codeLens{}, nil
	}

	canonicalURI, err := analysis.PathToURI(filename)
	if err != nil {
		return []codeLens{}, nil
	}
	semanticLenses := snapshot.Lenses(canonicalURI)
	result := make([]codeLens, 0, len(semanticLenses))
	for _, semanticLens := range semanticLenses {
		lensCommand := command{
			Title:   semanticLens.Title,
			Command: "editor.action.showReferences",
			Arguments: []any{
				canonicalURI,
				semanticLens.Range.Start,
				semanticLens.Targets,
			},
		}
		result = append(result, codeLens{
			Range:   semanticLens.Range,
			Command: lensCommand,
		})
	}
	return result, nil
}

func (s *serverState) snapshotForModule(ctx context.Context, moduleDir string) (analysis.Snapshot, error) {
	generation := s.documents.Generation()
	s.cacheMu.RLock()
	cached, ok := s.snapshots[moduleDir]
	s.cacheMu.RUnlock()
	if ok && cached.generation == generation {
		return cached.snapshot, nil
	}

	snapshot, err := analysis.AnalyzeModule(ctx, moduleDir, s.documents.Overlay(moduleDir))
	if err != nil {
		return analysis.Snapshot{}, err
	}
	if s.documents.Generation() == generation {
		s.cacheMu.Lock()
		s.snapshots[moduleDir] = cachedSnapshot{generation: generation, snapshot: snapshot}
		s.cacheMu.Unlock()
	}
	return snapshot, nil
}

func (s *serverState) workspaceRootFor(filename string) string {
	var best string
	for _, root := range s.workspaceRoots {
		relative, err := filepath.Rel(root, filename)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if len(root) > len(best) {
			best = root
		}
	}
	return best
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
