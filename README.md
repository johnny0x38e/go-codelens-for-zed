# Go CodeLens for Zed

**English** | [简体中文](README.zh-CN.md)

Shows Go references and interface implementations as CodeLens entries in Zed. A CodeLens entry is the small clickable label above a declaration.

Go interfaces are implemented implicitly, so an interface does not list its implementers and a concrete type does not say which interfaces it satisfies. `gopls` remains the primary Go language server but does not provide this relationship view. Go CodeLens runs as a separate auxiliary language server beside it, adds reference and implementation entries, and leaves completion, diagnostics, formatting, rename, and other Go editing features to `gopls`.

The project was inspired by [Go Implementation Lens for Visual Studio Code](https://github.com/anirudhagarwal365/go-implementation-lens), created by [Anirudh Agarwal](https://github.com/anirudhagarwal365). This repository is an independent implementation for Zed, built on Go's type system.

<img src="assets/screenshot.png" alt="CodeLens">

## Features

- `0 references`, `1 reference`, or `N references` on interfaces, functions, and methods.
- Reference counts on used named types, package-level variables, constants, named struct fields, and type parameters; unused declarations in these categories are hidden.
- `N implementations` on named interfaces and explicit interface methods, with accurate navigation targets.
- `Implements: Interface` on named types and `Implements: Interface.Method` on receiver methods; a declaration that satisfies several interfaces shows all of them.
- Handles internal and external `_test.go` test packages.
- Uses the nearest `go.work` file (Go's workspace file) for multi-module projects; single-module projects need no work file.
- Applies incremental document changes from the editor, so unsaved edits are analyzed immediately.

Interface relationships use `go/types.Implements`, including pointer receivers and promoted methods. Method relationships require the same package and name plus a `types.Identical` signature. Reference counts come from `go/types.Info.Defs` and `go/types.Info.Uses`; declaration sites are excluded, duplicate locations from production and test packages are merged, and unrelated symbols with the same name are ignored.

## How it works

```text
Zed
├── gopls                    primary Go language server
└── Go CodeLens extension
    ├── src/lib.rs           Rust/WebAssembly adapter
    └── go-codelens-ls       auxiliary CodeLens language server (separate process)
```

The adapter looks for `go-codelens-ls` on the project's `PATH`; if absent, it downloads the matching GitHub Release file, verifies it against `SHA256SUMS`, and caches the executable in Zed's extension work directory. The CodeLens server finds the module or `go.work` workspace, loads one consistent set of type information, and returns standard CodeLens results over the Language Server Protocol (LSP). The server entry point is `server/cmd/go-codelens-ls`; implementation packages live under `server/internal`.

- **Exact results:** every relationship is checked by `go/types`; name-based indexes only narrow candidates.
- **Incremental analysis:** body edits recheck the packages that own the changed files, API edits recheck their dependents, and structural or module changes fall back to a full `packages.Load`.
- **Read-only results:** updates copy only changed data, so readers share unchanged maps, slices, and target lists; outdated results are never returned.
- **Fast edits:** a UTF-8 text buffer with a line-start index applies UTF-16 edits without rebuilding the document.
- **Bounded concurrency and caches:** requests for the same document version share one analysis, abandoned work is canceled, and module and result caches have fixed limits.
- **Low disk I/O:** files are read only for opened or changed documents and exact compiled-source paths; by default it writes no logs, index, or persistent cache. `GO_CODELENS_TRACE` records LSP method calls.
- **Stable and portable:** output is sorted, deduplicated, and uses LSP UTF-16 positions; the CodeLens server is built without a C compiler and supports macOS, Linux, and Windows on arm64 and x86-64, with SHA-256-checked releases.

## Performance

Tested on an unmodified shallow Kubernetes checkout (no full history) on an Apple M1 Pro with Go 1.26.5.

| Kubernetes test checkout                          |                                      Value |
| ------------------------------------------------- | -----------------------------------------: |
| Commit                                            | `b882c60b4023bdf09264c2d5d30a2cadebc240fb` |
| Checkout size                                     |                                    395 MiB |
| Go files under version control (excluding vendor) |                                     13,414 |
| Modules in `go.work`                              |                                         34 |
| Packages in the workspace                         |                                      3,029 |
| File used for the CodeLens request                |                   `pkg/probe/exec/exec.go` |

| Measurement                                        |     Result |
| -------------------------------------------------- | ---------: |
| First full workspace analysis and CodeLens request |  4m42.692s |
| Repeat request in the same process (cached result) | 249.958 µs |
| CodeLens entries returned                          |         10 |
| Time to return after a request is canceled         |   4.542 µs |
| Time for the analyzer to stop after cancellation   |   1.967 ms |
| Peak process memory                                |   3.69 GiB |
| Peak memory reported by macOS                      |   6.66 GiB |
| Swap used                                          |          0 |

This is one observation, not a sampled benchmark. The first analysis includes all 34 modules with tests enabled; most of the time is the initial `packages.Load`. Cached repeat requests return in microseconds, and all ten expected CodeLens entries were present.

`just bench` runs the correctness, race, benchmark, allocation, and profiling tests; profiles go under `$TMPDIR`.

## Development and build

Requirements: Zed 1.14.2+, Go 1.27+, Rust with `wasm32-wasip2`, just 1.56+, and golangci-lint.

Build and install the CodeLens server for the current host with `just install` (default `~/.local/bin`; set `GO_CODELENS_BIN_DIR` if Zed uses another directory). Marketplace installs need no local tools: when there is no `PATH` override, Zed downloads the matching binary.

| Command | Purpose |
| --- | --- |
| `just build` / `just build linux amd64` | Build the host server or a specific target |
| `just build-wasm` | Build the WebAssembly adapter |
| `just install` | Build and install the host server |
| `just fmt` | Format Go, Rust, and the justfile |
| `just test` / `just race` | Run tests / the race detector |
| `just vet` / `just lint` | Run `go vet` / lint Go and Rust |
| `just check` | Run the complete local verification suite |
| `just bench` | Run tests, benchmarks, and profiling |
| `just set-version 0.3.3` | Update release versions |
| `just check-version` | Check release version consistency |

`just build [os] [arch] [output]` accepts the same targets and output path as the build scripts; relative paths start at the repository root:

```sh
just build windows arm64
just build linux amd64 bin/custom-server
```

On Windows, `just` runs tasks through Git Bash's `sh`, so `sh` must be on `PATH`; build and install call PowerShell 7 (`pwsh`) directly, and version checks also need `awk`.

Supported targets:

| Operating system | Architecture | Release target identifier   |
| ---------------- | ------------ | --------------------------- |
| macOS            | arm64        | `aarch64-apple-darwin`      |
| macOS            | x86-64       | `x86_64-apple-darwin`       |
| Linux            | arm64        | `aarch64-unknown-linux-gnu` |
| Linux            | x86-64       | `x86_64-unknown-linux-gnu`  |
| Windows          | arm64        | `aarch64-pc-windows-msvc`   |
| Windows          | x86-64       | `x86_64-pc-windows-msvc`    |

After installing the CodeLens server, run `zed: install dev extension` and select this repository, or `zed: rebuild dev extension` after changes. `.zed/settings.json` keeps `gopls` and `go-codelens` active together.

`just check` runs the full suite. Tasks set `GOWORK=off`, use `TMPDIR`/`TEMP`/`/tmp` for temporary files, and put `GOCACHE` in `go-build-cache` under that directory (an existing `GOCACHE` is kept). Race sets `GOMAXPROCS=1` for the Go 1.27 `go/types` issue in `AGENTS.md`; `BENCHMARK_OUTPUT_DIR` selects another output directory.

`just set-version 0.3.3` updates the version fields in `extension.toml`, `Cargo.toml`, and `Cargo.lock` and restores them if validation fails. Builds inject the version; direct `go build` or `go run` reports `dev`. CI runs tests and builds that do not need a C compiler on all six targets, the race detector on Linux amd64, and Rust/WebAssembly checks. A matching `v<version>` tag publishes six executables and `SHA256SUMS`; the extension downloads only files for its own version.

A `v<version>` tag also opens a registry update PR through [huacnlee/zed-extension-action](https://github.com/huacnlee/zed-extension-action). Configure a repository secret named `COMMITTER_TOKEN` with a classic GitHub personal access token that has `repo` and `workflow` scopes. The action opens the PR; a maintainer still reviews and merges it. This automation takes effect after the extension is accepted into `zed-industries/extensions`.

## Current Zed limitations

- As of Zed 1.16.2, clicking a multi-target CodeLens always opens Zed's multibuffer; that path ignores `lsp_results_location`, so `"picker"` does not affect CodeLens clicks. Picker results still work through Zed navigation, such as cmd-clicking a symbol or binding `editor: find all references` with `"open_results_in": "picker"`; changing CodeLens clicks needs support from Zed itself.
- CodeLens font styling follows Zed's editor settings; the LSP and extension APIs do not support per-entry fonts.
- Zed's extension API does not expose the VS Code extension's custom gutter decorations or tree-view sidebar; relationship data is shown as clickable CodeLens entries.
- Dependencies outside the selected module or `go.work` workspace provide type information but no CodeLens entries. Empty interfaces report zero useful implementations.
- Marketplace versions need a matching `v<version>` GitHub Release with all six executables and `SHA256SUMS`; a development copy can use a `go-codelens-ls` on `PATH` before that release exists.

## License

Go CodeLens for Zed is available under the [MIT License](LICENSE).
