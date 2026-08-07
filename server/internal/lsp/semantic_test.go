package lsp

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
)

func TestServeReturnsLensesForRepositoryFixtureWithoutDidOpen(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test filename")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", ".."))
	documentURI := semanticTestFileURI(t, filepath.Join(repositoryRoot, "examples", "poc", "main.go"))

	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(t, repositoryRoot)},
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

func TestServeAnalyzesAllGoWorkModules(t *testing.T) {
	workspaceRoot := t.TempDir()
	contractModule := filepath.Join(workspaceRoot, "contract")
	appModule := filepath.Join(workspaceRoot, "app")
	if err := os.MkdirAll(contractModule, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(%q) error = %v", contractModule, err)
	}
	if err := os.MkdirAll(appModule, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(%q) error = %v", appModule, err)
	}
	writeSemanticTestFile(t, filepath.Join(workspaceRoot, "go.work"), []byte("go 1.26\n\nuse (\n\t./contract\n\t./app\n)\n"))
	writeSemanticTestFile(t, filepath.Join(contractModule, "go.mod"), []byte("module example.com/contract\n\ngo 1.26\n"))
	writeSemanticTestFile(t, filepath.Join(appModule, "go.mod"), []byte("module example.com/app\n\ngo 1.26\n"))
	contractSource := []byte(`package contract

type Greeter interface {
	Greet() string
}

func Welcome(g Greeter) string { return g.Greet() }
`)
	appSource := []byte(`package app

import "example.com/contract"

type Person struct{}

func (Person) Greet() string { return "hello" }

func Use() string { return contract.Welcome(Person{}) }
`)
	contractFilename := filepath.Join(contractModule, "contract.go")
	appFilename := filepath.Join(appModule, "app.go")
	writeSemanticTestFile(t, contractFilename, contractSource)
	writeSemanticTestFile(t, appFilename, appSource)

	contractURI := semanticTestFileURI(t, contractFilename)
	appURI := semanticTestFileURI(t, appFilename)
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(t, workspaceRoot)},
	})
	writeSemanticTestCodeLensRequest(t, input, 2, contractURI)
	writeSemanticTestCodeLensRequest(t, input, 3, appURI)
	writeTestFrame(t, input, map[string]any{"jsonrpc": "2.0", "method": "exit"})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	responses := readSemanticTestResponsesByID(t, reader, 2)
	contractLenses := semanticTestResponseLenses(t, responses[2])
	appLenses := semanticTestResponseLenses(t, responses[3])

	interfaceLens := findSemanticTestLens(t, contractLenses, "1 implementation", 2)
	assertSemanticTestTargets(t, interfaceLens, appURI, 4, 5)
	welcomeReferences := findSemanticTestLens(t, contractLenses, "1 reference", 6)
	assertSemanticTestTargets(t, welcomeReferences, appURI, 8, 36)
	personLens := findSemanticTestLens(t, appLenses, "Implements: Greeter", 4)
	assertSemanticTestTargets(t, personLens, contractURI, 2, 5)
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

	rootURI := semanticTestFileURI(t, moduleDir)
	documentURI := semanticTestFileURI(t, filename)
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
		if !ok || semanticTestStartLine(lens["range"]) != 10 || !semanticTestIsImplementationLens(lens) {
			continue
		}
		t.Fatalf("Broken received unexpected implementation CodeLens: %#v", lens)
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

	documentURI := semanticTestFileURI(t, filename)
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(t, moduleDir)},
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

	rootURI := semanticTestFileURI(t, moduleDir)
	contractURI := semanticTestFileURI(t, contractFilename)
	storeURI := semanticTestFileURI(t, storeFilename)
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
	responses := readSemanticTestResponsesByID(t, reader, 2)
	contractLenses := semanticTestResponseLenses(t, responses[2])
	storeLenses := semanticTestResponseLenses(t, responses[3])

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
		if ok && semanticTestStartLine(lens["range"]) == 14 && semanticTestIsImplementationLens(lens) {
			t.Fatalf("WrongStore.Get received unexpected implementation CodeLens: %#v", lens)
		}
	}
}

