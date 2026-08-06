package lsp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
)

var benchmarkPathContained bool

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
	state.analyzeScope = func(_ context.Context, _ analysis.AnalysisScope, _ map[string][]byte) (analysis.Snapshot, error) {
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

func BenchmarkWriteFrame(b *testing.B) {
	message := response{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Result:  []codeLens{},
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
