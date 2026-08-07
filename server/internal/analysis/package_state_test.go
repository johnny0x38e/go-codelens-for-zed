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

	"github.com/johnny/go-codelens-for-zed/server/internal/testutil"
	"golang.org/x/tools/go/packages"
)

func TestScopeIndexOwnsNormalInternalAndExternalTestFiles(t *testing.T) {
	t.Parallel()

	workspace := testutil.WriteWorkspace(t, []testutil.ModuleSpec{
		{
			ModulePath:          "example.com/index-primary",
			PackageCount:        2,
			FilesPerPackage:     1,
			DeclarationsPerFile: 1,
			IncludeTests:        true,
		},
		{
			ModulePath:          "example.com/index-independent",
			PackageCount:        1,
			FilesPerPackage:     1,
			DeclarationsPerFile: 1,
			IncludeTests:        true,
		},
	})
	scope, err := FindAnalysisScope(workspace.Modules[0].EntryFile, workspace.Root)
	if err != nil {
		t.Fatalf("FindAnalysisScope() error = %v", err)
	}
	index := loadScopeIndexForTest(t, scope)

	const basePath = "example.com/index-primary/pkg000"
	base := findPackageStateForTest(t, index, basePath, "")
	internal := findPackageStateForTest(t, index, basePath, basePath)
	external := findPackageStateForTest(t, index, basePath+"_test", basePath)
	if base.goVersion != "1.26" || internal.goVersion != "1.26" || external.goVersion != "1.26" {
		t.Fatalf(
			"package Go versions = %q, %q, %q; want module metadata 1.26",
			base.goVersion,
			internal.goVersion,
			external.goVersion,
		)
	}
	wantPattern := scope.Patterns[0]
	scope.Patterns[0] = "mutated"
	if index.scope.Patterns[0] != wantPattern {
		t.Fatalf("scope index retained caller-owned patterns: %q", index.scope.Patterns)
	}
	module := workspace.Modules[0]
	assertFileOwnersForTest(t, index, module.PackageFiles[0], []string{base.id, internal.id})
	assertFileOwnersForTest(t, index, module.PackageFiles[1], []string{internal.id})
	assertFileOwnersForTest(t, index, module.PackageFiles[2], []string{external.id})

	for _, packagePath := range []string{
		"example.com/index-primary/pkg000",
		"example.com/index-primary/pkg001",
		"example.com/index-independent/pkg000",
	} {
		if !scopeIndexContainsPackagePath(index, packagePath) {
			t.Errorf("scope index does not contain package path %q", packagePath)
		}
	}

	dependent := findPackageStateForTest(t, index, "example.com/index-primary/pkg001", "")
	closure := index.reverseClosure([]string{base.id})
	if !slices.Contains(closure, dependent.id) {
		t.Fatalf("reverse closure for %q = %q, want dependent %q", base.id, closure, dependent.id)
	}
	if slices.Index(closure, base.id) > slices.Index(closure, dependent.id) {
		t.Fatalf("reverse closure is not dependency-first: %q", closure)
	}
	for _, packageID := range closure {
		state := index.packages[packageID]
		if state != nil && strings.HasPrefix(state.packagePath, "example.com/index-independent/") {
			t.Fatalf("reverse closure contains unrelated package %q", packageID)
		}
	}
	if second := index.reverseClosure([]string{base.id}); !reflect.DeepEqual(second, closure) {
		t.Fatalf("reverse closure changed between calls: %q then %q", closure, second)
	}
}

func TestPackageImporterUsesCanonicalLoadedTypeGraph(t *testing.T) {
	t.Parallel()

	fixture := testutil.WriteModule(t, testutil.ModuleSpec{
		ModulePath:          "example.com/importer",
		PackageCount:        2,
		FilesPerPackage:     1,
		DeclarationsPerFile: 1,
		IncludeTests:        true,
	})
	index := loadScopeIndexForTest(t, ModuleAnalysisScope(fixture.Root))
	const packagePath = "example.com/importer/pkg000"
	base := findPackageStateForTest(t, index, packagePath, "")
	internal := findPackageStateForTest(t, index, packagePath, packagePath)
	if base.types == internal.types {
		t.Fatal("base and internal-test variants unexpectedly share one types.Package")
	}

	importer := index.importer()
	got, err := importer.Import(packagePath)
	if err != nil {
		t.Fatalf("Import(%q) error = %v", packagePath, err)
	}
	if got != base.types {
		t.Fatalf("Import(%q) = %p, want canonical base package %p", packagePath, got, base.types)
	}
	from, err := importer.ImportFrom(packagePath, fixture.Root, 0)
	if err != nil {
		t.Fatalf("ImportFrom(%q) error = %v", packagePath, err)
	}
	if from != base.types {
		t.Fatalf("ImportFrom(%q) = %p, want %p", packagePath, from, base.types)
	}
	if testingPackage, importErr := importer.Import("testing"); importErr != nil || testingPackage == nil {
		t.Fatalf("Import(\"testing\") = %p, %v; want retained standard-library package", testingPackage, importErr)
	}
	if _, importErr := importer.Import("example.com/missing"); importErr == nil {
		t.Fatal("Import() unexpectedly used an implicit disk fallback")
	}
}