func TestServeReturnsExactReferenceLenses(t *testing.T) {
	moduleDir := t.TempDir()
	contractSource := []byte(`package sample

type Greeter interface {
Greet() string
}

const Greeting = "hello"
var Default Greeter

func GreetAll(g Greeter) string {
return g.Greet() + Greeting
}

func Identity[T any](value T) T { return value }

type Unused struct{}

func Uncalled() {}
`)
	implementationSource := []byte(`package sample

type Person struct {
Name string
}

func (p Person) Greet() string { return p.Name }

func use() string {
var g Greeter = Person{Name: "Zed"}
Default = g
Identity(g)
return GreetAll(g) + Greeting + Person{}.Greet()
}

func (Person) UnusedMethod() {}
`)
	writeSemanticTestFile(t, filepath.Join(moduleDir, "go.mod"), []byte("module example.com/sample\n\ngo 1.26\n"))
	contractFilename := filepath.Join(moduleDir, "contract.go")
	implementationFilename := filepath.Join(moduleDir, "implementation.go")
	writeSemanticTestFile(t, contractFilename, contractSource)
	writeSemanticTestFile(t, implementationFilename, implementationSource)

	contractURI := semanticTestFileURI(t, contractFilename)
	implementationURI := semanticTestFileURI(t, implementationFilename)
	input := new(bytes.Buffer)
	writeTestFrame(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(t, moduleDir)},
	})
	writeSemanticTestDidOpen(t, input, contractURI, contractSource)
	writeSemanticTestDidOpen(t, input, implementationURI, implementationSource)
	writeSemanticTestCodeLensRequest(t, input, 2, contractURI)
	writeSemanticTestCodeLensRequest(t, input, 3, implementationURI)
	writeTestFrame(t, input, map[string]any{"jsonrpc": "2.0", "method": "exit"})

	output := new(bytes.Buffer)
	if err := Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	responses := readSemanticTestResponsesByID(t, reader, 2)
	contractLenses := semanticTestResponseLenses(t, responses[2])
	implementationLenses := semanticTestResponseLenses(t, responses[3])

	greeterReferences := findSemanticTestLens(t, contractLenses, "3 references", 2)
	assertSemanticTestTargetStarts(t, greeterReferences,
		semanticTestTarget{URI: contractURI, Line: 7, Character: 12},
		semanticTestTarget{URI: contractURI, Line: 9, Character: 16},
		semanticTestTarget{URI: implementationURI, Line: 9, Character: 6},
	)
	assertSemanticTestTitlesOnLine(t, contractLenses, 2, "3 references", "1 implementation")

	methodReferences := findSemanticTestLens(t, contractLenses, "2 references", 3)
	assertSemanticTestTargetStarts(t, methodReferences,
		semanticTestTarget{URI: contractURI, Line: 10, Character: 9},
		semanticTestTarget{URI: implementationURI, Line: 12, Character: 41},
	)
	greetingReferences := findSemanticTestLens(t, contractLenses, "2 references", 6)
	assertSemanticTestTargetStarts(t, greetingReferences,
		semanticTestTarget{URI: contractURI, Line: 10, Character: 19},
		semanticTestTarget{URI: implementationURI, Line: 12, Character: 21},
	)
	defaultReferences := findSemanticTestLens(t, contractLenses, "1 reference", 7)
	assertSemanticTestTargetStarts(t, defaultReferences,
		semanticTestTarget{URI: implementationURI, Line: 10, Character: 0},
	)
	greetAllReferences := findSemanticTestLens(t, contractLenses, "1 reference", 9)
	assertSemanticTestTargetStarts(t, greetAllReferences,
		semanticTestTarget{URI: implementationURI, Line: 12, Character: 7},
	)
	identityReferences := findSemanticTestLens(t, contractLenses, "1 reference", 13)
	assertSemanticTestTargetStarts(t, identityReferences,
		semanticTestTarget{URI: implementationURI, Line: 11, Character: 0},
	)
	typeParameterReferences := findSemanticTestLens(t, contractLenses, "2 references", 13)
	assertSemanticTestTargetStarts(t, typeParameterReferences,
		semanticTestTarget{URI: contractURI, Line: 13, Character: 27},
		semanticTestTarget{URI: contractURI, Line: 13, Character: 30},
	)
	_ = findSemanticTestLens(t, contractLenses, "0 references", 17)
	assertSemanticTestNoReferenceLens(t, contractLenses, 15)

	personReferences := findSemanticTestLens(t, implementationLenses, "4 references", 2)
	assertSemanticTestTargetStarts(t, personReferences,
		semanticTestTarget{URI: implementationURI, Line: 6, Character: 8},
		semanticTestTarget{URI: implementationURI, Line: 9, Character: 16},
		semanticTestTarget{URI: implementationURI, Line: 12, Character: 32},
		semanticTestTarget{URI: implementationURI, Line: 15, Character: 6},
	)
	fieldReferences := findSemanticTestLens(t, implementationLenses, "2 references", 3)
	assertSemanticTestTargetStarts(t, fieldReferences,
		semanticTestTarget{URI: implementationURI, Line: 6, Character: 42},
		semanticTestTarget{URI: implementationURI, Line: 9, Character: 23},
	)
	concreteMethodReferences := findSemanticTestLens(t, implementationLenses, "2 references", 6)
	assertSemanticTestTargetStarts(t, concreteMethodReferences,
		semanticTestTarget{URI: contractURI, Line: 10, Character: 9},
		semanticTestTarget{URI: implementationURI, Line: 12, Character: 41},
	)
	_ = findSemanticTestLens(t, implementationLenses, "0 references", 8)
	_ = findSemanticTestLens(t, implementationLenses, "0 references", 15)
}

