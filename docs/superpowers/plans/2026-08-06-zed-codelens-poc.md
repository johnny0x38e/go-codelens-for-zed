# Zed Go Implementation CodeLens POC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a minimal Zed extension and Go stdio language server that coexist with gopls and render one clickable CodeLens in every opened Go document.

**Architecture:** Zed loads a thin Rust WASM extension that launches a native Go sidecar. The sidecar implements only the LSP methods needed by this proof of concept and returns a standard `editor.action.showReferences` command so Zed owns navigation UI. The POC deliberately does not proxy gopls or perform Go semantic analysis.

**Tech Stack:** Zed extension API 0.7.0, Rust 2024 edition, Go 1.26 standard library, JSON-RPC 2.0/LSP over stdio.

## Global Constraints

- Keep gopls enabled; the POC language server must run alongside it.
- Test observable LSP behavior through framed stdio messages, not private parser helpers.
- Use no Go third-party dependency in the POC server.
- Return CodeLens commands using `editor.action.showReferences` with `[uri, position, locations]` arguments.
- Do not implement semantic indexing, references search, gutter UI, or sidebar UI in this milestone.
- Support the current development platform first: macOS arm64 with Zed 1.14.2.

---

## File Structure

- `extension.toml`: Zed extension metadata and the auxiliary Go language-server registration.
- `Cargo.toml`: Rust WASM extension crate and Zed extension API dependency.
- `src/lib.rs`: Finds the development sidecar binary and returns its command to Zed.
- `server/go.mod`: Isolated Go module for the native sidecar.
- `server/cmd/go-implementation-lens-ls/main.go`: stdio process entry point.
- `server/internal/lsp/server.go`: JSON-RPC framing, request dispatch, and LSP responses.
- `server/internal/lsp/server_test.go`: black-box protocol tests through the public `Serve` seam.
- `scripts/build-server.sh`: builds the native sidecar into `bin/` for dev-extension testing.
- `examples/poc/main.go`: Go document used for manual Zed verification.
- `README.md`: dev build, dev-extension install, settings, and POC verification instructions.

### Task 1: Stdio LSP tracer bullet

**Files:**
- Create: `server/go.mod`
- Create: `server/cmd/go-implementation-lens-ls/main.go`
- Create: `server/internal/lsp/server.go`
- Test: `server/internal/lsp/server_test.go`

**Interfaces:**
- Produces: `func Serve(ctx context.Context, in io.Reader, out io.Writer) error`
- Produces: executable `go-implementation-lens-ls` speaking LSP Content-Length framing on stdin/stdout.

- [x] **Step 1: Write the failing initialize protocol test**

Send a framed `initialize` request through `Serve` and assert that response `id: 1` declares `capabilities.codeLensProvider.resolveProvider == false` and `textDocumentSync == 1`.

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lsp -run TestServeInitialize -v`

Expected: FAIL because `Serve` is not defined.

- [x] **Step 3: Implement minimal framing and initialize dispatch**

Read headers through `bufio.Reader`, require a valid non-negative `Content-Length`, decode one JSON-RPC object, and write a framed JSON-RPC response. Return protocol errors as JSON-RPC error objects without logging to stdout.

- [x] **Step 4: Run the initialize test**

Run: `go test ./internal/lsp -run TestServeInitialize -v`

Expected: PASS.

- [x] **Step 5: Write the failing CodeLens protocol test**

Send `textDocument/codeLens` for `file:///workspace/main.go` and assert exactly one Lens with:

```json
{
  "range": {"start":{"line":0,"character":0},"end":{"line":0,"character":0}},
  "command": {
    "title": "Go Implementation Lens: POC",
    "command": "editor.action.showReferences",
    "arguments": [
      "file:///workspace/main.go",
      {"line":0,"character":0},
      [{"uri":"file:///workspace/main.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}}}]
    ]
  }
}
```

- [x] **Step 6: Run the CodeLens test to verify it fails**

Run: `go test ./internal/lsp -run TestServeCodeLens -v`

Expected: FAIL because the method is not handled.

- [x] **Step 7: Implement the static CodeLens response and lifecycle methods**

Handle `initialized`, `textDocument/didOpen`, `textDocument/didChange`, `textDocument/codeLens`, `shutdown`, and `exit`. Ignore notifications that do not need responses and return LSP method-not-found for unsupported requests.

- [x] **Step 8: Run all Go tests**

Run: `go test ./...`

Expected: PASS.

- [x] **Step 9: Commit**

```bash
git add server
git commit -m "feat: add static CodeLens LSP server"
```

### Task 2: Zed language-server adapter

**Files:**
- Create: `extension.toml`
- Create: `Cargo.toml`
- Create: `src/lib.rs`
- Create: `scripts/build-server.sh`

**Interfaces:**
- Consumes: native executable `bin/go-implementation-lens-ls-aarch64-apple-darwin`.
- Produces: Zed language server id `go-implementation-lens` attached to language `Go` with language id `go`.

- [x] **Step 1: Add the extension manifest and Rust crate**

Register `[language_servers.go-implementation-lens]` for `languages = ["Go"]`, use extension schema 1 and Rust extension API 0.7.0. Zed language-server launch does not require an explicit extension capability in the source manifest.

- [x] **Step 2: Implement the development command adapter**

Implement `zed::Extension::language_server_command` and resolve the binary relative to the extension worktree. Return an actionable error containing the build script path when the binary is absent.

- [x] **Step 3: Add the deterministic development build script**

Build `./cmd/go-implementation-lens-ls` with `GOOS=darwin GOARCH=arm64` into `../bin/go-implementation-lens-ls-aarch64-apple-darwin` without downloading dependencies.

- [x] **Step 4: Verify both artifacts build**

Run: `./scripts/build-server.sh`

Expected: the native binary exists and exits only after receiving LSP `exit`.

Run: `cargo check --target wasm32-wasip2`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add extension.toml Cargo.toml src scripts
git commit -m "feat: register CodeLens sidecar with Zed"
```

### Task 3: Zed 1.14.2 end-to-end verification

**Files:**
- Create: `examples/poc/main.go`
- Create: `README.md`

**Interfaces:**
- Consumes: Zed dev-extension installer and the two artifacts from Tasks 1 and 2.
- Produces: documented, repeatable evidence that the auxiliary server coexists with gopls and its Lens navigates.

- [ ] **Step 1: Add the manual fixture and setup documentation**

Document these settings:

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

- [ ] **Step 2: Install the directory as a Zed dev extension**

Run `zed: install dev extension`, choose this repository, and open `examples/poc/main.go`.

- [ ] **Step 3: Verify the observable seams**

Confirm the static Lens appears, clicking it navigates to the document location, gopls diagnostics/completion still work, and switching `code_lens` between `on`, `menu`, and `off` behaves as Zed documents.

- [ ] **Step 4: Record exact Zed behavior and logs**

Add the verified Zed version, required settings, observed multi-target UI, and any adapter limitation to `README.md`. Do not claim untested platforms.

- [ ] **Step 5: Commit**

```bash
git add examples README.md
git commit -m "docs: verify CodeLens POC in Zed"
```

## POC Exit Criteria

- The LSP protocol tests pass without gopls or network access.
- Zed loads the dev extension and starts the native sidecar.
- The static CodeLens is visible and clickable in a Go file.
- gopls continues providing its existing Go functionality and CodeLens.
- The outcome is documented with the exact tested Zed version.
