// Package lsp implements the JSON-RPC language server that exposes Go semantic
// relationships as Code Lens results to Zed.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

const textDocumentSyncIncremental = 2

// serverVersion is supplied by the build scripts through -ldflags -X. Direct
// go build and go run invocations report dev rather than a release version.
var serverVersion = "dev"

// serverState separates metadata and snapshot locks so path/scope discovery
// cannot block completed CodeLens cache hits. Both bounded caches use simple
// insertion-order rings; hit promotion would add lock traffic to the hot path.
type serverState struct {
	workspaceRoots           []string
	documents                *documentStore
	metadataMu               sync.RWMutex
	analysisScopes           map[analysisScopeCacheKey]cachedAnalysisScope
	analysisScopeKeys        []analysisScopeCacheKey
	analysisScopeCursor      int
	documentPaths            map[string]documentPath
	cacheMu                  sync.RWMutex
	snapshots                map[string]cachedSnapshot
	snapshotKeys             []string
	snapshotCursor           int
	analysisFlights          map[string]*analysisFlight
	analysisContext          context.Context
	findScope                func(string, string) (analysis.AnalysisScope, error)
	engine                   *analysis.Engine
	forgetScope              func(string)
	analyzeScope             func(context.Context, analysis.AnalysisScope, analysis.ChangeSet, map[string][]byte) (analysis.Snapshot, error)
	watchedFilesSupported    bool
	watchedFilesRegistration watchedFilesRegistrationState
	codeLensRefreshSupported bool
	refreshMu                sync.Mutex
	refreshPending           bool
	refreshSequence          uint64
}

// Serve handles Language Server Protocol messages until the client sends exit
// or the input stream reaches EOF.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return ServeWithTrace(ctx, in, out, nil)
}

// ServeWithTrace is Serve with optional request/response method tracing. The
// trace stream is diagnostic only: nil disables tracing, and write failures
// never interrupt LSP traffic.
func ServeWithTrace(ctx context.Context, in io.Reader, out, trace io.Writer) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	return serveWithState(serveCtx, in, out, trace, newServerStateWithContext(serveCtx))
}

func serveWithState(ctx context.Context, in io.Reader, out, trace io.Writer, state *serverState) (serveErr error) {
	reader := bufio.NewReader(in)
	stream := &responseStream{out: out, trace: trace}
	requests := activeRequests{requests: make(map[string]*activeRequest)}
	var requestWorkers sync.WaitGroup
	defer func() {
		stream.close()
		requests.cancelAll()
		state.cancelAllAnalysisFlights()
		requestWorkers.Wait()
		if serveErr == nil {
			serveErr = stream.firstError()
		}
	}()

	for {
		if err := stream.firstError(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		body, err := readFrame(reader)
		if errors.Is(err, io.EOF) {
			requestWorkers.Wait()
			return nil
		}
		if err != nil {
			return err
		}

		var message request
		if err := json.Unmarshal(body, &message); err != nil {
			return fmt.Errorf("decode JSON-RPC message: %w", err)
		}
		// Method-less JSON-RPC messages are responses to requests initiated by the
		// server. Consume them here to prevent an erroneous method-not-found reply
		// and commit dynamic capability state only after the client acknowledges it.
		if message.Method == "" && len(message.ID) > 0 {
			var reply response
			if err := json.Unmarshal(body, &reply); err != nil {
				return fmt.Errorf("decode JSON-RPC response: %w", err)
			}
			if err := state.handleWatchedFilesRegistrationResponse(reply); err != nil {
				stream.operationError("request", registerCapabilityMethod, err)
			}
			continue
		}
		stream.received(message.Method)
		if message.Method == "exit" {
			requestWorkers.Wait()
			return nil
		}
		if len(message.ID) == 0 {
			if message.Method == "$/cancelRequest" {
				requests.cancel(message.Params)
			} else {
				if err := state.handleNotification(message.Method, message.Params); err != nil {
					stream.operationError("notification", message.Method, err)
				}
				if message.Method == "initialized" {
					if registrationMessage, ok := state.watchedFilesRegistrationRequest(); ok {
						stream.write(registerCapabilityMethod, registrationMessage)
					}
				}
				state.flushCodeLensRefresh(stream)
			}
			continue
		}
		if message.Method == "textDocument/codeLens" {
			requestCtx, active := requests.start(ctx, message.ID)
			requestWorkers.Go(func() {
				defer requests.finish(message.ID, active)
				stream.write(message.Method, codeLensResponse(requestCtx, state, message))
				state.flushCodeLensRefresh(stream)
			})
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
						"textDocumentSync": textDocumentSyncOptions{
							OpenClose: true,
							Change:    textDocumentSyncIncremental,
							Save:      textDocumentSaveOptions{IncludeText: false},
						},
						"codeLensProvider": map[string]any{
							"resolveProvider": false,
						},
					},
					"serverInfo": map[string]any{
						"name":    "go-codelens-ls",
						"version": serverVersion,
					},
				},
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

		stream.write(message.Method, reply)
		if err := stream.firstError(); err != nil {
			return err
		}
	}
}

