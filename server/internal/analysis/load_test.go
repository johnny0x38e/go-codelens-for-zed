package analysis

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestEngineRejectsSyntaxErrorsInOtherLoadedFiles(t *testing.T) {
	root := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(root, "go.mod"), "module example.com/syntax-gate\n\ngo 1.27\n")
	path := filepath.Join(root, "sample.go")
	writeAnalysisTestFile(t, path, "package sample\nfunc Target() {}\nvar Use = Target\n")
	scope := ModuleAnalysisScope(root)
	engine := NewEngine()
	before, err := engine.Analyze(t.Context(), scope, ChangeSet{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(root, "broken.go")
	writeAnalysisTestFile(t, broken, "package sample\nfunc Broken(\n")
	if _, err := engine.Analyze(
		t.Context(),
		scope,
		NewChangeSet(Change{Path: broken, Kind: ChangeCreated}),
		nil,
	); err == nil {
		t.Fatal("syntax-invalid package produced a publishable snapshot")
	}
	if got := engine.states[scope.CacheKey()].snapshot; !reflect.DeepEqual(got, before) {
		t.Fatal("syntax-invalid load replaced the last successful engine state")
	}
}
