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
	"golang.org/x/sync/singleflight"
)

const jsonRPCVersion = "2.0"

type serverState struct {
	workspaceRoots []string
	documents      *documentStore
	metadataMu     sync.RWMutex
	analysisScopes map[analysisScopeCacheKey]cachedAnalysisScope
	documentPaths  map[string]documentPath
	cacheMu        sync.RWMutex
	snapshots      map[string]cachedSnapshot
	analysisGroup  singleflight.Group
	findScope      func(string, string) (analysis.AnalysisScope, error)
	analyzeScope   func(context.Context, analysis.AnalysisScope, map[string][]byte) (analysis.Snapshot, error)
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

type cachedSnapshot struct {
	scope            analysis.AnalysisScope
	revision         uint64
	snapshotRevision uint64
	valid            bool
	snapshot         analysis.Snapshot
}

// Serve handles Language Server Protocol messages until the client sends exit
// or the input stream reaches EOF.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return ServeWithTrace(ctx, in, out, nil)
}

// ServeWithTrace is Serve with optional request/response method tracing. The
// trace stream is diagnostic only: nil disables tracing, and write failures
// never interrupt LSP traffic.
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
						"name":    "go-codelens-ls",
						"version": "0.3.1",
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
		documents:      newDocumentStore(),
		analysisScopes: make(map[analysisScopeCacheKey]cachedAnalysisScope),
		documentPaths:  make(map[string]documentPath),
		snapshots:      make(map[string]cachedSnapshot),
		findScope:      analysis.FindAnalysisScope,
		analyzeScope:   analysis.AnalyzeScope,
	}
}

func traceMethod(trace io.Writer, direction, method string) {
	if trace == nil {
		return
	}
	_, _ = fmt.Fprintf(trace, "%s %s\n", direction, method) //nolint:errcheck // Diagnostic tracing is best effort.
}

func (s *serverState) initialize(params json.RawMessage) {
	var input initializeParams
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
		var input didOpenTextDocumentParams
		if json.Unmarshal(params, &input) == nil {
			path, changed := s.documents.Open(input.TextDocument.URI, input.TextDocument.Text)
			if changed {
				s.invalidateSnapshotsForPath(path)
			}
			if isModuleMetadataPath(path) {
				s.invalidateAnalysisScopes()
			}
		}
	case "textDocument/didChange":
		var input didChangeTextDocumentParams
		if json.Unmarshal(params, &input) == nil && len(input.ContentChanges) > 0 {
			path, changed := s.documents.Change(input.TextDocument.URI, input.ContentChanges[len(input.ContentChanges)-1].Text)
			if changed {
				s.invalidateSnapshotsForPath(path)
			}
			if isModuleMetadataPath(path) {
				s.invalidateAnalysisScopes()
			}
		}
	case "textDocument/didClose":
		var input textDocumentParams
		if json.Unmarshal(params, &input) == nil {
			path, changed := s.documents.Close(input.TextDocument.URI)
			if changed {
				s.invalidateSnapshotsForPath(path)
			}
			if isModuleMetadataPath(path) {
				s.invalidateAnalysisScopes()
			}
			s.forgetDocumentPath(input.TextDocument.URI)
		}
	case "textDocument/didSave":
		var input textDocumentParams
		if json.Unmarshal(params, &input) == nil {
			path, changed := s.documents.Save(input.TextDocument.URI)
			if changed {
				s.invalidateSnapshotsForPath(path)
			}
			if isModuleMetadataPath(path) {
				s.invalidateAnalysisScopes()
			}
		}
	case "workspace/didChangeWatchedFiles":
		s.handleWatchedFiles(params)
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
		return []codeLens{}, nil //nolint:nilerr // Analysis failures suppress optional lenses without disrupting editing.
	}

	canonicalURI := document.uri
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
	s.analysisScopes[key] = cachedAnalysisScope{scope: scope, err: err}
	s.metadataMu.Unlock()
	return scope, err
}

func (s *serverState) invalidateAnalysisScopes() {
	s.metadataMu.Lock()
	clear(s.analysisScopes)
	s.metadataMu.Unlock()
}

func (s *serverState) handleWatchedFiles(params json.RawMessage) {
	paths, err := watchedFilePaths(params)
	if err != nil {
		s.invalidateAnalysisScopes()
		s.invalidateAllSnapshots()
		return
	}
	if slices.ContainsFunc(paths, isModuleMetadataPath) {
		s.invalidateAnalysisScopes()
	}
	s.invalidateSnapshotsForPaths(paths)
}

