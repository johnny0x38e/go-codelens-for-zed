package analysis

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"sync"
)

// ChangeKind describes why a path must be reconsidered by semantic analysis.
type ChangeKind uint8

const (
	// ChangeContent identifies an edit to an existing source file.
	ChangeContent ChangeKind = iota
	// ChangeCreated identifies a newly created file.
	ChangeCreated
	// ChangeDeleted identifies a deleted file.
	ChangeDeleted
	// ChangeMetadata identifies a module or workspace metadata change.
	ChangeMetadata
)

// fallbackReason identifies why Engine chose the complete analyzer instead of
// reusing retained semantic state. Values are internal metric labels and never
// become user-visible protocol errors.
type fallbackReason uint8

const (
	fallbackNone fallbackReason = iota
	fallbackInitialAnalysis
	fallbackScopeChanged
	fallbackModuleMetadata
	fallbackFileCreated
	fallbackFileDeleted
	fallbackUnknownFileOwner
	fallbackRetainedIndex
	fallbackSourceUnavailable
	fallbackParseFailure
	fallbackSourceStructure
	fallbackUncachedImport
	fallbackPackageGraph
	fallbackIncompletePackage
	fallbackSemanticUncertainty
)

func (r fallbackReason) String() string {
	switch r {
	case fallbackNone:
		return "none"
	case fallbackInitialAnalysis:
		return "initial_analysis"
	case fallbackScopeChanged:
		return "scope_changed"
	case fallbackModuleMetadata:
		return "module_metadata"
	case fallbackFileCreated:
		return "file_created"
	case fallbackFileDeleted:
		return "file_deleted"
	case fallbackUnknownFileOwner:
		return "unknown_file_owner"
	case fallbackRetainedIndex:
		return "retained_index"
	case fallbackSourceUnavailable:
		return "source_unavailable"
	case fallbackParseFailure:
		return "parse_failure"
	case fallbackSourceStructure:
		return "source_structure"
	case fallbackUncachedImport:
		return "uncached_import"
	case fallbackPackageGraph:
		return "package_graph"
	case fallbackIncompletePackage:
		return "incomplete_package"
	case fallbackSemanticUncertainty:
		return "semantic_uncertainty"
	default:
		return "unknown"
	}
}

type incrementalFallbackError struct {
	reason fallbackReason
	detail string
}

func (e *incrementalFallbackError) Error() string {
	return errIncrementalFallback.Error() + ": " + e.detail
}

func (*incrementalFallbackError) Unwrap() error {
	return errIncrementalFallback
}

func fallbackReasonFromError(err error) (fallbackReason, bool) {
	fallback, ok := errors.AsType[*incrementalFallbackError](err)
	if !ok {
		return fallbackNone, false
	}
	return fallback.reason, true
}

// Change is one canonical filesystem change captured for an analysis revision.
type Change struct {
	Path string
	Kind ChangeKind
}

// ChangeSet is an immutable, canonical set of filesystem changes. Its zero
// value is a valid empty set.
type ChangeSet struct {
	first   Change
	rest    []Change
	present bool
}

// NewChangeSet canonicalizes and deduplicates changes. Content edits never
// replace a stronger create, delete, or metadata event for the same path;
// otherwise the latest event wins.
func NewChangeSet(changes ...Change) ChangeSet {
	if len(changes) == 0 {
		return ChangeSet{}
	}
	if len(changes) == 1 {
		change := changes[0]
		change.Path = canonicalChangePath(change.Path)
		return singletonChangeSet(change)
	}

	byPath := make(map[string]Change, len(changes))
	for _, change := range changes {
		change.Path = canonicalChangePath(change.Path)
		existing, ok := byPath[change.Path]
		if ok && existing.Kind != ChangeContent && change.Kind == ChangeContent {
			continue
		}
		byPath[change.Path] = change
	}

	canonical := slices.AppendSeq(make([]Change, 0, len(byPath)), maps.Values(byPath))
	slices.SortFunc(canonical, func(left, right Change) int {
		return cmp.Compare(left.Path, right.Path)
	})
	return canonicalChangeSet(canonical)
}

// Changes returns a defensive copy of the canonical changes.
func (s ChangeSet) Changes() []Change {
	if !s.present {
		return nil
	}
	changes := make([]Change, 1, 1+len(s.rest))
	changes[0] = s.first
	return append(changes, s.rest...)
}

// Empty reports whether the set contains no changes.
func (s ChangeSet) Empty() bool {
	return !s.present
}

