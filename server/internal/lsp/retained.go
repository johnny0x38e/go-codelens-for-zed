package lsp

import "github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"

const maxMappedDocuments = 128

func (cached *cachedSnapshot) positionMappingFor(uri string) *positionMapping {
	if cached.positionsLost {
		return nil
	}
	if mapping := cached.positions[uri]; mapping != nil {
		return mapping
	}
	if len(cached.positions) >= maxMappedDocuments {
		cached.positions, cached.positionsLost = nil, true
		return nil
	}
	if cached.positions == nil {
		cached.positions = make(map[string]*positionMapping)
	}
	mapping := newPositionMapping()
	cached.positions[uri] = mapping
	return mapping
}

func (s *serverState) applyDocumentEdits(path string, edits []positionEdit) {
	if len(edits) == 0 {
		return
	}
	uri, err := analysis.PathToURI(path)
	if err != nil {
		return
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for key, cached := range s.snapshots {
		if !analysisScopeContainsPath(cached.scope, path) {
			continue
		}
		if mapping := cached.positionMappingFor(uri); mapping != nil {
			for _, edit := range edits {
				mapping.apply(edit)
			}
		}
		s.snapshots[key] = cached
	}
}

// suppressDocumentPositions handles a dirty open or an external rewrite for
// which the server has no edit sequence relative to the retained snapshot.
func (s *serverState) suppressDocumentPositions(path string) {
	uri, err := analysis.PathToURI(path)
	if err != nil {
		return
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for key, cached := range s.snapshots {
		if !analysisScopeContainsPath(cached.scope, path) {
			continue
		}
		if mapping := cached.positionMappingFor(uri); mapping != nil {
			mapping.spans, mapping.scratch = nil, nil
		}
		s.snapshots[key] = cached
	}
}

func (s *serverState) discardDocumentPositions(path string) {
	uri, err := analysis.PathToURI(path)
	if err != nil {
		return
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for key, cached := range s.snapshots {
		if cached.positions[uri] == nil {
			continue
		}
		if _, stale := cached.snapshot.StaleFile(uri); !stale {
			// Discarding edits restores the exact disk revision of this snapshot.
			delete(cached.positions, uri)
		} else {
			// Disk may itself contain a newer, unsuccessful save. Its positions
			// cannot be inferred from the discarded editor buffer.
			cached.positions[uri] = &positionMapping{}
		}
		s.snapshots[key] = cached
	}
}

func (s *serverState) hasBlockedScopeForPath(path string) bool {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	for _, cached := range s.snapshots {
		if cached.blocked && analysisScopeContainsPath(cached.scope, path) {
			return true
		}
	}
	return false
}

func (s *serverState) noteSavedRefresh(path string) {
	s.cacheMu.RLock()
	ready := false
	for _, cached := range s.snapshots {
		if analysisScopeContainsPath(cached.scope, path) && s.documents.ReadyForAnalysis(cached.scope) {
			ready = true
			break
		}
	}
	s.cacheMu.RUnlock()
	if ready {
		s.noteCodeLensRefresh()
	}
}

func (s *serverState) retainedDocumentLenses(document documentPath) []codeLens {
	s.cacheMu.RLock()
	cached, ok := s.snapshots[document.lastScopeKey]
	s.cacheMu.RUnlock()
	if !ok {
		return []codeLens{}
	}
	return s.retainedCodeLenses(cached.scope, document.uri, true)
}

// retainedCodeLenses reads the snapshot and its coordinate mapping under one
// lock. A publication or edit between analysis completion and serialization
// therefore cannot combine coordinates from different revisions.
func (s *serverState) retainedCodeLenses(scope analysis.AnalysisScope, uri string, verifyRetained bool) []codeLens {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	cached, ok := s.snapshots[scope.CacheKey()]
	if !ok || cached.positionsLost {
		return []codeLens{}
	}
	var validation retainedFreshness
	var freshness *retainedFreshness
	if !cached.valid || verifyRetained {
		validation.snapshot, validation.documents = cached.snapshot, s.documents
		freshness = &validation
		if !freshness.allows(uri) {
			return []codeLens{}
		}
	}
	lenses := cached.snapshot.Lenses(uri)
	result := make([]codeLens, 0, len(lenses))
	for _, lens := range lenses {
		value, mapped := cached.positions[uri].mapRange(lens.Range)
		if !mapped {
			continue
		}
		result = append(result, codeLens{
			Range: value,
			Command: command{
				Title:   lens.Title,
				Command: "editor.action.showReferences",
				Arguments: showReferencesArguments{
					uri,
					value.Start,
					mapRetainedTargets(lens.Targets, cached.positions, freshness),
				},
			},
		})
	}
	return result
}

// mapRetainedTargets shares immutable targets unless an edit moves or removes
// a location. A removed target is omitted instead of navigating to stale text;
// the displayed count intentionally remains the last successful semantic count.
func mapRetainedTargets(
	targets []analysis.Location,
	positions map[string]*positionMapping,
	freshness *retainedFreshness,
) []analysis.Location {
	var mapped []analysis.Location
	for index, target := range targets {
		value, ok := positions[target.URI].mapRange(target.Range)
		if freshness != nil && !freshness.allows(target.URI) {
			ok = false
		}
		if mapped == nil && (!ok || value != target.Range) {
			mapped = make([]analysis.Location, 0, len(targets))
			mapped = append(mapped, targets[:index]...)
		}
		if mapped != nil && ok {
			target.Range = value
			mapped = append(mapped, target)
		}
	}
	if mapped == nil {
		return targets
	}
	return mapped
}
