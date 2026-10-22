package lsp

import (
	"context"
	"errors"
	"slices"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

const maxSnapshotCacheEntries = 32

var (
	errAnalysisObsolete = errors.New("analysis became obsolete")
	errAnalysisDeferred = errors.New("analysis is waiting for saved documents")
)

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

// cachedSnapshot retains the last successful immutable result while newer edits
// are pending. valid means it is current; an invalid result may still be shown
// with intentionally old counts and text-mapped coordinates. positions and the
// revision state are protected by cacheMu.
type cachedSnapshot struct {
	scope          analysis.AnalysisScope
	revision       uint64
	valid          bool
	blocked        bool
	snapshot       analysis.Snapshot
	pendingChanges analysis.ChangeSet
	positions      map[string]*positionMapping
	positionsLost  bool
}

func (s *serverState) snapshotForScope(
	ctx context.Context,
	scope analysis.AnalysisScope,
	requestedPath ...string,
) (analysis.Snapshot, error) {
	cacheKey := scope.CacheKey()
	for {
		if err := ctx.Err(); err != nil {
			return analysis.Snapshot{}, err
		}
		// A flight belongs to the server revision, not to any one request, so
		// coalesced callers can cancel independently.
		snapshot, flight, cached := s.snapshotOrFlight(scope, requestedPath...) //nolint:contextcheck
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

func (s *serverState) snapshotOrFlight(
	scope analysis.AnalysisScope,
	requestedPath ...string,
) (analysis.Snapshot, *analysisFlight, bool) {
	cacheKey := scope.CacheKey()
	s.cacheMu.RLock()
	cached, ok := s.snapshots[cacheKey]
	if ok && sameAnalysisScope(cached.scope, scope) &&
		(cached.valid || cached.blocked || !s.documents.ReadyForAnalysis(scope)) {
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
		if len(requestedPath) > 0 {
			cached.pendingChanges = analysis.NewChangeSet(analysis.Change{Path: requestedPath[0]})
		}
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
	if cached.valid || cached.blocked || !s.documents.ReadyForAnalysis(scope) {
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
	snapshot, err := s.analyzeSavedScope(ctx, flight)
	if err == nil {
		err = ctx.Err()
	}
	flight.cancel()

	// Publish only if both the scope and revision are still current. Obsolete
	// results are emptied before waking waiters so their graph can be reclaimed.
	s.cacheMu.Lock()
	cached, ok := s.snapshots[cacheKey]
	current := ok && !flight.abandoned && cached.revision == flight.revision &&
		sameAnalysisScope(cached.scope, flight.scope)
	if current && err == nil && !s.documents.ReadyForAnalysis(flight.scope) {
		err = errAnalysisDeferred
	}
	if !current {
		snapshot = analysis.Snapshot{}
		err = errAnalysisObsolete
	} else if err == nil {
		cached.snapshot = snapshot
		cached.valid = true
		cached.pendingChanges = analysis.ChangeSet{}
		cached.positions = nil
		cached.positionsLost = false
		s.snapshots[cacheKey] = cached
	} else if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		// A failed or syntax-blocked revision must not clear working lenses or
		// restart expensive work on every client request. A later notification
		// clears blocked and retries against the new saved revision.
		cached.blocked = !errors.Is(err, errAnalysisDeferred)
		s.snapshots[cacheKey] = cached
		snapshot, err = cached.snapshot, nil
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

func (s *serverState) analyzeSavedScope(ctx context.Context, flight *analysisFlight) (analysis.Snapshot, error) {
	// Cancellation is cooperative: a parser/checker may still own its working
	// graph after its request has returned. Do not start another expensive
	// analysis until that operation actually exits, even across cache eviction.
	select {
	case s.analysisSlot <- struct{}{}:
		defer func() { <-s.analysisSlot }()
	case <-ctx.Done():
		return analysis.Snapshot{}, ctx.Err()
	}
	// Both select arms can be ready. Recheck before reading or parsing source.
	if err := ctx.Err(); err != nil {
		return analysis.Snapshot{}, err
	}
	if !s.documents.ReadyForAnalysis(flight.scope) {
		return analysis.Snapshot{}, errAnalysisDeferred
	}
	if err := checkChangedGoSyntax(ctx, flight.changes, s.documents); err != nil {
		return analysis.Snapshot{}, err
	}
	// Dirty overlays deliberately never enter the semantic engine. The entire
	// scope must be saved, so Go can reuse dependency export data as well.
	return s.analyzeScope(ctx, flight.scope, flight.changes, nil)
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
		if cached.valid || s.analysisFlights[cacheKey] != nil {
			s.engine.InvalidateScope(cacheKey)
		}
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
		if cached.valid || s.analysisFlights[cacheKey] != nil {
			s.engine.InvalidateScope(cacheKey)
		}
		cached = invalidateCachedSnapshot(cached, analysis.NewChangeSet(scopeChanges...))
		s.snapshots[cacheKey] = cached
		s.cancelAnalysisFlightLocked(cacheKey)
	}
}

func (s *serverState) invalidateAllSnapshots(changes analysis.ChangeSet) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for cacheKey, cached := range s.snapshots {
		if cached.valid || s.analysisFlights[cacheKey] != nil {
			s.engine.InvalidateScope(cacheKey)
		}
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
	cached.valid = false
	cached.blocked = false
	// Keep one last-success result, not a history of semantic graphs. Its text
	// mappings remain valid until a newer successful analysis replaces both.
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
	return pathsEqual(left.Root, right.Root) && pathsEqual(left.WorkFile, right.WorkFile) &&
		slices.EqualFunc(left.ModuleRoots, right.ModuleRoots, pathsEqual) &&
		slices.Equal(left.Patterns, right.Patterns)
}
