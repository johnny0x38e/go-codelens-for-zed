package lsp

import (
	"bytes"
	"fmt"
	"os"
	"sync"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

// openDocument retains the editor's current buffer and lifecycle state.
// Clean documents stay open so a later incremental change can be applied
// without rereading the file from disk.
type openDocument struct {
	buffer       *textBuffer
	uri          string
	canonicalURI string
	version      int64
	dirty        bool
	syntaxValid  bool
}

// documentStore owns editor buffers and confirmed-save readiness. Syntax is
// checked only at open/save boundaries, never on the incremental edit path.
type documentStore struct {
	mu         sync.RWMutex
	documents  map[string]*openDocument
	pathsByURI map[string]string
}

func newDocumentStore() *documentStore {
	return &documentStore{
		documents:  make(map[string]*openDocument),
		pathsByURI: make(map[string]string),
	}
}

func (s *documentStore) Open(uri, text string, version int64) (string, bool, error) {
	path, err := analysis.URIToPath(uri)
	if err != nil {
		return "", false, fmt.Errorf("open document URI: %w", err)
	}

	canonicalURI, err := analysis.PathToURI(path)
	if err != nil {
		return "", false, fmt.Errorf("canonicalize open document URI: %w", err)
	}
	content := []byte(text)
	// This one lifecycle read determines whether analysis needs an overlay. The
	// store performs no polling, background scans, or persistent writes.
	diskContent, readErr := os.ReadFile(
		path,
	) //nolint:gosec // LSP document URIs intentionally identify local workspace files.
	dirty := readErr != nil || !bytes.Equal(diskContent, content)
	syntaxValid := !dirty && validSavedGoSyntax(path, content)

	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.documents[path]
	if existing != nil && version <= existing.version {
		s.pathsByURI[uri] = path
		return path, false, nil
	}

	changed := effectiveOverlayChanged(existing, dirty, text)
	if existing != nil {
		delete(s.pathsByURI, existing.uri)
	}
	s.documents[path] = &openDocument{
		buffer:       newTextBuffer(text),
		uri:          uri,
		canonicalURI: canonicalURI,
		version:      version,
		dirty:        dirty,
		syntaxValid:  syntaxValid,
	}
	s.pathsByURI[uri] = path
	return path, changed, nil
}

func (s *documentStore) Change(
	uri string,
	version int64,
	changes []textDocumentContentChangeEvent,
) (string, bool, error) {
	path, changed, _, err := s.ChangeRecorded(uri, version, changes, nil)
	return path, changed, err
}

func (s *documentStore) ChangeRecorded(
	uri string,
	version int64,
	changes []textDocumentContentChangeEvent,
	edits []positionEdit,
) (string, bool, []positionEdit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, ok := s.pathsByURI[uri]
	if !ok {
		return "", false, edits, fmt.Errorf("change unopened document URI %q", uri)
	}
	document := s.documents[path]
	if document == nil {
		return path, false, edits, fmt.Errorf("change unopened document %q", path)
	}
	if version <= document.version {
		return path, false, edits, nil
	}

	changed, edits, err := document.buffer.applyRecorded(changes, edits)
	if err != nil {
		return path, false, edits, fmt.Errorf("change document %q: %w", path, err)
	}
	document.version = version
	if changed {
		document.dirty = true
	}
	return path, changed, edits, nil
}

// Save reports whether confirmed-save readiness changed or disk confirmation
// failed. Repeated saves of unchanged, confirmed bytes need no new analysis.
func (s *documentStore) Save(uri string) (string, bool) {
	s.mu.RLock()
	path, ok := s.pathsByURI[uri]
	s.mu.RUnlock()
	if !ok {
		return "", false
	}

	// A save notification is not proof that disk already matches the buffer.
	// Keep analysis blocked until byte-for-byte agreement is confirmed.
	diskContent, readErr := os.ReadFile(
		path,
	) //nolint:gosec // LSP document URIs intentionally identify local workspace files.
	syntaxValid := readErr == nil && validSavedGoSyntax(path, diskContent)
	s.mu.Lock()
	defer s.mu.Unlock()
	document := s.documents[path]
	if document == nil {
		return path, false
	}

	if readErr != nil || !document.buffer.equalsBytes(diskContent) {
		document.dirty = true
		return path, true
	}
	changed := document.dirty || document.syntaxValid != syntaxValid
	document.dirty = false
	document.syntaxValid = syntaxValid
	return path, changed
}

// ReadyForAnalysis checks only open-document metadata. The scope includes all
// modules of a go.work workspace; unrelated scopes cannot block one another.
func (s *documentStore) ReadyForAnalysis(scope analysis.AnalysisScope) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for path, document := range s.documents {
		if analysisScopeContainsPath(scope, path) && (document.dirty || !document.syntaxValid) {
			return false
		}
	}
	return true
}

// SyntaxVerified avoids parsing the same confirmed saved bytes again before
// analysis. Any subsequent editor change makes this false immediately; loader
// parse errors still reject concurrent external rewrites.
func (s *documentStore) SyntaxVerified(path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	document := s.documents[path]
	return document != nil && !document.dirty && document.syntaxValid
}

// IsOpenURI checks canonical target URIs without path conversion or retaining
// metadata for every closed file reached through a navigation target.
func (s *documentStore) IsOpenURI(uri string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, document := range s.documents {
		if document.canonicalURI == uri {
			return true
		}
	}
	return false
}

// IsOpen distinguishes editor-managed coordinates from untracked disk rewrites.
func (s *documentStore) IsOpen(path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.documents[path] != nil
}

func (s *documentStore) Close(uri string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, ok := s.pathsByURI[uri]
	if !ok {
		return "", false
	}
	delete(s.pathsByURI, uri)
	document := s.documents[path]
	if document == nil {
		return path, false
	}
	delete(s.documents, path)
	return path, document.dirty
}

func (s *documentStore) Overlay(moduleDir string) map[string][]byte {
	return s.OverlayForRoots([]string{moduleDir})
}

func (s *documentStore) OverlayForRoots(roots []string) map[string][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Clean open documents need no overlay and therefore no source copy. Dirty
	// buffers are copied while holding the read lock, then become immutable.
	result := make(map[string][]byte)
	for path, document := range s.documents {
		if !document.dirty || !pathWithinAnyRoot(path, roots) {
			continue
		}
		result[path] = document.buffer.bytes()
	}
	return result
}

func effectiveOverlayChanged(existing *openDocument, dirty bool, text string) bool {
	if existing == nil {
		return dirty
	}
	if existing.dirty != dirty {
		return true
	}
	return dirty && !existing.buffer.equalsString(text)
}

func pathWithinAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		if pathWithinRoot(path, root) {
			return true
		}
	}
	return false
}

func pathWithinRoot(path, root string) bool {
	if root == "" || len(path) <= len(root) || !pathPrefix(path, root) {
		return false
	}
	if os.IsPathSeparator(root[len(root)-1]) {
		return true
	}

	return os.IsPathSeparator(path[len(root)])
}
