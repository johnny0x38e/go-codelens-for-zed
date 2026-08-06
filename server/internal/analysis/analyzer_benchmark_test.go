package analysis

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
)

func BenchmarkAnalyzeModuleRepositoryFixture(b *testing.B) {
	_, benchmarkFile, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("runtime.Caller() did not return the benchmark filename")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(benchmarkFile), "..", "..", ".."))
	moduleDir := filepath.Join(repositoryRoot, "examples", "poc")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := AnalyzeModule(context.Background(), moduleDir, nil); err != nil {
			b.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
		}
	}
}
