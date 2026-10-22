package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

func TestDocumentStoreRequiresEveryScopedDocumentSavedAndParsable(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first", "a.go")
	second := filepath.Join(root, "second", "b_test.go")
	outside := filepath.Join(t.TempDir(), "outside.go")
	const source = "package sample\n\nfunc Value() {}\n"
	for _, path := range []string{first, second, outside} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeSemanticTestFile(t, path, []byte(source))
	}
	store := newDocumentStore()
	for _, path := range []string{first, second, outside} {
		if _, _, err := store.Open(semanticTestFileURI(t, path), source, 1); err != nil {
			t.Fatal(err)
		}
	}
	scope := analysis.AnalysisScope{Root: root, ModuleRoots: []string{filepath.Dir(first), filepath.Dir(second)}}
	assertReady := func(want bool) {
		t.Helper()
		if got := store.ReadyForAnalysis(scope); got != want {
			t.Fatalf("ReadyForAnalysis() = %t, want %t", got, want)
		}
	}
	change := func(path, content string, version int64) {
		t.Helper()
		if _, _, err := store.Change(
			semanticTestFileURI(t, path),
			version,
			[]textDocumentContentChangeEvent{{Text: content}},
		); err != nil {
			t.Fatal(err)
		}
	}
	assertReady(true)
	change(outside, source+"// outside\n", 2)
	assertReady(true)
	change(first, source+"// A\n", 2)
	change(second, source+"// B\n", 2)
	assertReady(false)
	store.Save(semanticTestFileURI(t, first))
	assertReady(false)
	writeSemanticTestFile(t, first, []byte(source+"// A\n"))
	store.Save(semanticTestFileURI(t, first))
	assertReady(false)
	writeSemanticTestFile(t, second, []byte(source+"// B\n"))
	store.Save(semanticTestFileURI(t, second))
	assertReady(true)

	broken := source + "func Broken(\n"
	change(second, broken, 3)
	writeSemanticTestFile(t, second, []byte(broken))
	store.Save(semanticTestFileURI(t, second))
	assertReady(false)
	// Type errors are not syntax errors and must not turn this into a build gate.
	typeError := source + "var Unknown = Missing\n"
	change(second, typeError, 4)
	writeSemanticTestFile(t, second, []byte(typeError))
	store.Save(semanticTestFileURI(t, second))
	assertReady(true)
}

func TestSaveReconfirmsDiskAgreementForPreviouslyCleanBuffers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.go")
	const source = "package sample\n"
	writeSemanticTestFile(t, path, []byte(source))
	store := newDocumentStore()
	uri := semanticTestFileURI(t, path)
	if _, _, err := store.Open(uri, source, 1); err != nil {
		t.Fatal(err)
	}
	writeSemanticTestFile(t, path, []byte(source+"var New = 1\n"))
	store.Save(uri)
	if store.ReadyForAnalysis(analysis.ModuleAnalysisScope(filepath.Dir(path))) {
		t.Fatal("save trusted stale clean metadata despite disk/buffer disagreement")
	}
}

func TestChangedSavedSyntaxChecksClosedFilesAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.go")
	writeSemanticTestFile(t, path, []byte("package sample\nfunc Broken(\n"))
	changes := analysis.NewChangeSet(analysis.Change{Path: path})
	if err := checkChangedGoSyntax(t.Context(), changes, nil); !errors.Is(err, errSavedSyntax) {
		t.Fatalf("syntax error = %v, want errSavedSyntax", err)
	}
	writeSemanticTestFile(t, path, []byte("package sample\nvar Value = Missing\n"))
	if err := checkChangedGoSyntax(t.Context(), changes, nil); err != nil {
		t.Fatalf("syntax check rejected a type error: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := checkChangedGoSyntax(ctx, changes, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled syntax check = %v", err)
	}
}
