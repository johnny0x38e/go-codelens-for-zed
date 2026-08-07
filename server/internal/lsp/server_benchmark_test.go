package lsp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
	"github.com/johnny/go-codelens-for-zed/server/internal/testutil"
)

var (
	benchmarkCodeLenses      []codeLens
	benchmarkDocumentChanged bool
	benchmarkDocumentPath    string
	benchmarkPathContained   bool
	benchmarkTextChanged     bool
)

func BenchmarkSnapshotForScopeCacheHit(b *testing.B) {
	moduleDir := b.TempDir()
	writeBenchmarkFile(b, filepath.Join(moduleDir, "go.mod"), []byte("module example.com/bench\n\ngo 1.26\n"))
	writeBenchmarkFile(b, filepath.Join(moduleDir, "bench.go"), []byte("package bench\n\ntype Runner interface { Run() }\ntype Worker struct{}\nfunc (Worker) Run() {}\n"))

	state := newServerState()
	scope := analysis.ModuleAnalysisScope(moduleDir)
	if _, err := state.snapshotForScope(context.Background(), scope); err != nil {
		b.Fatalf("prime snapshot: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := state.snapshotForScope(context.Background(), scope); err != nil {
			b.Fatalf("cached snapshot: %v", err)
		}
	}
}

func BenchmarkUnrelatedScopeInvalidation(b *testing.B) {
	workspaceRoot := b.TempDir()
	state := newServerState()
	state.analyzeScope = func(_ context.Context, _ analysis.AnalysisScope, _ analysis.ChangeSet, _ map[string][]byte) (analysis.Snapshot, error) {
		return analysis.Snapshot{}, nil
	}

	const scopeCount = 32
	scopes := make([]analysis.AnalysisScope, 0, scopeCount)
	for index := range scopeCount {
		scope := analysis.ModuleAnalysisScope(filepath.Join(workspaceRoot, strconv.Itoa(index)))
		scopes = append(scopes, scope)
		if _, err := state.snapshotForScope(context.Background(), scope); err != nil {
			b.Fatalf("prime scope %d: %v", index, err)
		}
	}
	changedPath := filepath.Join(scopes[0].Root, "edited.go")
	unrelatedScope := scopes[len(scopes)-1]

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		state.invalidateSnapshotsForPath(changedPath)
		if _, err := state.snapshotForScope(context.Background(), unrelatedScope); err != nil {
			b.Fatalf("cached unrelated scope: %v", err)
		}
	}
}

func BenchmarkPathWithinRoot(b *testing.B) {
	path := "/workspace/module/internal/package/file.go"
	root := "/workspace/module"

	b.ReportAllocs()
	for b.Loop() {
		benchmarkPathContained = pathWithinRoot(path, root)
	}
}

func BenchmarkAnalysisScopeCacheHit(b *testing.B) {
	moduleRoot := b.TempDir()
	filename := filepath.Join(moduleRoot, "internal", "package", "file.go")
	state := newServerState()
	state.findScope = func(_, _ string) (analysis.AnalysisScope, error) {
		return analysis.ModuleAnalysisScope(moduleRoot), nil
	}
	if _, err := state.analysisScopeFor(filename); err != nil {
		b.Fatalf("prime analysis scope: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := state.analysisScopeFor(filename); err != nil {
			b.Fatalf("cached analysis scope: %v", err)
		}
	}
}

func BenchmarkAnalysisScopeDiscoveryWithoutCache(b *testing.B) {
	moduleRoot := b.TempDir()
	writeBenchmarkFile(b, filepath.Join(moduleRoot, "go.mod"), []byte("module example.com/bench\n\ngo 1.26\n"))
	filename := filepath.Join(moduleRoot, "internal", "package", "file.go")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := analysis.FindAnalysisScope(filename, moduleRoot); err != nil {
			b.Fatalf("discover analysis scope: %v", err)
		}
	}
}

