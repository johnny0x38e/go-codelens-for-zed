package lsp

import (
	"errors"
	"path/filepath"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

const maxAnalysisScopeCacheEntries = 4096

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
