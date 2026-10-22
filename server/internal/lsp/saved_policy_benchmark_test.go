package lsp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/testutil"
)

// BenchmarkEditorCodeLensRequests64Packages includes actual LSP notifications,
// retained coordinate mapping, and result construction, not just buffer edits.
func BenchmarkEditorCodeLensRequests64Packages(b *testing.B) {
	for _, saved := range []bool{false, true} {
		for _, broken := range []bool{false, true} {
			b.Run(fmt.Sprintf("saved=%t/broken=%t", saved, broken), func(b *testing.B) {
				fixture := testutil.WriteModule(b, testutil.ModuleSpec{
					ModulePath: "example.com/editor-bench", PackageCount: 64,
					FilesPerPackage: 2, DeclarationsPerFile: 8, IncludeTests: true,
				})
				source, err := os.ReadFile(fixture.EntryFile)
				if err != nil {
					b.Fatal(err)
				}
				uri := semanticTestFileURI(b, fixture.EntryFile)
				params := mustMarshalBenchmarkJSON(
					b,
					textDocumentParams{TextDocument: textDocumentIdentifier{URI: uri}},
				)
				state := newServerState()
				open := mustMarshalBenchmarkJSON(b, didOpenTextDocumentParams{TextDocument: openedTextDocument{
					URI: uri, Version: 1, Text: string(source),
				}})
				if openErr := state.handleNotification("textDocument/didOpen", open); openErr != nil {
					b.Fatal(openErr)
				}
				if _, lensErr := state.semanticCodeLenses(b.Context(), params); lensErr != nil {
					b.Fatal(lensErr)
				}
				var calls atomic.Int64
				original := state.analyzeScope
				state.analyzeScope = func(ctx context.Context, scope analysis.AnalysisScope, changes analysis.ChangeSet, overlays map[string][]byte) (analysis.Snapshot, error) {
					calls.Add(1)
					return original(ctx, scope, changes, overlays)
				}
				variants := [2]string{}
				for index := range variants {
					variants[index] = string(source) + fmt.Sprintf("\n// editor edit %d\n", index)
					if broken {
						variants[index] = strings.Replace(
							string(source),
							"return 0\n",
							fmt.Sprintf("return (%d +\n", index),
							1,
						)
					}
				}
				b.ReportAllocs()
				for iteration := 0; b.Loop(); iteration++ {
					content := variants[iteration%len(variants)]
					change := mustMarshalBenchmarkJSON(b, didChangeTextDocumentParams{
						TextDocument:   versionedTextDocumentIdentifier{URI: uri, Version: int64(iteration + 2)},
						ContentChanges: []textDocumentContentChangeEvent{{Text: content}},
					})
					if changeErr := state.handleNotification("textDocument/didChange", change); changeErr != nil {
						b.Fatal(changeErr)
					}
					if saved {
						writeBenchmarkFile(b, fixture.EntryFile, []byte(content))
						if saveErr := state.handleNotification("textDocument/didSave", params); saveErr != nil {
							b.Fatal(saveErr)
						}
					}
					benchmarkCodeLenses, err = state.semanticCodeLenses(b.Context(), params)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(calls.Load())/float64(b.N), "analyses/op")
				b.ReportMetric(float64(len(benchmarkCodeLenses)), "lenses")
			})
		}
	}
}
