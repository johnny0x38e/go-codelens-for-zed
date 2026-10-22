package lsp

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

func TestRetainedLensesMoveWithoutAnalysisAndDiscardRestoresCoordinates(t *testing.T) {
	root, _, _, referencePath := writeFreshnessFixture(t)
	path := filepath.Join(root, "lib.go")
	source := "package sample\n\nfunc Target() {}\n"
	reference := "package sample\n\nfunc Use() { Target() }\n"
	state := newServerStateWithContext(t.Context())
	openSavedTest(t, state, path, source)
	openSavedTest(t, state, referencePath, reference)
	before := lensesSavedTest(t, state, path)
	original := state.analyzeScope
	state.analyzeScope = func(context.Context, analysis.AnalysisScope, analysis.ChangeSet, map[string][]byte) (analysis.Snapshot, error) {
		t.Error("dirty buffers entered semantic analysis")
		return analysis.Snapshot{}, nil
	}
	changeSavedTest(t, state, path, "// 😀\n"+source, 2)
	changeSavedTest(t, state, referencePath, "// target moved\n"+reference, 2)
	moved := lensesSavedTest(t, state, path)
	if len(moved) != len(before) || moved[0].Range.Start.Line != before[0].Range.Start.Line+1 {
		t.Fatalf("retained declaration did not move: %#v", moved)
	}
	if got := moved[0].Command.Arguments[2].([]analysis.Location)[0].Range.Start.Line; got != 3 {
		t.Fatalf("retained reference line = %d, want 3", got)
	}
	notifySavedTest(t, state, "textDocument/didClose", textDocumentParams{
		TextDocument: textDocumentIdentifier{URI: semanticTestFileURI(t, path)},
	})
	restored := lensesSavedTest(t, state, path)
	if len(restored) != len(before) || restored[0].Range != before[0].Range {
		t.Fatalf("discarding edits did not restore saved coordinates: %#v", restored)
	}
	if got := before[0].Command.Arguments[2].([]analysis.Location)[0].Range.Start.Line; got != 2 {
		t.Fatal("mapping mutated an earlier immutable result")
	}
	state.analyzeScope = original
}

func TestRetainedLensesRejectSilentClosedFileRewritesWhileDirty(t *testing.T) {
	root, _, _, referencePath := writeFreshnessFixture(t)
	path := filepath.Join(root, "lib.go")
	source := "package sample\n\nfunc Target() {}\n"
	state := newServerStateWithContext(t.Context())
	openSavedTest(t, state, path, source)
	_ = lensesSavedTest(t, state, path)
	state.analyzeScope = func(context.Context, analysis.AnalysisScope, analysis.ChangeSet, map[string][]byte) (analysis.Snapshot, error) {
		t.Error("dirty scope entered analysis after an external rewrite")
		return analysis.Snapshot{}, nil
	}
	changeSavedTest(t, state, path, source+"// unsaved\n", 2)
	writeFreshnessExternalEdit(t, referencePath)
	lenses := lensesSavedTest(t, state, path)
	if len(lenses) == 0 || lenses[0].Command.Title != "1 reference" {
		t.Fatalf("external rewrite cleared the retained display: %#v", lenses)
	}
	if targets := lenses[0].Command.Arguments[2].([]analysis.Location); len(targets) != 0 {
		t.Fatalf("retained result navigates to an externally obsolete position: %#v", targets)
	}
}

func TestRetainedLensesSurviveScopeDiscoveryAndTransitionFailures(t *testing.T) {
	root, _, _, _ := writeFreshnessFixture(t)
	path := filepath.Join(root, "lib.go")
	state := newServerStateWithContext(t.Context())
	before := lensesSavedTest(t, state, path)
	state.findScope = func(string, string) (analysis.AnalysisScope, error) {
		return analysis.AnalysisScope{}, errors.New("invalid go.work")
	}
	state.invalidateAnalysisScopes()
	if got := lensesSavedTest(t, state, path); !reflect.DeepEqual(got, before) {
		t.Fatal("failed scope discovery cleared successful lenses")
	}
	state.findScope = func(string, string) (analysis.AnalysisScope, error) {
		scope := analysis.ModuleAnalysisScope(root)
		scope.WorkFile = filepath.Join(root, "new.work")
		return scope, nil
	}
	state.analyzeScope = func(context.Context, analysis.AnalysisScope, analysis.ChangeSet, map[string][]byte) (analysis.Snapshot, error) {
		return analysis.Snapshot{}, errors.New("new workspace cannot load")
	}
	state.invalidateAnalysisScopes()
	if got := lensesSavedTest(t, state, path); !reflect.DeepEqual(got, before) {
		t.Fatal("failed scope transition cleared successful lenses")
	}
}

func TestRetainedLensesSuppressOverwrittenDeclarationAndTargets(t *testing.T) {
	root, _, _, referencePath := writeFreshnessFixture(t)
	path := filepath.Join(root, "lib.go")
	source := "package sample\n\nfunc Target() {}\n"
	reference := "package sample\n\nfunc Use() { Target() }\n"
	state := newServerStateWithContext(t.Context())
	openSavedTest(t, state, path, source)
	openSavedTest(t, state, referencePath, reference)
	_ = lensesSavedTest(t, state, path)
	changeSavedTest(t, state, referencePath, strings.Replace(reference, "Target()", "Other()", 1), 2)
	retained := lensesSavedTest(t, state, path)
	if len(retained) == 0 || retained[0].Command.Title != "1 reference" {
		t.Fatalf("editing a reference lost the retained semantic count: %#v", retained)
	}
	if targets := retained[0].Command.Arguments[2].([]analysis.Location); len(targets) != 0 {
		t.Fatalf("overwritten reference still has navigation targets: %#v", targets)
	}
	changeSavedTest(t, state, path, strings.Replace(source, "Target", "Other", 1), 2)
	if lenses := lensesSavedTest(t, state, path); len(lenses) != 0 {
		t.Fatalf("overwritten declaration retained its old lens: %#v", lenses)
	}
}