func watchedFilePaths(params json.RawMessage) ([]string, error) {
	var input didChangeWatchedFilesParams
	if err := json.Unmarshal(params, &input); err != nil {
		return nil, fmt.Errorf("decode watched file changes: %w", err)
	}
	paths := make([]string, 0, len(input.Changes))
	for _, change := range input.Changes {
		path, err := analysis.URIToPath(change.URI)
		if err != nil {
			return nil, fmt.Errorf("decode watched file URI: %w", err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func isModuleMetadataPath(path string) bool {
	base := filepath.Base(path)
	return base == "go.mod" || base == "go.work"
}

func (s *serverState) snapshotForScope(ctx context.Context, scope analysis.AnalysisScope) (analysis.Snapshot, error) {
	cacheKey := scope.CacheKey()
	cached, revision, ok := s.cachedSnapshotForScope(scope)
	if ok {
		return cached, nil
	}

	result, err, _ := s.analysisGroup.Do(cacheKey+"\x00"+strconv.FormatUint(revision, 10), func() (any, error) {
		cached, currentRevision, ok := s.cachedSnapshotForScope(scope)
		if ok {
			return cached, nil
		}
		if currentRevision != revision {
			return s.snapshotForScope(ctx, scope)
		}

		snapshot, analyzeErr := s.analyzeScope(ctx, scope, s.documents.OverlayForRoots(scope.ModuleRoots))
		if analyzeErr != nil {
			return analysis.Snapshot{}, analyzeErr
		}
		s.storeSnapshot(scope, revision, snapshot)
		return snapshot, nil
	})
	if err != nil {
		return analysis.Snapshot{}, err
	}
	snapshot, ok := result.(analysis.Snapshot)
	if !ok {
		return analysis.Snapshot{}, fmt.Errorf("analysis flight returned %T, want analysis.Snapshot", result)
	}
	return snapshot, nil
}

func (s *serverState) cachedSnapshotForScope(scope analysis.AnalysisScope) (analysis.Snapshot, uint64, bool) {
	cacheKey := scope.CacheKey()
	s.cacheMu.RLock()
	cached, ok := s.snapshots[cacheKey]
	if ok && cached.valid && cached.snapshotRevision == cached.revision {
		s.cacheMu.RUnlock()
		return cached.snapshot, cached.revision, true
	}
	if ok && sameAnalysisScope(cached.scope, scope) {
		s.cacheMu.RUnlock()
		return analysis.Snapshot{}, cached.revision, false
	}
	s.cacheMu.RUnlock()

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	cached, ok = s.snapshots[cacheKey]
	if !ok {
		cached.scope = scope
		s.snapshots[cacheKey] = cached
		return analysis.Snapshot{}, cached.revision, false
	}
	if !sameAnalysisScope(cached.scope, scope) {
		cached.scope = scope
		cached = invalidateCachedSnapshot(cached)
		s.snapshots[cacheKey] = cached
		return analysis.Snapshot{}, cached.revision, false
	}
	if cached.valid && cached.snapshotRevision == cached.revision {
		return cached.snapshot, cached.revision, true
	}
	return analysis.Snapshot{}, cached.revision, false
}

func (s *serverState) storeSnapshot(scope analysis.AnalysisScope, revision uint64, snapshot analysis.Snapshot) {
	cacheKey := scope.CacheKey()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	cached, ok := s.snapshots[cacheKey]
	if !ok || cached.revision != revision || !sameAnalysisScope(cached.scope, scope) {
		return
	}
	cached.snapshot = snapshot
	cached.snapshotRevision = revision
	cached.valid = true
	s.snapshots[cacheKey] = cached
}

func (s *serverState) invalidateSnapshotsForPath(path string) {
	s.invalidateSnapshotsForPaths([]string{path})
}

func (s *serverState) invalidateSnapshotsForPaths(paths []string) {
	if len(paths) == 0 {
		return
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for cacheKey, cached := range s.snapshots {
		containsChangedPath := false
		for _, path := range paths {
			if analysisScopeContainsPath(cached.scope, path) {
				containsChangedPath = true
				break
			}
		}
		if !containsChangedPath {
			continue
		}
		cached = invalidateCachedSnapshot(cached)
		s.snapshots[cacheKey] = cached
	}
}

func (s *serverState) invalidateAllSnapshots() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for cacheKey, cached := range s.snapshots {
		cached = invalidateCachedSnapshot(cached)
		s.snapshots[cacheKey] = cached
	}
}

func invalidateCachedSnapshot(cached cachedSnapshot) cachedSnapshot {
	cached.revision++
	cached.snapshotRevision = 0
	cached.valid = false
	// Keep the revision tombstone so a new request cannot join obsolete
	// single-flight work, but release the potentially large semantic graph
	// immediately.
	cached.snapshot = analysis.Snapshot{}
	return cached
}

func analysisScopeContainsPath(scope analysis.AnalysisScope, path string) bool {
	if scope.WorkFile != "" && path == scope.WorkFile {
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
	if left.Root != right.Root || left.WorkFile != right.WorkFile ||
		len(left.ModuleRoots) != len(right.ModuleRoots) || len(left.Patterns) != len(right.Patterns) {
		return false
	}
	for index := range left.ModuleRoots {
		if left.ModuleRoots[index] != right.ModuleRoots[index] {
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
		if filename != root && !pathWithinRoot(filename, root) {
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
