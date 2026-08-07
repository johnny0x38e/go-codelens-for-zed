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

	gotPath, changed, err := store.Open(uri, saved, 1)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if gotPath != path || changed {
		t.Fatalf("clean Open() = (%q, %t), want (%q, false)", gotPath, changed, path)
	}
	if got := len(store.Overlay(moduleDir)); got != 0 {
		t.Fatalf("overlay length after clean open = %d, want 0", got)
	}

	changedContent := "package sample\n\nvar Changed = true\n"
	gotPath, effectiveChange, err := store.Change(
		uri,
		2,
		[]textDocumentContentChangeEvent{{Text: changedContent}},
	)
	if err != nil {
		t.Fatalf("Change() error = %v", err)
	}
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

func TestDocumentStoreSaveMismatchKeepsOverlayAndReportsChange(t *testing.T) {
	moduleDir := t.TempDir()
	path := filepath.Join(moduleDir, "sample.go")
	if err := os.WriteFile(path, []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uri := semanticTestFileURI(path)
	store := newDocumentStore()

	if _, _, err := store.Open(uri, "package sample\n", 1); err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	const changedContent = "package sample\n\nvar Unsaved = true\n"
	changes := []textDocumentContentChangeEvent{{Text: changedContent}}
	if _, changed, err := store.Change(uri, 2, changes); err != nil || !changed {
		t.Fatal("dirty Change() did not report an effective change")
	}
	gotPath, changed := store.Save(uri)
	if gotPath != path || !changed {
		t.Fatalf("mismatched Save() = (%q, %t), want (%q, true)", gotPath, changed, path)
	}

	overlay := store.Overlay(moduleDir)
	if got := string(overlay[path]); len(overlay) != 1 || got != changedContent {
		t.Fatalf("overlay after mismatched save = %#v, want editor buffer %q", overlay, changedContent)
	}

	if err := os.WriteFile(path, []byte(changedContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if gotPath, changed := store.Save(uri); gotPath != path || changed {
		t.Fatalf("matching Save() = (%q, %t), want (%q, false)", gotPath, changed, path)
	}
	if got := len(store.Overlay(moduleDir)); got != 0 {
		t.Fatalf("overlay length after matching save = %d, want 0", got)
	}
}

func TestDocumentStoreTracksEffectiveOverlayChanges(t *testing.T) {
	moduleDir := t.TempDir()
	insidePath := filepath.Join(moduleDir, "sample.go")
	outsidePath := filepath.Join(t.TempDir(), "outside.go")
	insideURI := semanticTestFileURI(insidePath)
	outsideURI := semanticTestFileURI(outsidePath)
	store := newDocumentStore()

	if _, changed, err := store.Open(insideURI, "package sample\n", 1); err != nil || !changed {
		t.Fatal("dirty Open() did not report an effective change")
	}
	identical := []textDocumentContentChangeEvent{{Text: "package sample\n"}}
	if _, changed, err := store.Change(insideURI, 2, identical); err != nil || changed {
		t.Fatal("identical Change() reported an effective change")
	}
	if _, _, err := store.Open(outsideURI, "package outside\n", 1); err != nil {
		t.Fatalf("Open(outside) error = %v", err)
	}

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
	if _, _, err := store.Open(semanticTestFileURI(firstPath), "package first\n", 1); err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	if _, _, err := store.Open(semanticTestFileURI(secondPath), "package second\n", 1); err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	if _, _, err := store.Open(semanticTestFileURI(outsidePath), "package notes\n", 1); err != nil {
		t.Fatalf("Open(outside) error = %v", err)
	}

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

func TestDocumentStoreIgnoresStaleVersionsAndRetainsSavedOpenDocument(t *testing.T) {
	moduleDir := t.TempDir()
	path := filepath.Join(moduleDir, "sample.go")
	original := "package sample\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	uri := semanticTestFileURI(path)
	store := newDocumentStore()

	gotPath, changed, err := store.Open(uri, original, 1)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if gotPath != path || changed {
		t.Fatalf("Open() = (%q, %t), want (%q, false)", gotPath, changed, path)
	}

	changes := []textDocumentContentChangeEvent{
		textBufferRangeChange(0, 14, 0, 14, "\nvar Changed = true"),
	}
	gotPath, changed, err = store.Change(uri, 2, changes)
	if err != nil {
		t.Fatalf("Change() error = %v", err)
	}
	if gotPath != path || !changed {
		t.Fatalf("Change() = (%q, %t), want (%q, true)", gotPath, changed, path)
	}
	wantDirty := "package sample\nvar Changed = true\n"
	if got := string(store.Overlay(moduleDir)[path]); got != wantDirty {
		t.Fatalf("dirty overlay = %q, want %q", got, wantDirty)
	}

	gotPath, changed, err = store.Change(uri, 2, []textDocumentContentChangeEvent{{Text: original}})
	if err != nil {
		t.Fatalf("stale Change() error = %v", err)
	}
	if gotPath != path || changed {
		t.Fatalf("stale Change() = (%q, %t), want (%q, false)", gotPath, changed, path)
	}
	if got := string(store.Overlay(moduleDir)[path]); got != wantDirty {
		t.Fatalf("stale change modified overlay to %q", got)
	}

	if err := os.WriteFile(path, []byte(wantDirty), 0o600); err != nil {
		t.Fatal(err)
	}
	gotPath, changed = store.Save(uri)
	if gotPath != path || changed {
		t.Fatalf("Save() = (%q, %t), want (%q, false)", gotPath, changed, path)
	}
	if got := len(store.Overlay(moduleDir)); got != 0 {
		t.Fatalf("overlay length after save = %d, want 0", got)
	}
	document, ok := store.documents[path]
	if !ok {
		t.Fatal("Save() removed the open document")
	}
	if got := string(document.buffer.bytes()); got != wantDirty {
		t.Fatalf("saved open buffer = %q, want %q", got, wantDirty)
	}

	store.Close(uri)
	if _, ok := store.documents[path]; ok {
		t.Fatal("Close() retained the open document")
	}
}

func TestDocumentStoreInvalidIncrementalChangePreservesVersionAndContent(t *testing.T) {
	moduleDir := t.TempDir()
	path := filepath.Join(moduleDir, "sample.go")
	original := "package sample\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	uri := semanticTestFileURI(path)
	store := newDocumentStore()
	if _, _, err := store.Open(uri, original, 5); err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	valid := []textDocumentContentChangeEvent{
		textBufferRangeChange(0, 14, 0, 14, " // v6"),
	}
	if _, changed, err := store.Change(uri, 6, valid); err != nil || !changed {
		t.Fatalf("valid Change() = (changed %t, error %v), want (true, nil)", changed, err)
	}
	want := "package sample // v6\n"

	invalid := []textDocumentContentChangeEvent{
		textBufferRangeChange(9, 0, 9, 0, "invalid"),
	}
	if _, changed, err := store.Change(uri, 7, invalid); err == nil || changed {
		t.Fatalf("invalid Change() = (changed %t, error %v), want (false, non-nil)", changed, err)
	}
	document := store.documents[path]
	if document.version != 6 {
		t.Fatalf("version after invalid change = %d, want 6", document.version)
	}
	if got := string(store.Overlay(moduleDir)[path]); got != want {
		t.Fatalf("overlay after invalid change = %q, want %q", got, want)
	}

	if _, changed, err := store.Change(uri, 7, []textDocumentContentChangeEvent{{Text: original}}); err != nil || !changed {
		t.Fatalf("replacement Change() = (changed %t, error %v), want (true, nil)", changed, err)
	}
	if document.version != 7 {
		t.Fatalf("version after replacement = %d, want 7", document.version)
	}
}
