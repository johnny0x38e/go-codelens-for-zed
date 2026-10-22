package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/testutil"
)

func notifySavedTest(t testing.TB, state *serverState, method string, params any) {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.handleNotification(method, encoded); err != nil {
		t.Fatal(err)
	}
}

func openSavedTest(t testing.TB, state *serverState, path, source string) {
	t.Helper()
	notifySavedTest(t, state, "textDocument/didOpen", didOpenTextDocumentParams{
		TextDocument: openedTextDocument{URI: semanticTestFileURI(t, path), Version: 1, Text: source},
	})
}

func changeSavedTest(t testing.TB, state *serverState, path, source string, version int64) {
	t.Helper()
	notifySavedTest(t, state, "textDocument/didChange", didChangeTextDocumentParams{
		TextDocument:   versionedTextDocumentIdentifier{URI: semanticTestFileURI(t, path), Version: version},
		ContentChanges: []textDocumentContentChangeEvent{{Text: source}},
	})
}

func saveSavedTest(t testing.TB, state *serverState, path string) {
	t.Helper()
	notifySavedTest(t, state, "textDocument/didSave", textDocumentParams{
		TextDocument: textDocumentIdentifier{URI: semanticTestFileURI(t, path)},
	})
}

func lensesSavedTest(t *testing.T, state *serverState, path string) []codeLens {
	t.Helper()
	params, err := json.Marshal(
		textDocumentParams{TextDocument: textDocumentIdentifier{URI: semanticTestFileURI(t, path)}},
	)
	if err != nil {
		t.Fatal(err)
	}
	lenses, err := state.semanticCodeLenses(t.Context(), params)
	if err != nil {
		t.Fatal(err)
	}
	return lenses
}

