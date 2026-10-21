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
	buffer  *textBuffer
	uri     string
	version int64
	dirty   bool
}

// documentStore owns open editor buffers. Only dirty buffers are exposed as
// analysis overlays, and callers receive immutable copies after releasing mu.
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

	content := []byte(text)
	// This one lifecycle read determines whether analysis needs an overlay. The
	// store performs no polling, background scans, or persistent writes.
	diskContent, readErr := os.ReadFile(
		path,
	) //nolint:gosec // LSP document URIs intentionally identify local workspace files.
	dirty := readErr != nil || !bytes.Equal(diskContent, content)

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
		buffer:  newTextBuffer(text),
		uri:     uri,
		version: version,
		dirty:   dirty,
	}
	s.pathsByURI[uri] = path
	return path, changed, nil
}

func (s *documentStore) Change(
	uri string,
	version int64,
	changes []textDocumentContentChangeEvent,
) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, ok := s.pathsByURI[uri]
	if !ok {
		return "", false, fmt.Errorf("change unopened document URI %q", uri)
	}
	document := s.documents[path]
	if document == nil {
		return path, false, fmt.Errorf("change unopened document %q", path)
	}
	if version <= document.version {
		return path, false, nil
	}

	changed, err := document.buffer.apply(changes)
	if err != nil {
		return path, false, fmt.Errorf("change document %q: %w", path, err)
	}
	document.version = version
	if changed {
		document.dirty = true
	}
	return path, changed, nil
}

func (s *documentStore) Save(uri string) (string, bool) {
	s.mu.RLock()
	path, ok := s.pathsByURI[uri]
	s.mu.RUnlock()
	if !ok {
		return "", false
	}

	// Only a confirmed byte-for-byte disk match can retire the overlay. A failed
	// or not-yet-visible save leaves the editor buffer authoritative while still
	// invalidating analysis for the save event.
	diskContent, readErr := os.ReadFile(
		path,
	) //nolint:gosec // LSP document URIs intentionally identify local workspace files.
	s.mu.Lock()
	defer s.mu.Unlock()
	document := s.documents[path]
	if document == nil || !document.dirty {
		return path, false
	}

	if readErr != nil || !document.buffer.equalsBytes(diskContent) {
		return path, true
	}
	document.dirty = false
	return path, false
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