func BenchmarkSemanticCodeLensesCacheHit(b *testing.B) {
	moduleRoot := b.TempDir()
	writeBenchmarkFile(b, filepath.Join(moduleRoot, "go.mod"), []byte("module example.com/bench\n\ngo 1.26\n"))
	filename := filepath.Join(moduleRoot, "bench.go")
	writeBenchmarkFile(b, filename, []byte("package bench\n\ntype Runner interface { Run() }\ntype Worker struct{}\nfunc (Worker) Run() {}\nfunc Use(r Runner) { r.Run() }\n"))
	params, err := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": semanticTestFileURI(filename)},
	})
	if err != nil {
		b.Fatal(err)
	}
	state := newServerState()
	state.workspaceRoots = []string{moduleRoot}
	if _, err := state.semanticCodeLenses(context.Background(), params); err != nil {
		b.Fatalf("prime Code Lenses: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := state.semanticCodeLenses(context.Background(), params); err != nil {
			b.Fatalf("cached Code Lenses: %v", err)
		}
	}
}

func BenchmarkDocumentFullTextChange256KiB(b *testing.B) {
	path := filepath.Join(b.TempDir(), "large.go")
	uri := semanticTestFileURI(path)
	textA, _, _, _ := benchmarkTextDocument256KiB()
	textB := textA[:len(textA)-1] + "y"
	store := newDocumentStore()
	if _, _, err := store.Open(uri, textA, 1); err != nil {
		b.Fatalf("open benchmark document: %v", err)
	}
	changes := [][]textDocumentContentChangeEvent{
		{{Text: textB}},
		{{Text: textA}},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; b.Loop(); iteration++ {
		var err error
		benchmarkDocumentPath, benchmarkDocumentChanged, err = store.Change(
			uri,
			int64(iteration+2),
			changes[iteration%len(changes)],
		)
		if err != nil {
			b.Fatalf("change benchmark document: %v", err)
		}
	}
}

func BenchmarkDocumentIncrementalChange256KiB(b *testing.B) {
	path := filepath.Join(b.TempDir(), "large.go")
	uri := semanticTestFileURI(path)
	content, targetLine, _, _ := benchmarkTextDocument256KiB()
	store := newDocumentStore()
	if _, _, err := store.Open(uri, content, 1); err != nil {
		b.Fatalf("open benchmark document: %v", err)
	}
	const character = 12
	changes := [][]textDocumentContentChangeEvent{
		{
			textBufferRangeChange(targetLine, character, targetLine, character+1, ""),
			textBufferRangeChange(targetLine, character, targetLine, character, "2"),
		},
		{
			textBufferRangeChange(targetLine, character, targetLine, character+1, ""),
			textBufferRangeChange(targetLine, character, targetLine, character, "1"),
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; b.Loop(); iteration++ {
		var err error
		benchmarkDocumentPath, benchmarkDocumentChanged, err = store.Change(
			uri,
			int64(iteration+2),
			changes[iteration%len(changes)],
		)
		if err != nil {
			b.Fatalf("change benchmark document: %v", err)
		}
	}
}

func BenchmarkTextBufferIncrementalEdit256KiB(b *testing.B) {
	content, targetLine, _, _ := benchmarkTextDocument256KiB()
	buffer := newTextBuffer(content)
	initialBacking := &buffer.data[0]
	const character = 12
	changes := [][]textDocumentContentChangeEvent{
		{
			textBufferRangeChange(targetLine, character, targetLine, character+1, ""),
			textBufferRangeChange(targetLine, character, targetLine, character, "2"),
		},
		{
			textBufferRangeChange(targetLine, character, targetLine, character+1, ""),
			textBufferRangeChange(targetLine, character, targetLine, character, "1"),
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; b.Loop(); iteration++ {
		var err error
		benchmarkTextChanged, err = buffer.apply(changes[iteration%len(changes)])
		if err != nil {
			b.Fatalf("incremental edit: %v", err)
		}
	}
	b.StopTimer()
	if &buffer.data[0] != initialBacking {
		b.Fatal("steady-state incremental edits replaced the backing allocation")
	}
}

func BenchmarkTextBufferRandomJump256KiB(b *testing.B) {
	content, _, firstLine, secondLine := benchmarkTextDocument256KiB()
	buffer := newTextBuffer(content)
	const character = 12
	changeBatches := [][]textDocumentContentChangeEvent{
		{textBufferRangeChange(firstLine, character, firstLine, character+1, "2")},
		{textBufferRangeChange(secondLine, character, secondLine, character+1, "2")},
		{textBufferRangeChange(firstLine, character, firstLine, character+1, "1")},
		{textBufferRangeChange(secondLine, character, secondLine, character+1, "1")},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; b.Loop(); iteration++ {
		var err error
		benchmarkTextChanged, err = buffer.apply(changeBatches[iteration%len(changeBatches)])
		if err != nil {
			b.Fatalf("random-jump edit: %v", err)
		}
	}
}

func BenchmarkEditToCodeLensIncremental64Packages(b *testing.B) {
	fixture := testutil.WriteModule(b, testutil.ModuleSpec{
		ModulePath:          "example.com/edit-to-codelens",
		PackageCount:        64,
		FilesPerPackage:     2,
		DeclarationsPerFile: 8,
		IncludeTests:        true,
	})
	content, err := os.ReadFile(fixture.EntryFile)
	if err != nil {
		b.Fatalf("read entry file: %v", err)
	}
	uri := semanticTestFileURI(fixture.EntryFile)
	codeLensParams := mustMarshalBenchmarkJSON(b, textDocumentParams{
		TextDocument: textDocumentIdentifier{URI: uri},
	})
	changes := [][]textDocumentContentChangeEvent{
		{{Text: string(content) + "\n// benchmark edit A\n"}},
		{{Text: string(content) + "\n// benchmark edit B\n"}},
	}
	state := newServerState()
	state.workspaceRoots = []string{fixture.Root}
	if _, _, err := state.documents.Open(uri, string(content), 1); err != nil {
		b.Fatalf("open benchmark document: %v", err)
	}
	if _, err := state.semanticCodeLenses(context.Background(), codeLensParams); err != nil {
		b.Fatalf("prime Code Lenses: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; b.Loop(); iteration++ {
		path, changed, changeErr := state.documents.Change(
			uri,
			int64(iteration+2),
			changes[iteration%len(changes)],
		)
		if changeErr != nil {
			b.Fatalf("change benchmark document: %v", changeErr)
		}
		state.invalidateDocumentChange(path, changed)
		benchmarkCodeLenses, err = state.semanticCodeLenses(context.Background(), codeLensParams)
		if err != nil {
			b.Fatalf("Code Lenses after edit: %v", err)
		}
	}
}

func BenchmarkWriteFrame(b *testing.B) {
	message := response{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Result: []codeLens{{
			Range: analysis.Range{
				Start: analysis.Position{Line: 4, Character: 5},
				End:   analysis.Position{Line: 4, Character: 8},
			},
			Command: command{
				Title:   "1 reference",
				Command: "editor.action.showReferences",
				Arguments: showReferencesArguments{
					"file:///workspace/bench.go",
					analysis.Position{Line: 4, Character: 5},
					[]analysis.Location{{
						URI: "file:///workspace/use.go",
						Range: analysis.Range{
							Start: analysis.Position{Line: 8, Character: 2},
							End:   analysis.Position{Line: 8, Character: 5},
						},
					}},
				},
			},
		}},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := writeFrame(io.Discard, message); err != nil {
			b.Fatalf("write frame: %v", err)
		}
	}
}

func writeBenchmarkFile(b *testing.B, path string, content []byte) {
	b.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		b.Fatalf("os.WriteFile(%q): %v", path, err)
	}
}

func mustMarshalBenchmarkJSON(b *testing.B, value any) json.RawMessage {
	b.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		b.Fatalf("json.Marshal(%T): %v", value, err)
	}
	return encoded
}

func benchmarkTextDocument256KiB() (content string, middleLine, firstLine, secondLine int) {
	const (
		documentSize = 256 << 10
		prefix       = "package large\n\n"
		line         = "var Value = 1234567890\n"
	)
	lineCount := (documentSize - len(prefix)) / len(line)
	remainder := documentSize - len(prefix) - lineCount*len(line)
	content = prefix + strings.Repeat(line, lineCount) + strings.Repeat("x", remainder)
	middleLine = 2 + lineCount/2
	firstLine = 2 + lineCount/4
	secondLine = 2 + 3*lineCount/4
	return content, middleLine, firstLine, secondLine
}
