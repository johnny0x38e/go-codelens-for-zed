#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
server_dir="$repository_root/server"
temporary_root=${TMPDIR:-/tmp}
go_cache_dir=${GOCACHE:-"$temporary_root/go-build-cache"}
benchmark_output_dir=${BENCHMARK_OUTPUT_DIR:-"$temporary_root/go-codelens-benchmarks"}

mkdir -p "$go_cache_dir" "$benchmark_output_dir"
cd "$server_dir"

export TMPDIR="$temporary_root"
export GOCACHE="$go_cache_dir"

go test ./...
go test -race ./...

benchmark_pattern='Benchmark(AnalyzeScopeScale|AnalyzeModuleRepositoryFixture|SnapshotForScopeCacheHit|AnalysisScopeCacheHit|SemanticCodeLensesCacheHit|DocumentFullTextChange256KiB|DocumentIncrementalChange256KiB|TextBufferIncrementalEdit256KiB|TextBufferRandomJump256KiB|EditToCodeLensIncremental64Packages)$'
go test -run '^$' -bench "$benchmark_pattern" -benchmem -count=5 ./internal/analysis ./internal/lsp

profile_benchmark='^BenchmarkAnalyzeScopeScale/Packages_64$'
go test -run '^$' -bench "$profile_benchmark" -benchtime=10x \
	-cpuprofile "$benchmark_output_dir/analyze-scope-64.cpu.pprof" ./internal/analysis
go test -run '^$' -bench "$profile_benchmark" -benchtime=10x \
	-memprofile "$benchmark_output_dir/analyze-scope-64.mem.pprof" ./internal/analysis

echo "Profiles written to $benchmark_output_dir"
