package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

func TestAnalysisScopeCacheEvictsOldestInsertionWithoutPromotingHits(t *testing.T) {
	root := t.TempDir()
	state := newServerState()
	state.findScope = func(filename, _ string) (analysis.AnalysisScope, error) {
		return analysis.ModuleAnalysisScope(filepath.Dir(filename)), nil
	}

	files := make([]string, 0, maxAnalysisScopeCacheEntries+1)
	for index := range maxAnalysisScopeCacheEntries {
		filename := filepath.Join(root, strconv.Itoa(index), "sample.go")
		files = append(files, filename)
		if _, err := state.analysisScopeFor(filename); err != nil {
			t.Fatalf("analysisScopeFor(%d) error = %v", index, err)
		}
	}
	if _, err := state.analysisScopeFor(files[0]); err != nil {
		t.Fatalf("hot analysisScopeFor() error = %v", err)
	}

	overflowFile := filepath.Join(root, "overflow", "sample.go")
	if _, err := state.analysisScopeFor(overflowFile); err != nil {
		t.Fatalf("overflow analysisScopeFor() error = %v", err)
	}

	state.metadataMu.RLock()
	defer state.metadataMu.RUnlock()
	if got := len(state.analysisScopes); got != maxAnalysisScopeCacheEntries {
		t.Fatalf("analysis scope cache length = %d, want %d", got, maxAnalysisScopeCacheEntries)
	}
	firstKey := analysisScopeCacheKey{directory: filepath.Dir(files[0])}
	if _, exists := state.analysisScopes[firstKey]; exists {
		t.Fatal("hot read promoted the oldest analysis scope entry")
	}
	secondKey := analysisScopeCacheKey{directory: filepath.Dir(files[1])}
	if _, exists := state.analysisScopes[secondKey]; !exists {
		t.Fatal("analysis scope cache evicted a newer entry")
	}
}

func TestAnalysisScopeMetadataInvalidationClearsEvictionRing(t *testing.T) {
	root := t.TempDir()
	state := newServerState()
	state.findScope = func(filename, _ string) (analysis.AnalysisScope, error) {
		return analysis.ModuleAnalysisScope(filepath.Dir(filename)), nil
	}
	for index := range 3 {
		filename := filepath.Join(root, strconv.Itoa(index), "sample.go")
		if _, err := state.analysisScopeFor(filename); err != nil {
			t.Fatalf("analysisScopeFor(%d) error = %v", index, err)
		}
	}

	state.invalidateAnalysisScopes()
	state.metadataMu.RLock()
	defer state.metadataMu.RUnlock()
	if len(state.analysisScopes) != 0 || len(state.analysisScopeKeys) != 0 || state.analysisScopeCursor != 0 {
		t.Fatalf(
			"invalidated scope cache retains map=%d keys=%d cursor=%d",
			len(state.analysisScopes),
			len(state.analysisScopeKeys),
			state.analysisScopeCursor,
		)
	}
}

