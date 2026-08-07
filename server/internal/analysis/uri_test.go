package analysis

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPathToURIRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "source with spaces", "示例.go")
	uri, err := PathToURI(path)
	if err != nil {
		t.Fatalf("PathToURI() error = %v", err)
	}
	got, err := URIToPath(uri)
	if err != nil {
		t.Fatalf("URIToPath() error = %v", err)
	}
	if !samePath(got, path) {
		t.Fatalf("URI round trip = %q, want %q", got, path)
	}
}

func TestURIToPathRejectsNonFileAndOpaqueURIs(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{"https://example.com/source.go", "file:opaque"} {
		if _, err := URIToPath(uri); err == nil {
			t.Fatalf("URIToPath(%q) unexpectedly succeeded", uri)
		}
	}
}

func TestLocationResolverReadsSourceOnceAndUsesUTF16Columns(t *testing.T) {
	t.Parallel()

	const source = "package sample\n\nvar First int\nfunc use() { println(\"😀\", First) }\n"
	filename := filepath.Join(t.TempDir(), "sample.go")
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, filename, source, 0)
	if err != nil {
		t.Fatalf("parser.ParseFile() error = %v", err)
	}

	var identifiers []*ast.Ident
	ast.Inspect(parsed, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok && identifier.Name == "First" {
			identifiers = append(identifiers, identifier)
		}
		return true
	})
	if len(identifiers) != 2 {
		t.Fatalf("found %d First identifiers, want 2", len(identifiers))
	}

	resolver := newLocationResolver(nil)
	reads := 0
	resolver.readFile = func(path string) ([]byte, error) {
		reads++
		if path != filename {
			t.Fatalf("read path = %q, want %q", path, filename)
		}
		return []byte(source), nil
	}

	declaration, err := resolver.identLocation(fileSet, identifiers[0])
	if err != nil {
		t.Fatalf("resolve declaration: %v", err)
	}
	usage, err := resolver.identLocation(fileSet, identifiers[1])
	if err != nil {
		t.Fatalf("resolve usage: %v", err)
	}
	if reads != 1 {
		t.Fatalf("source reads = %d, want 1", reads)
	}
	if declaration.Range.Start != (Position{Line: 2, Character: 4}) {
		t.Fatalf("declaration start = %#v, want line 2 character 4", declaration.Range.Start)
	}
	if usage.Range.Start != (Position{Line: 3, Character: 27}) {
		t.Fatalf("usage start = %#v, want line 3 character 27", usage.Range.Start)
	}
	if declaration.URI != usage.URI {
		t.Fatalf("URIs differ: %q and %q", declaration.URI, usage.URI)
	}
}

func TestFindAnalysisScopeUsesContainingGoWork(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	appModule := filepath.Join(workspaceRoot, "app")
	contractModule := filepath.Join(workspaceRoot, "contract")
	writeAnalysisTestFile(t, filepath.Join(appModule, "go.mod"), "module example.com/app\n\ngo 1.26\n")
	writeAnalysisTestFile(t, filepath.Join(contractModule, "go.mod"), "module example.com/contract\n\ngo 1.26\n")
	writeAnalysisTestFile(t, filepath.Join(workspaceRoot, "go.work"), "go 1.26\n\nuse (\n\t./contract\n\t./app\n)\n")
	filename := filepath.Join(appModule, "app.go")
	writeAnalysisTestFile(t, filename, "package app\n")

	scope, err := FindAnalysisScope(filename, workspaceRoot)
	if err != nil {
		t.Fatalf("FindAnalysisScope() error = %v", err)
	}
	if scope.Root != workspaceRoot {
		t.Fatalf("scope.Root = %q, want %q", scope.Root, workspaceRoot)
	}
	if scope.WorkFile != filepath.Join(workspaceRoot, "go.work") {
		t.Fatalf("scope.WorkFile = %q", scope.WorkFile)
	}
	wantModules := []string{appModule, contractModule}
	if !reflect.DeepEqual(scope.ModuleRoots, wantModules) {
		t.Fatalf("scope.ModuleRoots = %#v, want %#v", scope.ModuleRoots, wantModules)
	}
	wantPatterns := []string{"./app/...", "./contract/..."}
	if !reflect.DeepEqual(scope.Patterns, wantPatterns) {
		t.Fatalf("scope.Patterns = %#v, want %#v", scope.Patterns, wantPatterns)
	}
}

func TestFindAnalysisScopeFallsBackForModuleNotUsedByGoWork(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	includedModule := filepath.Join(workspaceRoot, "included")
	unlistedModule := filepath.Join(workspaceRoot, "unlisted")
	writeAnalysisTestFile(t, filepath.Join(includedModule, "go.mod"), "module example.com/included\n\ngo 1.26\n")
	writeAnalysisTestFile(t, filepath.Join(unlistedModule, "go.mod"), "module example.com/unlisted\n\ngo 1.26\n")
	writeAnalysisTestFile(t, filepath.Join(workspaceRoot, "go.work"), "go 1.26\n\nuse ./included\n")
	filename := filepath.Join(unlistedModule, "unlisted.go")
	writeAnalysisTestFile(t, filename, "package unlisted\n")

	scope, err := FindAnalysisScope(filename, workspaceRoot)
	if err != nil {
		t.Fatalf("FindAnalysisScope() error = %v", err)
	}
	if scope.Root != unlistedModule || scope.WorkFile != "" {
		t.Fatalf("scope = %#v, want module-only scope rooted at %q", scope, unlistedModule)
	}
	if !reflect.DeepEqual(scope.ModuleRoots, []string{unlistedModule}) ||
		!reflect.DeepEqual(scope.Patterns, []string{"./..."}) {
		t.Fatalf("module-only scope = %#v", scope)
	}
}