func TestReloadPlanMapsContentToExactOwnersAndFallsBackConservatively(t *testing.T) {
	t.Parallel()

	fixture := testutil.WriteModule(t, testutil.ModuleSpec{
		ModulePath:          "example.com/reload-plan",
		PackageCount:        1,
		FilesPerPackage:     1,
		DeclarationsPerFile: 1,
		IncludeTests:        true,
	})
	index := loadScopeIndexForTest(t, ModuleAnalysisScope(fixture.Root))

	for _, path := range fixture.PackageFiles {
		plan := index.reloadPlanFor(NewChangeSet(Change{Path: path, Kind: ChangeContent}))
		if plan.full {
			t.Fatalf("content plan for known file %q requested full reload", path)
		}
		if want := index.fileOwners[canonicalChangePath(path)]; !reflect.DeepEqual(plan.packageIDs, want) {
			t.Fatalf("content plan for %q = %q, want owners %q", path, plan.packageIDs, want)
		}
	}
	if plan := index.reloadPlanFor(ChangeSet{}); plan.full || len(plan.packageIDs) != 0 {
		t.Fatalf("empty reload plan = %#v, want zero plan", plan)
	}

	unknown := filepath.Join(fixture.Root, "unknown.go")
	for _, change := range []Change{
		{Path: fixture.EntryFile, Kind: ChangeCreated},
		{Path: fixture.EntryFile, Kind: ChangeDeleted},
		{Path: filepath.Join(fixture.Root, "go.mod"), Kind: ChangeMetadata},
		{Path: unknown, Kind: ChangeContent},
	} {
		if plan := index.reloadPlanFor(NewChangeSet(change)); !plan.full {
			t.Errorf("reload plan for %#v = %#v, want full fallback", change, plan)
		}
	}
}

func TestScopeIndexReplacementRejectsMixedTypeUniverses(t *testing.T) {
	t.Parallel()

	oldDependencyTypes := types.NewPackage("example.com/dependency", "dependency")
	oldDependentTypes := types.NewPackage("example.com/dependent", "dependent")
	unrelatedTypes := types.NewPackage("example.com/unrelated", "unrelated")
	oldDependentTypes.SetImports([]*types.Package{oldDependencyTypes})
	oldDependencyFacts := &packageFacts{packageID: "dependency", packagePath: oldDependencyTypes.Path()}
	oldDependentFacts := &packageFacts{packageID: "dependent", packagePath: oldDependentTypes.Path()}
	dependency := &packageState{
		id:          "dependency",
		packagePath: oldDependencyTypes.Path(),
		name:        oldDependencyTypes.Name(),
		types:       oldDependencyTypes,
		facts:       oldDependencyFacts,
	}
	dependent := &packageState{
		id:          "dependent",
		packagePath: oldDependentTypes.Path(),
		name:        oldDependentTypes.Name(),
		imports:     map[string]string{oldDependencyTypes.Path(): dependency.id},
		types:       oldDependentTypes,
		facts:       oldDependentFacts,
	}
	unrelated := &packageState{
		id:          "unrelated",
		packagePath: unrelatedTypes.Path(),
		name:        unrelatedTypes.Name(),
		types:       unrelatedTypes,
		facts:       &packageFacts{packageID: "unrelated", packagePath: unrelatedTypes.Path()},
	}
	index, err := newScopeIndexFromStates(AnalysisScope{Root: t.TempDir()}, map[string]*packageState{
		dependency.id: dependency,
		dependent.id:  dependent,
		unrelated.id:  unrelated,
	})
	if err != nil {
		t.Fatalf("newScopeIndexFromStates() error = %v", err)
	}

	newDependencyTypes := types.NewPackage(oldDependencyTypes.Path(), oldDependencyTypes.Name())
	newDependencyFacts := &packageFacts{packageID: dependency.id, packagePath: newDependencyTypes.Path()}
	newDependency := &packageState{
		id:          dependency.id,
		packagePath: newDependencyTypes.Path(),
		name:        newDependencyTypes.Name(),
		types:       newDependencyTypes,
		facts:       newDependencyFacts,
	}
	if _, replaceErr := index.withPackageStates(map[string]*packageState{dependency.id: newDependency}); replaceErr == nil {
		t.Fatal("withPackageStates() accepted a dependent that still imports the old type universe")
	}

	newDependentTypes := types.NewPackage(oldDependentTypes.Path(), oldDependentTypes.Name())
	newDependentTypes.SetImports([]*types.Package{newDependencyTypes})
	newDependent := &packageState{
		id:          dependent.id,
		packagePath: newDependentTypes.Path(),
		name:        newDependentTypes.Name(),
		imports:     map[string]string{newDependencyTypes.Path(): newDependency.id},
		types:       newDependentTypes,
		facts:       &packageFacts{packageID: dependent.id, packagePath: newDependentTypes.Path()},
	}
	replaced, err := index.withPackageStates(map[string]*packageState{
		newDependency.id: newDependency,
		newDependent.id:  newDependent,
	})
	if err != nil {
		t.Fatalf("coherent withPackageStates() error = %v", err)
	}
	if replaced.packages[dependency.id] != newDependency || replaced.packages[dependent.id] != newDependent {
		t.Fatalf("replacement did not retain the supplied package states: %#v", replaced.packages)
	}
	if index.packages[dependency.id] != dependency || index.packages[dependent.id] != dependent {
		t.Fatal("replacement mutated the previous immutable scope index")
	}
	if replaced.packages[unrelated.id] != unrelated {
		t.Fatal("replacement did not preserve the unrelated package state pointer")
	}
	for _, retained := range replaced.importTypes {
		if retained == oldDependencyTypes || retained == oldDependentTypes {
			t.Fatalf("replacement importer still retains old types.Package %p", retained)
		}
	}
	for _, state := range replaced.packages {
		if state.facts == oldDependencyFacts || state.facts == oldDependentFacts {
			t.Fatalf("replacement still retains old package facts %p", state.facts)
		}
	}
}