func TestSnapshotCacheEvictionCancelsFlightAndForgetsEngineScope(t *testing.T) {
	root := t.TempDir()
	state := newServerState()
	firstScope := analysis.ModuleAnalysisScope(filepath.Join(root, "0"))
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	state.analyzeScope = func(
		ctx context.Context,
		scope analysis.AnalysisScope,
		_ analysis.ChangeSet,
		_ map[string][]byte,
	) (analysis.Snapshot, error) {
		if scope.CacheKey() == firstScope.CacheKey() {
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			return analysis.Snapshot{}, ctx.Err()
		}
		return analysis.Snapshot{}, nil
	}
	forgotten := make(map[string]int)
	state.forgetScope = func(cacheKey string) {
		forgotten[cacheKey]++
	}

	_, firstFlight, cached := state.snapshotOrFlight(firstScope)
	if cached || firstFlight == nil {
		t.Fatal("first scope did not start an analysis flight")
	}
	<-firstStarted
	for index := 1; index < maxSnapshotCacheEntries; index++ {
		scope := analysis.ModuleAnalysisScope(filepath.Join(root, strconv.Itoa(index)))
		if _, err := state.snapshotForScope(context.Background(), scope); err != nil {
			t.Fatalf("snapshotForScope(%d) error = %v", index, err)
		}
	}
	overflowScope := analysis.ModuleAnalysisScope(filepath.Join(root, "overflow"))
	if _, err := state.snapshotForScope(context.Background(), overflowScope); err != nil {
		t.Fatalf("overflow snapshotForScope() error = %v", err)
	}

	select {
	case <-firstCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("evicted analysis flight was not canceled")
	}
	<-firstFlight.done
	if _, err := state.consumeAnalysisFlight(firstFlight); !errors.Is(err, errAnalysisObsolete) {
		t.Fatalf("evicted analysis flight error = %v, want %v", err, errAnalysisObsolete)
	}

	state.cacheMu.RLock()
	defer state.cacheMu.RUnlock()
	if got := len(state.snapshots); got != maxSnapshotCacheEntries {
		t.Fatalf("snapshot cache length = %d, want %d", got, maxSnapshotCacheEntries)
	}
	if _, exists := state.snapshots[firstScope.CacheKey()]; exists {
		t.Fatal("snapshot eviction retained the oldest semantic snapshot")
	}
	if state.analysisFlights[firstScope.CacheKey()] != nil {
		t.Fatal("snapshot eviction retained the canceled analysis flight")
	}
	if got := forgotten[firstScope.CacheKey()]; got != 1 {
		t.Fatalf("Engine.ForgetScope calls = %d, want 1", got)
	}
}

func TestSnapshotCacheHitDoesNotPromoteInsertionOrder(t *testing.T) {
	root := t.TempDir()
	state := newServerState()
	state.analyzeScope = func(
		_ context.Context,
		_ analysis.AnalysisScope,
		_ analysis.ChangeSet,
		_ map[string][]byte,
	) (analysis.Snapshot, error) {
		return analysis.Snapshot{}, nil
	}
	forgotten := make(map[string]int)
	state.forgetScope = func(cacheKey string) {
		forgotten[cacheKey]++
	}

	firstScope := analysis.ModuleAnalysisScope(filepath.Join(root, "0"))
	for index := range maxSnapshotCacheEntries {
		scope := analysis.ModuleAnalysisScope(filepath.Join(root, strconv.Itoa(index)))
		if _, err := state.snapshotForScope(context.Background(), scope); err != nil {
			t.Fatalf("snapshotForScope(%d) error = %v", index, err)
		}
	}
	if _, err := state.snapshotForScope(context.Background(), firstScope); err != nil {
		t.Fatalf("hot snapshotForScope() error = %v", err)
	}
	if _, err := state.snapshotForScope(
		context.Background(),
		analysis.ModuleAnalysisScope(filepath.Join(root, "overflow")),
	); err != nil {
		t.Fatalf("overflow snapshotForScope() error = %v", err)
	}

	state.cacheMu.RLock()
	defer state.cacheMu.RUnlock()
	if _, exists := state.snapshots[firstScope.CacheKey()]; exists {
		t.Fatal("snapshot cache hit promoted the oldest insertion")
	}
	if got := forgotten[firstScope.CacheKey()]; got != 1 {
		t.Fatalf("Engine.ForgetScope calls = %d, want 1", got)
	}
}

