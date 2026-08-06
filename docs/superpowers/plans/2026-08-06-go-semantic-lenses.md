# Go Semantic Implementation Lenses Implementation Plan

**Goal:** Replace the static proof-of-concept Lens with accurate Go interface, named-type, interface-method, and concrete-method relationship lenses computed from the active module.

**Implementation status:** Completed and verified on 2026-08-06. The task-level commit snippets below document the intended boundaries; the finished slices were consolidated into one semantic milestone commit after end-to-end Zed verification.

**Architecture:** Keep `gopls` running independently and extend the auxiliary Go language server with a semantic engine built on `golang.org/x/tools/go/packages` and `go/types`. The language server tracks full-text editor overlays, loads the module containing the requested document, computes exact type and method relationships, caches the resulting snapshot until an LSP document or watched-file notification invalidates it, and converts relationships into Zed-compatible `editor.action.showReferences` CodeLens commands.

**Tech Stack:** Go 1.26, `golang.org/x/tools/go/packages` v0.44.0, `go/ast`, `go/types`, JSON-RPC 2.0/LSP over stdio, Zed extension API 0.7.0.

## Global Constraints

- Keep `gopls` enabled alongside `go-implementation-lens`; do not proxy or replace it.
- Test user-visible semantic behavior through framed stdio LSP requests and real temporary Go modules.
- Use `types.Implements` for full type/interface relationships; never infer a full implementation from method names alone.
- Treat either `T` or `*T` implementing an interface as a relationship displayed on the declaration of `T`.
- Match method lenses by method name and `types.Identical` signatures, including partial implementations, to preserve the useful method-level behavior of the VSCode extension without its signature false positives.
- Restrict relationship candidates to packages in the current Go module; imported dependencies provide type information but do not receive or contribute workspace Lens declarations.
- Suppress the universal implementation set of empty interfaces by reporting zero implementations and by not attaching empty-interface relationships to every concrete type.
- Return deterministic titles and target ordering by sorting names, URIs, lines, and characters.
- Convert Go byte offsets to LSP UTF-16 character offsets.
- Keep the native sidecar on the Zed worktree `PATH`; do not commit native binaries or `extension.wasm`.

---

## File Structure

- `server/internal/analysis/model.go`: semantic Lens, range, position, and location value types shared inside the Go server.
- `server/internal/analysis/uri.go`: `file:` URI conversion, module-root discovery, source lookup, and byte-offset-to-UTF-16 conversion.
- `server/internal/analysis/analyzer.go`: module loading, declaration collection, exact full-interface relationships, method signature relationships, sorting, and per-document Lens assembly.
- `server/internal/lsp/documents.go`: full-text LSP overlay store and generation counter.
- `server/internal/lsp/server.go`: stateful initialize/notification handling, semantic snapshot caching, and CodeLens JSON conversion.
- `server/internal/lsp/server_test.go`: protocol lifecycle and framing coverage retained from the POC.
- `server/internal/lsp/semantic_test.go`: end-to-end stdio semantic behavior using temporary Go modules.
- `server/go.mod` and `server/go.sum`: pinned `go/packages` dependency and transitive module checksums.
- `examples/poc/main.go`: manual interface, multiple implementation, pointer receiver, partial signature, and method Lens fixture.
- `README.md`: semantic behavior, architecture correction relative to `RESEARCH.md`, limitations, and verification commands.
- `extension.toml` and `Cargo.toml`: semantic milestone version `0.1.0`.

### Task 1: Exact interface and named-type lenses

**Files:**
- Create: `server/internal/analysis/model.go`
- Create: `server/internal/analysis/uri.go`
- Create: `server/internal/analysis/analyzer.go`
- Create: `server/internal/lsp/semantic_test.go`
- Modify: `server/internal/lsp/server.go`
- Modify: `server/internal/lsp/server_test.go`
- Modify: `server/go.mod`
- Create: `server/go.sum`

**Interfaces:**
- Produces: `analysis.AnalyzeModule(ctx context.Context, moduleDir string, overlays map[string][]byte) (analysis.Snapshot, error)`.
- Produces: `analysis.Snapshot.Lenses(uri string) []analysis.Lens`.
- Produces: `analysis.Lens{Range analysis.Range, Title string, Targets []analysis.Location}`.
- Consumes: LSP `initialize.rootUri` or the first `workspaceFolders[].uri` and `textDocument/codeLens.textDocument.uri`.

- [x] **Step 1: Write the failing stdio semantic test**

Create a temporary module containing:

```go
package sample

type Greeter interface {
	Greet() string
}

type Person struct{}

func (Person) Greet() string { return "hello" }

type Broken struct{}

func (Broken) Greet(prefix string) string { return prefix }
```

Send `initialize` with the temporary module URI, `initialized`, `textDocument/didOpen` with the same content, then `textDocument/codeLens`. Assert an interface Lens titled `1 implementation` targets `Person`, a type Lens titled `Implements: Greeter` targets `Greeter`, and no type Lens is attached to `Broken`.

