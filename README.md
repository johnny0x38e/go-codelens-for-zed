# Go Implementation Lens for Zed

An experimental Zed extension that adds Go implementation information through an auxiliary Language Server Protocol (LSP) server. The current milestone is a thin end-to-end proof of concept: it proves that Zed can load the extension, run a native Go sidecar alongside `gopls`, render a CodeLens, and execute the standard `editor.action.showReferences` command.

The CodeLens is static in this milestone. Go-aware interface and implementation analysis is the next milestone.

## Architecture

The extension has two small parts:

- `src/lib.rs` is a Rust/Wasm Zed adapter. It registers the auxiliary language server and launches its native executable.
- `server/` is the Go LSP sidecar. It currently implements `initialize`, `shutdown`, `exit`, and `textDocument/codeLens` over stdio.

Keeping semantic analysis in Go lets the production implementation use Go-native package and type information while the Zed-specific layer stays minimal.

## Run the proof of concept

Requirements:

- Zed 1.14.2 or newer
- Go 1.26 or newer for this checkout
- Rust installed with `rustup`; Zed extensions use the `wasm32-wasip2` target
- macOS on Apple Silicon for the current development build script

Build and install the native sidecar into `~/.local/bin`, which is already on the Zed workspace `PATH` in the tested setup:

```sh
./scripts/install-server.sh
```

Set `GO_IMPLEMENTATION_LENS_BIN_DIR` if Zed inherits a different directory on `PATH`.

In Zed, run `zed: install dev extension` and choose this repository. Then open `examples/poc/main.go` from this repository.

The checked-in `.zed/settings.json` enables inline CodeLens and starts both servers for Go:

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

`"..."` is Zed's language-server wildcard: it retains any other Go servers that are registered but not named explicitly.

When the proof of concept is running, `Go Implementation Lens: POC` appears above the `Greeter` interface declaration. Activating it invokes Zed's references navigation with the location returned by the sidecar; the cursor moves to the declaration. `gopls` remains enabled as the primary Go server. The root `go.work` includes both Go modules so `gopls` can load this checkout as a workspace.

This flow was verified end to end on Zed 1.14.2 for Apple Silicon macOS: Zed started both `gopls` and `go-implementation-lens-ls`, sent `textDocument/codeLens`, rendered the returned title, and executed the returned `editor.action.showReferences` command. In this Zed version a Lens anchored at `(0, 0)` is not visible above the first line, so the fixture deliberately anchors the static Lens at the interface declaration.

The three Zed display modes were also checked with this fixture: `on` renders the title inline, `off` hides it, and `menu` renders the single available action and executes it when clicked. The checked-in setting is restored to `on`.

After rebuilding a loaded dev extension with `zed: rebuild dev extension`, reopen or reload the workspace if the sidecar was stopped but not restarted automatically.

## Verify the two build seams

```sh
cd server
env TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test ./...
cd ..
cargo check --target wasm32-wasip2
```

## Current limitations

- The server returns one static CodeLens at a fixed location in every Go document; it does not yet inspect Go syntax or types.
- The static POC range is fixed to line 5 (LSP line 4), matching `examples/poc/main.go`; shorter files may clip that range. The semantic milestone will calculate ranges from real declarations.
- The development build only targets Apple Silicon macOS.
- The native sidecar is deliberately ignored by Git. The proof of concept locates it on `PATH`; Zed's extension publishing rules require language-server extensions to download their server or locate it on `PATH` rather than commit the binary into the extension repository.
- Packaging, updates, checksums, additional platforms, and the final license choice are outside this proof-of-concept milestone.
