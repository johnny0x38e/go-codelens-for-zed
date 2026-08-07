package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
)

const (
	jsonRPCVersion               = "2.0"
	serverVersion                = "0.3.2"
	requestCanceledCode          = -32800
	textDocumentSyncIncremental  = 2
	watchedFileCreated           = 1
	watchedFileChanged           = 2
	watchedFileDeleted           = 3
	maxAnalysisScopeCacheEntries = 4096
	maxSnapshotCacheEntries      = 32
	registerCapabilityMethod     = "client/registerCapability"
	watchedFilesMethod           = "workspace/didChangeWatchedFiles"
	watchedFilesRegistrationID   = "go-codelens-watched-files"
)

var errAnalysisObsolete = errors.New("analysis became obsolete")

// watchedFilesRegistrationState distinguishes a sent request from a capability
// that the client has acknowledged. The LSP client may reject a dynamic
// registration even after advertising support for the mechanism.
type watchedFilesRegistrationState uint8

const (
	watchedFilesUnregistered watchedFilesRegistrationState = iota
	watchedFilesRegistrationPending
	watchedFilesRegistered
)

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
}

// analysisFlight coalesces all requests for one scope revision. It belongs to
// the revision rather than the first caller, allowing waiters to cancel
// independently without discarding work another request still needs.
type analysisFlight struct {
	scope     analysis.AnalysisScope
	revision  uint64
	changes   analysis.ChangeSet
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	abandoned bool
	completed bool
	snapshot  analysis.Snapshot
	err       error
}

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

type analysisScopeCacheKey struct {
	directory     string
	workspaceRoot string
}

type cachedAnalysisScope struct {
	scope analysis.AnalysisScope
	err   error
}

type documentPath struct {
	filename string
	uri      string
}

