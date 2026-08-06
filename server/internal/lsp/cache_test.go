package lsp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
)

func TestSnapshotCacheInvalidatesOnlyChangedScope(t *testing.T) {
	firstRoot := filepath.Join(t.TempDir(), "first")
	secondRoot := filepath.Join(t.TempDir(), "second")
	firstScope := analysis.ModuleAnalysisScope(firstRoot)
	secondScope := analysis.ModuleAnalysisScope(secondRoot)
	state := newServerState()

	callCounts := make(map[string]int)
	state.analyzeScope = func(_ context.Context, scope analysis.AnalysisScope, _ map[string][]byte) (analysis.Snapshot, error) {
		callCounts[scope.CacheKey()]++
		return analysis.Snapshot{}, nil
	}

	if _, err := state.snapshotForScope(context.Background(), firstScope); err != nil {
		t.Fatal(err)
	}
	if _, err := state.snapshotForScope(context.Background(), secondScope); err != nil {
		t.Fatal(err)
	}

	params, err := json.Marshal(map[string]any{
		"textDocument":   map[string]any{"uri": semanticTestFileURI(filepath.Join(firstRoot, "sample.go"))},
		"contentChanges": []map[string]any{{"text": "package first\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	state.handleNotification("textDocument/didChange", params)

	if _, err := state.snapshotForScope(context.Background(), secondScope); err != nil {
		t.Fatal(err)
	}
	if _, err := state.snapshotForScope(context.Background(), firstScope); err != nil {
		t.Fatal(err)
	}

	if got := callCounts[firstScope.CacheKey()]; got != 2 {
		t.Fatalf("first scope analysis calls = %d, want 2", got)
	}
	if got := callCounts[secondScope.CacheKey()]; got != 1 {
		t.Fatalf("second scope analysis calls = %d, want 1", got)
	}
}

func TestSnapshotInvalidationReleasesStaleSemanticData(t *testing.T) {
	moduleRoot := t.TempDir()
	writeSemanticTestFile(t, filepath.Join(moduleRoot, "go.mod"), []byte("module example.com/release\n\ngo 1.26\n"))
	filename := filepath.Join(moduleRoot, "release.go")
	writeSemanticTestFile(t, filename, []byte(`package release

type Runner interface { Run() }
type Worker struct{}
func (Worker) Run() {}
`))

	state := newServerState()
	scope := analysis.ModuleAnalysisScope(moduleRoot)
	snapshot, err := state.snapshotForScope(context.Background(), scope)
	if err != nil {
		t.Fatalf("prime snapshot: %v", err)
	}
	uri := semanticTestFileURI(filename)
	if got := len(snapshot.Lenses(uri)); got == 0 {
		t.Fatal("primed snapshot has no semantic lenses")
	}

	state.invalidateSnapshotsForPath(filename)
	state.cacheMu.RLock()
	cached := state.snapshots[scope.CacheKey()]
	state.cacheMu.RUnlock()
	if cached.valid {
		t.Fatal("invalidated snapshot remains valid")
	}
	if got := len(cached.snapshot.Lenses(uri)); got != 0 {
		t.Fatalf("invalidated cache retains %d semantic lenses, want 0", got)
	}
}

func TestSnapshotForScopeCoalescesConcurrentAnalysis(t *testing.T) {
	state := newServerState()
	scope := analysis.ModuleAnalysisScope(t.TempDir())
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var calls atomic.Int64
	state.analyzeScope = func(_ context.Context, _ analysis.AnalysisScope, _ map[string][]byte) (analysis.Snapshot, error) {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		<-release
		return analysis.Snapshot{}, nil
	}

	const requestCount = 16
	errors := make(chan error, requestCount)
	var requests sync.WaitGroup
	requests.Add(requestCount)
	for range requestCount {
		go func() {
			defer requests.Done()
			_, err := state.snapshotForScope(context.Background(), scope)
			errors <- err
		}()
	}
	<-started
	close(release)
	requests.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent analysis calls = %d, want 1", got)
	}
}

func TestAnalysisScopeCacheUsesDirectoriesAndMetadataInvalidation(t *testing.T) {
	t.Parallel()

	moduleRoot := t.TempDir()
	state := newServerState()
	var calls int
	state.findScope = func(filename, workspaceRoot string) (analysis.AnalysisScope, error) {
		calls++
		if workspaceRoot != "" {
			t.Fatalf("workspace root = %q, want empty", workspaceRoot)
		}
		return analysis.ModuleAnalysisScope(moduleRoot), nil
	}

	firstFile := filepath.Join(moduleRoot, "package", "first.go")
	secondFile := filepath.Join(moduleRoot, "package", "second.go")
	if _, err := state.analysisScopeFor(firstFile); err != nil {
		t.Fatal(err)
	}
	if _, err := state.analysisScopeFor(secondFile); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("scope finder calls for one directory = %d, want 1", calls)
	}

	state.handleNotification("workspace/didChangeWatchedFiles", watchedFilesParams(t, filepath.Join(moduleRoot, "package", "first.go")))
	if _, err := state.analysisScopeFor(firstFile); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("scope finder calls after Go source change = %d, want 1", calls)
	}

	state.handleNotification("workspace/didChangeWatchedFiles", watchedFilesParams(t, filepath.Join(moduleRoot, "go.mod")))
	if _, err := state.analysisScopeFor(firstFile); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("scope finder calls after go.mod change = %d, want 2", calls)
	}
}