func TestServeDoesNotPublishObsoleteLensesAfterUnsavedSignatureChange(t *testing.T) {
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

	documentURI := semanticTestFileURI(t, filename)
	ctx := t.Context()
	state := newServerStateWithContext(ctx)
	originalAnalyzeScope := state.analyzeScope
	analysisStarted := make(chan struct{})
	var analysisCalls atomic.Int64
	state.analyzeScope = func(
		analysisCtx context.Context,
		scope analysis.AnalysisScope,
		changes analysis.ChangeSet,
		overlay map[string][]byte,
	) (analysis.Snapshot, error) {
		if analysisCalls.Add(1) == 1 {
			close(analysisStarted)
			<-analysisCtx.Done()
			return analysis.Snapshot{}, analysisCtx.Err()
		}
		return originalAnalyzeScope(analysisCtx, scope, changes, overlay)
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
		"params":  map[string]any{"rootUri": semanticTestFileURI(t, moduleDir)},
	})
	writeSemanticTestDidOpen(t, inputWriter, documentURI, savedSource)
	writeSemanticTestCodeLensRequest(t, inputWriter, 2, documentURI)
	<-analysisStarted
	writeTestFrame(t, inputWriter, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didChange",
		"params": map[string]any{
			"textDocument": map[string]any{"uri": documentURI, "version": 2},
			"contentChanges": []any{
				map[string]any{"text": string(changedSource)},
			},
		},
	})
	writeSemanticTestCodeLensRequest(t, inputWriter, 3, documentURI)
	writeTestFrame(t, inputWriter, map[string]any{"jsonrpc": "2.0", "method": "exit"})
	if err := inputWriter.Close(); err != nil {
		t.Fatalf("close LSP input: %v", err)
	}
	if err := <-serveResult; err != nil {
		t.Fatalf("serveWithState() error = %v", err)
	}

	reader := bufio.NewReader(output)
	_ = readTestFrame(t, reader)
	responses := readSemanticTestResponsesByID(t, reader, 2)
	for _, id := range []int{2, 3} {
		lenses := semanticTestResponseLenses(t, responses[id])
		zeroTypeImplementations := findSemanticTestLens(t, lenses, "0 implementations", 2)
		assertSemanticTestTargetStarts(t, zeroTypeImplementations)
		zeroMethodImplementations := findSemanticTestLens(t, lenses, "0 implementations", 3)
		assertSemanticTestTargetStarts(t, zeroMethodImplementations)
		for _, value := range lenses {
			lens, ok := value.(map[string]any)
			if !ok {
				continue
			}
			line := semanticTestStartLine(lens["range"])
			if (line == 6 || line == 8) && semanticTestIsImplementationLens(lens) {
				t.Fatalf("response %d published an obsolete implementation CodeLens: %#v", id, lens)
			}
		}
	}
	if got := analysisCalls.Load(); got != 2 {
		t.Fatalf("analysis calls = %d, want one canceled and one current analysis", got)
	}
}

