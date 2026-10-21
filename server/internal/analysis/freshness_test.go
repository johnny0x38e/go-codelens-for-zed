package analysis

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEngineDetectsAndRefreshesSilentExternalRewrite(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(
		t,
		filepath.Join(moduleDir, "go.mod"),
		"module example.com/freshness\n\ngo 1.26\n",
	)
	declarationPath := filepath.Join(moduleDir, "lib.go")
	referencePath := filepath.Join(moduleDir, "use.go")
	writeAnalysisTestFile(t, declarationPath, "package sample\n\nfunc Target() {}\n")
	referenceSource := "package sample\n\nfunc Use() { Target() }\n"
	writeAnalysisTestFile(t, referencePath, referenceSource)

	declarationURI, err := PathToURI(declarationPath)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", declarationPath, err)
	}
	referenceURI, err := PathToURI(referencePath)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", referencePath, err)
	}

	ctx := context.Background()
	scope := ModuleAnalysisScope(moduleDir)
	engine := NewEngine()
	snapshot, err := engine.Analyze(ctx, scope, ChangeSet{}, nil)
	if err != nil {
		t.Fatalf("Engine.Analyze() error = %v", err)
	}
	assertFreshForTest(t, snapshot, declarationURI, referenceURI)

	rewritten := "// external rewrite\n" + referenceSource
	writeAnalysisTestFile(t, referencePath, rewritten)
	change, stale := snapshot.StaleFile(referenceURI)
	if !stale {
		t.Fatal("silent external rewrite was not detected")
	}
	wantChange := Change{Path: referencePath, Kind: ChangeContent}
	if change != wantChange {
		t.Fatalf("StaleFile() change = %#v, want %#v", change, wantChange)
	}
	if _, stale := snapshot.StaleFile(declarationURI); stale {
		t.Fatal("unchanged declaration file was reported stale")
	}

	refreshed, err := engine.Analyze(ctx, scope, NewChangeSet(change), nil)
	if err != nil {
		t.Fatalf("Engine.Analyze() after rewrite error = %v", err)
	}
	assertFreshForTest(t, refreshed, declarationURI, referenceURI)
	assertReferenceTargetLineForTest(t, refreshed, declarationURI, 3)

	touchedAt := time.Now().Add(time.Hour)
	if err := os.Chtimes(referencePath, touchedAt, touchedAt); err != nil {
		t.Fatalf("os.Chtimes(%q) error = %v", referencePath, err)
	}
	if _, stale := refreshed.StaleFile(referenceURI); !stale {
		t.Fatal("modification-time-only change was not detected")
	}
}

func assertFreshForTest(t *testing.T, snapshot Snapshot, uris ...string) {
	t.Helper()
	for _, uri := range uris {
		if change, stale := snapshot.StaleFile(uri); stale {
			t.Fatalf("snapshot reports %q stale with %#v", uri, change)
		}
	}
}

func assertReferenceTargetLineForTest(t *testing.T, snapshot Snapshot, declarationURI string, line int) {
	t.Helper()
	for _, lens := range snapshot.Lenses(declarationURI) {
		if lens.Kind != LensKindReferences {
			continue
		}
		if len(lens.Targets) != 1 {
			t.Fatalf("reference lens targets = %#v, want one", lens.Targets)
		}
		if got := lens.Targets[0].Range.Start.Line; got != line {
			t.Fatalf("reference target line = %d, want %d", got, line)
		}
		return
	}
	t.Fatalf("no reference lens for %q in %#v", declarationURI, snapshot.Lenses(declarationURI))
}
