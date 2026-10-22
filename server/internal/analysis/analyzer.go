// Package analysis loads Go modules and workspaces and derives immutable Code
// Lens snapshots for implementation and reference relationships.
package analysis

import (
	"context"

	"golang.org/x/tools/go/packages"
)

// scopeAnalysis optionally carries the immutable indexes required by Engine.
// One-shot callers receive only the compact Snapshot.
type scopeAnalysis struct {
	snapshot Snapshot
	index    *scopeIndex
}

// AnalyzeModule loads all packages in moduleDir and computes exact named type
// implementation relationships and declaration references, including tests.
// Callers must not mutate overlays until AnalyzeModule returns.
func AnalyzeModule(ctx context.Context, moduleDir string, overlays map[string][]byte) (Snapshot, error) {
	return AnalyzeScope(ctx, ModuleAnalysisScope(moduleDir), overlays)
}

// AnalyzeScope loads all packages and test variants in scope together and
// computes relationships across every included module. Callers must not mutate
// overlays until AnalyzeScope returns.
func AnalyzeScope(ctx context.Context, scope AnalysisScope, overlays map[string][]byte) (Snapshot, error) {
	result, err := analyzeScope(ctx, scope, overlays, false)
	return result.snapshot, err
}

func analyzeScope(
	ctx context.Context,
	scope AnalysisScope,
	overlays map[string][]byte,
	retainIndex bool,
) (scopeAnalysis, error) {
	if err := ctx.Err(); err != nil {
		return scopeAnalysis{}, err
	}
	absoluteOverlays, err := canonicalOverlays(overlays)
	if err != nil {
		return scopeAnalysis{}, err
	}
	loaded, err := loadScopePackages(ctx, scope, absoluteOverlays)
	if err != nil {
		return scopeAnalysis{}, err
	}

	packagesToInspect := packagesForAnalysis(loaded.packages)
	facts, err := extractPackageFacts(packagesToInspect, newLocationResolver(loaded.sourceByPath))
	if err != nil {
		return scopeAnalysis{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return scopeAnalysis{}, ctxErr
	}
	result := scopeAnalysis{}
	if retainIndex {
		// Incremental mode pays the one-time indexing cost so later edits can
		// copy and recompute only package-owned buckets and relationship rows.
		result.index, err = buildScopeIndex(scope, packagesToInspect, facts, loaded.sourceByPath)
		if err != nil {
			return scopeAnalysis{}, err
		}
		result.snapshot = snapshotFromIndexes(result.index.facts, result.index.relationships)
	} else {
		// One-shot analysis avoids retaining the type graph and its indexes.
		result.snapshot = snapshotFromFacts(facts)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return scopeAnalysis{}, ctxErr
	}
	return result, nil
}

func analyzeLoadedPackages(loaded []*packages.Package, sourceByPath map[string][]byte) (Snapshot, error) {
	locations := newLocationResolver(sourceByPath)
	facts, err := extractPackageFacts(loaded, locations)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotFromFacts(facts), nil
}
