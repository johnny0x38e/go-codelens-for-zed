package analysis

import (
	"context"
	"fmt"
	"testing"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/testutil"
)

var (
	benchmarkScopeIndex     *scopeIndex
	benchmarkEngineSnapshot Snapshot
)

func BenchmarkEngineAnalyzeCacheHit(b *testing.B) {
	scope := ModuleAnalysisScope(b.TempDir())
	snapshot := newSnapshot(map[string][]Lens{"benchmark": {{Title: "benchmark"}}})
	engine := NewEngine()
	engine.analyze = func(_ context.Context, _ AnalysisScope, _ map[string][]byte) (scopeAnalysis, error) {
		return scopeAnalysis{snapshot: snapshot}, nil
	}
	if _, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil); err != nil {
		b.Fatalf("prime Engine.Analyze(): %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		cached, err := engine.Analyze(context.Background(), scope, NewChangeSet(), nil)
		if err != nil {
			b.Fatalf("cached Engine.Analyze(): %v", err)
		}
		benchmarkEngineSnapshot = cached
	}
}

func BenchmarkAnalyzeScopeScale(b *testing.B) {
	for _, packageCount := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("Packages_%d", packageCount), func(b *testing.B) {
			fixture := testutil.WriteModule(b, testutil.ModuleSpec{
				ModulePath:          "example.com/scale",
				PackageCount:        packageCount,
				FilesPerPackage:     2,
				DeclarationsPerFile: 8,
				IncludeTests:        true,
			})

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				snapshot, err := AnalyzeModule(context.Background(), fixture.Root, nil)
				if err != nil {
					b.Fatalf("AnalyzeModule(%q): %v", fixture.Root, err)
				}
				benchmarkAnalysisSnapshot = snapshot
			}
		})
	}
}

func BenchmarkBuildScopeIndexScale(b *testing.B) {
	for _, packageCount := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("Packages_%d", packageCount), func(b *testing.B) {
			fixture := testutil.WriteModule(b, testutil.ModuleSpec{
				ModulePath:          "example.com/index-scale",
				PackageCount:        packageCount,
				FilesPerPackage:     2,
				DeclarationsPerFile: 8,
				IncludeTests:        true,
			})
			scope := ModuleAnalysisScope(fixture.Root)
			loaded, err := loadScopePackages(context.Background(), scope, nil)
			if err != nil {
				b.Fatalf("loadScopePackages(%q): %v", fixture.Root, err)
			}
			packagesToInspect := packagesForAnalysis(loaded.packages)
			facts, err := extractPackageFacts(
				packagesToInspect,
				newLocationResolver(loaded.sourceByPath),
			)
			if err != nil {
				b.Fatalf("extractPackageFacts(%q): %v", fixture.Root, err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				index, indexErr := buildScopeIndex(
					scope,
					packagesToInspect,
					facts,
					loaded.sourceByPath,
				)
				if indexErr != nil {
					b.Fatalf("buildScopeIndex(%q): %v", fixture.Root, indexErr)
				}
				benchmarkScopeIndex = index
			}
		})
	}
}
