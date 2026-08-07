package analysis

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/testutil"
)

func TestEngineMatchesFullAnalysisExactly(t *testing.T) {
	fixture := testutil.WriteModule(t, testutil.ModuleSpec{
		ModulePath:          "example.com/engine-parity",
		PackageCount:        2,
		FilesPerPackage:     2,
		DeclarationsPerFile: 3,
		IncludeTests:        true,
	})
	scope := ModuleAnalysisScope(fixture.Root)
	want, err := AnalyzeScope(context.Background(), scope, nil)
	if err != nil {
		t.Fatalf("AnalyzeScope() error = %v", err)
	}

	engine := NewEngine()
	got, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil)
	if err != nil {
		t.Fatalf("Engine.Analyze() error = %v", err)
	}
	assertSnapshotsEqual(t, got, want, fixture.PackageFiles)

	source, err := os.ReadFile(fixture.EntryFile)
	if err != nil {
		t.Fatalf("read entry file: %v", err)
	}
	overlays := map[string][]byte{fixture.EntryFile: append(source, "\n// parity edit\n"...)}
	want, err = AnalyzeScope(context.Background(), scope, overlays)
	if err != nil {
		t.Fatalf("changed AnalyzeScope() error = %v", err)
	}
	got, err = engine.Analyze(
		context.Background(),
		scope,
		NewChangeSet(Change{Path: fixture.EntryFile, Kind: ChangeContent}),
		overlays,
	)
	if err != nil {
		t.Fatalf("changed Engine.Analyze() error = %v", err)
	}
	assertSnapshotsEqual(t, got, want, fixture.PackageFiles)
}

func assertSnapshotsEqual(t *testing.T, got, want Snapshot, paths []string) {
	t.Helper()
	if reflect.DeepEqual(got.lensMap(), want.lensMap()) {
		return
	}
	for _, path := range paths {
		uri, err := PathToURI(path)
		if err != nil {
			t.Fatalf("PathToURI(%q) error = %v", path, err)
		}
		if !reflect.DeepEqual(got.Lenses(uri), want.Lenses(uri)) {
			t.Fatalf("Engine lenses for %q = %#v, want %#v", uri, got.Lenses(uri), want.Lenses(uri))
		}
	}
	t.Fatalf("Engine snapshot = %#v, want %#v", got.lensMap(), want.lensMap())
}

func TestNewChangeSetCanonicalizesAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "package", "..", "sample.go")
	canonicalPath := filepath.Join(root, "sample.go")
	metadataPath := filepath.Join(root, "go.mod")

	changes := NewChangeSet(
		Change{Path: path, Kind: ChangeContent},
		Change{Path: canonicalPath, Kind: ChangeCreated},
		Change{Path: canonicalPath, Kind: ChangeContent},
		Change{Path: canonicalPath, Kind: ChangeDeleted},
		Change{Path: metadataPath, Kind: ChangeMetadata},
	)
	want := []Change{
		{Path: metadataPath, Kind: ChangeMetadata},
		{Path: canonicalPath, Kind: ChangeDeleted},
	}
	if got := changes.Changes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangeSet.Changes() = %#v, want %#v", got, want)
	}

	returned := changes.Changes()
	returned[0].Path = "mutated"
	if got := changes.Changes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangeSet mutated through returned slice: %#v", got)
	}
}

func TestEngineForgetScopeReleasesState(t *testing.T) {
	fixture := testutil.WriteModule(t, testutil.ModuleSpec{
		ModulePath:          "example.com/engine-forget",
		PackageCount:        1,
		FilesPerPackage:     1,
		DeclarationsPerFile: 1,
	})
	scope := ModuleAnalysisScope(fixture.Root)
	engine := NewEngine()
	if _, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil); err != nil {
		t.Fatalf("Engine.Analyze() error = %v", err)
	}
	if len(engine.states) != 1 {
		t.Fatalf("engine state count = %d, want 1", len(engine.states))
	}
	state := engine.states[scope.CacheKey()]
	if state.index == nil || len(state.index.packages) == 0 {
		t.Fatal("Engine.Analyze() did not retain the package index from full analysis")
	}
	for packageID, packageState := range state.index.packages {
		if packageState.facts == nil {
			t.Fatalf("retained package %q has no semantic facts", packageID)
		}
	}

	engine.ForgetScope(scope.CacheKey())
	if len(engine.states) != 0 {
		t.Fatalf("engine state count after ForgetScope() = %d, want 0", len(engine.states))
	}
}

