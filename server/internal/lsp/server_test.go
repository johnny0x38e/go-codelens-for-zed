package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
)

func TestServeInitialize(t *testing.T) {
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "exit",
	})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	response := readTestFrame(t, output)
	if got := response["id"]; got != float64(1) {
		t.Fatalf("response id = %#v, want 1", got)
	}

	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("response result = %#v, want object", response["result"])
	}
	capabilities, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities = %#v, want object", result["capabilities"])
	}
	syncOptions, ok := capabilities["textDocumentSync"].(map[string]any)
	if !ok {
		t.Fatalf("textDocumentSync = %#v, want object", capabilities["textDocumentSync"])
	}
	if got := syncOptions["openClose"]; got != true {
		t.Fatalf("textDocumentSync.openClose = %#v, want true", got)
	}
	if got := syncOptions["change"]; got != float64(2) {
		t.Fatalf("textDocumentSync.change = %#v, want 2", got)
	}
	saveOptions, ok := syncOptions["save"].(map[string]any)
	if !ok || saveOptions["includeText"] != false {
		t.Fatalf("textDocumentSync.save = %#v, want includeText false", syncOptions["save"])
	}
	codeLensProvider, ok := capabilities["codeLensProvider"].(map[string]any)
	if !ok {
		t.Fatalf("codeLensProvider = %#v, want object", capabilities["codeLensProvider"])
	}
	if got := codeLensProvider["resolveProvider"]; got != false {
		t.Fatalf("resolveProvider = %#v, want false", got)
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo = %#v, want object", result["serverInfo"])
	}
	if got := serverInfo["name"]; got != "go-codelens-ls" {
		t.Fatalf("serverInfo.name = %#v, want go-codelens-ls", got)
	}
	if got := serverInfo["version"]; got != serverVersion {
		t.Fatalf("serverInfo.version = %#v, want %s", got, serverVersion)
	}
}

func TestServeRegistersWatchedFiles(t *testing.T) {
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"capabilities": map[string]any{
				"workspace": map[string]any{
					"didChangeWatchedFiles": map[string]any{
						"dynamicRegistration": true,
					},
				},
			},
		},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      "go-codelens-watched-files",
		"result":  nil,
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "exit",
	})

	output := new(bytes.Buffer)
	state := newServerState()
	if err := serveWithState(context.Background(), input, output, nil, state); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if state.watchedFilesRegistration != watchedFilesRegistered {
		t.Fatalf(
			"watched-file registration state = %v, want registered",
			state.watchedFilesRegistration,
		)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	registrationRequest := readTestFrame(t, reader)
	if got := registrationRequest["method"]; got != "client/registerCapability" {
		t.Fatalf("registration request method = %#v, want client/registerCapability", got)
	}
	if got := registrationRequest["id"]; got != "go-codelens-watched-files" {
		t.Fatalf("registration request id = %#v, want go-codelens-watched-files", got)
	}

	params, ok := registrationRequest["params"].(map[string]any)
	if !ok {
		t.Fatalf("registration params = %#v, want object", registrationRequest["params"])
	}
	registrations, ok := params["registrations"].([]any)
	if !ok || len(registrations) != 1 {
		t.Fatalf("registrations = %#v, want one registration", params["registrations"])
	}
	registration, ok := registrations[0].(map[string]any)
	if !ok {
		t.Fatalf("registration = %#v, want object", registrations[0])
	}
	if got := registration["method"]; got != "workspace/didChangeWatchedFiles" {
		t.Fatalf("registered method = %#v, want workspace/didChangeWatchedFiles", got)
	}
	if got := registration["id"]; got != "go-codelens-watched-files" {
		t.Fatalf("registration id = %#v, want go-codelens-watched-files", got)
	}
	options, ok := registration["registerOptions"].(map[string]any)
	if !ok {
		t.Fatalf("registerOptions = %#v, want object", registration["registerOptions"])
	}
	watchers, ok := options["watchers"].([]any)
	if !ok {
		t.Fatalf("watchers = %#v, want array", options["watchers"])
	}
	patterns := make([]string, 0, len(watchers))
	for _, value := range watchers {
		watcher, watcherOK := value.(map[string]any)
		if !watcherOK {
			t.Fatalf("watcher = %#v, want object", value)
		}
		pattern, patternOK := watcher["globPattern"].(string)
		if !patternOK {
			t.Fatalf("globPattern = %#v, want string", watcher["globPattern"])
		}
		patterns = append(patterns, pattern)
	}
	wantPatterns := []string{"**/*.go", "**/go.mod", "**/go.work"}
	if !slices.Equal(patterns, wantPatterns) {
		t.Fatalf("watcher patterns = %#v, want %#v", patterns, wantPatterns)
	}
	if _, err := readFrame(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected response after registration acknowledgement: %v", err)
	}
}