func newServerState() *serverState {
	return newServerStateWithContext(context.Background())
}

func newServerStateWithContext(ctx context.Context) *serverState {
	engine := analysis.NewEngine()
	return &serverState{
		documents:       newDocumentStore(),
		analysisScopes:  make(map[analysisScopeCacheKey]cachedAnalysisScope),
		documentPaths:   make(map[string]documentPath),
		snapshots:       make(map[string]cachedSnapshot),
		analysisFlights: make(map[string]*analysisFlight),
		analysisContext: ctx,
		findScope:       analysis.FindAnalysisScope,
		engine:          engine,
		forgetScope:     engine.ForgetScope,
		analyzeScope:    engine.Analyze,
	}
}

func (s *serverState) initialize(params json.RawMessage) {
	var input initializeParams
	if err := json.Unmarshal(params, &input); err != nil {
		return
	}

	s.watchedFilesSupported = input.Capabilities.Workspace.DidChangeWatchedFiles.DynamicRegistration
	s.watchedFilesRegistration = watchedFilesUnregistered
	s.codeLensRefreshSupported = input.Capabilities.Workspace.CodeLens.RefreshSupport

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

func (s *serverState) handleNotification(method string, params json.RawMessage) error {
	switch method {
	case "textDocument/didOpen":
		var input didOpenTextDocumentParams
		if err := json.Unmarshal(params, &input); err != nil {
			return fmt.Errorf("decode textDocument/didOpen: %w", err)
		}
		path, changed, err := s.documents.Open(
			input.TextDocument.URI,
			input.TextDocument.Text,
			input.TextDocument.Version,
		)
		if err != nil {
			return err
		}
		s.invalidateDocumentChange(path, changed)
	case "textDocument/didChange":
		var input didChangeTextDocumentParams
		if err := json.Unmarshal(params, &input); err != nil {
			return fmt.Errorf("decode textDocument/didChange: %w", err)
		}
		path, changed, err := s.documents.Change(
			input.TextDocument.URI,
			input.TextDocument.Version,
			input.ContentChanges,
		)
		if err != nil {
			return err
		}
		s.invalidateDocumentChange(path, changed)
	case "textDocument/didClose":
		var input textDocumentParams
		if err := json.Unmarshal(params, &input); err != nil {
			return fmt.Errorf("decode textDocument/didClose: %w", err)
		}
		path, changed := s.documents.Close(input.TextDocument.URI)
		s.invalidateDocumentChange(path, changed)
		s.forgetDocumentPath(input.TextDocument.URI)
	case "textDocument/didSave":
		var input textDocumentParams
		if err := json.Unmarshal(params, &input); err != nil {
			return fmt.Errorf("decode textDocument/didSave: %w", err)
		}
		path, changed := s.documents.Save(input.TextDocument.URI)
		s.invalidateDocumentChange(path, changed)
	case watchedFilesMethod:
		s.handleWatchedFiles(params)
	}
	return nil
}

func (s *serverState) invalidateDocumentChange(path string, changed bool) {
	if !changed {
		return
	}
	kind := analysis.ChangeContent
	if isModuleMetadataPath(path) {
		kind = analysis.ChangeMetadata
	}
	s.invalidateSnapshotsForChange(analysis.Change{Path: path, Kind: kind})
	if kind == analysis.ChangeMetadata {
		s.invalidateAnalysisScopes()
	}
}
