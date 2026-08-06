package lsp

import (
	"bufio"
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestServeReturnsLensesForRepositoryFixtureWithoutDidOpen(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test filename")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", ".."))
	documentURI := semanticTestFileURI(filepath.Join(repositoryRoot, "examples", "poc", "main.go"))

	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(repositoryRoot)},
	})
	writeSemanticTestCodeLensRequest(t, input, 2, documentURI)
	writeTestFrame(t, input, map[string]any{"jsonrpc": "2.0", "method": "exit"})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	lenses := semanticTestResponseLenses(t, readTestFrame(t, reader))
	_ = findSemanticTestLens(t, lenses, "2 implementations", 4)
	_ = findSemanticTestLens(t, lenses, "Implements: Greeter", 8)
	_ = findSemanticTestLens(t, lenses, "Implements: Greeter", 16)
}

func TestServeReturnsSemanticTypeLenses(t *testing.T) {
	moduleDir := t.TempDir()
	source := []byte(`package sample

type Greeter interface {
	Greet() string
}

type Person struct{}

func (Person) Greet() string { return "hello" }

type Broken struct{}

func (Broken) Greet(prefix string) string { return prefix }
`)
	writeSemanticTestFile(t, filepath.Join(moduleDir, "go.mod"), []byte("module example.com/sample\n\ngo 1.26\n"))
	filename := filepath.Join(moduleDir, "sample.go")
	writeSemanticTestFile(t, filename, source)

	rootURI := semanticTestFileURI(moduleDir)
	documentURI := semanticTestFileURI(filename)
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": rootURI},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didOpen",
		"params": map[string]any{
			"textDocument": map[string]any{
				"uri":        documentURI,
				"languageId": "go",
				"version":    1,
				"text":       string(source),
			},
		},
	})
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "textDocument/codeLens",
		"params": map[string]any{
			"textDocument": map[string]any{"uri": documentURI},
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

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader) // initialize response
	response := readTestFrame(t, reader)
	lenses, ok := response["result"].([]any)
	if !ok {
		t.Fatalf("result = %#v, want CodeLens array", response["result"])
	}

	interfaceLens := findSemanticTestLens(t, lenses, "1 implementation", 2)
	assertSemanticTestRangeStart(t, interfaceLens["range"], 2, 5)
	assertSemanticTestTargets(t, interfaceLens, documentURI, 6, 5)

	typeLens := findSemanticTestLens(t, lenses, "Implements: Greeter", 6)
	assertSemanticTestRangeStart(t, typeLens["range"], 6, 5)
	assertSemanticTestTargets(t, typeLens, documentURI, 2, 5)

	for _, value := range lenses {
		lens, ok := value.(map[string]any)
		if !ok || semanticTestStartLine(lens["range"]) != 10 {
			continue
		}
		t.Fatalf("Broken received unexpected CodeLens: %#v", lens)
	}
}

func TestServeSuppressesUniversalEmptyInterfaceRelationships(t *testing.T) {
	moduleDir := t.TempDir()
	source := []byte(`package sample

type Any interface{}

type Value struct{}
`)
	writeSemanticTestFile(t, filepath.Join(moduleDir, "go.mod"), []byte("module example.com/sample\n\ngo 1.26\n"))
	filename := filepath.Join(moduleDir, "sample.go")
	writeSemanticTestFile(t, filename, source)

	documentURI := semanticTestFileURI(filename)
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(moduleDir)},
	})
	writeSemanticTestDidOpen(t, input, documentURI, source)
	writeSemanticTestCodeLensRequest(t, input, 2, documentURI)
	writeTestFrame(t, input, map[string]any{"jsonrpc": "2.0", "method": "exit"})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	lenses := semanticTestResponseLenses(t, readTestFrame(t, reader))
	zeroImplementations := findSemanticTestLens(t, lenses, "0 implementations", 2)
	assertSemanticTestTargetStarts(t, zeroImplementations)
	for _, value := range lenses {
		lens, ok := value.(map[string]any)
		if ok && semanticTestStartLine(lens["range"]) == 4 {
			t.Fatalf("empty interface produced a concrete type CodeLens: %#v", lens)
		}
	}
}

