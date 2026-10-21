package analysis

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

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
