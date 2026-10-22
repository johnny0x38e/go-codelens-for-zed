# Go CodeLens for Zed

**English** | [简体中文](README.zh-CN.md)

Shows symbol reference counts and interface implementation relationships for Go code in Zed, with click-to-navigate support.

Go CodeLens runs alongside `gopls` as an auxiliary language server. Completion, diagnostics, formatting, rename, and other Go editing features remain with `gopls`.

The project was inspired by [Go Implementation Lens for Visual Studio Code](https://github.com/anirudhagarwal365/go-implementation-lens), created by [Anirudh Agarwal](https://github.com/anirudhagarwal365). This repository is an independent implementation for Zed, built on Go's type system.

<img src="assets/screenshot.png" alt="CodeLens">

## Install the development version

The extension is not yet available in Zed's extension registry. To try it locally:

1. Install Zed 1.14.2+, Go 1.27+, Rust with the `wasm32-wasip2` target, and just 1.56+.
2. Clone this repository, then build and install the server:

    ```sh
    git clone https://github.com/johnny0x38e/go-codelens-for-zed.git
    cd go-codelens-for-zed
    just install
    ```

    The server is installed to `~/.local/bin` by default; make sure Zed can find it on `PATH`.

3. In Zed, run `zed: install dev extension` and select the cloned repository.

No explicit `language_servers` configuration is needed. If CodeLens is not visible, enable `"code_lens": "on"` in Zed's `settings.json`.

After changing the extension, run `zed: rebuild dev extension`.

## Features

- `0 references`, `1 reference`, or `N references` on interfaces, functions, and methods.
- Reference counts on used named types, package-level variables, constants, named struct fields, and type parameters; unused declarations in these categories are hidden.
- `N implementations` on named interfaces and explicit interface methods, with accurate navigation targets.
- `Implements: Interface` on named types and `Implements: Interface.Method` on receiver methods; a declaration that satisfies several interfaces shows all of them.
- Handles internal and external `_test.go` test packages.
- Uses the nearest `go.work` file (Go's workspace file) for multi-module projects; single-module projects need no work file.

Relationships use Go's type information, including pointer receivers and promoted methods. Counts exclude declaration sites and merge duplicate locations from production and test packages.

## How it works

```text
Zed
├── gopls                    primary Go language server
└── Go CodeLens extension
    ├── src/lib.rs           Rust/WebAssembly adapter
    └── go-codelens-ls       auxiliary CodeLens language server (separate process)
```

The Rust/WebAssembly adapter uses `go-codelens-ls` from the project's `PATH`. If absent, it downloads the matching GitHub Release binary, verifies it against `SHA256SUMS`, and caches it in Zed's extension work directory.

The native server (`server/cmd/go-codelens-ls`) loads Go type information for the module or `go.work` workspace and returns standard LSP CodeLens results. It writes no logs, indexes, or persistent caches by default. Set `GO_CODELENS_TRACE` to trace LSP method calls.

## Development and build

Requirements: Zed 1.14.2+, Go 1.27+, Rust with `wasm32-wasip2`, just 1.56+, and golangci-lint.

Build and install the CodeLens server for the current host with `just install` (default `~/.local/bin`; set `GO_CODELENS_BIN_DIR` if Zed uses another directory). Marketplace installs need no local tools: when there is no `PATH` override, Zed downloads the matching binary.

| Command                                 | Purpose                                    |
| --------------------------------------- | ------------------------------------------ |
| `just build` / `just build linux amd64` | Build the host server or a specific target |
| `just build-wasm`                       | Build the WebAssembly adapter              |
| `just install`                          | Build and install the host server          |
| `just fmt`                              | Format Go, Rust, and the justfile          |
| `just test` / `just race`               | Run tests / the race detector              |
| `just vet` / `just lint`                | Run `go vet` / lint Go and Rust            |
| `just check`                            | Run the complete local verification suite  |
| `just bench`                            | Run tests, benchmarks, and profiling       |
| `just set-version 0.3.3`                | Update release versions                    |
| `just check-version`                    | Check release version consistency          |

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
