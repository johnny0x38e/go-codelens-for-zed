# Performance

**English** | [简体中文](PERFORMANCE.zh-CN.md)

This file records reproducible performance measurements for the Go CodeLens
language server. Run benchmarks from `server/` with an otherwise idle machine.

## Environment

- Date: 2026-08-06
- Platform: Apple Silicon macOS
- Hardware: Apple M1 Pro
- OS: macOS 26.6, arm64
- Go toolchain: go1.26.5 darwin/arm64

## Coarse pre-benchmark reference

Before dedicated benchmarks existed, 20 repository-fixture analyses completed in
2.495 seconds of package test time (about 124.75 ms per analysis). The complete
`go test` process reached 129,236,992 bytes maximum RSS. This includes test and
tooling overhead and is retained only as historical context.

## Stage 1 benchmark results

Commands are run from `server/` with `TMPDIR=/tmp` and
`GOCACHE=/tmp/go-build-cache`.

### Baseline

`BenchmarkAnalyzeModuleRepositoryFixture` exercises an uncached
`packages.Load` and the complete analyzer against `examples/poc`:

| Sample | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 96,481,566 | 1,714,770 | 14,764 |
| 2 | 95,643,924 | 1,665,461 | 14,720 |
| 3 | 95,984,476 | 1,665,135 | 14,720 |
| 4 | 92,764,333 | 1,663,843 | 14,715 |
| 5 | 91,441,580 | 1,662,859 | 14,708 |

`BenchmarkSnapshotForScopeCacheHit` exercises the cached LSP snapshot lookup:

| Sample | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 24.92 | 0 | 0 |
| 2 | 25.14 | 0 | 0 |
| 3 | 24.72 | 0 | 0 |
| 4 | 24.53 | 0 | 0 |
| 5 | 24.48 | 0 | 0 |

The roughly four-million-fold latency gap between an analysis and a cache hit
makes avoiding false invalidation the highest-leverage first-stage change.

### After clean-overlay and method-candidate indexing

Analyzer samples:

| Sample | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 94,240,962 | 1,715,056 | 14,762 |
| 2 | 105,521,978 | 1,663,979 | 14,719 |
| 3 | 101,817,951 | 1,665,066 | 14,721 |
| 4 | 94,349,406 | 1,664,670 | 14,720 |
| 5 | 92,432,979 | 1,663,667 | 14,716 |

Cache-hit samples:

| Sample | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 24.33 | 0 | 0 |
| 2 | 24.57 | 0 | 0 |
| 3 | 25.74 | 0 | 0 |
| 4 | 24.99 | 0 | 0 |
| 5 | 24.33 | 0 | 0 |

The analyzer results remain inside baseline run-to-run variance. That is
expected for the small fixture: `packages.Load` dominates and the fixture has
too few unrelated methods for candidate indexing to be measurable. The index
changes worst-case method matching from all interface methods times all
concrete methods to exact method-identity buckets.

### Profiles and next-stage decision

The 10-iteration profile run measured 91,571,104 ns/op, 1,727,590 B/op, and
14,776 allocations/op. CPU sampling collected only 70 ms during 1.01 seconds of
wall time because much of `packages.Load` waits on its external Go command.
Within those sparse samples, `go/packages` driver work accounted for 42.86% of
cumulative samples and parser work for another 14.29%.

The largest allocation paths were:

| Path | Cumulative allocation share |
| --- | ---: |
| `go/packages.(*golistState).createDriverResponse` | 38.10% |
| `go/packages.(*loader).loadPackage` | 24.56% |
| Go command output buffering (`bytes.Buffer.ReadFrom`) | 17.43% |
| `gcimporter.UImportData` | 16.41% |
| JSON decoding under the package driver | 16.29% |

Shares overlap because they are cumulative call-path values. The evidence
routes the next stage toward preventing unnecessary package loads: scope-local
cache invalidation, coalesced concurrent analysis, and cancellation of obsolete
work. Micro-optimizing lens formatting is not currently justified.

## Stage 2: scope-local invalidation and request coalescing

The LSP cache now keeps a revision per analysis scope. Effective in-memory
changes invalidate only snapshots whose module roots contain the changed path.
Concurrent cold requests use a single-flight key containing both scope and
revision, so new edits cannot join obsolete work.

After adding the revision checks, the cache-hit fast path was kept under a read
lock and measured as follows:

| Sample | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 21.38 | 0 | 0 |
| 2 | 21.64 | 0 | 0 |
| 3 | 21.52 | 0 | 0 |
| 4 | 21.42 | 0 | 0 |
| 5 | 21.50 | 0 | 0 |

The mean cache-hit cost is 21.49 ns, about 13.2% lower than the 24.76 ns Stage
1 baseline, with zero allocations retained.

`BenchmarkUnrelatedScopeInvalidation` keeps 32 module snapshots and repeatedly
invalidates one module before reading a different module:

| Sample | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 12,867 | 496 | 31 |
| 2 | 12,762 | 496 | 31 |
| 3 | 12,602 | 496 | 31 |
| 4 | 12,767 | 496 | 31 |
| 5 | 12,694 | 496 | 31 |

