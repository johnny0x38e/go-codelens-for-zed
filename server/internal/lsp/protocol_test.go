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
	const raw = `{"textDocument":{"uri":"file:///workspace/main.go","version":7},"contentChanges":[{"text":"package main\n"},{"text":"package main\n\nfunc main() {}\n"}]}`

	var params didChangeTextDocumentParams
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if params.TextDocument.URI != "file:///workspace/main.go" {
		t.Fatalf("TextDocument.URI = %q, want file:///workspace/main.go", params.TextDocument.URI)
	}
	if len(params.ContentChanges) != 2 {
		t.Fatalf("len(ContentChanges) = %d, want 2", len(params.ContentChanges))
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