func TestEngineForgetScopeDoesNotRetainHistoricalKeys(t *testing.T) {
	engine := NewEngine()
	engine.analyze = func(_ context.Context, _ AnalysisScope, _ map[string][]byte) (scopeAnalysis, error) {
		return scopeAnalysis{snapshot: newSnapshot(nil)}, nil
	}

	root := t.TempDir()
	const scopeCount = 128
	for index := range scopeCount {
		scope := ModuleAnalysisScope(filepath.Join(root, "scope-"+strconv.Itoa(index)))
		if _, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil); err != nil {
			t.Fatalf("Engine.Analyze() scope %d error = %v", index, err)
		}
		engine.ForgetScope(scope.CacheKey())
	}

	engine.mu.RLock()
	stateCount := len(engine.states)
	controlCount := len(engine.scopeControls)
	engine.mu.RUnlock()
	if stateCount != 0 {
		t.Fatalf("engine state count after forgetting scopes = %d, want 0", stateCount)
	}
	if controlCount != 0 {
		t.Fatalf("engine scope control count after forgetting scopes = %d, want 0", controlCount)
	}
}

func TestEngineForgetScopeRejectsStalePublication(t *testing.T) {
	scope := ModuleAnalysisScope(t.TempDir())
	prime := newSnapshot(map[string][]Lens{"prime": {{Title: "prime"}}})
	stale := newSnapshot(map[string][]Lens{"stale": {{Title: "stale"}}})
	fresh := newSnapshot(map[string][]Lens{"fresh": {{Title: "fresh"}}})
	started := make(chan struct{})
	release := make(chan struct{})

	engine := NewEngine()
	calls := 0
	engine.analyze = func(_ context.Context, _ AnalysisScope, _ map[string][]byte) (scopeAnalysis, error) {
		calls++
		switch calls {
		case 1:
			return scopeAnalysis{snapshot: prime}, nil
		case 2:
			close(started)
			<-release
			return scopeAnalysis{snapshot: stale}, nil
		default:
			return scopeAnalysis{snapshot: fresh}, nil
		}
	}

	if _, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil); err != nil {
		t.Fatalf("prime Engine.Analyze() error = %v", err)
	}
	staleResult := make(chan error, 1)
	go func() {
		_, err := engine.Analyze(
			context.Background(),
			scope,
			NewChangeSet(Change{Path: filepath.Join(scope.Root, "sample.go"), Kind: ChangeContent}),
			nil,
		)
		staleResult <- err
	}()

	<-started
	engine.ForgetScope(scope.CacheKey())
	got, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil)
	if err != nil {
		t.Fatalf("replacement Engine.Analyze() error = %v", err)
	}
	if !reflect.DeepEqual(got, fresh) {
		t.Fatalf("replacement Engine.Analyze() = %#v, want fresh snapshot", got)
	}
	close(release)
	if err := <-staleResult; err != nil {
		t.Fatalf("stale Engine.Analyze() error = %v", err)
	}

	engine.mu.RLock()
	retained := engine.states[scope.CacheKey()].snapshot
	engine.mu.RUnlock()
	if !reflect.DeepEqual(retained, fresh) {
		t.Fatalf("state after forgotten analysis returned = %#v, want fresh snapshot", retained)
	}
}

func TestEngineFailedAnalysisReleasesScopeControl(t *testing.T) {
	engine := NewEngine()
	errAnalysisFailed := errors.New("analysis failed")
	engine.analyze = func(_ context.Context, _ AnalysisScope, _ map[string][]byte) (scopeAnalysis, error) {
		return scopeAnalysis{}, errAnalysisFailed
	}

	root := t.TempDir()
	const scopeCount = 128
	for index := range scopeCount {
		scope := ModuleAnalysisScope(filepath.Join(root, "scope-"+strconv.Itoa(index)))
		if _, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil); !errors.Is(err, errAnalysisFailed) {
			t.Fatalf("Engine.Analyze() scope %d error = %v, want %v", index, err, errAnalysisFailed)
		}
	}

	engine.mu.RLock()
	stateCount := len(engine.states)
	controlCount := len(engine.scopeControls)
	engine.mu.RUnlock()
	if stateCount != 0 || controlCount != 0 {
		t.Fatalf("engine counts after failed analyses = (%d states, %d controls), want zero", stateCount, controlCount)
	}
}