func TestSavedPolicyKeepsLensesUntilAllFilesSavedWithValidSyntax(t *testing.T) {
	root, declarationURI, _, referencePath := writeFreshnessFixture(t)
	declarationPath := filepath.Join(root, "lib.go")
	declaration := "package sample\n\nfunc Target() {}\n"
	reference := "package sample\n\nfunc Use() { Target() }\n"
	state := newServerStateWithContext(t.Context())
	openSavedTest(t, state, declarationPath, declaration)
	openSavedTest(t, state, referencePath, reference)
	var calls atomic.Int64
	original := state.analyzeScope
	state.analyzeScope = func(ctx context.Context, scope analysis.AnalysisScope, changes analysis.ChangeSet, overlays map[string][]byte) (analysis.Snapshot, error) {
		calls.Add(1)
		if len(overlays) != 0 {
			t.Error("saved analysis received dirty overlays")
		}
		return original(ctx, scope, changes, overlays)
	}
	before := lensesSavedTest(t, state, declarationPath)
	if len(before) == 0 || before[0].Command.Title != "1 reference" {
		t.Fatalf("initial lenses = %#v", before)
	}
	changeSavedTest(t, state, declarationPath, declaration+"// saved A\n", 2)
	changeSavedTest(t, state, referencePath, reference+"// unsaved B\n", 2)
	writeSemanticTestFile(t, declarationPath, []byte(declaration+"// saved A\n"))
	saveSavedTest(t, state, declarationPath)
	if after := lensesSavedTest(t, state, declarationPath); !reflect.DeepEqual(after, before) {
		t.Fatalf("saving A while B is dirty changed retained lenses: %#v", after)
	}
	if state.refreshPending {
		t.Fatal("saving A requested a refresh while B was still dirty")
	}
	saveSavedTest(t, state, referencePath) // disk mismatch must still block analysis.
	_ = lensesSavedTest(t, state, declarationPath)
	broken := reference + "func Broken(\n"
	changeSavedTest(t, state, referencePath, broken, 3)
	writeSemanticTestFile(t, referencePath, []byte(broken))
	saveSavedTest(t, state, referencePath)
	for range 3 {
		after := lensesSavedTest(t, state, declarationPath)
		if len(after) == 0 || after[0].Command.Title != "1 reference" {
			t.Fatalf("syntax-blocked save lost existing lenses: %#v", after)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("analysis calls before all-valid save = %d, want 1", calls.Load())
	}
	updated := "package sample\n\nfunc Use() { Target(); Target() }\n"
	changeSavedTest(t, state, referencePath, updated, 4)
	writeSemanticTestFile(t, referencePath, []byte(updated))
	saveSavedTest(t, state, referencePath)
	after := lensesSavedTest(t, state, declarationPath)
	if len(after) == 0 || after[0].Command.Title != "2 references" || calls.Load() != 2 {
		t.Fatalf("valid saved update = %#v after %d analyses", after, calls.Load())
	}
	state.refreshMu.Lock()
	state.refreshPending = false
	state.refreshMu.Unlock()
	for range 3 {
		saveSavedTest(t, state, declarationPath)
		_ = lensesSavedTest(t, state, declarationPath)
	}
	if calls.Load() != 2 || state.refreshPending {
		t.Fatal("unchanged saves restarted analysis or requested unnecessary refreshes")
	}
	state.cacheMu.RLock()
	cached := state.snapshots[analysis.ModuleAnalysisScope(root).CacheKey()]
	state.cacheMu.RUnlock()
	if !cached.valid || len(cached.positions) != 0 || len(cached.snapshot.Lenses(declarationURI)) == 0 {
		t.Fatal("successful saved analysis did not replace the old result and its mappings")
	}
}

func TestSavedPolicyRetainsResultAfterFailedAnalysisWithoutRetryStorm(t *testing.T) {
	root, _, _, _ := writeFreshnessFixture(t)
	path := filepath.Join(root, "lib.go")
	source := "package sample\n\nfunc Target() {}\n"
	state := newServerStateWithContext(t.Context())
	openSavedTest(t, state, path, source)
	before := lensesSavedTest(t, state, path)
	var calls int
	state.analyzeScope = func(context.Context, analysis.AnalysisScope, analysis.ChangeSet, map[string][]byte) (analysis.Snapshot, error) {
		calls++
		return analysis.Snapshot{}, errors.New("test loader failure")
	}
	changeSavedTest(t, state, path, source+"// saved\n", 2)
	writeSemanticTestFile(t, path, []byte(source+"// saved\n"))
	saveSavedTest(t, state, path)
	for range 4 {
		if got := lensesSavedTest(t, state, path); !reflect.DeepEqual(got, before) {
			t.Fatalf("failed update replaced successful lenses: %#v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("failed revision was retried %d times, want 1", calls)
	}
	saveSavedTest(t, state, path)
	_ = lensesSavedTest(t, state, path)
	if calls != 2 {
		t.Fatal("an explicit save did not allow retrying a failed analysis")
	}
}

func TestSavedPolicyBlocksColdAndClosedSyntaxErrors(t *testing.T) {
	root, _, _, reference := writeFreshnessFixture(t)
	path := filepath.Join(root, "lib.go")
	broken := "package sample\nfunc Broken(\n"
	writeSemanticTestFile(t, path, []byte(broken))
	state := newServerStateWithContext(t.Context())
	var calls int
	state.analyzeScope = func(context.Context, analysis.AnalysisScope, analysis.ChangeSet, map[string][]byte) (analysis.Snapshot, error) {
		calls++
		return analysis.Snapshot{}, nil
	}
	if got := lensesSavedTest(t, state, path); len(got) != 0 || calls != 0 {
		t.Fatalf("invalid initial source entered semantic analysis: %d lenses, %d calls", len(got), calls)
	}
	state.invalidateSnapshotsForPath(reference)
	writeSemanticTestFile(t, reference, []byte(broken))
	if got := lensesSavedTest(t, state, path); len(got) != 0 || calls != 0 {
		t.Fatalf("invalid closed source entered semantic analysis: %d lenses, %d calls", len(got), calls)
	}
}

func TestSavedPolicyWaitsAcrossGoWorkModules(t *testing.T) {
	fixture := testutil.WriteWorkspace(t, []testutil.ModuleSpec{
		{ModulePath: "example.com/first", PackageCount: 1, FilesPerPackage: 1, DeclarationsPerFile: 1},
		{ModulePath: "example.com/second", PackageCount: 1, FilesPerPackage: 1, DeclarationsPerFile: 1},
	})
	scope := analysis.AnalysisScope{
		Root:        fixture.Root,
		WorkFile:    fixture.WorkFile,
		ModuleRoots: []string{fixture.Modules[0].Root, fixture.Modules[1].Root},
	}
	state := newServerStateWithContext(t.Context())
	var calls int
	state.analyzeScope = func(context.Context, analysis.AnalysisScope, analysis.ChangeSet, map[string][]byte) (analysis.Snapshot, error) {
		calls++
		return analysis.Snapshot{}, nil
	}
	if _, err := state.snapshotForScope(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	for _, module := range fixture.Modules {
		const source = "package sample\n"
		openSavedTest(t, state, module.EntryFile, source) // Deliberately differs from disk.
		writeSemanticTestFile(t, module.EntryFile, []byte(source))
	}
	saveSavedTest(t, state, fixture.Modules[0].EntryFile)
	_, _ = state.snapshotForScope(t.Context(), scope)
	if calls != 1 {
		t.Fatal("saving one go.work module analyzed another dirty module")
	}
	saveSavedTest(t, state, fixture.Modules[1].EntryFile)
	_, _ = state.snapshotForScope(t.Context(), scope)
	if calls != 2 {
		t.Fatalf("analysis calls after all modules were saved = %d, want 2", calls)
	}
}
