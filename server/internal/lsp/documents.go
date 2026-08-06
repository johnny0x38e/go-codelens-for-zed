package lsp

import (
	"bytes"
	"path/filepath"
	"sync"

	"github.com/johnny/go-implementation-lens-for-zed/server/internal/analysis"
)

type documentStore struct {
	mu         sync.RWMutex
	documents  map[string][]byte
	generation uint64
}

func newDocumentStore() *documentStore {
	return &documentStore{documents: make(map[string][]byte)}
}

func (s *documentStore) Open(uri, text string) {
	s.set(uri, []byte(text))
}

func (s *documentStore) Change(uri, text string) {
	s.set(uri, []byte(text))
}

func (s *documentStore) Close(uri string) {
	path, err := analysis.URIToPath(uri)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.documents[path]; !ok {
		return
	}
	delete(s.documents, path)
	s.generation++
}

func (s *documentStore) Invalidate() {
	s.mu.Lock()
	s.generation++
	s.mu.Unlock()
}

func (s *documentStore) Overlay(moduleDir string) map[string][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string][]byte)
	for path, content := range s.documents {
		if !pathWithinRoot(path, moduleDir) {
			continue
		}
		result[path] = append([]byte(nil), content...)
	}
	return result
}

func (s *documentStore) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}

func (s *documentStore) set(uri string, content []byte) {
	path, err := analysis.URIToPath(uri)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.documents[path]; ok && bytes.Equal(existing, content) {
		return
	}
	s.documents[path] = append([]byte(nil), content...)
	s.generation++
}

func pathWithinRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && relative != "." &&
		!filepath.IsAbs(relative) && !hasParentPrefix(relative)
}

func hasParentPrefix(relative string) bool {
	return len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator)
}
