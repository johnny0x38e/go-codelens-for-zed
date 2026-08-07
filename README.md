# Go CodeLens for Zed

**English** | [简体中文](README.zh-CN.md)

Shows Go references and interface implementations as CodeLens entries in Zed.

## Why this project exists

Go interfaces are implemented implicitly. That makes the language flexible, but it also means an important relationship is often invisible at the declaration site: an interface does not list its implementers, and a concrete type does not declare which interfaces it satisfies. For day-to-day navigation and review, being able to see those relationships immediately is extremely valuable.

`gopls` remains the right primary language server for Go, but it does not currently provide the relationship-oriented CodeLens view this project needs. Go CodeLens therefore runs as a focused auxiliary language server beside `gopls`; it adds reference and implementation lenses without proxying or replacing any of the normal Go tooling.

The project was inspired by [Go Implementation Lens for Visual Studio Code](https://github.com/anirudhagarwal365/go-implementation-lens), created by [Anirudh Agarwal](https://github.com/anirudhagarwal365). Its workflow and presentation proved how useful this information can be. This repository is an independent implementation for Zed, built around Go's type system and further engineered for exact results, incremental analysis, low allocation overhead, and minimal disk I/O.

## Screenshots

<img src="assets/screenshot.png" alt="CodeLens">

## Features

- `0 references`, `1 reference`, or `N references` on interfaces, functions, and methods.
- Reference counts on used named types, package variables, constants, named struct fields, and type parameters. Unused declarations in these categories are hidden to reduce noise.
- `N implementations` on named interfaces and explicit interface methods, with exact navigation targets.
- `Implements: Interface` on named types and `Implements: Interface.Method` on receiver methods. Multiple relationships are displayed together.
- Complete support for internal and external `_test.go` package variants.
- Multi-module analysis through the nearest applicable `go.work`; ordinary modules work without a work file.
- Immediate analysis of unsaved edits through incremental LSP document synchronization.

Full interface relationships are decided by `go/types.Implements`, including pointer method sets and promoted methods. Method relationships first use the package-aware Go method identity and then require a `types.Identical` signature. Reference counts are also semantic: they join `go/types.Info.Defs` and `go/types.Info.Uses`, exclude declaration locations, collapse duplicate production/test-variant locations, and never count unrelated symbols merely because their names match.

## Architecture

```text
Zed Editor
├── gopls                         primary Go language features
└── Go CodeLens extension
    ├── src/lib.rs                small Rust/Wasm Zed adapter
    └── go-codelens-ls            native, cgo-free Go LSP sidecar
        ├── internal/lsp          JSON-RPC, document sync, caches, cancellation
        └── internal/analysis     scopes, type facts, indexes, relationships
            └── go/packages + go/ast + go/types
```

The adapter first looks for `go-codelens-ls` on the worktree `PATH`, which keeps local development overrides simple. If none is present, it selects the matching versioned GitHub Release asset, verifies it against `SHA256SUMS`, and caches the native binary in Zed's extension work directory. The sidecar then discovers the applicable module or `go.work` scope, loads a coherent Go type universe, extracts compact semantic facts, and returns standard LSP CodeLens commands. Keeping the sidecar independent lets `gopls` continue to own completion, diagnostics, formatting, rename, and the rest of the Go editor experience.

The Go entry point intentionally follows the conventional layout `server/cmd/go-codelens-ls`, while implementation packages remain under `server/internal` because they are not a public Go API.

## Technical design

- **Exact semantics:** relationships are verified by `go/types`; name-based indexes only narrow candidate sets and never determine correctness.
- **Incremental engine:** body edits recheck known package owners, API edits recheck the reverse dependency closure, and structural/module changes fall back to one complete `packages.Load`.
- **Immutable revisions:** package facts, relationship indexes, and snapshots use copy-on-write updates, allowing concurrent readers to share unchanged maps, slices, and target lists safely.
- **Fast document editing:** a UTF-8 gap buffer and line-start index apply incremental UTF-16 LSP edits without rebuilding the full document.
- **Bounded concurrency and caches:** same-revision requests share one analysis flight; stale work cannot publish, abandoned work is canceled, and retained scope/snapshot caches have fixed bounds.
- **Low disk activity:** only exact lifecycle or compiled-source reads are performed. By default the plugin writes no logs, semantic index, or persistent cache; an explicit `GO_CODELENS_TRACE` opt-in may write protocol method traces, and Go tooling may use the standard Go build cache during package loading.
- **Deterministic output:** lenses and navigation targets are sorted, deduplicated, and represented with correct LSP UTF-16 positions.
- **Portable and verified delivery:** the cgo-free Go sidecar supports macOS, Linux, and Windows on arm64 and x86-64. Release assets are version-pinned and SHA-256 verified; native tests and builds on all six targets are release gates.

## Performance

Performance work is guarded by unit, differential, integration, fuzz, race, allocation, cancellation, and end-to-end benchmark coverage. The first large-project acceptance fixture used an unmodified shallow checkout of Kubernetes on an Apple M1 Pro with Go 1.26.5.

| Kubernetes fixture            |                                      Value |
| ----------------------------- | -----------------------------------------: |
| Commit                        | `b882c60b4023bdf09264c2d5d30a2cadebc240fb` |
| Checkout size                 |                                    395 MiB |
| Tracked non-vendor Go files   |                                     13,414 |
| Modules in `go.work`          |                                         34 |
| Packages across the workspace |                                      3,029 |
| Sample document               |                   `pkg/probe/exec/exec.go` |

| Measurement                                        |     Result |
| -------------------------------------------------- | ---------: |
| First full workspace analysis and CodeLens request |  4m42.692s |
| Same-process semantic snapshot cache hit           | 249.958 µs |
| CodeLens entries returned                          |         10 |
| Canceled request return latency                    |   4.542 µs |
| Underlying analyzer cancellation latency           |   1.967 ms |
| Maximum resident set size                          |   3.69 GiB |
| macOS peak memory footprint                        |   6.66 GiB |
| Swaps                                              |          0 |

This is a single real-project acceptance observation, not a statistically sampled benchmark. The cold result intentionally covers all 34 work-file modules with tests enabled; its dominant cost is the initial `packages.Load`/Go toolchain package graph. Once published, the immutable semantic snapshot serves repeated requests in microseconds without re-entering the analyzer. All ten expected Kubernetes lenses were preserved.

Run `./scripts/benchmark-server.sh` to execute the maintained correctness, race, benchmark, allocation, and profile suite. Profiles default to a directory under `$TMPDIR`, outside the repository.

## Development and build

Requirements for this checkout:

- Zed 1.14.2 or newer
- Go 1.26 or newer
- Rust installed through `rustup`, including the `wasm32-wasip2` target

For local development on macOS or Linux, build the current host binary and install it to `~/.local/bin`:

```sh
./scripts/install-server.sh
```

On Windows, run the equivalent PowerShell installer:

```powershell
pwsh -File .\scripts\install-server.ps1
```

`~/.local/bin` is only the development installer's default, not a fixed Zed extension location. Set `GO_CODELENS_BIN_DIR` when Zed inherits a different directory on `PATH`. Marketplace installations need neither script: when no PATH override exists, Zed downloads the version-matched binary into its own platform-dependent extension work directory.

The build scripts also accept explicit targets, which is useful for checking all build outputs from one machine:

```sh
./scripts/build-server.sh linux amd64
./scripts/build-server.sh windows arm64
```

```powershell
pwsh -File .\scripts\build-server.ps1 -TargetOS windows -TargetArch arm64
```

Supported release targets are:

| Operating system | Architecture | Release target triple       |
| ---------------- | ------------ | --------------------------- |
| macOS            | arm64        | `aarch64-apple-darwin`      |
| macOS            | x86-64       | `x86_64-apple-darwin`       |
| Linux            | arm64        | `aarch64-unknown-linux-gnu` |
| Linux            | x86-64       | `x86_64-unknown-linux-gnu`  |
| Windows          | arm64        | `aarch64-pc-windows-msvc`   |
| Windows          | x86-64       | `x86_64-pc-windows-msvc`    |

After installing a local sidecar, run `zed: install dev extension` and select this repository, or run `zed: rebuild dev extension` after subsequent changes. The checked-in `.zed/settings.json` keeps `gopls` and `go-codelens` active together.

Run the full local verification seams with:

```sh
cd server
env GOWORK=off TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test ./...
env GOWORK=off TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test -race ./...
env GOWORK=off TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go vet ./...
golangci-lint run --config ../.golangci.yaml ./...
cd ..
cargo fmt -- --check
cargo test
cargo build --target wasm32-wasip2
```

Prepare a release version with one command:

```sh
./scripts/set-version.sh 0.3.3
```

The script prepares all edits before touching the working tree, synchronizes `extension.toml`, `Cargo.toml`, `Cargo.lock`, and the native server's reported version, then validates the locked Cargo metadata. If validation fails, it restores the original files. The release workflow calls the same script in read-only `--check` mode so local preparation and CI cannot drift apart.

CI runs the Go suite and cgo-free build on native GitHub-hosted runners for all six targets, plus the race detector on Linux amd64 and the Rust/Wasm checks separately. A tag named `v<version>` triggers the release workflow only when the tag, `Cargo.toml`, `extension.toml`, and the native server's reported version match. The workflow publishes six raw native binaries and a deterministic `SHA256SUMS`; the extension adapter only downloads assets for its own exact version.

`examples/poc/main.go` is the small functional fixture. Native binaries, benchmark profiles, and `extension.wasm` are intentionally excluded from version control.

## Current Zed limitations

- On Zed 1.14.2, a multi-target CodeLens compatibility command opens a multibuffer even when `lsp_results_location` is set to `"picker"`. Picker behavior for CodeLens requires an upstream Zed capability.
- CodeLens typography is controlled by Zed's editor settings; the LSP and extension APIs do not expose per-lens font styling.
- Zed's extension API does not expose the VSCode extension's custom gutter decorations or tree-view sidebar. Their essential relationship data is represented through navigable CodeLens entries.
- Dependencies outside the selected module/`go.work` scope contribute type information but do not receive lenses. Empty interfaces intentionally report zero useful implementations.
- A marketplace version requires a matching `v<version>` GitHub Release containing all six binaries and `SHA256SUMS`; a development checkout can continue to use a PATH-installed sidecar before that release exists.

## License

Go CodeLens for Zed is available under the [MIT License](LICENSE).
