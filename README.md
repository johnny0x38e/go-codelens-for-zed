# Go CodeLens for Zed

**English** | [简体中文](README.zh-CN.md)

A Zed extension that shows exact Go references and interface implementation relationships as CodeLens entries. It runs a small native Go language server alongside `gopls`; `gopls` remains Zed's primary Go language server.

Inspired by [Go Implementation Lens for Visual Studio Code](https://github.com/anirudhagarwal365/go-implementation-lens), independently reimplemented for Zed and optimized for high performance, low memory overhead, and minimal disk I/O.

## Features

For the active Go analysis scope, the extension shows:

- `0 references`, `1 reference`, or `N references` on interfaces, functions, and methods.
- Reference counts on used named types, package variables, constants, named struct fields, and type parameters. Unused declarations in these categories are hidden to avoid noise.
- `N implementations` on named interfaces, linked to the implementing named types.
- `Implements: Interface` on named types, linked back to their interfaces.
- `N implementations` on explicit interface methods, linked to matching receiver methods.
- `Implements: Interface.Method` on receiver methods, including useful matches on partial implementations.

Full interface relationships use `go/types.Implements`, including pointer method sets, so a type is never reported as a complete implementation from method names alone. Method relationships require the same Go method identity and a `types.Identical` signature; methods with the right name but wrong parameters or results are excluded.

References are also semantic rather than textual. The analyzer joins declaration objects from `go/types.Info.Defs` with uses from `go/types.Info.Uses` across all loaded packages and test variants in the scope. Declaration locations are excluded, duplicate ordinary/test package variants are collapsed by source location, and same-named but unrelated symbols are not counted. Reference navigation targets are the exact usage locations.

When the document's module is listed by a containing `go.work`, the scope contains every module in that work file. Otherwise, the scope is the document's module. Internal and external `_test.go` packages participate in reference counts, implementation relationships, CodeLens entries, and navigation.

The server consumes Zed's full-text document notifications, so unsaved edits immediately invalidate and recompute the affected semantic snapshot. Lens titles and navigation targets are deterministic, and source positions use LSP UTF-16 offsets.

## Architecture

The extension has two parts:

- `src/lib.rs` is the Rust/Wasm Zed adapter. It registers the auxiliary language server and locates its native executable on the worktree `PATH`.
- `server/` is the Go LSP sidecar. It uses `golang.org/x/tools/go/packages`, `golang.org/x/mod/modfile`, `go/ast`, and `go/types` to analyze the applicable module or `go.work` workspace.

The sidecar deliberately does not proxy or replace `gopls`. The proof of concept established that Zed can run both language servers for Go, so keeping them independent avoids forwarding every gopls capability and keeps this extension focused on implementation lenses.

## Install the development extension

Requirements:

- Zed 1.14.2 or newer
- Go 1.26 or newer for this checkout
- Rust installed with `rustup`; Zed extensions use the `wasm32-wasip2` target
- Apple Silicon macOS for the current native development build script

Build and install the native sidecar into `~/.local/bin`:

```sh
./scripts/install-server.sh
```

Set `GO_CODELENS_BIN_DIR` if Zed inherits a different directory on `PATH`.

In Zed, run `zed: install dev extension` and choose this repository. If it is already installed, run `zed: rebuild dev extension`. Then open `examples/poc/main.go` from this repository. Reopen or reload the workspace if Zed stopped the old sidecar without restarting it.

The checked-in `.zed/settings.json` enables inline CodeLens and keeps both Go servers active:

```json
{
  "code_lens": "on",
  "languages": {
    "Go": {
      "language_servers": ["gopls", "go-codelens", "..."]
    }
  }
}
```

`"..."` retains any other Go language servers registered with Zed.

The example should show:

- Reference lenses on the interface, interface method, functions, used types, and fields.
- `2 implementations` on `Greeter` and `Greeter.Greet`.
- `Implements: Greeter` on `Person` and `Robot`.
- `Implements: Greeter.Greet` on both correct `Greet` methods.
- No implementation Lens on `BrokenGreeter` or its incompatible `Greet(int)` method.

Activating a Lens invokes Zed's standard `editor.action.showReferences` compatibility command. A single target navigates directly.

On Zed 1.14.2, clicking a multi-target CodeLens always opens Zed's multibuffer results, even when `lsp_results_location` is set to `"picker"`. Zed maps all three CodeLens compatibility commands (`editor.action.showReferences`, `editor.action.goToLocations`, and `editor.action.peekLocations`) to the same multibuffer-only path. The picker setting currently applies only to native editor actions. To open the picker today, place the cursor on the declaration and run `editor: go to implementation` or `editor: find all references` from the command palette.

## Verify the build seams

```sh
cd server
env GOWORK=off TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test -race ./...
cd ..
cargo fmt -- --check
cargo build --target wasm32-wasip2
./scripts/install-server.sh
```

## Current limitations

- Analysis is scoped to the nearest containing `go.work` that lists the document's module, or to that module when no applicable work file exists. Imported dependencies outside the scope provide type information but do not receive Lens entries.
- Empty interfaces report zero implementations instead of treating every named type as a useful target.
- Method Lens entries cover explicit interface method declarations and explicit receiver method declarations. Complete interface/type relationships still account for embedded and promoted methods through `go/types`.
- Zed 1.14.2 does not expose a picker-capable CodeLens command or extension action API. Making a multi-target Lens click honor `lsp_results_location` requires a Zed upstream change.
- Code Lens typography is controlled by Zed: it uses the editor's buffer font at 90% of the configured buffer font size. Neither LSP CodeLens nor Zed's extension API exposes per-Lens font styling; changing `buffer_font_family` or `buffer_font_size` also changes normal editor text.
- Zed's current extension API does not expose the VSCode extension's custom gutter decorations or tree-view sidebar. Their relationship data is represented through navigable CodeLens entries where possible.
- The development build script currently targets Apple Silicon macOS. Publishing requires platform downloads, checksums, and updates.
- Native binaries and `extension.wasm` are intentionally not committed.

## License

Licensed under the [MIT License](LICENSE).