The mean 12.74 µs routing cost preserves the unrelated cached snapshot instead
of paying roughly 92-106 ms for another package load. A race-enabled stress
test also verifies that 16 concurrent requests for one cold scope/revision call
the analyzer exactly once.

## Stage 3: analysis I/O and request-path allocations

The measurement commands were:

```text
go test ./internal/analysis -run '^$' -bench '^BenchmarkAnalyzeModuleRepositoryFixture$' -benchmem -count=5
go test ./internal/lsp -run '^$' -bench 'Benchmark(SnapshotForScopeCacheHit|UnrelatedScopeInvalidation|SemanticCodeLensesCacheHit)$' -benchmem -count=5
go test ./internal/lsp -run '^$' -bench 'BenchmarkAnalysisScope(DiscoveryWithoutCache|CacheHit)$' -benchmem -count=5
```

Stage 3 makes source-location resolution local to one analysis. Each source
file's bytes and canonical URI are resolved once, UTF-16 conversion scans only
the identifier's current line, and no source content is retained in the
snapshot. `TestLocationResolverReadsSourceOnceAndUsesUTF16Columns` enforces the
single-read invariant while covering a non-BMP character.

The analyzer benchmark before Stage 3 was:

| Sample | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 89,747,594 | 1,714,667 | 14,762 |
| 2 | 89,612,156 | 1,665,234 | 14,719 |
| 3 | 91,196,746 | 1,664,078 | 14,719 |
| 4 | 108,979,426 | 1,663,864 | 14,716 |
| 5 | 116,383,958 | 1,664,147 | 14,717 |

After source reuse and removal of redundant overlay copies:

| Sample | ns/op | B/op | allocs/op |
| ---: | ---: | ---: | ---: |
| 1 | 87,287,580 | 1,681,037 | 14,567 |
| 2 | 87,787,394 | 1,637,195 | 14,526 |
| 3 | 86,553,833 | 1,636,155 | 14,521 |
| 4 | 86,252,452 | 1,635,683 | 14,522 |
| 5 | 88,128,301 | 1,636,355 | 14,524 |

Excluding each run's first warm-up sample, the mean fell by 27,984 B/op
(1.68%) and about 195 allocations/op (1.32%). Wall time is not treated as an
improvement because the external `packages.Load` command dominates and the
samples show substantial scheduler and filesystem variance.

Path containment is now a lexical, separator-aware check over already
canonical absolute paths. This changes the 32-scope unrelated-invalidation
benchmark as follows:

| Sample | Before ns/op | After ns/op |
| ---: | ---: | ---: |
| 1 | 12,499 | 700.0 |
| 2 | 12,491 | 687.7 |
| 3 | 12,403 | 687.0 |
| 4 | 12,281 | 647.9 |
| 5 | 12,469 | 672.9 |

The mean fell from 12,428.6 ns/op to 679.1 ns/op, a 94.5% reduction. Bytes
fell from 496 B/op to zero and allocations from 31 per operation to zero.
Watched source-file changes now use the same targeted invalidation; `go.mod`
and `go.work` changes additionally clear the scope-discovery cache.

The unchanged snapshot lookup fast path measured 21.14, 20.93, 21.06, 20.87,
and 21.88 ns/op, with zero bytes and zero allocations in every sample. Its
21.18 ns/op mean remains consistent with Stage 2.

Scope discovery itself is cached by document directory and workspace root.
`BenchmarkAnalysisScopeDiscoveryWithoutCache` and
`BenchmarkAnalysisScopeCacheHit` measured:

| Sample | Uncached ns/op | Cached ns/op |
| ---: | ---: | ---: |
| 1 | 9,794 | 180.0 |
| 2 | 9,614 | 182.3 |
| 3 | 8,927 | 182.0 |
| 4 | 9,381 | 188.0 |
| 5 | 9,041 | 182.2 |

The means are 9,351.4 ns/op and 182.9 ns/op respectively. The cached path is
about 51.1 times faster, changes 1,680 B/op and 17 allocations/op to zero, and
avoids repeat metadata I/O.

Finally, cached semantic Code Lens requests reuse immutable snapshot views and
canonical document identities:

| Sample | Before ns/op | After ns/op |
| ---: | ---: | ---: |
| 1 | 3,162 | 1,933 |
| 2 | 3,221 | 1,945 |
| 3 | 3,212 | 1,939 |
| 4 | 3,196 | 1,956 |
| 5 | 3,764 | 1,960 |

The mean fell from 3,311.0 ns/op to 1,946.6 ns/op, a 41.2% reduction. Bytes
fell from 3,568 B/op to 2,192 B/op (38.6%) and allocations from 56 to 45
(19.6%). Document-identity cache entries are released on
`textDocument/didClose`, bounding their lifetime.

Invalidation now also drops the stale snapshot's Lens and navigation-target
graph immediately while retaining only its small revision tombstone. That
revision is required to prevent a request from joining obsolete in-flight
single-flight work. `TestSnapshotInvalidationReleasesStaleSemanticData` covers
this memory-lifetime boundary.

A hand-built response-header buffer was also benchmarked and rejected: it
reduced `BenchmarkWriteFrame` from roughly 260 ns/op to 239 ns/op but increased
memory from 112 B/op and two allocations to 176 B/op and three allocations.
The allocation-heavier implementation was reverted.
