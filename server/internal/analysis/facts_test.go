package analysis

import (
	"context"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestExtractPackageFactsProducesStableLogicalIDsAcrossLoads(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/facts

go 1.26
`)
	productionFile := filepath.Join(moduleDir, "facts.go")
	writeAnalysisTestFile(t, productionFile, `package facts

type Box[T any] struct {
	Value T
}

type Reader interface {
	Read()
}

const Answer = 42
var Global Box[int]

func Make[U any](value U) U { return value }
func (Box[T]) Read() {}
`)
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "facts_test.go"), `package facts

import "testing"

func TestInternal(t *testing.T) {
	var reader Reader = Box[int]{}
	reader.Read()
}
`)
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "external_test.go"), `package facts_test

import (
	"testing"

	"example.com/facts"
)

func TestExternal(t *testing.T) {
	_ = facts.Make(facts.Answer)
}
`)

	first := loadPackageFactsForTest(t, moduleDir)
	second := loadPackageFactsForTest(t, moduleDir)
	firstIDs := logicalIDsForPackage(first, "example.com/facts")
	secondIDs := logicalIDsForPackage(second, "example.com/facts")
	if !reflect.DeepEqual(firstIDs, secondIDs) {
		t.Fatalf("logical symbol IDs changed across identical loads\nfirst:  %#v\nsecond: %#v", firstIDs, secondIDs)
	}

	want := []symbolID{
		newSymbolID("example.com/facts", "", "Answer", symbolConstant, 0),
		newSymbolID("example.com/facts", "", "Box", symbolType, 0),
		newSymbolID("example.com/facts", "", "Global", symbolVariable, 0),
		newSymbolID("example.com/facts", "", "Make", symbolFunction, 0),
		newSymbolID("example.com/facts", "", "Reader", symbolType, 0),
		newSymbolID("example.com/facts", "func:Make", "U", symbolTypeParameter, 0),
		newSymbolID("example.com/facts", "type:Box", "Read", symbolMethod, 0),
		newSymbolID("example.com/facts", "type:Box", "T", symbolTypeParameter, 0),
		newSymbolID("example.com/facts", "type:Box", "Value", symbolField, 0),
		newSymbolID("example.com/facts", "type:Reader", "Read", symbolMethod, 0),
	}
	for _, id := range want {
		if _, ok := firstIDs[id]; !ok {
			t.Errorf("stable declaration ID missing: %#v", id)
		}
	}

	productionURI, err := PathToURI(productionFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", productionFile, err)
	}
	boxID := newSymbolID("example.com/facts", "", "Box", symbolType, 0)
	var productionOwners []string
	for packageID, facts := range first {
		if facts.packagePath != "example.com/facts" || !factsContainDeclaration(facts, boxID, productionURI) {
			continue
		}
		productionOwners = append(productionOwners, packageID)
	}
	slices.Sort(productionOwners)
	if len(productionOwners) < 2 {
		t.Fatalf("production declaration owners = %q, want base and internal-test package variants", productionOwners)
	}
	if !slices.ContainsFunc(productionOwners, func(packageID string) bool {
		return strings.Contains(packageID, "[")
	}) {
		t.Fatalf("production declaration owners = %q, want an internal-test variant ID", productionOwners)
	}

	foundExternalPackage := false
	for _, facts := range first {
		if facts.packagePath == "example.com/facts_test" {
			foundExternalPackage = true
		}
	}
	if !foundExternalPackage {
		t.Fatal("external test facts are missing their distinct example.com/facts_test package path")
	}
}

func TestPackageFactTypesDoNotRetainLoaderSyntax(t *testing.T) {
	t.Parallel()

	forbidden := map[reflect.Type]string{
		reflect.TypeFor[*ast.File]():         "AST file",
		reflect.TypeFor[*token.FileSet]():    "token file set",
		reflect.TypeFor[*types.Info]():       "types info",
		reflect.TypeFor[*packages.Package](): "loaded package",
		reflect.TypeFor[*locationResolver](): "location resolver",
	}
	roots := []reflect.Type{
		reflect.TypeFor[packageFacts](),
		reflect.TypeFor[declarationFact](),
		reflect.TypeFor[directUseFact](),
		reflect.TypeFor[methodUseFact](),
		reflect.TypeFor[namedTypeFact](),
		reflect.TypeFor[interfaceFact](),
		reflect.TypeFor[methodFact](),
	}
	for _, root := range roots {
		assertFactTypeMemoryBoundary(t, root, root.Name(), forbidden, make(map[reflect.Type]struct{}))
	}
}

func loadPackageFactsForTest(t *testing.T, moduleDir string) map[string]*packageFacts {
	t.Helper()

	loaded, err := loadScopePackages(context.Background(), ModuleAnalysisScope(moduleDir), nil)
	if err != nil {
		t.Fatalf("loadScopePackages(%q) error = %v", moduleDir, err)
	}
	facts, err := extractPackageFacts(
		packagesForAnalysis(loaded.packages),
		newLocationResolver(loaded.sourceByPath),
	)
	if err != nil {
		t.Fatalf("extractPackageFacts(%q) error = %v", moduleDir, err)
	}
	return facts
}

func logicalIDsForPackage(factsByPackage map[string]*packageFacts, packagePath string) map[symbolID]struct{} {
	result := make(map[symbolID]struct{})
	for _, facts := range factsByPackage {
		if facts.packagePath != packagePath {
			continue
		}
		for _, declaration := range facts.declarations {
			result[declaration.id] = struct{}{}
		}
	}
	return result
}

func factsContainDeclaration(facts *packageFacts, id symbolID, uri string) bool {
	return slices.ContainsFunc(facts.declarations, func(declaration declarationFact) bool {
		return declaration.id == id && declaration.location.URI == uri
	})
}

func assertFactTypeMemoryBoundary(
	t *testing.T,
	typeToCheck reflect.Type,
	path string,
	forbidden map[reflect.Type]string,
	seen map[reflect.Type]struct{},
) {
	t.Helper()

	if description, blocked := forbidden[typeToCheck]; blocked {
		t.Errorf("%s retains forbidden %s through %s", path, description, typeToCheck)
		return
	}
	if typeToCheck.PkgPath() == "go/ast" {
		t.Errorf("%s retains forbidden AST type %s", path, typeToCheck)
		return
	}
	if typeToCheck.PkgPath() == "go/types" {
		// Exact Go relationship objects are the only deliberately retained
		// checker results. Their private implementation is outside this contract.
		return
	}
	if _, ok := seen[typeToCheck]; ok {
		return
	}
	seen[typeToCheck] = struct{}{}

	switch typeToCheck.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		if typeToCheck.Kind() == reflect.Slice && typeToCheck.Elem().Kind() == reflect.Uint8 {
			t.Errorf("%s retains source-like byte slice %s", path, typeToCheck)
			return
		}
		assertFactTypeMemoryBoundary(t, typeToCheck.Elem(), path+" -> "+typeToCheck.String(), forbidden, seen)
	case reflect.Map:
		assertFactTypeMemoryBoundary(t, typeToCheck.Key(), path+" key", forbidden, seen)
		assertFactTypeMemoryBoundary(t, typeToCheck.Elem(), path+" value", forbidden, seen)
	case reflect.Struct:
		for field := range typeToCheck.Fields() {
			assertFactTypeMemoryBoundary(t, field.Type, path+"."+field.Name, forbidden, seen)
		}
	}
}