func TestSnapshotCacheInvalidatesOnlyChangedScope(t *testing.T) {
	firstRoot := filepath.Join(t.TempDir(), "first")
	secondRoot := filepath.Join(t.TempDir(), "second")
	firstScope := analysis.ModuleAnalysisScope(firstRoot)
	secondScope := analysis.ModuleAnalysisScope(secondRoot)
	state := newServerState()

	callCounts := make(map[string]int)
	state.analyzeScope = func(_ context.Context, scope analysis.AnalysisScope, _ analysis.ChangeSet, _ map[string][]byte) (analysis.Snapshot, error) {
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
		"textDocument": map[string]any{
			"uri":     semanticTestFileURI(t, filepath.Join(firstRoot, "sample.go")),
			"version": 1,
			"text":    "package first\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.handleNotification("textDocument/didOpen", params); err != nil {
		t.Fatalf("handle didOpen: %v", err)
	}

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
	uri := semanticTestFileURI(t, filename)
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

func TestInvalidIncrementalNotificationPreservesSnapshot(t *testing.T) {
	moduleRoot := t.TempDir()
	writeSemanticTestFile(
		t,
		filepath.Join(moduleRoot, "go.mod"),
		[]byte("module example.com/invalid-edit\n\ngo 1.26\n"),
	)
	filename := filepath.Join(moduleRoot, "sample.go")
	source := []byte("package sample\n")
	writeSemanticTestFile(t, filename, source)
	uri := semanticTestFileURI(t, filename)

	state := newServerState()
	state.workspaceRoots = []string{moduleRoot}
	openParams, err := json.Marshal(didOpenTextDocumentParams{
		TextDocument: openedTextDocument{URI: uri, Version: 1, Text: string(source)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if openErr := state.handleNotification("textDocument/didOpen", openParams); openErr != nil {
		t.Fatalf("handle didOpen: %v", openErr)
	}

	scope := analysis.ModuleAnalysisScope(moduleRoot)
	if _, snapshotErr := state.snapshotForScope(context.Background(), scope); snapshotErr != nil {
		t.Fatalf("prime snapshot: %v", snapshotErr)
	}
	state.cacheMu.RLock()
	before := state.snapshots[scope.CacheKey()]
	state.cacheMu.RUnlock()

	invalidParams, err := json.Marshal(didChangeTextDocumentParams{
		TextDocument: versionedTextDocumentIdentifier{URI: uri, Version: 2},
		ContentChanges: []textDocumentContentChangeEvent{
			textBufferRangeChange(99, 0, 99, 0, "invalid"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.handleNotification("textDocument/didChange", invalidParams); err == nil {
		t.Fatal("invalid didChange succeeded")
	}

	state.cacheMu.RLock()
	after := state.snapshots[scope.CacheKey()]
	state.cacheMu.RUnlock()
	if !after.valid {
		t.Fatal("invalid didChange invalidated the cached snapshot")
	}
	if after.revision != before.revision {
		t.Fatalf("snapshot revision after invalid didChange = %d, want %d", after.revision, before.revision)
	}
}

func TestSnapshotFlightRetainsChangesAcrossInvalidation(t *testing.T) {
	state := newServerState()
	scope := analysis.ModuleAnalysisScope(t.TempDir())
	firstPath := filepath.Join(scope.Root, "first.go")
	secondPath := filepath.Join(scope.Root, "second.go")
	secondStarted := make(chan struct{})

	var calls atomic.Int64
	captured := make([]analysis.ChangeSet, 0, 3)
	state.analyzeScope = func(
		ctx context.Context,
		_ analysis.AnalysisScope,
		changes analysis.ChangeSet,
		_ map[string][]byte,
	) (analysis.Snapshot, error) {
		captured = append(captured, changes)
		if calls.Add(1) == 2 {
			close(secondStarted)
			<-ctx.Done()
			return analysis.Snapshot{}, ctx.Err()
		}
		return analysis.Snapshot{}, nil
	}

	if _, snapshotErr := state.snapshotForScope(context.Background(), scope); snapshotErr != nil {
		t.Fatalf("prime snapshot: %v", snapshotErr)
	}
	state.invalidateSnapshotsForChange(analysis.Change{Path: firstPath, Kind: analysis.ChangeContent})

	result := make(chan error, 1)
	go func() {
		_, err := state.snapshotForScope(context.Background(), scope)
		result <- err
	}()
	<-secondStarted
	state.invalidateSnapshotsForChange(analysis.Change{Path: secondPath, Kind: analysis.ChangeCreated})
	if err := <-result; err != nil {
		t.Fatalf("snapshot after invalidation: %v", err)
	}

	if got := captured[0].Changes(); len(got) != 0 {
		t.Fatalf("initial analysis changes = %#v, want none", got)
	}
	wantSecond := analysis.NewChangeSet(analysis.Change{Path: firstPath, Kind: analysis.ChangeContent}).Changes()
	if got := captured[1].Changes(); !reflect.DeepEqual(got, wantSecond) {
		t.Fatalf("obsolete flight changes = %#v, want %#v", got, wantSecond)
	}
	wantThird := analysis.NewChangeSet(
		analysis.Change{Path: firstPath, Kind: analysis.ChangeContent},
		analysis.Change{Path: secondPath, Kind: analysis.ChangeCreated},
	).Changes()
	if got := captured[2].Changes(); !reflect.DeepEqual(got, wantThird) {
		t.Fatalf("replacement flight changes = %#v, want %#v", got, wantThird)
	}

	state.cacheMu.RLock()
	pending := state.snapshots[scope.CacheKey()].pendingChanges
	state.cacheMu.RUnlock()
	if got := pending.Changes(); len(got) != 0 {
		t.Fatalf("pending changes after successful snapshot = %#v, want none", got)
	}
}

func TestWatchedFileChangesPreserveLifecycleKinds(t *testing.T) {
	root := t.TempDir()
	createdPath := filepath.Join(root, "created.go")
	changedPath := filepath.Join(root, "changed.go")
	deletedPath := filepath.Join(root, "deleted.go")
	metadataPath := filepath.Join(root, "go.mod")
	params, err := json.Marshal(didChangeWatchedFilesParams{
		Changes: []fileEvent{
			{URI: semanticTestFileURI(t, createdPath), Type: watchedFileCreated},
			{URI: semanticTestFileURI(t, changedPath), Type: watchedFileChanged},
			{URI: semanticTestFileURI(t, deletedPath), Type: watchedFileDeleted},
			{URI: semanticTestFileURI(t, metadataPath), Type: watchedFileChanged},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	changes, err := watchedFileChanges(params)
	if err != nil {
		t.Fatalf("watchedFileChanges() error = %v", err)
	}
	want := analysis.NewChangeSet(
		analysis.Change{Path: createdPath, Kind: analysis.ChangeCreated},
		analysis.Change{Path: changedPath, Kind: analysis.ChangeContent},
		analysis.Change{Path: deletedPath, Kind: analysis.ChangeDeleted},
		analysis.Change{Path: metadataPath, Kind: analysis.ChangeMetadata},
	).Changes()
	if got := analysis.NewChangeSet(changes...).Changes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("watchedFileChanges() = %#v, want %#v", got, want)
	}
}

func TestSnapshotForScopeCoalescesConcurrentAnalysis(t *testing.T) {
	state := newServerState()
	scope := analysis.ModuleAnalysisScope(t.TempDir())
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var calls atomic.Int64
	state.analyzeScope = func(_ context.Context, _ analysis.AnalysisScope, _ analysis.ChangeSet, _ map[string][]byte) (analysis.Snapshot, error) {
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

func TestCompletedSnapshotFlightReleasesResult(t *testing.T) {
	state := newServerState()
	root := t.TempDir()
	writeSemanticTestFile(t, filepath.Join(root, "go.mod"), []byte("module example.com/flight-release\n\ngo 1.26\n"))
	filename := filepath.Join(root, "sample.go")
	writeSemanticTestFile(t, filename, []byte("package sample\n\nfunc Value() {}\nvar Use = Value\n"))
	scope := analysis.ModuleAnalysisScope(root)
	want, err := analysis.AnalyzeScope(context.Background(), scope, nil)
	if err != nil {
		t.Fatalf("AnalyzeScope() error = %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	uri := semanticTestFileURI(t, filename)
	state.analyzeScope = func(_ context.Context, _ analysis.AnalysisScope, _ analysis.ChangeSet, _ map[string][]byte) (analysis.Snapshot, error) {
		close(started)
		<-release
		return want, nil
	}

	result := make(chan analysis.Snapshot, 1)
	resultErr := make(chan error, 1)
	go func() {
		snapshot, snapshotErr := state.snapshotForScope(context.Background(), scope)
		result <- snapshot
		resultErr <- snapshotErr
	}()
	<-started
	state.cacheMu.RLock()
	flight := state.analysisFlights[scope.CacheKey()]
	state.cacheMu.RUnlock()
	if flight == nil {
		t.Fatal("analysis flight was not registered")
	}
	close(release)
	snapshot := <-result
	if err := <-resultErr; err != nil {
		t.Fatalf("snapshotForScope() error = %v", err)
	}
	if got := len(snapshot.Lenses(uri)); got != 1 {
		t.Fatalf("returned snapshot Lens count = %d, want 1", got)
	}

	state.cacheMu.RLock()
	defer state.cacheMu.RUnlock()
	if flight.waiters != 0 {
		t.Fatalf("completed flight waiters = %d, want 0", flight.waiters)
	}
	if got := len(flight.snapshot.Lenses(uri)); got != 0 {
		t.Fatalf("completed flight retains %d lenses, want 0", got)
	}
}

func TestIncrementalFlightLifetimeRemainsBounded(t *testing.T) {
	state := newServerState()
	scope := analysis.ModuleAnalysisScope(t.TempDir())
	changedPath := filepath.Join(scope.Root, "sample.go")
	state.analyzeScope = func(_ context.Context, _ analysis.AnalysisScope, _ analysis.ChangeSet, _ map[string][]byte) (analysis.Snapshot, error) {
		return analysis.Snapshot{}, nil
	}

	for revision := 1; revision <= 1_000; revision++ {
		state.invalidateSnapshotsForPath(changedPath)
		if _, err := state.snapshotForScope(context.Background(), scope); err != nil {
			t.Fatalf("snapshotForScope() revision %d error = %v", revision, err)
		}
		state.cacheMu.RLock()
		snapshotCount := len(state.snapshots)
		flightCount := len(state.analysisFlights)
		state.cacheMu.RUnlock()
		if snapshotCount != 1 || flightCount != 0 {
			t.Fatalf(
				"revision %d cache counts = snapshots %d, flights %d; want 1, 0",
				revision,
				snapshotCount,
				flightCount,
			)
		}
	}
}

func TestSnapshotForScopeCancelsAndRetriesObsoleteAnalysis(t *testing.T) {
	state := newServerState()
	scope := analysis.ModuleAnalysisScope(t.TempDir())
	started := make(chan struct{})
	canceled := make(chan struct{})
	var calls atomic.Int64
	state.analyzeScope = func(ctx context.Context, _ analysis.AnalysisScope, _ analysis.ChangeSet, _ map[string][]byte) (analysis.Snapshot, error) {
		if calls.Add(1) != 1 {
			return analysis.Snapshot{}, nil
		}
		close(started)
		<-ctx.Done()
		close(canceled)
		return analysis.Snapshot{}, ctx.Err()
	}

	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRequest()
	result := make(chan error, 1)
	go func() {
		_, err := state.snapshotForScope(requestCtx, scope)
		result <- err
	}()

	<-started
	state.invalidateSnapshotsForPath(filepath.Join(scope.Root, "changed.go"))
	select {
	case <-canceled:
	case <-requestCtx.Done():
		t.Fatal("obsolete analysis was not canceled")
	}
	if err := <-result; err != nil {
		t.Fatalf("snapshotForScope() error = %v, want retry success", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("analysis calls = %d, want 2", got)
	}
}

func TestSnapshotForScopeRequestCancellationPreservesSharedAnalysis(t *testing.T) {
	state := newServerState()
	scope := analysis.ModuleAnalysisScope(t.TempDir())
	started := make(chan struct{})
	release := make(chan struct{})
	state.analyzeScope = func(ctx context.Context, _ analysis.AnalysisScope, _ analysis.ChangeSet, _ map[string][]byte) (analysis.Snapshot, error) {
		close(started)
		select {
		case <-release:
			return analysis.Snapshot{}, nil
		case <-ctx.Done():
			return analysis.Snapshot{}, ctx.Err()
		}
	}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	go func() {
		_, err := state.snapshotForScope(firstCtx, scope)
		firstResult <- err
	}()
	<-started
	go func() {
		_, err := state.snapshotForScope(context.Background(), scope)
		secondResult <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		state.cacheMu.RLock()
		flight := state.analysisFlights[scope.CacheKey()]
		joined := flight != nil && flight.waiters == 2
		state.cacheMu.RUnlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second request did not join shared analysis")
		}
		runtime.Gosched()
	}

	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first request error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-secondResult; err != nil {
		t.Fatalf("second request error = %v, want shared analysis success", err)
	}
}

func TestSnapshotForScopeFinalCancellationDoesNotPoisonNextRequest(t *testing.T) {
	state := newServerState()
	scope := analysis.ModuleAnalysisScope(t.TempDir())
	started := make(chan struct{})
	canceled := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(release)

	var calls atomic.Int64
	state.analyzeScope = func(ctx context.Context, _ analysis.AnalysisScope, _ analysis.ChangeSet, _ map[string][]byte) (analysis.Snapshot, error) {
		if calls.Add(1) != 1 {
			return analysis.Snapshot{}, nil
		}
		close(started)
		<-ctx.Done()
		close(canceled)
		<-releaseFirst
		return analysis.Snapshot{}, ctx.Err()
	}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := state.snapshotForScope(firstCtx, scope)
		firstResult <- err
	}()
	<-started

	state.cacheMu.RLock()
	firstFlight := state.analysisFlights[scope.CacheKey()]
	state.cacheMu.RUnlock()
	if firstFlight == nil {
		t.Fatal("first analysis flight was not registered")
	}

	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first request error = %v, want context.Canceled", err)
	}
	<-canceled

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSecond()
	if _, err := state.snapshotForScope(secondCtx, scope); err != nil {
		t.Fatalf("new request after final cancellation error = %v, want fresh analysis success", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("analysis calls = %d, want 2", got)
	}

	release()
	<-firstFlight.done
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

	if err := state.handleNotification(
		"workspace/didChangeWatchedFiles",
		watchedFilesParams(t, filepath.Join(moduleRoot, "package", "first.go")),
	); err != nil {
		t.Fatalf("handle watched Go file: %v", err)
	}
	if _, err := state.analysisScopeFor(firstFile); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("scope finder calls after Go source change = %d, want 1", calls)
	}

	if err := state.handleNotification(
		"workspace/didChangeWatchedFiles",
		watchedFilesParams(t, filepath.Join(moduleRoot, "go.mod")),
	); err != nil {
		t.Fatalf("handle watched go.mod: %v", err)
	}
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
	state.analyzeScope = func(_ context.Context, scope analysis.AnalysisScope, _ analysis.ChangeSet, _ map[string][]byte) (analysis.Snapshot, error) {
		callCounts[scope.CacheKey()]++
		return analysis.Snapshot{}, nil
	}
	if _, err := state.snapshotForScope(context.Background(), firstScope); err != nil {
		t.Fatal(err)
	}
	if _, err := state.snapshotForScope(context.Background(), secondScope); err != nil {
		t.Fatal(err)
	}

	if err := state.handleNotification(
		"workspace/didChangeWatchedFiles",
		watchedFilesParams(t, filepath.Join(firstScope.Root, "changed.go")),
	); err != nil {
		t.Fatalf("handle watched file: %v", err)
	}
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
	uri := semanticTestFileURI(t, filepath.Join(t.TempDir(), "sample.go"))
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
	if err := state.handleNotification("textDocument/didClose", params); err != nil {
		t.Fatalf("handle didClose: %v", err)
	}
	if len(state.documentPaths) != 0 {
		t.Fatalf("document path cache length after close = %d, want 0", len(state.documentPaths))
	}
}

func watchedFilesParams(t *testing.T, paths ...string) json.RawMessage {
	t.Helper()
	changes := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		changes = append(changes, map[string]any{"uri": semanticTestFileURI(t, path), "type": 2})
	}
	params, err := json.Marshal(map[string]any{"changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	return params
}