func TestWatchedFilesRegistrationWaitsForAcknowledgement(t *testing.T) {
	state := newServerState()
	state.watchedFilesSupported = true

	if _, ok := state.watchedFilesRegistrationRequest(); !ok {
		t.Fatal("watchedFilesRegistrationRequest() did not create a supported registration")
	}
	if state.watchedFilesRegistration != watchedFilesRegistrationPending {
		t.Fatalf(
			"watched-file registration state before acknowledgement = %v, want pending",
			state.watchedFilesRegistration,
		)
	}
	if _, ok := state.watchedFilesRegistrationRequest(); ok {
		t.Fatal("watchedFilesRegistrationRequest() duplicated a pending registration")
	}

	reply := response{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage(`"go-codelens-watched-files"`),
		Result:  json.RawMessage("null"),
	}
	if err := state.handleWatchedFilesRegistrationResponse(reply); err != nil {
		t.Fatalf("handleWatchedFilesRegistrationResponse() error = %v", err)
	}
	if state.watchedFilesRegistration != watchedFilesRegistered {
		t.Fatalf(
			"watched-file registration state after acknowledgement = %v, want registered",
			state.watchedFilesRegistration,
		)
	}
}

func TestServeWatchedFilesRegistrationErrorLeavesRegistrationRetryable(t *testing.T) {
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"capabilities": map[string]any{
				"workspace": map[string]any{
					"didChangeWatchedFiles": map[string]any{
						"dynamicRegistration": true,
					},
				},
			},
		},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      "go-codelens-watched-files",
		"error": map[string]any{
			"code":    -32601,
			"message": "dynamic registration rejected",
		},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "exit",
	})

	output := new(bytes.Buffer)
	trace := new(bytes.Buffer)
	state := newServerState()
	if err := serveWithState(context.Background(), input, output, trace, state); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if state.watchedFilesRegistration != watchedFilesUnregistered {
		t.Fatalf(
			"watched-file registration state = %v, want unregistered",
			state.watchedFilesRegistration,
		)
	}
	if _, ok := state.watchedFilesRegistrationRequest(); !ok {
		t.Fatal("watched-file registration was not retryable after an error response")
	}
	if got := trace.String(); !strings.Contains(
		got,
		"request client/registerCapability error: register workspace/didChangeWatchedFiles: client error -32601: dynamic registration rejected\n",
	) {
		t.Fatalf("trace = %q, want registration error", got)
	}
}

func TestServeSkipsWatchedFilesRegistrationWithoutClientSupport(t *testing.T) {
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "exit",
	})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	if _, err := readFrame(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected registration without client support: %v", err)
	}
}

func TestServeCodeLensMalformedParamsReturnsInvalidParams(t *testing.T) {
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "textDocument/codeLens",
		"params":  map[string]any{"textDocument": map[string]any{}},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "exit",
	})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	response := readTestFrame(t, output)
	errorValue, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %#v, want object", response["error"])
	}
	if got := errorValue["code"]; got != float64(-32602) {
		t.Fatalf("error code = %#v, want -32602", got)
	}
}