// Merge returns a new set containing s followed by newer changes from next.
func (s ChangeSet) Merge(next ChangeSet) ChangeSet {
	if s.Empty() {
		return next
	}
	if next.Empty() {
		return s
	}
	if changeSetsEqual(s, next) {
		return s
	}
	if len(s.rest) == 0 && len(next.rest) == 0 && s.first.Path == next.first.Path {
		if s.first.Kind != ChangeContent && next.first.Kind == ChangeContent {
			return s
		}
		return next
	}

	merged := make([]Change, 0, s.len()+next.len())
	leftIndex, rightIndex := 0, 0
	for leftIndex < s.len() && rightIndex < next.len() {
		left, right := s.at(leftIndex), next.at(rightIndex)
		switch cmp.Compare(left.Path, right.Path) {
		case -1:
			merged = append(merged, left)
			leftIndex++
		case 1:
			merged = append(merged, right)
			rightIndex++
		default:
			if left.Kind != ChangeContent && right.Kind == ChangeContent {
				merged = append(merged, left)
			} else {
				merged = append(merged, right)
			}
			leftIndex++
			rightIndex++
		}
	}
	for ; leftIndex < s.len(); leftIndex++ {
		merged = append(merged, s.at(leftIndex))
	}
	for ; rightIndex < next.len(); rightIndex++ {
		merged = append(merged, next.at(rightIndex))
	}
	return canonicalChangeSet(merged)
}

func singletonChangeSet(change Change) ChangeSet {
	return ChangeSet{first: change, present: true}
}

func canonicalChangeSet(changes []Change) ChangeSet {
	if len(changes) == 0 {
		return ChangeSet{}
	}
	return ChangeSet{first: changes[0], rest: changes[1:], present: true}
}

func (s ChangeSet) len() int {
	if !s.present {
		return 0
	}
	return 1 + len(s.rest)
}

func (s ChangeSet) at(index int) Change {
	if index == 0 {
		return s.first
	}
	return s.rest[index-1]
}

func changeSetsEqual(left, right ChangeSet) bool {
	if left.present != right.present || left.first != right.first || len(left.rest) != len(right.rest) {
		return false
	}
	return slices.Equal(left.rest, right.rest)
}

type engineScopeState struct {
	scope    AnalysisScope
	snapshot Snapshot
	index    *scopeIndex
}

// engineScopeControl is a non-reusable publication token for one cache key.
// Analyses retain the pointer while running; invalidation replaces it and
// forgetting removes it, so pointer identity rejects obsolete publication
// without retaining historical cache keys. active is protected by Engine.mu.
type engineScopeControl struct {
	active int
}

// Engine owns reusable semantic state for analysis scopes. Known content-only
// edits reuse the retained package graph; lifecycle, metadata, and unsafe
// structural changes use the complete analyzer as a correctness fallback.
type Engine struct {
	mu            sync.RWMutex
	states        map[string]engineScopeState
	scopeControls map[string]*engineScopeControl
	analyze       func(context.Context, AnalysisScope, map[string][]byte) (scopeAnalysis, error)
	// observeFallback is an internal tracing hook. Production leaves it nil, so
	// the hot path pays only one predictable branch when a full load is needed.
	observeFallback func(fallbackReason)
}

// NewEngine creates an empty semantic analysis engine.
func NewEngine() *Engine {
	return &Engine{
		states:        make(map[string]engineScopeState),
		scopeControls: make(map[string]*engineScopeControl),
		analyze: func(
			ctx context.Context,
			scope AnalysisScope,
			overlays map[string][]byte,
		) (scopeAnalysis, error) {
			return analyzeScope(ctx, scope, overlays, true)
		},
	}
}

