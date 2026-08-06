package lsp

import (
	"encoding/json"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *responseError  `json:"error,omitempty"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initializeParams struct {
	RootURI          string            `json:"rootUri"`
	WorkspaceFolders []workspaceFolder `json:"workspaceFolders"`
}

type workspaceFolder struct {
	URI string `json:"uri"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type openedTextDocument struct {
	URI  string `json:"uri"`
	Text string `json:"text"`
}

type didOpenTextDocumentParams struct {
	TextDocument openedTextDocument `json:"textDocument"`
}

type textDocumentContentChangeEvent struct {
	Text string `json:"text"`
}

type didChangeTextDocumentParams struct {
	TextDocument   textDocumentIdentifier           `json:"textDocument"`
	ContentChanges []textDocumentContentChangeEvent `json:"contentChanges"`
}

type textDocumentParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type fileEvent struct {
	URI string `json:"uri"`
}

type didChangeWatchedFilesParams struct {
	Changes []fileEvent `json:"changes"`
}

type codeLens struct {
	Range   analysis.Range `json:"range"`
	Command command        `json:"command"`
}

type command struct {
	Title     string `json:"title"`
	Command   string `json:"command"`
	Arguments []any  `json:"arguments,omitempty"`
}
