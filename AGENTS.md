# Repository Guidelines

These instructions apply to the entire repository.

## Project purpose

Go CodeLens for Zed is an auxiliary language server extension. It adds semantic Go reference and interface-implementation CodeLens entries while `gopls` remains Zed's primary Go language server. Preserve that separation: do not proxy `gopls` or broaden the sidecar into a general-purpose Go language server.

Correctness and performance are both release requirements. Optimizations must preserve existing lenses, titles, reference counts, navigation targets, `_test.go` behavior, `go.work` behavior, cancellation semantics, and unsaved-document behavior.

## Repository layout

- `src/lib.rs`: thin Rust/Wasm Zed adapter. It validates the platform, honors a worktree `PATH` override, and otherwise downloads the exact versioned native server after SHA-256 verification.
- `server/cmd/go-codelens-ls`: native server entry point. Keep command wiring small.
- `server/internal/lsp`: `server.go` owns the message loop and dispatch; `transport.go` owns framing and request cancellation; `cache.go` caches paths and scopes; `snapshots.go` owns snapshot caching, coalescing, and invalidation; `watch.go` handles file watching and refresh; `lenses.go` serves semantic lenses. Document synchronization and protocol types remain separate.
- `server/internal/analysis`: `analyzer.go` coordinates full analysis; `scope.go` discovers modules/workspaces; `load.go` loads packages; `fingerprint.go` computes syntax fingerprints; `recheck.go` plans incremental work; `typecheck.go` rechecks retained packages; `rematch.go` rebuilds method-reference matches. Fact extraction, indexes, relationships, snapshot construction, lens formatting, and source locations stay in their own files.
- `server/internal/testutil`: deterministic Go fixtures shared by tests and benchmarks.
- `examples/poc`: small manual acceptance fixture.
- `justfile`: local tasks grouped as `build`, `check`, `perf`, and `release`. Run `just` to list tasks, or `just --list --group build` to filter a group. Keep build, install, benchmark, and version logic in `scripts`, which CI can also call directly.
- `scripts`: standalone development build, install, benchmark, and version helpers.
- `README.md` and `README.zh-CN.md`: public English and Chinese project documentation; keep them semantically synchronized.

Keep the package boundary `cmd/go-codelens-ls -> internal/lsp -> internal/analysis`. The `internal` packages are implementation details, not a public Go API. Do not move packages merely to shorten import paths or mix package-layout churn into performance work.

## Analysis correctness invariants

- Use `go/types` as the final authority. Candidate indexes may narrow work, but names alone must never establish an implementation or reference relationship.
- Complete interface relationships must continue to use `types.Implements`, including pointer and promoted method sets. Method matches must retain package-aware identity and exact signature checks.
- Preserve one coherent `*types.Package` universe across retained packages. If an incremental result cannot be proven safe, return the typed fallback and perform one complete analysis.
- Treat published package facts, relationship indexes, and snapshots as immutable. Incremental updates may share unchanged maps, slices, and backing arrays only under that contract.
- Keep production, internal-test, and external-test package variants distinct internally, then deduplicate user-visible results by exact source location where required.
- LSP positions are UTF-16 code-unit positions. Do not replace them with byte or rune offsets.
- A canceled, invalidated, or obsolete analysis must never publish stale state.

## Performance and I/O rules

- Measure before and after non-trivial performance changes with stable fixtures. Record latency, bytes, allocations, cancellation behavior, and relevant correctness output.
- Prefer algorithmic reductions, bounded state, immutable sharing, and exact invalidation over pooling by default. Add a pool only with benchmark and retained-memory evidence.
- Keep cache sizes bounded. Do not add hit promotion or background maintenance that turns read-only cache hits into write-lock operations without evidence.
- Avoid directory-wide scans when retained package ownership or exact compiled-file paths are available.
- Dirty editor buffers are the source of truth. Copy them only when producing immutable analysis overlays.
- The extension must not write logs, semantic indexes, or persistent caches during normal operation. Standard Go build-cache activity from `go/packages` is acceptable.
- Keep native builds cgo-free unless a reviewed requirement proves otherwise.
- Cross-compilation is not native runtime validation. Do not claim a platform is release-verified until URI, path, process, and language-server behavior have been exercised on that platform.

