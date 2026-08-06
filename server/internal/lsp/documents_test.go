package lsp

import (
	"path/filepath"
	"testing"
)

func TestDocumentStoreTracksEffectiveOverlayChanges(t *testing.T) {
	moduleDir := t.TempDir()
	insidePath := filepath.Join(moduleDir, "sample.go")
	outsidePath := filepath.Join(t.TempDir(), "outside.go")
	insideURI := semanticTestFileURI(insidePath)
	outsideURI := semanticTestFileURI(outsidePath)
	store := newDocumentStore()

	store.Open(insideURI, "package sample\n")
	if got := store.Generation(); got != 1 {
		t.Fatalf("generation after open = %d, want 1", got)
	}
	store.Change(insideURI, "package sample\n")
	if got := store.Generation(); got != 1 {
		t.Fatalf("generation after identical change = %d, want 1", got)
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
