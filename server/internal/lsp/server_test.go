package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
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
	if got := capabilities["textDocumentSync"]; got != float64(1) {
		t.Fatalf("textDocumentSync = %#v, want 1", got)
	}
	codeLensProvider, ok := capabilities["codeLensProvider"].(map[string]any)
	if !ok {
		t.Fatalf("codeLensProvider = %#v, want object", capabilities["codeLensProvider"])
	}
	if got := codeLensProvider["resolveProvider"]; got != false {
		t.Fatalf("resolveProvider = %#v, want false", got)
	}
}

func TestServeCodeLens(t *testing.T) {
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
	if !ok || len(lenses) != 1 {
		t.Fatalf("result = %#v, want one CodeLens", response["result"])
	}

	lens, ok := lenses[0].(map[string]any)
	if !ok {
		t.Fatalf("lens = %#v, want object", lenses[0])
	}
	assertTestRange(t, lens["range"], 4, 0)
	command, ok := lens["command"].(map[string]any)
	if !ok {
		t.Fatalf("command = %#v, want object", lens["command"])
	}
	if got := command["title"]; got != "Go Implementation Lens: POC" {
		t.Fatalf("command title = %#v", got)
	}
	if got := command["command"]; got != "editor.action.showReferences" {
		t.Fatalf("command id = %#v", got)
	}

	arguments, ok := command["arguments"].([]any)
	if !ok || len(arguments) != 3 {
		t.Fatalf("command arguments = %#v, want three arguments", command["arguments"])
	}
	if got := arguments[0]; got != uri {
		t.Fatalf("command uri = %#v, want %q", got, uri)
	}
	assertTestPosition(t, arguments[1], 4, 0)
	locations, ok := arguments[2].([]any)
	if !ok || len(locations) != 1 {
		t.Fatalf("locations = %#v, want one location", arguments[2])
	}
	location, ok := locations[0].(map[string]any)
	if !ok || location["uri"] != uri {
		t.Fatalf("location = %#v, want uri %q", locations[0], uri)
	}
	assertTestRange(t, location["range"], 4, 0)
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

func assertTestRange(t *testing.T, value any, line, character float64) {
	t.Helper()

	rangeValue, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("range = %#v, want object", value)
	}
	assertTestPosition(t, rangeValue["start"], line, character)
	assertTestPosition(t, rangeValue["end"], line, character)
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

	reader := bufio.NewReader(src)
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
