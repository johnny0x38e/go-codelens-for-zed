package lsp

import (
	"encoding/json"
	"testing"
)

func TestInitializeParamsDecode(t *testing.T) {
	const raw = `{"rootUri":"file:///workspace","workspaceFolders":[{"uri":"file:///workspace/one","name":"one"},{"uri":"file:///workspace/two","name":"two"}]}`

	var params initializeParams
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if params.RootURI != "file:///workspace" {
		t.Fatalf("RootURI = %q, want file:///workspace", params.RootURI)
	}
	if len(params.WorkspaceFolders) != 2 {
		t.Fatalf("len(WorkspaceFolders) = %d, want 2", len(params.WorkspaceFolders))
	}
	if params.WorkspaceFolders[0].URI != "file:///workspace/one" || params.WorkspaceFolders[1].URI != "file:///workspace/two" {
		t.Fatalf("WorkspaceFolders = %#v, want both workspace URIs", params.WorkspaceFolders)
	}
}

func TestDidChangeParamsDecode(t *testing.T) {
	const raw = `{"textDocument":{"uri":"file:///workspace/main.go","version":7},"contentChanges":[{"range":{"start":{"line":0,"character":1},"end":{"line":0,"character":3}},"rangeLength":2,"text":"x"},{"text":"package main\n\nfunc main() {}\n"}]}`

	var params didChangeTextDocumentParams
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if params.TextDocument.URI != "file:///workspace/main.go" {
		t.Fatalf("TextDocument.URI = %q, want file:///workspace/main.go", params.TextDocument.URI)
	}
	if params.TextDocument.Version != 7 {
		t.Fatalf("TextDocument.Version = %d, want 7", params.TextDocument.Version)
	}
	if len(params.ContentChanges) != 2 {
		t.Fatalf("len(ContentChanges) = %d, want 2", len(params.ContentChanges))
	}
	first := params.ContentChanges[0]
	if first.Range == nil || first.Range.Start.Line != 0 || first.Range.Start.Character != 1 ||
		first.Range.End.Line != 0 || first.Range.End.Character != 3 {
		t.Fatalf("first content change range = %#v", first.Range)
	}
	if first.RangeLength == nil || *first.RangeLength != 2 {
		t.Fatalf("first content change rangeLength = %#v, want 2", first.RangeLength)
	}
	if got := params.ContentChanges[1].Text; got != "package main\n\nfunc main() {}\n" {
		t.Fatalf("last content change = %q", got)
	}
}

func TestWatchedFileParamsDecode(t *testing.T) {
	const raw = `{"changes":[{"uri":"file:///workspace/go.mod","type":2},{"uri":"file:///workspace/main.go","type":1}]}`

	var params didChangeWatchedFilesParams
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(params.Changes) != 2 {
		t.Fatalf("len(Changes) = %d, want 2", len(params.Changes))
	}
	if params.Changes[0].URI != "file:///workspace/go.mod" || params.Changes[1].URI != "file:///workspace/main.go" {
		t.Fatalf("Changes = %#v, want both watched file URIs", params.Changes)
	}
}