func TestServeReturnsCrossFileMethodLenses(t *testing.T) {
	moduleDir := t.TempDir()
	contractSource := []byte(`package sample

type Store interface {
	Put(key string) error
	Get(key string) (string, error)
}
`)
	storeSource := []byte(`package sample

type MemoryStore struct{}

func (*MemoryStore) Put(key string) error { return nil }

func (*MemoryStore) Get(key string) (string, error) { return "", nil }

type PartialStore struct{}

func (PartialStore) Put(key string) error { return nil }

type WrongStore struct{}

func (WrongStore) Get(key string) error { return nil }
`)
	writeSemanticTestFile(t, filepath.Join(moduleDir, "go.mod"), []byte("module example.com/sample\n\ngo 1.26\n"))
	contractFilename := filepath.Join(moduleDir, "contract.go")
	storeFilename := filepath.Join(moduleDir, "store.go")
	writeSemanticTestFile(t, contractFilename, contractSource)
	writeSemanticTestFile(t, storeFilename, storeSource)

	rootURI := semanticTestFileURI(moduleDir)
	contractURI := semanticTestFileURI(contractFilename)
	storeURI := semanticTestFileURI(storeFilename)
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": rootURI},
	})
	writeSemanticTestDidOpen(t, input, contractURI, contractSource)
	writeSemanticTestDidOpen(t, input, storeURI, storeSource)
	writeSemanticTestCodeLensRequest(t, input, 2, contractURI)
	writeSemanticTestCodeLensRequest(t, input, 3, storeURI)
	writeTestFrame(t, input, map[string]any{"jsonrpc": "2.0", "method": "exit"})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	contractLenses := semanticTestResponseLenses(t, readTestFrame(t, reader))
	storeLenses := semanticTestResponseLenses(t, readTestFrame(t, reader))

	storeHeader := findSemanticTestLens(t, contractLenses, "1 implementation", 2)
	assertSemanticTestTargets(t, storeHeader, storeURI, 2, 5)
	putContract := findSemanticTestLens(t, contractLenses, "2 implementations", 3)
	assertSemanticTestTargetStarts(t, putContract,
		semanticTestTarget{URI: storeURI, Line: 4, Character: 20},
		semanticTestTarget{URI: storeURI, Line: 10, Character: 20},
	)
	getContract := findSemanticTestLens(t, contractLenses, "1 implementation", 4)
	assertSemanticTestTargets(t, getContract, storeURI, 6, 20)

	memoryType := findSemanticTestLens(t, storeLenses, "Implements: Store", 2)
	assertSemanticTestTargets(t, memoryType, contractURI, 2, 5)
	memoryPut := findSemanticTestLens(t, storeLenses, "Implements: Store.Put", 4)
	assertSemanticTestTargets(t, memoryPut, contractURI, 3, 1)
	_ = findSemanticTestLens(t, storeLenses, "Implements: Store.Get", 6)
	partialPut := findSemanticTestLens(t, storeLenses, "Implements: Store.Put", 10)
	assertSemanticTestTargets(t, partialPut, contractURI, 3, 1)

	for _, value := range storeLenses {
		lens, ok := value.(map[string]any)
		if ok && semanticTestStartLine(lens["range"]) == 14 {
			t.Fatalf("WrongStore.Get received unexpected CodeLens: %#v", lens)
		}
	}
}

func TestServeInvalidatesSemanticLensesForUnsavedSignatureChange(t *testing.T) {
	moduleDir := t.TempDir()
	savedSource := []byte(`package sample

type Greeter interface {
	Greet() string
}

type Person struct{}

func (Person) Greet() string { return "hello" }
`)
	changedSource := []byte(`package sample

type Greeter interface {
	Greet(prefix string) string
}

type Person struct{}

func (Person) Greet() string { return "hello" }
`)
	writeSemanticTestFile(t, filepath.Join(moduleDir, "go.mod"), []byte("module example.com/sample\n\ngo 1.26\n"))
	filename := filepath.Join(moduleDir, "sample.go")
	writeSemanticTestFile(t, filename, savedSource)

	documentURI := semanticTestFileURI(filename)
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(moduleDir)},
	})
	writeSemanticTestDidOpen(t, input, documentURI, savedSource)
	writeSemanticTestCodeLensRequest(t, input, 2, documentURI)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didChange",
		"params": map[string]any{
			"textDocument": map[string]any{"uri": documentURI, "version": 2},
			"contentChanges": []any{
				map[string]any{"text": string(changedSource)},
			},
		},
	})
	writeSemanticTestCodeLensRequest(t, input, 3, documentURI)
	writeTestFrame(t, input, map[string]any{"jsonrpc": "2.0", "method": "exit"})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	beforeChange := semanticTestResponseLenses(t, readTestFrame(t, reader))
	afterChange := semanticTestResponseLenses(t, readTestFrame(t, reader))

	_ = findSemanticTestLens(t, beforeChange, "1 implementation", 2)
	_ = findSemanticTestLens(t, beforeChange, "Implements: Greeter", 6)
	_ = findSemanticTestLens(t, beforeChange, "Implements: Greeter.Greet", 8)

	zeroTypeImplementations := findSemanticTestLens(t, afterChange, "0 implementations", 2)
	assertSemanticTestTargetStarts(t, zeroTypeImplementations)
	zeroMethodImplementations := findSemanticTestLens(t, afterChange, "0 implementations", 3)
	assertSemanticTestTargetStarts(t, zeroMethodImplementations)
	for _, value := range afterChange {
		lens, ok := value.(map[string]any)
		if !ok {
			continue
		}
		line := semanticTestStartLine(lens["range"])
		if line == 6 || line == 8 {
			t.Fatalf("stale implementation CodeLens remains after didChange: %#v", lens)
		}
	}
}

func writeSemanticTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}

func semanticTestFileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func writeSemanticTestDidOpen(t *testing.T, input *bytes.Buffer, uri string, source []byte) {
	t.Helper()
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didOpen",
		"params": map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": "go",
				"version":    1,
				"text":       string(source),
			},
		},
	})
}

func writeSemanticTestCodeLensRequest(t *testing.T, input *bytes.Buffer, id int, uri string) {
	t.Helper()
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "textDocument/codeLens",
		"params": map[string]any{
			"textDocument": map[string]any{"uri": uri},
		},
	})
}

func semanticTestResponseLenses(t *testing.T, response map[string]any) []any {
	t.Helper()
	lenses, ok := response["result"].([]any)
	if !ok {
		t.Fatalf("result = %#v, want CodeLens array", response["result"])
	}
	return lenses
}

func findSemanticTestLens(t *testing.T, lenses []any, title string, line int) map[string]any {
	t.Helper()
	for _, value := range lenses {
		lens, ok := value.(map[string]any)
		if !ok || semanticTestStartLine(lens["range"]) != line {
			continue
		}
		command, ok := lens["command"].(map[string]any)
		if ok && command["title"] == title {
			return lens
		}
	}
	t.Fatalf("no CodeLens titled %q on line %d in %#v", title, line, lenses)
	return nil
}

func assertSemanticTestRangeStart(t *testing.T, value any, line, character float64) {
	t.Helper()
	rangeValue, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("range = %#v, want object", value)
	}
	assertTestPosition(t, rangeValue["start"], line, character)
}

func assertSemanticTestTargets(t *testing.T, lens map[string]any, uri string, line, character float64) {
	t.Helper()
	command, ok := lens["command"].(map[string]any)
	if !ok {
		t.Fatalf("command = %#v, want object", lens["command"])
	}
	arguments, ok := command["arguments"].([]any)
	if !ok || len(arguments) != 3 {
		t.Fatalf("arguments = %#v, want three values", command["arguments"])
	}
	targets, ok := arguments[2].([]any)
	if !ok || len(targets) != 1 {
		t.Fatalf("targets = %#v, want one location", arguments[2])
	}
	target, ok := targets[0].(map[string]any)
	if !ok || target["uri"] != uri {
		t.Fatalf("target = %#v, want uri %q", targets[0], uri)
	}
	rangeValue, ok := target["range"].(map[string]any)
	if !ok {
		t.Fatalf("target range = %#v, want object", target["range"])
	}
	assertTestPosition(t, rangeValue["start"], line, character)
}

type semanticTestTarget struct {
	URI       string
	Line      float64
	Character float64
}

func assertSemanticTestTargetStarts(t *testing.T, lens map[string]any, expected ...semanticTestTarget) {
	t.Helper()
	command, ok := lens["command"].(map[string]any)
	if !ok {
		t.Fatalf("command = %#v, want object", lens["command"])
	}
	arguments, ok := command["arguments"].([]any)
	if !ok || len(arguments) != 3 {
		t.Fatalf("arguments = %#v, want three values", command["arguments"])
	}
	targets, ok := arguments[2].([]any)
	if !ok || len(targets) != len(expected) {
		t.Fatalf("targets = %#v, want %d locations", arguments[2], len(expected))
	}
	for i, wanted := range expected {
		target, ok := targets[i].(map[string]any)
		if !ok || target["uri"] != wanted.URI {
			t.Fatalf("target %d = %#v, want uri %q", i, targets[i], wanted.URI)
		}
		rangeValue, ok := target["range"].(map[string]any)
		if !ok {
			t.Fatalf("target %d range = %#v, want object", i, target["range"])
		}
		assertTestPosition(t, rangeValue["start"], wanted.Line, wanted.Character)
	}
}

func semanticTestStartLine(value any) int {
	rangeValue, ok := value.(map[string]any)
	if !ok {
		return -1
	}
	position, ok := rangeValue["start"].(map[string]any)
	if !ok {
		return -1
	}
	line, ok := position["line"].(float64)
	if !ok {
		return -1
	}
	return int(line)
}