func TestPackageStateTypesDoNotRetainLoaderSyntax(t *testing.T) {
	t.Parallel()

	forbidden := map[reflect.Type]string{
		reflect.TypeFor[*ast.File]():         "AST file",
		reflect.TypeFor[*token.FileSet]():    "token file set",
		reflect.TypeFor[*types.Info]():       "types info",
		reflect.TypeFor[*packages.Package](): "loaded package",
		reflect.TypeFor[*locationResolver](): "location resolver",
	}
	for _, root := range []reflect.Type{
		reflect.TypeFor[packageState](),
		reflect.TypeFor[scopeIndex](),
		reflect.TypeFor[reloadPlan](),
		reflect.TypeFor[packageImporter](),
	} {
		assertFactTypeMemoryBoundary(t, root, root.Name(), forbidden, make(map[reflect.Type]struct{}))
	}
}

func loadScopeIndexForTest(t *testing.T, scope AnalysisScope) *scopeIndex {
	t.Helper()

	loaded, err := loadScopePackages(context.Background(), scope, nil)
	if err != nil {
		t.Fatalf("loadScopePackages() error = %v", err)
	}
	packagesToInspect := packagesForAnalysis(loaded.packages)
	facts, err := extractPackageFacts(packagesToInspect, newLocationResolver(loaded.sourceByPath))
	if err != nil {
		t.Fatalf("extractPackageFacts() error = %v", err)
	}
	index, err := buildScopeIndex(scope, packagesToInspect, facts, loaded.sourceByPath)
	if err != nil {
		t.Fatalf("buildScopeIndex() error = %v", err)
	}
	return index
}

func findPackageStateForTest(
	t *testing.T,
	index *scopeIndex,
	packagePath string,
	forTest string,
) *packageState {
	t.Helper()

	var matches []*packageState
	for _, state := range index.packages {
		if state.packagePath == packagePath && state.forTest == forTest {
			matches = append(matches, state)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("package states for path %q, forTest %q = %#v, want one", packagePath, forTest, matches)
	}
	return matches[0]
}

func assertFileOwnersForTest(t *testing.T, index *scopeIndex, path string, want []string) {
	t.Helper()

	want = append([]string(nil), want...)
	slices.Sort(want)
	if got := index.fileOwners[canonicalChangePath(path)]; !reflect.DeepEqual(got, want) {
		t.Fatalf("file owners for %q = %q, want %q", path, got, want)
	}
}

func scopeIndexContainsPackagePath(index *scopeIndex, packagePath string) bool {
	for _, state := range index.packages {
		if state.packagePath == packagePath {
			return true
		}
	}
	return false
}
