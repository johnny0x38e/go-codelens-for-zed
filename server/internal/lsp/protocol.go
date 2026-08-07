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

type cancelRequestParams struct {
	ID json.RawMessage `json:"id"`
}

type initializeParams struct {
	RootURI          string             `json:"rootUri"`
	WorkspaceFolders []workspaceFolder  `json:"workspaceFolders"`
	Capabilities     clientCapabilities `json:"capabilities"`
}

type clientCapabilities struct {
	Workspace workspaceClientCapabilities `json:"workspace"`
}

type workspaceClientCapabilities struct {
	DidChangeWatchedFiles didChangeWatchedFilesClientCapabilities `json:"didChangeWatchedFiles"`
}

type didChangeWatchedFilesClientCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration"`
}

type workspaceFolder struct {
	URI string `json:"uri"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type openedTextDocument struct {
	URI     string `json:"uri"`
	Version int64  `json:"version"`
	Text    string `json:"text"`
}

type didOpenTextDocumentParams struct {
	TextDocument openedTextDocument `json:"textDocument"`
}

type textDocumentContentChangeEvent struct {
	Range       *analysis.Range `json:"range,omitempty"`
	RangeLength *uint32         `json:"rangeLength,omitempty"`
	Text        string          `json:"text"`
}

type versionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int64  `json:"version"`
}

type didChangeTextDocumentParams struct {
	TextDocument   versionedTextDocumentIdentifier  `json:"textDocument"`
	ContentChanges []textDocumentContentChangeEvent `json:"contentChanges"`
}

type textDocumentSyncOptions struct {
	OpenClose bool                    `json:"openClose"`
	Change    int                     `json:"change"`
	Save      textDocumentSaveOptions `json:"save"`
}

type textDocumentSaveOptions struct {
	IncludeText bool `json:"includeText"`
}

type textDocumentParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type fileEvent struct {
	URI  string `json:"uri"`
	Type int    `json:"type"`
}

type didChangeWatchedFilesParams struct {
	Changes []fileEvent `json:"changes"`
}

type registrationParams struct {
	Registrations []registration `json:"registrations"`
}

type registrationRequest struct {
	JSONRPC string             `json:"jsonrpc"`
	ID      string             `json:"id"`
	Method  string             `json:"method"`
	Params  registrationParams `json:"params"`
}

type registration struct {
	ID              string                               `json:"id"`
	Method          string                               `json:"method"`
	RegisterOptions didChangeWatchedFilesRegisterOptions `json:"registerOptions"`
}

type didChangeWatchedFilesRegisterOptions struct {
	Watchers []fileSystemWatcher `json:"watchers"`
}

type fileSystemWatcher struct {
	GlobPattern string `json:"globPattern"`
}

type codeLens struct {
	Range   analysis.Range `json:"range"`
	Command command        `json:"command"`
}

type command struct {
	Title     string                  `json:"title"`
	Command   string                  `json:"command"`
	Arguments showReferencesArguments `json:"arguments,omitempty"`
}

// showReferencesArguments is fixed-size because the command always carries
// exactly [document URI, position, locations]. An array avoids a per-lens slice
// allocation while preserving the JSON array required by Zed.
type showReferencesArguments [3]any
