# Go Implementation Lens for Zed

A Zed extension that shows exact Go interface implementation relationships as CodeLens entries. It runs a small native Go language server alongside `gopls`; `gopls` remains Zed's primary Go language server.

## Features

For the active Go module, the extension shows:

- `N implementations` on named interfaces, linked to the implementing named types.
- `Implements: Interface` on named types, linked back to their interfaces.
- `N implementations` on explicit interface methods, linked to matching receiver methods.
- `Implements: Interface.Method` on receiver methods, including useful matches on partial implementations.

Full interface relationships use `go/types.Implements`, including pointer method sets, so a type is never reported as a complete implementation from method names alone. Method relationships require the same Go method identity and a `types.Identical` signature; methods with the right name but wrong parameters or results are excluded.

The server consumes Zed's full-text document notifications, so unsaved edits immediately invalidate and recompute the affected semantic snapshot. Lens titles and navigation targets are deterministic, and source positions use LSP UTF-16 offsets.

## Architecture

The extension has two parts:

- `src/lib.rs` is the Rust/Wasm Zed adapter. It registers the auxiliary language server and locates its native executable on the worktree `PATH`.
- `server/` is the Go LSP sidecar. It uses `golang.org/x/tools/go/packages`, `go/ast`, and `go/types` to analyze the module containing the requested document.

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

Set `GO_IMPLEMENTATION_LENS_BIN_DIR` if Zed inherits a different directory on `PATH`.

In Zed, run `zed: install dev extension` and choose this repository. If it is already installed, run `zed: rebuild dev extension`. Then open `examples/poc/main.go` from this repository. Reopen or reload the workspace if Zed stopped the old sidecar without restarting it.

The checked-in `.zed/settings.json` enables inline CodeLens and keeps both Go servers active:

```json
{
  "code_lens": "on",
  "languages": {
    "Go": {
      "language_servers": ["gopls", "go-implementation-lens", "..."]
    }
  }
}
```

`"..."` retains any other Go language servers registered with Zed.

The example should show:

- `2 implementations` on `Greeter` and `Greeter.Greet`.
- `Implements: Greeter` on `Person` and `Robot`.
- `Implements: Greeter.Greet` on both correct `Greet` methods.
- No implementation Lens on `BrokenGreeter` or its incompatible `Greet(int)` method.

Activating a Lens invokes Zed's standard `editor.action.showReferences` command. A single target navigates directly; multiple targets open Zed's references results.

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

- Analysis is scoped to the Go module containing the requested document. Imported dependencies provide type information but do not receive Lens entries.
- Empty interfaces report zero implementations instead of treating every named type as a useful target.
- Method Lens entries cover explicit interface method declarations and explicit receiver method declarations. Complete interface/type relationships still account for embedded and promoted methods through `go/types`.
- Zed's current extension API does not expose the VSCode extension's custom gutter decorations or tree-view sidebar. Their relationship data is represented through navigable CodeLens entries where possible.
- The development build script currently targets Apple Silicon macOS. Publishing requires platform downloads, checksums, updates, and a final license choice.
- Native binaries and `extension.wasm` are intentionally not committed.