func TestServeCodeLensOutsideModuleReturnsEmpty(t *testing.T) {
	const uri = "file:///workspace/main.go"

	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "textDocument/codeLens",
		"params": map[string]any{
			"textDocument": map[string]any{"uri": uri},
		},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "exit",
	})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	response := readTestFrame(t, output)
	if got := response["id"]; got != float64(2) {
		t.Fatalf("response id = %#v, want 2", got)
	}
	lenses, ok := response["result"].([]any)
	if !ok || len(lenses) != 0 {
		t.Fatalf("result = %#v, want empty CodeLens array", response["result"])
	}
}

func TestServeCancelsCodeLensRequest(t *testing.T) {
	ctx := t.Context()
	moduleRoot := t.TempDir()
	filename := filepath.Join(moduleRoot, "sample.go")
	uri := semanticTestFileURI(filename)
	state := newServerStateWithContext(ctx)
	state.findScope = func(_, _ string) (analysis.AnalysisScope, error) {
		return analysis.ModuleAnalysisScope(moduleRoot), nil
	}
	analysisStarted := make(chan struct{})
	analysisCanceled := make(chan struct{})
	state.analyzeScope = func(
		analysisCtx context.Context,
		_ analysis.AnalysisScope,
		_ analysis.ChangeSet,
		_ map[string][]byte,
	) (analysis.Snapshot, error) {
		close(analysisStarted)
		<-analysisCtx.Done()
		close(analysisCanceled)
		return analysis.Snapshot{}, analysisCtx.Err()
	}

	inputReader, inputWriter := io.Pipe()
	output := new(bytes.Buffer)
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- serveWithState(ctx, inputReader, output, nil, state)
	}()

	writeTestFrame(t, inputWriter, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(moduleRoot)},
	})
	writeTestFrame(t, inputWriter, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "textDocument/codeLens",
		"params":  map[string]any{"textDocument": map[string]any{"uri": uri}},
	})
	<-analysisStarted
	writeTestFrame(t, inputWriter, map[string]any{
		"jsonrpc": "2.0",
		"method":  "$/cancelRequest",
		"params":  map[string]any{"id": 2},
	})
	<-analysisCanceled
	writeTestFrame(t, inputWriter, map[string]any{"jsonrpc": "2.0", "method": "exit"})
	if err := inputWriter.Close(); err != nil {
		t.Fatalf("close LSP input: %v", err)
	}
	if err := <-serveResult; err != nil {
		t.Fatalf("serveWithState() error = %v", err)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	reply := readTestFrame(t, reader)
	if got := reply["id"]; got != float64(2) {
		t.Fatalf("response id = %#v, want 2", got)
	}
	errorValue, ok := reply["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %#v, want object", reply["error"])
	}
	if got := errorValue["code"]; got != float64(requestCanceledCode) {
		t.Fatalf("error code = %#v, want %d", got, requestCanceledCode)
	}
}

func TestServeWithTraceRecordsRequestAndResponseMethods(t *testing.T) {
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "exit",
	})

	output := new(bytes.Buffer)
	trace := new(bytes.Buffer)
	if err := ServeWithTrace(context.Background(), input, output, trace); err != nil {
		t.Fatalf("ServeWithTrace() error = %v", err)
	}

	if got := trace.String(); got != "--> initialize\n<-- initialize\n--> exit\n" {
		t.Fatalf("trace = %q", got)
	}
}

func assertTestPosition(t *testing.T, value any, line, character float64) {
	t.Helper()

	position, ok := value.(map[string]any)
	if !ok || position["line"] != line || position["character"] != character {
		t.Fatalf("position = %#v, want line %v character %v", value, line, character)
	}
}

func writeTestFrame(t *testing.T, dst io.Writer, message any) {
	t.Helper()

	body, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if _, err := fmt.Fprintf(dst, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		t.Fatalf("write header error = %v", err)
	}
	if _, err := dst.Write(body); err != nil {
		t.Fatalf("write body error = %v", err)
	}
}

func readTestFrame(t *testing.T, src io.Reader) map[string]any {
	t.Helper()

	reader, ok := src.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(src)
	}
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read header error = %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		contentLength, err = strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			t.Fatalf("parse Content-Length error = %v", err)
		}
	}
	if contentLength < 0 {
		t.Fatal("response has no Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatalf("read body error = %v", err)
	}

	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; body = %q", err, body)
	}
	return response
}
