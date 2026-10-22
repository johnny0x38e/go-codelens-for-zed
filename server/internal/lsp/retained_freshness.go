package lsp

import "github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"

type retainedFileCheck struct {
	uri   string
	stale bool
}

// retainedFreshness protects navigation while a dirty scope cannot reanalyze.
// Editor-managed locations use the buffer mappings. Closed-file locations are
// checked against the saved snapshot with the same fixed stat bound as fresh
// results, so an unreported external rewrite does not cause a known-bad jump.
// No cache or document source is copied on this path.
type retainedFreshness struct {
	snapshot   analysis.Snapshot
	documents  *documentStore
	checked    [maxFreshnessChecks]retainedFileCheck
	count      int
	staleFound bool
}

func (f *retainedFreshness) allows(uri string) bool {
	if f.count == len(f.checked) && !f.staleFound {
		return true
	}
	for _, check := range f.checked[:f.count] {
		if check.uri == uri {
			return !check.stale
		}
	}
	if f.count == len(f.checked) {
		return true
	}
	stale := false
	if !f.documents.IsOpenURI(uri) {
		_, stale = f.snapshot.StaleFile(uri)
	}
	f.staleFound = f.staleFound || stale
	f.checked[f.count] = retainedFileCheck{uri: uri, stale: stale}
	f.count++
	return !stale
}