func TestEngineConcurrentFailureKeepsScopeControlUntilSuccess(t *testing.T) {
	scope := ModuleAnalysisScope(t.TempDir())
	fresh := newSnapshot(map[string][]Lens{"fresh": {{Title: "fresh"}}})
	errAnalysisFailed := errors.New("analysis failed")
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	failFirst := make(chan struct{})
	completeSecond := make(chan struct{})

	engine := NewEngine()
	var calls atomic.Int32
	engine.analyze = func(_ context.Context, _ AnalysisScope, _ map[string][]byte) (scopeAnalysis, error) {
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
			<-failFirst
			return scopeAnalysis{}, errAnalysisFailed
		case 2:
			close(secondStarted)
			<-completeSecond
			return scopeAnalysis{snapshot: fresh}, nil
		default:
			panic("unexpected analysis call")
		}
	}

	firstResult := make(chan error, 1)
	go func() {
		_, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil)
		firstResult <- err
	}()
	<-firstStarted

	secondSnapshot := make(chan Snapshot, 1)
	secondResult := make(chan error, 1)
	go func() {
		snapshot, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil)
		secondSnapshot <- snapshot
		secondResult <- err
	}()
	<-secondStarted

	close(failFirst)
	if err := <-firstResult; !errors.Is(err, errAnalysisFailed) {
		t.Fatalf("first Engine.Analyze() error = %v, want %v", err, errAnalysisFailed)
	}
	engine.mu.RLock()
	control := engine.scopeControls[scope.CacheKey()]
	active := 0
	if control != nil {
		active = control.active
	}
	engine.mu.RUnlock()
	if control == nil || active != 1 {
		t.Fatalf("scope control after first failure = %#v, want one active analysis", control)
	}

	close(completeSecond)
	if err := <-secondResult; err != nil {
		t.Fatalf("second Engine.Analyze() error = %v", err)
	}
	if got := <-secondSnapshot; !reflect.DeepEqual(got, fresh) {
		t.Fatalf("second Engine.Analyze() = %#v, want fresh snapshot", got)
	}
	engine.mu.RLock()
	retained := engine.states[scope.CacheKey()].snapshot
	engine.mu.RUnlock()
	if !reflect.DeepEqual(retained, fresh) {
		t.Fatalf("retained snapshot after concurrent failure = %#v, want fresh", retained)
	}
}

func TestEngineReusesUnchangedStateAndRejectsStalePublication(t *testing.T) {
	scope := ModuleAnalysisScope(t.TempDir())
	prime := newSnapshot(map[string][]Lens{"prime": {{Title: "prime"}}})
	stale := newSnapshot(map[string][]Lens{"stale": {{Title: "stale"}}})
	fresh := newSnapshot(map[string][]Lens{"fresh": {{Title: "fresh"}}})
	started := make(chan struct{})
	release := make(chan struct{})

	engine := NewEngine()
	calls := 0
	engine.analyze = func(_ context.Context, _ AnalysisScope, _ map[string][]byte) (scopeAnalysis, error) {
		calls++
		switch calls {
		case 1:
			return scopeAnalysis{snapshot: prime}, nil
		case 2:
			close(started)
			<-release
			return scopeAnalysis{snapshot: stale}, nil
		default:
			return scopeAnalysis{snapshot: fresh}, nil
		}
	}

	got, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil)
	if err != nil {
		t.Fatalf("prime Engine.Analyze() error = %v", err)
	}
	if !reflect.DeepEqual(got, prime) {
		t.Fatalf("prime Engine.Analyze() = %#v, want %#v", got, prime)
	}
	got, err = engine.Analyze(context.Background(), scope, ChangeSet{}, nil)
	if err != nil {
		t.Fatalf("cached Engine.Analyze() error = %v", err)
	}
	if !reflect.DeepEqual(got, prime) || calls != 1 {
		t.Fatalf("cached Engine.Analyze() = %#v after %d full calls, want prime after 1", got, calls)
	}

	result := make(chan error, 1)
	go func() {
		_, analyzeErr := engine.Analyze(
			context.Background(),
			scope,
			NewChangeSet(Change{Path: filepath.Join(scope.Root, "sample.go"), Kind: ChangeContent}),
			nil,
		)
		result <- analyzeErr
	}()
	<-started
	engine.InvalidateScope(scope.CacheKey())
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("stale Engine.Analyze() error = %v", err)
	}

	engine.mu.RLock()
	retained := engine.states[scope.CacheKey()].snapshot
	engine.mu.RUnlock()
	if !reflect.DeepEqual(retained, prime) {
		t.Fatalf("state after stale publication = %#v, want retained prime snapshot", retained)
	}

	got, err = engine.Analyze(
		context.Background(),
		scope,
		NewChangeSet(Change{Path: filepath.Join(scope.Root, "sample.go"), Kind: ChangeContent}),
		nil,
	)
	if err != nil {
		t.Fatalf("replacement Engine.Analyze() error = %v", err)
	}
	if !reflect.DeepEqual(got, fresh) || calls != 3 {
		t.Fatalf("replacement Engine.Analyze() = %#v after %d calls, want fresh after 3", got, calls)
	}
}