- [x] **Step 2: Run the semantic test to verify red**

Run: `env TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test ./internal/lsp -run TestServeReturnsSemanticTypeLenses -v`

Expected: FAIL because the server still returns `Go Implementation Lens: POC` at the fixed fifth line.

- [x] **Step 3: Pin `go/packages` and define the semantic model**

Add:

```go
require golang.org/x/tools v0.44.0
```

Define integer `Position`, `Range`, `Location`, and `Lens` values plus a `Snapshot` keyed by canonical file URI. `Snapshot.Lenses` must return a defensive slice copy so cached state cannot be mutated by the LSP adapter.

- [x] **Step 4: Implement module loading and exact type relationships**

Load `./...` with `packages.LoadSyntax`, `Tests: false`, the active module directory, and absolute-path overlays. Collect named interface and non-interface type declarations from `Package.Syntax` and `Package.TypesInfo.Defs`. For every non-empty interface, add a full relationship when either:

```go
types.Implements(candidate, iface) || types.Implements(types.NewPointer(candidate), iface)
```

is true. Deduplicate pointer/value results and locations, then create interface count and concrete type `Implements: ...` lenses.

- [x] **Step 5: Convert semantic lenses to LSP CodeLens responses**

Parse initialize workspace roots, locate the closest `go.mod` at or below that root, call `AnalyzeModule`, and emit:

```json
{
  "range": {"start": {"line": 2, "character": 5}, "end": {"line": 2, "character": 12}},
  "command": {
    "title": "1 implementation",
    "command": "editor.action.showReferences",
    "arguments": ["file:///module/sample.go", {"line": 2, "character": 5}, [{"uri": "file:///module/sample.go", "range": {"start": {"line": 6, "character": 5}, "end": {"line": 6, "character": 11}}}]]
  }
}
```

Return an empty Lens array, not an LSP error, when the file is outside a Go module or the module has no usable typed packages. Return `-32602` only for malformed CodeLens params.

- [x] **Step 6: Run the semantic and lifecycle tests**

Run: `env TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test ./...`

Expected: PASS, with the old static POC assertion replaced by semantic assertions.

- [x] **Step 7: Commit the type relationship slice**

```bash
git add server
git commit -m "feat: compute Go type implementation lenses"
```

### Task 2: Cross-file, pointer-receiver, and method lenses

**Files:**
- Modify: `server/internal/analysis/analyzer.go`
- Modify: `server/internal/lsp/semantic_test.go`

**Interfaces:**
- Consumes: declaration and package snapshot types from Task 1.
- Produces: interface method count lenses and concrete method `Implements: Interface.Method` lenses using the same `analysis.Lens` contract.

- [x] **Step 1: Write the failing cross-file and method test**

Use two files in one temporary module. Put `Store` with `Put(string) error` and `Get(string) (string, error)` in `contract.go`. Put `*MemoryStore` with both exact methods, `PartialStore` with only `Put`, and `WrongStore.Get(string) error` in `store.go`. Assert:

- `Store` has exactly one full implementation target: `MemoryStore`.
- `MemoryStore` has `Implements: Store` despite pointer receivers.
- `Store.Put` has two exact method targets: `MemoryStore.Put` and `PartialStore.Put`.
- `Store.Get` has one target: `MemoryStore.Get`; `WrongStore.Get` is excluded by signature.
- `MemoryStore.Put` and `PartialStore.Put` both show `Implements: Store.Put`.
- Lens targets point into the correct cross-file declarations.

- [x] **Step 2: Run the method test to verify red**

Run: `env TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test ./internal/lsp -run TestServeReturnsCrossFileMethodLenses -v`

Expected: FAIL because Task 1 only emits named type/interface header lenses.

- [x] **Step 3: Collect explicit interface and receiver method declarations**

For named interface AST fields, map each named field to its `*types.Func` and the owning interface. For receiver `*ast.FuncDecl` values, resolve the receiver base `*types.Named`, method `*types.Func`, identifier range, and declaration location. Ignore functions without receivers and interface embeddings without an identifier declared on that line.

- [x] **Step 4: Build exact method relationships**

Relate an interface method declaration and concrete method declaration only when names match and:

```go
types.Identical(interfaceMethod.Type(), concreteMethod.Type())
```

is true. This relationship is independent of the full `types.Implements` result so a correctly matching method on a partial implementation remains discoverable, matching the useful VSCode behavior without accepting a wrong signature.

- [x] **Step 5: Emit and sort method lenses**

Emit `0 implementations`, `1 implementation`, or `N implementations` on every explicit interface method. Emit one concrete method Lens titled `Implements: A.Method, B.Method`, with qualified entries sorted by interface name and targets sorted by URI/range. Ensure interface header, interface method, type header, and concrete method lenses have stable ordering when they share a line.

- [x] **Step 6: Run all Go tests and commit**

Run: `env TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test ./...`

Expected: PASS.

```bash
git add server/internal/analysis server/internal/lsp/semantic_test.go
git commit -m "feat: add exact Go method implementation lenses"
```

