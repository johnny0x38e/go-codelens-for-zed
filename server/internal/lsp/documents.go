package lsp

import (
	"bytes"
	"os"
	"strings"
	"sync"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
)

// documentStore owns immutable copies of unsaved full-document contents.
// Callers receive defensive copies so analysis can run after releasing mu.
type documentStore struct {
	mu        sync.RWMutex
	documents map[string][]byte
}

func newDocumentStore() *documentStore {
	return &documentStore{documents: make(map[string][]byte)}
}

func (s *documentStore) Open(uri, text string) (string, bool) {
	path, err := analysis.URIToPath(uri)
	if err != nil {
		return "", false
	}
	content := []byte(text)
	diskContent, readErr := os.ReadFile(path) //nolint:gosec // LSP document URIs intentionally identify local workspace files.
	if readErr == nil && bytes.Equal(diskContent, content) {
		return path, s.remove(path)
	}
	return path, s.setPath(path, content)
}

func (s *documentStore) Change(uri, text string) (string, bool) {
	path, err := analysis.URIToPath(uri)
	if err != nil {
		return "", false
	}
	return path, s.setPath(path, []byte(text))
}

func (s *documentStore) Save(uri string) (string, bool) {
	path, err := analysis.URIToPath(uri)
	if err != nil {
		return "", false
	}
	diskContent, readErr := os.ReadFile(path) //nolint:gosec // LSP document URIs intentionally identify local workspace files.
	return path, s.removeIfSaved(path, diskContent, readErr)
}

func (s *documentStore) Close(uri string) (string, bool) {
	path, err := analysis.URIToPath(uri)
	if err != nil {
		return "", false
	}
	return path, s.remove(path)
}

func (s *documentStore) remove(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.documents[path]; !ok {
		return false
	}
	delete(s.documents, path)
	return true
}

func (s *documentStore) removeIfSaved(path string, diskContent []byte, readErr error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	overlay, ok := s.documents[path]
	if !ok {
		return false
	}
	delete(s.documents, path)
	if readErr != nil || !bytes.Equal(overlay, diskContent) {
		return true
	}
	return false
}

func (s *documentStore) Overlay(moduleDir string) map[string][]byte {
	return s.OverlayForRoots([]string{moduleDir})
}

func (s *documentStore) OverlayForRoots(roots []string) map[string][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string][]byte)
	for path, content := range s.documents {
		insideScope := false
		for _, root := range roots {
			if pathWithinRoot(path, root) {
				insideScope = true
				break
			}
		}
		if !insideScope {
			continue
		}
		result[path] = append([]byte(nil), content...)
	}
	return result
}

func (s *documentStore) setPath(path string, content []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.documents[path]; ok && bytes.Equal(existing, content) {
		return false
	}
	// Open and Change pass newly allocated byte slices that are never mutated
	// after this ownership transfer.
	s.documents[path] = content
	return true
}

func pathWithinRoot(path, root string) bool {
	if root == "" || len(path) <= len(root) || !strings.HasPrefix(path, root) {
		return false
	}
	if os.IsPathSeparator(root[len(root)-1]) {
		return true
	}
	return os.IsPathSeparator(path[len(root)])
}