func TestChangeSetMergePreservesStrongerLifecycleEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.go")
	created := NewChangeSet(Change{Path: path, Kind: ChangeCreated})
	content := NewChangeSet(Change{Path: path, Kind: ChangeContent})
	deleted := NewChangeSet(Change{Path: path, Kind: ChangeDeleted})

	if got := created.Merge(content).Changes(); !reflect.DeepEqual(got, created.Changes()) {
		t.Fatalf("created.Merge(content) = %#v, want %#v", got, created.Changes())
	}
	if got := content.Merge(deleted).Changes(); !reflect.DeepEqual(got, deleted.Changes()) {
		t.Fatalf("content.Merge(deleted) = %#v, want %#v", got, deleted.Changes())
	}
	if got := deleted.Merge(content).Changes(); !reflect.DeepEqual(got, deleted.Changes()) {
		t.Fatalf("deleted.Merge(content) = %#v, want %#v", got, deleted.Changes())
	}
}

func TestEngineUsesExactConservativeFallbackReasons(t *testing.T) {
	testCases := []struct {
		name                string
		original            string
		change              func(string, string) Change
		overlay             func(string) map[string][]byte
		mutate              func(*scopeIndex, string)
		ignoreOverlayOnFull bool
		wantReason          fallbackReason
	}{
		{
			name: "module metadata",
			change: func(moduleDir, _ string) Change {
				return Change{Path: filepath.Join(moduleDir, "go.mod"), Kind: ChangeMetadata}
			},
			wantReason: fallbackModuleMetadata,
		},
		{
			name: "created file",
			change: func(moduleDir, _ string) Change {
				return Change{Path: filepath.Join(moduleDir, "created.go"), Kind: ChangeCreated}
			},
			wantReason: fallbackFileCreated,
		},
		{
			name: "deleted file",
			change: func(_ string, sourceFile string) Change {
				return Change{Path: sourceFile, Kind: ChangeDeleted}
			},
			wantReason: fallbackFileDeleted,
		},
		{
			name: "unknown owner",
			change: func(moduleDir, _ string) Change {
				return Change{Path: filepath.Join(moduleDir, "unknown.go"), Kind: ChangeContent}
			},
			wantReason: fallbackUnknownFileOwner,
		},
		{
			name:   "build constraint",
			change: sourceContentChange,
			overlay: func(sourceFile string) map[string][]byte {
				return map[string][]byte{
					sourceFile: []byte("//go:build !never\n\npackage sample\n\nfunc Value() int { return 1 }\n"),
				}
			},
			wantReason: fallbackSourceStructure,
		},
		{
			name:   "uncached import",
			change: sourceContentChange,
			overlay: func(sourceFile string) map[string][]byte {
				return map[string][]byte{
					sourceFile: []byte("package sample\n\nimport \"fmt\"\n\nfunc Value() int { return len(fmt.Sprint(1)) }\n"),
				}
			},
			wantReason: fallbackUncachedImport,
		},
		{
			name:   "package clause",
			change: sourceContentChange,
			overlay: func(sourceFile string) map[string][]byte {
				return map[string][]byte{
					sourceFile: []byte("package renamed\n\nfunc Value() int { return 1 }\n"),
				}
			},
			wantReason: fallbackSourceStructure,
		},
		{
			name:   "hard parse error",
			change: sourceContentChange,
			overlay: func(sourceFile string) map[string][]byte {
				return map[string][]byte{sourceFile: []byte("package sample\n\nfunc (")}
			},
			ignoreOverlayOnFull: true,
			wantReason:          fallbackParseFailure,
		},
		{
			name:       "missing retained fingerprint",
			change:     sourceContentChange,
			mutate:     deleteSourceFingerprint,
			wantReason: fallbackRetainedIndex,
		},
		{
			name:       "unavailable retained source",
			change:     sourceContentChange,
			mutate:     replaceCompiledFileWithMissingPath,
			wantReason: fallbackSourceUnavailable,
		},
		{
			name:     "missing retained import",
			original: "package sample\n\nimport \"fmt\"\n\nfunc Value() int { return len(fmt.Sprint(1)) }\n",
			change:   sourceContentChange,
			overlay: func(sourceFile string) map[string][]byte {
				return map[string][]byte{
					sourceFile: []byte("package sample\n\nimport \"fmt\"\n\nfunc Value() int { return len(fmt.Sprint(2)) }\n"),
				}
			},
			mutate:     deleteRetainedImports,
			wantReason: fallbackUncachedImport,
		},
		{
			name:       "incomplete package",
			change:     sourceContentChange,
			mutate:     clearCompiledFiles,
			wantReason: fallbackIncompletePackage,
		},
		{
			name:       "package graph cycle",
			change:     sourceContentChange,
			mutate:     addSelfImportCycle,
			wantReason: fallbackPackageGraph,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			moduleDir := t.TempDir()
			writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), "module example.com/fallback\n\ngo 1.26\n")
			sourceFile := filepath.Join(moduleDir, "sample.go")
			original := testCase.original
			if original == "" {
				original = "package sample\n\nfunc Value() int { return 1 }\n"
			}
			writeAnalysisTestFile(t, sourceFile, original)

			scope := ModuleAnalysisScope(moduleDir)
			engine := NewEngine()
			fullCalls := 0
			var reasons []fallbackReason
			engine.observeFallback = func(reason fallbackReason) {
				reasons = append(reasons, reason)
			}
			engine.analyze = func(
				ctx context.Context,
				scope AnalysisScope,
				overlays map[string][]byte,
			) (scopeAnalysis, error) {
				fullCalls++
				if testCase.ignoreOverlayOnFull && fullCalls > 1 {
					overlays = nil
				}
				return analyzeScope(ctx, scope, overlays, true)
			}
			if _, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil); err != nil {
				t.Fatalf("prime Engine.Analyze() error = %v", err)
			}
			oldIndex := engine.states[scope.CacheKey()].index
			if testCase.mutate != nil {
				testCase.mutate(oldIndex, sourceFile)
			}
			changes := NewChangeSet(testCase.change(moduleDir, sourceFile))
			var overlays map[string][]byte
			if testCase.overlay != nil {
				overlays = testCase.overlay(sourceFile)
			}
			if _, err := engine.Analyze(context.Background(), scope, changes, overlays); err != nil {
				t.Fatalf("fallback Engine.Analyze() error = %v", err)
			}
			if fullCalls != 2 {
				t.Fatalf("full analysis calls = %d, want 2", fullCalls)
			}
			if got := reasons[len(reasons)-1]; got != testCase.wantReason {
				t.Fatalf("fallback reason = %v, want %v", got, testCase.wantReason)
			}
			if newIndex := engine.states[scope.CacheKey()].index; newIndex == oldIndex {
				t.Fatal("successful full fallback retained the previous scope index")
			}
		})
	}
}

