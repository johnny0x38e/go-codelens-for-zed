package lsp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDocumentStoreCleanOpenAndSaveTrackEffectiveChanges(t *testing.T) {
	moduleDir := t.TempDir()
	path := filepath.Join(moduleDir, "sample.go")
	saved := "package sample\n"
	if err := os.WriteFile(path, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	uri := semanticTestFileURI(path)
	store := newDocumentStore()

	gotPath, changed := store.Open(uri, saved)
	if gotPath != path || changed {
		t.Fatalf("clean Open() = (%q, %t), want (%q, false)", gotPath, changed, path)
	}
	if got := len(store.Overlay(moduleDir)); got != 0 {
		t.Fatalf("overlay length after clean open = %d, want 0", got)
	}

	changedContent := "package sample\n\nvar Changed = true\n"
	gotPath, effectiveChange := store.Change(uri, changedContent)
	if gotPath != path || !effectiveChange {
		t.Fatalf("dirty Change() = (%q, %t), want (%q, true)", gotPath, effectiveChange, path)
	}
	if err := os.WriteFile(path, []byte(changedContent), 0o600); err != nil {
		t.Fatal(err)
	}
	gotPath, effectiveChange = store.Save(uri)
	if gotPath != path || effectiveChange {
		t.Fatalf("matching Save() = (%q, %t), want (%q, false)", gotPath, effectiveChange, path)
	}
	if got := len(store.Overlay(moduleDir)); got != 0 {
		t.Fatalf("overlay length after save = %d, want 0", got)
	}
}

func TestDocumentStoreSaveMismatchReportsEffectiveChange(t *testing.T) {
	moduleDir := t.TempDir()
	path := filepath.Join(moduleDir, "sample.go")
	if err := os.WriteFile(path, []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uri := semanticTestFileURI(path)
	store := newDocumentStore()

	if _, changed := store.Change(uri, "package sample\n\nvar Unsaved = true\n"); !changed {
		t.Fatal("dirty Change() did not report an effective change")
	}
	gotPath, changed := store.Save(uri)
	if gotPath != path || !changed {
		t.Fatalf("mismatched Save() = (%q, %t), want (%q, true)", gotPath, changed, path)
	}

	if got := len(store.Overlay(moduleDir)); got != 0 {
		t.Fatalf("overlay length after mismatched save = %d, want 0", got)
	}
}

func TestDocumentStoreTracksEffectiveOverlayChanges(t *testing.T) {
	moduleDir := t.TempDir()
	insidePath := filepath.Join(moduleDir, "sample.go")
	outsidePath := filepath.Join(t.TempDir(), "outside.go")
	insideURI := semanticTestFileURI(insidePath)
	outsideURI := semanticTestFileURI(outsidePath)
	store := newDocumentStore()

	if _, changed := store.Open(insideURI, "package sample\n"); !changed {
		t.Fatal("dirty Open() did not report an effective change")
	}
	if _, changed := store.Change(insideURI, "package sample\n"); changed {
		t.Fatal("identical Change() reported an effective change")
	}
	store.Open(outsideURI, "package outside\n")

	overlay := store.Overlay(moduleDir)
	if len(overlay) != 1 || string(overlay[insidePath]) != "package sample\n" {
		t.Fatalf("module overlay = %#v, want only inside document", overlay)
	}
	overlay[insidePath][0] = 'X'
	if got := string(store.Overlay(moduleDir)[insidePath]); got != "package sample\n" {
		t.Fatalf("stored overlay changed through returned map: %q", got)
	}

	store.Close(insideURI)
	if got := len(store.Overlay(moduleDir)); got != 0 {
		t.Fatalf("overlay length after close = %d, want 0", got)
	}
}

func TestPathWithinRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		root string
		want bool
	}{
		{name: "nested", path: "/workspace/module/file.go", root: "/workspace/module", want: true},
		{name: "equal", path: "/workspace/module", root: "/workspace/module", want: false},
		{name: "sibling prefix", path: "/workspace/module-old/file.go", root: "/workspace/module", want: false},
		{name: "parent", path: "/workspace", root: "/workspace/module", want: false},
		{name: "filesystem root", path: "/workspace/module/file.go", root: "/", want: true},
		{name: "empty root", path: "/workspace/module/file.go", root: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := pathWithinRoot(test.path, test.root); got != test.want {
				t.Fatalf("pathWithinRoot(%q, %q) = %t, want %t", test.path, test.root, got, test.want)
			}
		})
	}
}

func TestDocumentStoreReturnsOverlaysAcrossWorkspaceModules(t *testing.T) {
	workspaceRoot := t.TempDir()
	firstModule := filepath.Join(workspaceRoot, "first")
	secondModule := filepath.Join(workspaceRoot, "second")
	firstPath := filepath.Join(firstModule, "first.go")
	secondPath := filepath.Join(secondModule, "second.go")
	outsidePath := filepath.Join(workspaceRoot, "notes.go")

	store := newDocumentStore()
	store.Open(semanticTestFileURI(firstPath), "package first\n")
	store.Open(semanticTestFileURI(secondPath), "package second\n")
	store.Open(semanticTestFileURI(outsidePath), "package notes\n")

	overlay := store.OverlayForRoots([]string{firstModule, secondModule})
	if len(overlay) != 2 {
		t.Fatalf("OverlayForRoots() = %#v, want two module documents", overlay)
	}
	if got := string(overlay[firstPath]); got != "package first\n" {
		t.Fatalf("first module overlay = %q", got)
	}
	if got := string(overlay[secondPath]); got != "package second\n" {
		t.Fatalf("second module overlay = %q", got)
	}
	if _, ok := overlay[outsidePath]; ok {
		t.Fatalf("OverlayForRoots() included unrelated workspace file %q", outsidePath)
	}
}