// Analyze returns a cached unchanged snapshot when possible and incrementally
// rechecks known content edits. Any edit that cannot be proven safe performs
// one complete analysis and replaces the retained immutable state.
func (e *Engine) Analyze(
	ctx context.Context,
	scope AnalysisScope,
	changes ChangeSet,
	overlays map[string][]byte,
) (Snapshot, error) {
	cacheKey := scope.CacheKey()
	emptyChanges := changes.Empty()
	e.mu.RLock()
	state, found := e.states[cacheKey]
	sameScope := found && sameEngineScope(state.scope, scope)
	if sameScope && emptyChanges {
		e.mu.RUnlock()
		return state.snapshot, nil
	}
	e.mu.RUnlock()

	// Slow work needs a publication lease. Re-read state under the write lock:
	// another analysis may have published after the optimistic read above.
	state, found, sameScope, control := e.beginAnalysis(cacheKey, scope, emptyChanges)
	if control == nil {
		return state.snapshot, nil
	}
	reason := fallbackInitialAnalysis
	if found {
		switch {
		case !sameScope:
			reason = fallbackScopeChanged
		case state.index == nil:
			reason = fallbackRetainedIndex
		}
	}
	if sameScope && state.index != nil && !changes.Empty() {
		// Incremental failures are typed correctness decisions, not best-effort
		// errors: only an explicitly safe result is ever published.
		plan := state.index.reloadPlanFor(changes)
		if !plan.full {
			index, err := recheckPackages(ctx, state.index, plan, overlays)
			if err == nil {
				result := scopeAnalysis{
					snapshot: applyLensChanges(
						state.snapshot,
						state.index.facts,
						index.facts,
						index.relationships,
						index.changedLenses,
					),
					index: index,
				}
				attachFileIdentities(&result)
				e.publish(cacheKey, control, scope, result)
				return result.snapshot, nil
			}
			if !errors.Is(err, errIncrementalFallback) {
				e.releaseAnalysis(cacheKey, control)
				return Snapshot{}, err
			}
			if typedReason, ok := fallbackReasonFromError(err); ok {
				reason = typedReason
			} else {
				reason = fallbackSemanticUncertainty
			}
		} else {
			reason = plan.reason
		}
	}

	if e.observeFallback != nil {
		e.observeFallback(reason)
	}
	result, err := e.analyze(ctx, scope, overlays)
	if err != nil {
		e.releaseAnalysis(cacheKey, control)
		return Snapshot{}, err
	}

	attachFileIdentities(&result)
	e.publish(cacheKey, control, scope, result)
	return result.snapshot, nil
}

// attachFileIdentities binds the identity map of the analysis revision to the
// snapshot returned to callers. Rechecking refreshes identities only for the
// affected compiled files, so unchanged entries stay shared.
func attachFileIdentities(result *scopeAnalysis) {
	if result.index == nil {
		return
	}
	result.snapshot = result.snapshot.withFileIdentities(result.index.fileIdentities)
}

func (e *Engine) beginAnalysis(
	cacheKey string,
	scope AnalysisScope,
	emptyChanges bool,
) (engineScopeState, bool, bool, *engineScopeControl) {
	e.mu.Lock()
	defer e.mu.Unlock()

	state, found := e.states[cacheKey]
	sameScope := found && sameEngineScope(state.scope, scope)
	if sameScope && emptyChanges {
		return state, found, sameScope, nil
	}

	control := e.scopeControls[cacheKey]
	if control == nil {
		control = &engineScopeControl{}
		e.scopeControls[cacheKey] = control
	}
	control.active++
	return state, found, sameScope, control
}

func (e *Engine) publish(
	cacheKey string,
	control *engineScopeControl,
	scope AnalysisScope,
	result scopeAnalysis,
) {
	e.finishAnalysis(cacheKey, control, scope, &result)
}

func (e *Engine) releaseAnalysis(cacheKey string, control *engineScopeControl) {
	e.finishAnalysis(cacheKey, control, AnalysisScope{}, nil)
}

func (e *Engine) finishAnalysis(
	cacheKey string,
	control *engineScopeControl,
	scope AnalysisScope,
	result *scopeAnalysis,
) {
	e.mu.Lock()
	current := e.scopeControls[cacheKey] == control
	// Pointer identity is the commit point for an analysis revision. No partially
	// constructed index is visible before this lock is acquired.
	if current && result != nil {
		e.states[cacheKey] = engineScopeState{
			scope:    cloneAnalysisScope(scope),
			snapshot: result.snapshot,
			index:    result.index,
		}
	}
	control.active--
	if current && control.active == 0 {
		if _, retained := e.states[cacheKey]; !retained {
			delete(e.scopeControls, cacheKey)
		}
	}
	e.mu.Unlock()
}

// ForgetScope releases reusable state for cacheKey and prevents an analysis
// that started earlier from publishing stale state after it returns.
func (e *Engine) ForgetScope(cacheKey string) {
	e.mu.Lock()
	delete(e.states, cacheKey)
	delete(e.scopeControls, cacheKey)
	e.mu.Unlock()
}

// InvalidateScope prevents in-flight work from publishing an obsolete result
// while retaining the last complete state for the next incremental analysis.
func (e *Engine) InvalidateScope(cacheKey string) {
	e.mu.Lock()
	if _, retained := e.states[cacheKey]; retained {
		e.scopeControls[cacheKey] = &engineScopeControl{}
	} else {
		// Removing the current pointer is sufficient to invalidate analyses that
		// captured it. A later analysis creates a distinct control on demand.
		delete(e.scopeControls, cacheKey)
	}
	e.mu.Unlock()
}

func canonicalChangePath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(absolute)
}

func sameEngineScope(left, right AnalysisScope) bool {
	return left.Root == right.Root &&
		left.WorkFile == right.WorkFile &&
		slices.Equal(left.ModuleRoots, right.ModuleRoots) &&
		slices.Equal(left.Patterns, right.Patterns)
}