func TestEngineReportsInitialAndScopeFallbackReasons(t *testing.T) {
	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), "module example.com/fallback-scope\n\ngo 1.26\n")
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "sample.go"), "package sample\n")

	engine := NewEngine()
	var reasons []fallbackReason
	engine.observeFallback = func(reason fallbackReason) {
		reasons = append(reasons, reason)
	}
	scope := ModuleAnalysisScope(moduleDir)
	if _, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil); err != nil {
		t.Fatalf("initial Engine.Analyze() error = %v", err)
	}
	changedScope := scope
	changedScope.Patterns = []string{"./sample"}
	// Cache keys normally encode scope fields. Seed the changed key explicitly
	// to verify the defensive same-scope check still forces a complete reload if
	// key construction or retained metadata ever disagree.
	engine.states[changedScope.CacheKey()] = engine.states[scope.CacheKey()]
	if _, err := engine.Analyze(context.Background(), changedScope, ChangeSet{}, nil); err != nil {
		t.Fatalf("scope-changed Engine.Analyze() error = %v", err)
	}
	want := []fallbackReason{fallbackInitialAnalysis, fallbackScopeChanged}
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("fallback reasons = %v, want %v", reasons, want)
	}
}

func sourceContentChange(_ string, sourceFile string) Change {
	return Change{Path: sourceFile, Kind: ChangeContent}
}

func deleteSourceFingerprint(index *scopeIndex, sourceFile string) {
	delete(index.fingerprints, sourceFile)
}

func addSelfImportCycle(index *scopeIndex, _ string) {
	for _, state := range index.packages {
		state.imports["example.com/fallback"] = state.id
		return
	}
}

func replaceCompiledFileWithMissingPath(index *scopeIndex, _ string) {
	for _, state := range index.packages {
		if len(state.files) > 0 {
			state.files[0].path += ".missing"
			return
		}
	}
}

func deleteRetainedImports(index *scopeIndex, _ string) {
	clear(index.importTypes)
}

func clearCompiledFiles(index *scopeIndex, _ string) {
	for _, state := range index.packages {
		state.files = nil
		return
	}
}