### Task 3: Unsaved-buffer overlays and snapshot invalidation

**Files:**
- Create: `server/internal/lsp/documents.go`
- Modify: `server/internal/lsp/server.go`
- Modify: `server/internal/lsp/semantic_test.go`

**Interfaces:**
- Produces: `documentStore.Open(uri, text string)`, `Change(uri, text string)`, `Close(uri string)`, `Overlay(moduleDir string) map[string][]byte`, and `Generation() uint64`.
- Consumes: LSP full-sync `didOpen`, `didChange`, `didSave`, `didClose`, and `workspace/didChangeWatchedFiles` notifications.

- [x] **Step 1: Write the failing signature-change test**

Open an interface and exact implementation, request lenses, then send a full-text `didChange` that changes the interface method from `Greet() string` to `Greet(prefix string) string`. Request lenses again and assert the interface changes from `1 implementation` to `0 implementations`, the type Lens disappears, and the concrete method Lens disappears without writing the changed content to disk.

- [x] **Step 2: Run the overlay test to verify red**

Run: `env TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test ./internal/lsp -run TestServeInvalidatesSemanticLensesForUnsavedSignatureChange -v`

Expected: FAIL because Task 2 analyzes only saved files and reuses no editor document state.

- [x] **Step 3: Implement full-text document storage**

Store canonical absolute file paths and byte content from `didOpen` and the last content change in each full-sync `didChange`. Delete an overlay on `didClose`. Increment a generation counter only when effective overlay state changes. Filter overlays to the requested module directory before calling `packages.Load`.

- [x] **Step 4: Cache semantic snapshots by module and generation**

Keep one snapshot per module with the generation that produced it. Reuse it for repeated CodeLens requests while the generation matches. Invalidate all snapshots on `didSave` and `workspace/didChangeWatchedFiles`; invalidate the containing module on open/change/close. Never hold the cache mutex while `packages.Load` runs.

- [x] **Step 5: Run lifecycle, semantic, and race tests**

Run: `env TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test -race ./...`

Expected: PASS, including the unsaved signature change.

- [x] **Step 6: Commit overlay correctness**

```bash
git add server/internal/lsp
git commit -m "feat: refresh semantic lenses for unsaved Go changes"
```

### Task 4: Zed semantic fixture and end-to-end verification

**Files:**
- Modify: `examples/poc/main.go`
- Modify: `README.md`
- Modify: `extension.toml`
- Modify: `Cargo.toml`
- Modify: `server/internal/lsp/server.go`

**Interfaces:**
- Consumes: installed `go-implementation-lens-ls` and Zed dev-extension rebuild workflow.
- Produces: a documented Zed 1.14.2 semantic Lens verification with multiple navigation targets.

- [x] **Step 1: Expand the POC fixture**

Keep `Greeter`, add `Person` with a value receiver, `Robot` with a pointer receiver, and `BrokenGreeter` whose `Greet(int) string` signature is wrong. The expected header Lens is `2 implementations`; `Person` and `Robot` show `Implements: Greeter`; `BrokenGreeter` shows no type Lens; and `Greeter.Greet` has two method targets.

- [x] **Step 2: Bump the semantic milestone version**

Set the extension and Rust crate versions to `0.1.0` and return server info version `0.1.0` from initialize.

- [x] **Step 3: Run all build seams**

Run:

```bash
cd server
env TMPDIR=/tmp GOCACHE=/tmp/go-build-cache go test -race ./...
cd ..
cargo fmt -- --check
cargo build --target wasm32-wasip2
./scripts/install-server.sh
```

Expected: all commands pass and the installed sidecar hash matches the build artifact.

- [x] **Step 4: Rebuild and verify in Zed**

Run `zed: rebuild dev extension`, reload the workspace, and open `examples/poc/main.go`. Verify both language servers start, semantic titles appear at actual declaration ranges, interface and method Lenses expose both implementation targets through Zed's results UI, type Lenses navigate back to `Greeter`, the wrong-signature type has no Lens, and editing a signature updates the Lens without saving.

- [x] **Step 5: Document the verified semantic milestone**

Explain that the live architecture intentionally differs from the proxy recommendation in `RESEARCH.md`: the auxiliary server performs Go-native semantic analysis while `gopls` remains independent. Record module scope, empty/generic interface behavior, the supported platform, and the exact test commands without claiming cross-module or generic-interface support.

- [x] **Step 6: Commit the verified milestone**

```bash
git add examples/poc/main.go README.md extension.toml Cargo.toml server/internal/lsp/server.go
git commit -m "feat: ship semantic Go implementation lenses"
```

## Semantic Milestone Exit Criteria

- Static fixed-line Lens code and title are gone.
- Interface and interface-method counts come from exact typed declarations.
- Named types use exact full-interface conformance, including pointer receivers.
- Concrete methods require exact signatures and update on unsaved edits.
- Cross-file declarations in the current module navigate through real locations.
- Multiple targets use Zed's native `editor.action.showReferences` result UI.
- `gopls` starts independently and retains its existing Go features.