func TestWatchedFileChangeInvalidatesOnlyContainingScope(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	firstScope := analysis.ModuleAnalysisScope(filepath.Join(workspaceRoot, "first"))
	secondScope := analysis.ModuleAnalysisScope(filepath.Join(workspaceRoot, "second"))
	state := newServerState()

	callCounts := make(map[string]int)
	state.analyzeScope = func(_ context.Context, scope analysis.AnalysisScope, _ map[string][]byte) (analysis.Snapshot, error) {
		callCounts[scope.CacheKey()]++
		return analysis.Snapshot{}, nil
	}
	if _, err := state.snapshotForScope(context.Background(), firstScope); err != nil {
		t.Fatal(err)
	}
	if _, err := state.snapshotForScope(context.Background(), secondScope); err != nil {
		t.Fatal(err)
	}

	state.handleNotification("workspace/didChangeWatchedFiles", watchedFilesParams(t, filepath.Join(firstScope.Root, "changed.go")))
	if _, err := state.snapshotForScope(context.Background(), firstScope); err != nil {
		t.Fatal(err)
	}
	if _, err := state.snapshotForScope(context.Background(), secondScope); err != nil {
		t.Fatal(err)
	}
	if got := callCounts[firstScope.CacheKey()]; got != 2 {
		t.Fatalf("first scope analysis calls = %d, want 2", got)
	}
	if got := callCounts[secondScope.CacheKey()]; got != 1 {
		t.Fatalf("second scope analysis calls = %d, want 1", got)
	}
}

func TestDocumentPathCacheIsReleasedOnClose(t *testing.T) {
	t.Parallel()

	state := newServerState()
	uri := semanticTestFileURI(filepath.Join(t.TempDir(), "sample.go"))
	if _, err := state.documentPathFor(uri); err != nil {
		t.Fatal(err)
	}
	if len(state.documentPaths) != 1 {
		t.Fatalf("document path cache length = %d, want 1", len(state.documentPaths))
	}
	params, err := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
	})
	if err != nil {
		t.Fatal(err)
	}
	state.handleNotification("textDocument/didClose", params)
	if len(state.documentPaths) != 0 {
		t.Fatalf("document path cache length after close = %d, want 0", len(state.documentPaths))
	}
}

func watchedFilesParams(t *testing.T, paths ...string) json.RawMessage {
	t.Helper()
	changes := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		changes = append(changes, map[string]any{"uri": semanticTestFileURI(path), "type": 2})
	}
	params, err := json.Marshal(map[string]any{"changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	return params
}