// cachedSnapshot retains a lightweight revision tombstone even while invalid.
// The large immutable Snapshot is cleared on invalidation and pendingChanges
// records the exact work the incremental engine must consider next.
type cachedSnapshot struct {
	scope            analysis.AnalysisScope
	revision         uint64
	snapshotRevision uint64
	valid            bool
	snapshot         analysis.Snapshot
	pendingChanges   analysis.ChangeSet
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
					if registrationRequest, ok := state.watchedFilesRegistrationRequest(); ok {
						stream.write(registerCapabilityMethod, registrationRequest)
					}
				}
			}
			continue
		}
		if message.Method == "textDocument/codeLens" {
			requestCtx, active := requests.start(ctx, message.ID)
			requestWorkers.Add(1)
			go func(message request) {
				defer requestWorkers.Done()
				defer requests.finish(message.ID, active)
				stream.write(message.Method, codeLensResponse(requestCtx, state, message))
			}(message)
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

func codeLensResponse(ctx context.Context, state *serverState, message request) response {
	result, err := state.semanticCodeLenses(ctx, message.Params)
	if err == nil {
		return response{JSONRPC: jsonRPCVersion, ID: message.ID, Result: result}
	}

	code := -32602
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = requestCanceledCode
	}
	return response{
		JSONRPC: jsonRPCVersion,
		ID:      message.ID,
		Error: &responseError{
			Code:    code,
			Message: err.Error(),
		},
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
	_, _ = fmt.Fprintf(s.trace, "%s %s error: %v\n", kind, method, err) //nolint:errcheck // Diagnostic tracing is best effort.
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

func (s *serverState) initialize(params json.RawMessage) {
	var input initializeParams
	if err := json.Unmarshal(params, &input); err != nil {
		return
	}

	s.watchedFilesSupported = input.Capabilities.Workspace.DidChangeWatchedFiles.DynamicRegistration
	s.watchedFilesRegistration = watchedFilesUnregistered

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

func (s *serverState) watchedFilesRegistrationRequest() (registrationRequest, bool) {
	if !s.watchedFilesSupported || s.watchedFilesRegistration != watchedFilesUnregistered {
		return registrationRequest{}, false
	}
	s.watchedFilesRegistration = watchedFilesRegistrationPending

	return registrationRequest{
		JSONRPC: jsonRPCVersion,
		ID:      watchedFilesRegistrationID,
		Method:  registerCapabilityMethod,
		Params: registrationParams{Registrations: []registration{{
			ID:     watchedFilesRegistrationID,
			Method: watchedFilesMethod,
			RegisterOptions: didChangeWatchedFilesRegisterOptions{Watchers: []fileSystemWatcher{
				{GlobPattern: "**/*.go"},
				{GlobPattern: "**/go.mod"},
				{GlobPattern: "**/go.work"},
			}},
		}}},
	}, true
}

func (s *serverState) handleWatchedFilesRegistrationResponse(reply response) error {
	if !isWatchedFilesRegistrationResponse(reply.ID) {
		return nil
	}
	if s.watchedFilesRegistration != watchedFilesRegistrationPending {
		return nil
	}
	if reply.Error != nil {
		s.watchedFilesRegistration = watchedFilesUnregistered
		return fmt.Errorf(
			"register %s: client error %d: %s",
			watchedFilesMethod,
			reply.Error.Code,
			reply.Error.Message,
		)
	}

	s.watchedFilesRegistration = watchedFilesRegistered
	return nil
}

func isWatchedFilesRegistrationResponse(rawID json.RawMessage) bool {
	var id string
	if err := json.Unmarshal(rawID, &id); err != nil {
		return false
	}
	return id == watchedFilesRegistrationID
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

func (s *serverState) semanticCodeLenses(ctx context.Context, params json.RawMessage) ([]codeLens, error) {
	var input textDocumentParams
	if err := json.Unmarshal(params, &input); err != nil {
		return nil, fmt.Errorf("invalid textDocument/codeLens params: %w", err)
	}
	if input.TextDocument.URI == "" {
		return nil, errors.New("textDocument/codeLens params must include textDocument.uri")
	}

	document, err := s.documentPathFor(input.TextDocument.URI)
	if err != nil {
		return nil, fmt.Errorf("invalid textDocument/codeLens document URI: %w", err)
	}
	filename := document.filename
	scope, err := s.analysisScopeFor(filename)
	if errors.Is(err, analysis.ErrNoModule) {
		return []codeLens{}, nil
	}
	if err != nil {
		return []codeLens{}, nil //nolint:nilerr // Scope discovery failures suppress optional lenses without disrupting editing.
	}
	snapshot, err := s.snapshotForScope(ctx, scope)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return []codeLens{}, nil // Analysis failures suppress optional lenses without disrupting editing.
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	canonicalURI := document.uri
	semanticLenses := snapshot.Lenses(canonicalURI)
	result := make([]codeLens, 0, len(semanticLenses))
	for _, semanticLens := range semanticLenses {
		lensCommand := command{
			Title:   semanticLens.Title,
			Command: "editor.action.showReferences",
			Arguments: showReferencesArguments{
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

func (s *serverState) documentPathFor(uri string) (documentPath, error) {
	s.metadataMu.RLock()
	cached, ok := s.documentPaths[uri]
	s.metadataMu.RUnlock()
	if ok {
		return cached, nil
	}

	filename, err := analysis.URIToPath(uri)
	if err != nil {
		return documentPath{}, err
	}
	canonicalURI, err := analysis.PathToURI(filename)
	if err != nil {
		return documentPath{}, err
	}
	if canonicalURI == uri {
		canonicalURI = uri
	}
	resolved := documentPath{filename: filename, uri: canonicalURI}

	s.metadataMu.Lock()
	if existing, loaded := s.documentPaths[uri]; loaded {
		s.metadataMu.Unlock()
		return existing, nil
	}

	s.documentPaths[uri] = resolved
	s.metadataMu.Unlock()
	return resolved, nil
}

func (s *serverState) forgetDocumentPath(uri string) {
	s.metadataMu.Lock()
	delete(s.documentPaths, uri)
	s.metadataMu.Unlock()
}

func (s *serverState) analysisScopeFor(filename string) (analysis.AnalysisScope, error) {
	// Module discovery is stable for a directory until go.mod/go.work metadata
	// changes, so cache both successful scopes and ErrNoModule results.
	key := analysisScopeCacheKey{
		directory:     filepath.Dir(filename),
		workspaceRoot: s.workspaceRootFor(filename),
	}

	s.metadataMu.RLock()
	cached, ok := s.analysisScopes[key]
	s.metadataMu.RUnlock()
	if ok {
		return cached.scope, cached.err
	}

	scope, err := s.findScope(filename, key.workspaceRoot)
	if err != nil && !errors.Is(err, analysis.ErrNoModule) {
		return analysis.AnalysisScope{}, err
	}
	s.metadataMu.Lock()
	if existing, loaded := s.analysisScopes[key]; loaded {
		s.metadataMu.Unlock()
		return existing.scope, existing.err
	}

	s.insertAnalysisScopeLocked(key, cachedAnalysisScope{scope: scope, err: err})
	s.metadataMu.Unlock()
	return scope, err
}

func (s *serverState) insertAnalysisScopeLocked(key analysisScopeCacheKey, cached cachedAnalysisScope) {
	if _, exists := s.analysisScopes[key]; exists {
		s.analysisScopes[key] = cached
		return
	}
	// This insertion-order ring bounds metadata for repositories that open files
	// from many directories. Hits remain read-only; metadata changes clear it.
	if s.analysisScopeKeys == nil {
		s.analysisScopeKeys = make([]analysisScopeCacheKey, 0, maxAnalysisScopeCacheEntries)
	}
	if len(s.analysisScopeKeys) < maxAnalysisScopeCacheEntries {
		s.analysisScopeKeys = append(s.analysisScopeKeys, key)
	} else {
		evicted := s.analysisScopeKeys[s.analysisScopeCursor]
		s.analysisScopeKeys[s.analysisScopeCursor] = key
		s.analysisScopeCursor = (s.analysisScopeCursor + 1) % maxAnalysisScopeCacheEntries
		delete(s.analysisScopes, evicted)
	}
	s.analysisScopes[key] = cached
}

func (s *serverState) invalidateAnalysisScopes() {
	s.metadataMu.Lock()
	clear(s.analysisScopes)
	clear(s.analysisScopeKeys)
	s.analysisScopeKeys = s.analysisScopeKeys[:0]
	s.analysisScopeCursor = 0
	s.metadataMu.Unlock()
}

func (s *serverState) handleWatchedFiles(params json.RawMessage) {
	changes, err := watchedFileChanges(params)
	if err != nil {
		s.invalidateAnalysisScopes()
		s.invalidateAllSnapshots(analysis.NewChangeSet(analysis.Change{Kind: analysis.ChangeMetadata}))
		return
	}
	if slices.ContainsFunc(changes, func(change analysis.Change) bool { return change.Kind == analysis.ChangeMetadata }) {
		s.invalidateAnalysisScopes()
	}

	s.invalidateSnapshotsForChanges(changes)
}

func watchedFileChanges(params json.RawMessage) ([]analysis.Change, error) {
	var input didChangeWatchedFilesParams
	if err := json.Unmarshal(params, &input); err != nil {
		return nil, fmt.Errorf("decode watched file changes: %w", err)
	}
	changes := make([]analysis.Change, 0, len(input.Changes))
	for _, event := range input.Changes {
		path, err := analysis.URIToPath(event.URI)
		if err != nil {
			return nil, fmt.Errorf("decode watched file URI: %w", err)
		}
		kind := analysis.ChangeContent
		switch event.Type {
		case watchedFileCreated:
			kind = analysis.ChangeCreated
		case watchedFileDeleted:
			kind = analysis.ChangeDeleted
		}
		if isModuleMetadataPath(path) {
			kind = analysis.ChangeMetadata
		}
		changes = append(changes, analysis.Change{Path: path, Kind: kind})
	}

	return changes, nil
}

func isModuleMetadataPath(path string) bool {
	base := filepath.Base(path)
	return base == "go.mod" || base == "go.work"
}

func (s *serverState) snapshotForScope(ctx context.Context, scope analysis.AnalysisScope) (analysis.Snapshot, error) {
	cacheKey := scope.CacheKey()
	for {
		// A flight belongs to the server revision, not to any one request, so
		// coalesced callers can cancel independently.
		snapshot, flight, cached := s.snapshotOrFlight(scope) //nolint:contextcheck
		if cached {
			return snapshot, nil
		}
		if err := ctx.Err(); err != nil {
			s.releaseAnalysisWaiter(cacheKey, flight)
			return analysis.Snapshot{}, err
		}

		select {
		case <-flight.done:
			snapshot, err := s.consumeAnalysisFlight(flight)
			if errors.Is(err, errAnalysisObsolete) {
				continue
			}
			return snapshot, err
		case <-ctx.Done():
			s.releaseAnalysisWaiter(cacheKey, flight)
			return analysis.Snapshot{}, ctx.Err()
		}
	}
}

func (s *serverState) snapshotOrFlight(scope analysis.AnalysisScope) (analysis.Snapshot, *analysisFlight, bool) {
	cacheKey := scope.CacheKey()
	s.cacheMu.RLock()
	cached, ok := s.snapshots[cacheKey]
	if ok && cached.valid && cached.snapshotRevision == cached.revision && sameAnalysisScope(cached.scope, scope) {
		s.cacheMu.RUnlock()
		return cached.snapshot, nil, true
	}
	s.cacheMu.RUnlock()

	// Recheck after upgrading the lock: another request may have published a
	// snapshot or created the single shared flight in the meantime.
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	cached, ok = s.snapshots[cacheKey]
	if !ok {
		cached.scope = scope
		s.insertSnapshotLocked(cacheKey, cached)
	} else if !sameAnalysisScope(cached.scope, scope) {
		s.forgetScope(cacheKey)
		cached.scope = scope
		cached = invalidateCachedSnapshot(
			cached,
			analysis.NewChangeSet(analysis.Change{Kind: analysis.ChangeMetadata}),
		)
		s.snapshots[cacheKey] = cached
		s.cancelAnalysisFlightLocked(cacheKey)
	}
	if cached.valid && cached.snapshotRevision == cached.revision {
		return cached.snapshot, nil, true
	}

	if flight := s.analysisFlights[cacheKey]; flight != nil &&
		flight.revision == cached.revision && sameAnalysisScope(flight.scope, scope) {
		flight.waiters++
		return analysis.Snapshot{}, flight, false
	}

	analysisCtx, cancel := context.WithCancel(s.analysisContext)
	flight := &analysisFlight{
		scope:    scope,
		revision: cached.revision,
		changes:  cached.pendingChanges,
		done:     make(chan struct{}),
		cancel:   cancel,
		waiters:  1,
	}
	s.analysisFlights[cacheKey] = flight
	go s.runAnalysisFlight(cacheKey, analysisCtx, flight)

	return analysis.Snapshot{}, flight, false
}

func (s *serverState) insertSnapshotLocked(cacheKey string, cached cachedSnapshot) {
	if _, exists := s.snapshots[cacheKey]; exists {
		s.snapshots[cacheKey] = cached
		return
	}
	// A fixed-size insertion ring bounds retained semantic graphs. Deliberately
	// avoiding LRU promotion keeps cache hits read-only and allocation-free.
	if s.snapshotKeys == nil {
		s.snapshotKeys = make([]string, 0, maxSnapshotCacheEntries)
	}
	if len(s.snapshotKeys) < maxSnapshotCacheEntries {
		s.snapshotKeys = append(s.snapshotKeys, cacheKey)
	} else {
		evicted := s.snapshotKeys[s.snapshotCursor]
		s.snapshotKeys[s.snapshotCursor] = cacheKey
		s.snapshotCursor = (s.snapshotCursor + 1) % maxSnapshotCacheEntries
		s.evictSnapshotLocked(evicted)
	}
	s.snapshots[cacheKey] = cached
}

func (s *serverState) evictSnapshotLocked(cacheKey string) {
	if _, exists := s.snapshots[cacheKey]; !exists {
		return
	}
	delete(s.snapshots, cacheKey)
	s.forgetScope(cacheKey)
	if flight := s.analysisFlights[cacheKey]; flight != nil {
		flight.abandoned = true
		delete(s.analysisFlights, cacheKey)
		flight.cancel()
	}
}

func (s *serverState) runAnalysisFlight(cacheKey string, ctx context.Context, flight *analysisFlight) {
	snapshot, err := s.analyzeScope(
		ctx,
		flight.scope,
		flight.changes,
		s.documents.OverlayForRoots(flight.scope.ModuleRoots),
	)
	flight.cancel()

	// Publish only if both the scope and revision are still current. Obsolete
	// results are emptied before waking waiters so their graph can be reclaimed.
	s.cacheMu.Lock()
	cached, ok := s.snapshots[cacheKey]
	current := ok && !flight.abandoned && cached.revision == flight.revision && sameAnalysisScope(cached.scope, flight.scope)
	if !current {
		snapshot = analysis.Snapshot{}
		err = errAnalysisObsolete
	} else if err == nil {
		cached.snapshot = snapshot
		cached.snapshotRevision = flight.revision
		cached.valid = true
		cached.pendingChanges = analysis.ChangeSet{}
		s.snapshots[cacheKey] = cached
	}
	if s.analysisFlights[cacheKey] == flight {
		delete(s.analysisFlights, cacheKey)
	}
	flight.snapshot = snapshot
	flight.err = err
	flight.completed = true
	close(flight.done)
	if flight.waiters == 0 {
		clearAnalysisFlightResult(flight)
	}
	s.cacheMu.Unlock()
}

func (s *serverState) consumeAnalysisFlight(flight *analysisFlight) (analysis.Snapshot, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	snapshot, err := flight.snapshot, flight.err
	if flight.waiters > 0 {
		flight.waiters--
	}
	if flight.waiters == 0 {
		clearAnalysisFlightResult(flight)
	}
	return snapshot, err
}

func (s *serverState) releaseAnalysisWaiter(cacheKey string, flight *analysisFlight) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	if flight.waiters == 0 {
		return
	}
	flight.waiters--
	if flight.waiters == 0 {
		if s.analysisFlights[cacheKey] != flight {
			if flight.completed {
				clearAnalysisFlightResult(flight)
			}
			return
		}
		// Stop new requests from joining work that no caller still needs. The
		// abandoned marker also prevents a late successful return from replacing
		// a newer flight's snapshot for the same revision.
		flight.abandoned = true
		delete(s.analysisFlights, cacheKey)
		flight.cancel()
	}
}

func clearAnalysisFlightResult(flight *analysisFlight) {
	// A completed flight may otherwise retain a full Snapshot until every
	// request goroutine releases its small flight pointer.
	flight.cancel = nil
	flight.scope = analysis.AnalysisScope{}
	flight.changes = analysis.ChangeSet{}
	flight.snapshot = analysis.Snapshot{}
	flight.err = nil
}

func (s *serverState) invalidateSnapshotsForPath(path string) {
	s.invalidateSnapshotsForChange(analysis.Change{Path: path, Kind: analysis.ChangeContent})
}

func (s *serverState) invalidateSnapshotsForChange(change analysis.Change) {
	changeSet := analysis.NewChangeSet(change)
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	for cacheKey, cached := range s.snapshots {
		if change.Path != "" && !analysisScopeContainsPath(cached.scope, change.Path) {
			continue
		}
		s.engine.InvalidateScope(cacheKey)
		cached = invalidateCachedSnapshot(cached, changeSet)
		s.snapshots[cacheKey] = cached
		s.cancelAnalysisFlightLocked(cacheKey)
	}
}

func (s *serverState) invalidateSnapshotsForChanges(changes []analysis.Change) {
	if len(changes) == 0 {
		return
	}
	if len(changes) == 1 {
		s.invalidateSnapshotsForChange(changes[0])
		return
	}

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	for cacheKey, cached := range s.snapshots {
		scopeChanges := make([]analysis.Change, 0, len(changes))
		for _, change := range changes {
			if change.Path == "" || analysisScopeContainsPath(cached.scope, change.Path) {
				scopeChanges = append(scopeChanges, change)
			}
		}
		if len(scopeChanges) == 0 {
			continue
		}
		s.engine.InvalidateScope(cacheKey)
		cached = invalidateCachedSnapshot(cached, analysis.NewChangeSet(scopeChanges...))
		s.snapshots[cacheKey] = cached
		s.cancelAnalysisFlightLocked(cacheKey)
	}
}

func (s *serverState) invalidateAllSnapshots(changes analysis.ChangeSet) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for cacheKey, cached := range s.snapshots {
		s.engine.InvalidateScope(cacheKey)
		cached = invalidateCachedSnapshot(cached, changes)
		s.snapshots[cacheKey] = cached
		s.cancelAnalysisFlightLocked(cacheKey)
	}
}

func (s *serverState) cancelAnalysisFlightLocked(cacheKey string) {
	if flight := s.analysisFlights[cacheKey]; flight != nil {
		flight.cancel()
	}
}

func (s *serverState) cancelAllAnalysisFlights() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for _, flight := range s.analysisFlights {
		flight.cancel()
	}
}

func invalidateCachedSnapshot(cached cachedSnapshot, changes analysis.ChangeSet) cachedSnapshot {
	cached.revision++
	cached.snapshotRevision = 0
	cached.valid = false
	// Keep the revision tombstone so a new request cannot join obsolete
	// single-flight work, but release the potentially large semantic graph
	// immediately.
	cached.snapshot = analysis.Snapshot{}
	cached.pendingChanges = cached.pendingChanges.Merge(changes)
	return cached
}

func analysisScopeContainsPath(scope analysis.AnalysisScope, path string) bool {
	if scope.WorkFile != "" && pathsEqual(path, scope.WorkFile) {
		return true
	}
	for _, root := range scope.ModuleRoots {
		if pathWithinRoot(path, root) {
			return true
		}
	}

	return false
}

func sameAnalysisScope(left, right analysis.AnalysisScope) bool {
	if !pathsEqual(left.Root, right.Root) || !pathsEqual(left.WorkFile, right.WorkFile) ||
		len(left.ModuleRoots) != len(right.ModuleRoots) || len(left.Patterns) != len(right.Patterns) {
		return false
	}
	for index := range left.ModuleRoots {
		if !pathsEqual(left.ModuleRoots[index], right.ModuleRoots[index]) {
			return false
		}
	}
	for index := range left.Patterns {
		if left.Patterns[index] != right.Patterns[index] {
			return false
		}
	}

	return true
}

func (s *serverState) workspaceRootFor(filename string) string {
	var best string
	for _, root := range s.workspaceRoots {
		if !pathsEqual(filename, root) && !pathWithinRoot(filename, root) {
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