## Go style

- Follow current Go community idioms and the repository's strict `.golangci.yaml` configuration.
- Every package needs an idiomatic package comment. Exported identifiers require useful documentation.
- Comment non-obvious invariants, correctness boundaries, ownership, concurrency, and performance tradeoffs. Explain why an algorithm is safe; do not narrate obvious syntax.
- Prefer named protocol and support types over large anonymous or function-local struct definitions.
- Keep receiver names consistent for each type.
- Prefer the standard library where it is clear and efficient. Add dependencies only with a concrete maintenance or performance justification.
- Preserve context cancellation on expensive loops and before publishing results.
- Use `gofmt`; do not hand-align Go code.

## Rust style

- Keep the adapter intentionally small and use idiomatic Rust 2024.
- Do not move semantic analysis into the Wasm adapter without measured evidence and a platform/distribution design review.
- Return actionable platform and binary-discovery errors without embedding machine-specific paths.
- Use `cargo fmt` and keep `cargo test` passing.

## Tests and verification

Run the narrowest relevant test while iterating, then run the full seams before handoff (just 1.56+):

```sh
just check
```

This checks version metadata and justfile/Rust formatting, runs Go and Rust tests,
the Go race detector, vet, golangci-lint (including Go formatting), Cargo Clippy,
and the Wasm build. The recipes set `GOWORK=off` and preserve existing `TMPDIR`
and `GOCACHE` overrides. The race recipe sets `GOMAXPROCS=1` for golang/go#81122:
x/tools sizes its type-checking semaphore at init, and Go 1.27 has an upstream
race between concurrent checkers. Remove that workaround once the toolchain
includes CL 841585. Windows tasks run through Git Bash's `sh`; the build and
install recipes invoke PowerShell 7 explicitly, and version checks also need
`awk` on `PATH`.

Use `just bench` (or `./scripts/benchmark-server.sh`) for the maintained benchmark/profile suite. Its profiles belong outside the repository. Differential tests should compare optimized paths with the full semantic analyzer; algorithm tests should preserve adversarial cases such as embedding, pointer receivers, test variants, UTF-16 surrogate pairs, cancellation, and stale revisions.

## Documentation and releases

- Keep public behavior, limitations, commands, platform claims, and performance figures aligned in both READMEs.
- The local `PERFORMANCE.md` and `PERFORMANCE.zh-CN.md` files are private working notebooks and are intentionally ignored. Do not add them to Git or link public documentation to them.
- Use `just set-version <version>` (or `./scripts/set-version.sh <version>`) to synchronize `extension.toml`,
  `Cargo.toml`, and `Cargo.lock`; do not update those version fields independently.
  Native build scripts inject the version from `extension.toml` through linker
  flags. Direct Go builds report `dev`; never hardcode a release version in Go.
- Native binaries, benchmark profiles, build directories, and `extension.wasm` are generated artifacts and must not be committed.
- Release assets are six versioned, cgo-free native binaries plus `SHA256SUMS`. Keep target naming synchronized between the build scripts, Rust adapter, and GitHub workflows.
- GitHub CI must exercise the six supported targets on native runners. Local cross-compilation remains a useful buildability check, not release proof.
- Preserve attribution to Anirudh Agarwal's original VSCode extension in the public README.
- Keep the project under the repository's MIT License unless the owner explicitly decides otherwise.

## Change discipline

- Inspect existing local changes before editing and never discard unrelated work.
- Avoid behavior changes during pure performance or documentation tasks.
- Prefer focused commits that separate algorithms, benchmarks, platform packaging, and documentation when commits are requested.
- Report fallbacks, unverified platform claims, benchmark environment details, and remaining release risks explicitly.
