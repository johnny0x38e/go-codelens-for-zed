package analysis

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAnalyzeModuleReturnsLensesForRepositoryFixture(t *testing.T) {
	t.Parallel()

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test filename")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", ".."))
	moduleDir := filepath.Join(repositoryRoot, "examples", "poc")
	mainFile := filepath.Join(moduleDir, "main.go")
	mainURI, err := PathToURI(mainFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", mainFile, err)
	}

	snapshot, err := AnalyzeModule(context.Background(), moduleDir, nil)
	if err != nil {
		t.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
	}
	lenses := snapshot.Lenses(mainURI)
	assertAnalysisLens(t, lenses, "2 implementations", 4)
	assertAnalysisLens(t, lenses, "Implements: Greeter", 8)
	assertAnalysisLens(t, lenses, "Implements: Greeter", 16)
}

func assertAnalysisLens(t *testing.T, lenses []Lens, title string, line int) {
	t.Helper()
	for _, lens := range lenses {
		if lens.Title == title && lens.Range.Start.Line == line {
			return
		}
	}
	t.Fatalf("no lens titled %q on line %d in %#v", title, line, lenses)
}