func TestServeIncrementalChangesMatchFinalDiskAnalysis(t *testing.T) {
	moduleDir := t.TempDir()
	writeSemanticTestFile(t, filepath.Join(moduleDir, "go.mod"), []byte("module example.com/incremental\n\ngo 1.26\n"))
	initialSource := []byte(`package sample

// 😀 verifies that the open buffer retains non-BMP UTF-8 text.
type Runner interface {
	Run(value string)
}

type Worker struct{}

func (Worker) Run(value int) {}
`)
	finalSource := []byte(`package sample

// 😀 verifies that the open buffer retains non-BMP UTF-8 text.
type Runner interface {
	Run(value string)
}

type Worker struct{}

func (*Worker) Run(value string) {}
`)
	filename := filepath.Join(moduleDir, "sample.go")
	writeSemanticTestFile(t, filename, initialSource)
	documentURI := semanticTestFileURI(t, filename)

	incrementalInput := new(bytes.Buffer)
	writeTestFrame(t, incrementalInput, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(t, moduleDir)},
	})
	writeSemanticTestDidOpen(t, incrementalInput, documentURI, initialSource)
	writeTestFrame(t, incrementalInput, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didChange",
		"params": map[string]any{
			"textDocument": map[string]any{"uri": documentURI, "version": 2},
			"contentChanges": []textDocumentContentChangeEvent{
				textBufferRangeChange(9, 6, 9, 6, "*"),
				textBufferRangeChange(9, 25, 9, 28, "string"),
			},
		},
	})
	writeSemanticTestCodeLensRequest(t, incrementalInput, 2, documentURI)
	writeTestFrame(t, incrementalInput, map[string]any{"jsonrpc": "2.0", "method": "exit"})

	incrementalOutput := new(bytes.Buffer)
	if err := Serve(context.Background(), incrementalInput, incrementalOutput); err != nil {
		t.Fatalf("Serve(incremental) error = %v", err)
	}
	incrementalReader := bufio.NewReader(incrementalOutput)
	_ = readTestFrame(t, incrementalReader)
	incrementalLenses := semanticTestResponseLenses(t, readTestFrame(t, incrementalReader))
	_ = findSemanticTestLens(t, incrementalLenses, "Implements: Runner", 7)
	_ = findSemanticTestLens(t, incrementalLenses, "Implements: Runner.Run", 9)

	writeSemanticTestFile(t, filename, finalSource)
	diskInput := new(bytes.Buffer)
	writeTestFrame(t, diskInput, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"rootUri": semanticTestFileURI(t, moduleDir)},
	})
	writeSemanticTestCodeLensRequest(t, diskInput, 2, documentURI)
	writeTestFrame(t, diskInput, map[string]any{"jsonrpc": "2.0", "method": "exit"})

	diskOutput := new(bytes.Buffer)
	if err := Serve(context.Background(), diskInput, diskOutput); err != nil {
		t.Fatalf("Serve(disk) error = %v", err)
	}
	diskReader := bufio.NewReader(diskOutput)
	_ = readTestFrame(t, diskReader)
	diskLenses := semanticTestResponseLenses(t, readTestFrame(t, diskReader))
	if !reflect.DeepEqual(incrementalLenses, diskLenses) {
		t.Fatalf("incremental lenses = %#v, want final disk lenses %#v", incrementalLenses, diskLenses)
	}
}

func writeSemanticTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}

func semanticTestFileURI(tb testing.TB, path string) string {
	tb.Helper()
	uri, err := analysis.PathToURI(path)
	if err != nil {
		tb.Fatalf("analysis.PathToURI(%q) error = %v", path, err)
	}
	return uri
}

func writeSemanticTestDidOpen(t *testing.T, input io.Writer, uri string, source []byte) {
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

func writeSemanticTestCodeLensRequest(t *testing.T, input io.Writer, id int, uri string) {
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

func readSemanticTestResponsesByID(t *testing.T, reader io.Reader, count int) map[int]map[string]any {
	t.Helper()
	responses := make(map[int]map[string]any, count)
	for range count {
		response := readTestFrame(t, reader)
		id, ok := response["id"].(float64)
		if !ok {
			t.Fatalf("response id = %#v, want number", response["id"])
		}
		responses[int(id)] = response
	}
	return responses
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

func assertSemanticTestTitlesOnLine(t *testing.T, lenses []any, line int, expected ...string) {
	t.Helper()
	var titles []string
	for _, value := range lenses {
		lens, ok := value.(map[string]any)
		if !ok || semanticTestStartLine(lens["range"]) != line {
			continue
		}
		command, ok := lens["command"].(map[string]any)
		if !ok {
			continue
		}
		if title, ok := command["title"].(string); ok {
			titles = append(titles, title)
		}
	}
	if len(titles) != len(expected) {
		t.Fatalf("CodeLens titles on line %d = %#v, want %#v", line, titles, expected)
	}
	for i := range expected {
		if titles[i] != expected[i] {
			t.Fatalf("CodeLens titles on line %d = %#v, want %#v", line, titles, expected)
		}
	}
}

func assertSemanticTestNoReferenceLens(t *testing.T, lenses []any, line int) {
	t.Helper()
	for _, value := range lenses {
		lens, ok := value.(map[string]any)
		if !ok || semanticTestStartLine(lens["range"]) != line {
			continue
		}
		command, ok := lens["command"].(map[string]any)
		if !ok {
			continue
		}
		title, ok := command["title"].(string)
		if !ok {
			continue
		}
		if strings.HasSuffix(title, " reference") || strings.HasSuffix(title, " references") {
			t.Fatalf("unexpected reference CodeLens on line %d: %#v", line, lens)
		}
	}
}

func semanticTestIsImplementationLens(lens map[string]any) bool {
	command, ok := lens["command"].(map[string]any)
	if !ok {
		return false
	}
	title, ok := command["title"].(string)
	if !ok {
		return false
	}
	return strings.Contains(title, " impl") || strings.HasPrefix(title, "Implements:")
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
