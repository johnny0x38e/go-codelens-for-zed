package analysis

import (
	"context"
	"errors"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/testutil"
)

func TestDeclarationFingerprintIgnoresBodiesAndTracksAPI(t *testing.T) {
	t.Parallel()

	original := []byte(`package sample

	import "fmt"

	const inferred = 1

	type Box[T any] struct{ Value T }

	func (b Box[T]) Format(prefix string) string {
		return fmt.Sprint(prefix, b.Value)
	}
`)
	bodyEdit := []byte(`package sample

	import "fmt"

	const inferred = 1

	type Box[T any] struct{ Value T }

	func (b Box[T]) Format(prefix string) string {
		// Comments and all statements inside the body are deliberately ignored.
		return prefix
	}
`)
	signatureEdit := []byte(`package sample

	import "fmt"

	const inferred = 1

	type Box[T any] struct{ Value T }

	func (b Box[T]) Format(prefix []byte) string {
		return fmt.Sprint(prefix, b.Value)
	}
`)
	initializerEdit := []byte(`package sample

	import "fmt"

	const inferred = "one"

	type Box[T any] struct{ Value T }

	func (b Box[T]) Format(prefix string) string {
		return fmt.Sprint(prefix, b.Value)
	}
`)

	want, err := declarationFingerprint(original)
	if err != nil {
		t.Fatalf("declarationFingerprint(original) error = %v", err)
	}
	got, err := declarationFingerprint(bodyEdit)
	if err != nil {
		t.Fatalf("declarationFingerprint(body edit) error = %v", err)
	}
	if got != want {
		t.Fatalf("body-only fingerprint = %x, want %x", got, want)
	}
	for name, source := range map[string][]byte{
		"signature":   signatureEdit,
		"initializer": initializerEdit,
	} {
		fingerprint, fingerprintErr := declarationFingerprint(source)
		if fingerprintErr != nil {
			t.Fatalf("declarationFingerprint(%s edit) error = %v", name, fingerprintErr)
		}
		if fingerprint == want {
			t.Errorf("%s edit did not change declaration fingerprint %x", name, want)
		}
	}
}

func TestEngineBodyEditAvoidsFullLoadAndPreservesCanonicalTypes(t *testing.T) {
	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/body-recheck

go 1.26
`)
	contractFile := filepath.Join(moduleDir, "contract", "contract.go")
	writeAnalysisTestFile(t, contractFile, `package contract

type Greeter interface { Greet() string }

func First() {}
func Second() {}
`)
	implementationFile := filepath.Join(moduleDir, "implementation", "implementation.go")
	original := `package implementation

import "example.com/body-recheck/contract"

type Person struct{}
func (Person) Greet() string { return "hello" }

func Use() {
	contract.First()
	_ = Person{}.Greet()
}
`
	changed := `package implementation

import "example.com/body-recheck/contract"

type Person struct{}
func (Person) Greet() string { return "hello" }

func Use() {
	contract.Second()
	_ = Person{}.Greet()
}
`
	writeAnalysisTestFile(t, implementationFile, original)

	scope := ModuleAnalysisScope(moduleDir)
	engine := NewEngine()
	fullCalls := 0
	engine.analyze = func(
		ctx context.Context,
		scope AnalysisScope,
		overlays map[string][]byte,
	) (scopeAnalysis, error) {
		fullCalls++
		return analyzeScope(ctx, scope, overlays, true)
	}
	if _, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil); err != nil {
		t.Fatalf("prime Engine.Analyze() error = %v", err)
	}
	oldIndex := engine.states[scope.CacheKey()].index
	oldContract := findPackageStateForTest(t, oldIndex, "example.com/body-recheck/contract", "")
	oldImplementation := findPackageStateForTest(t, oldIndex, "example.com/body-recheck/implementation", "")
	oldDeclarations := oldImplementation.facts.declarations

	overlays := map[string][]byte{implementationFile: []byte(changed)}
	got, err := engine.Analyze(
		context.Background(),
		scope,
		NewChangeSet(Change{Path: implementationFile, Kind: ChangeContent}),
		overlays,
	)
	if err != nil {
		t.Fatalf("incremental Engine.Analyze() error = %v", err)
	}
	if fullCalls != 1 {
		t.Fatalf("full analysis calls after body edit = %d, want 1", fullCalls)
	}

	newIndex := engine.states[scope.CacheKey()].index
	newContract := findPackageStateForTest(t, newIndex, "example.com/body-recheck/contract", "")
	newImplementation := findPackageStateForTest(t, newIndex, "example.com/body-recheck/implementation", "")
	if newContract != oldContract {
		t.Fatal("body edit replaced an unrelated package state")
	}
	if newImplementation.types != oldImplementation.types {
		t.Fatal("body edit replaced the owning package's canonical types.Package")
	}
	if len(oldDeclarations) > 0 && &newImplementation.facts.declarations[0] != &oldDeclarations[0] {
		t.Fatal("body edit replaced declaration facts instead of only use facts")
	}

	want, err := AnalyzeScope(context.Background(), scope, overlays)
	if err != nil {
		t.Fatalf("fresh AnalyzeScope() error = %v", err)
	}
	assertSnapshotsEqual(t, got, want, []string{contractFile, implementationFile})

	shifted := []byte(`package implementation

import "example.com/body-recheck/contract"

// This body-only edit also moves every following declaration.

type Person struct{}
func (Person) Greet() string { return "hello" }

func Use() {
	contract.Second()
	_ = Person{}.Greet()
}
`)
	got, err = engine.Analyze(
		context.Background(),
		scope,
		NewChangeSet(Change{Path: implementationFile, Kind: ChangeContent}),
		map[string][]byte{implementationFile: shifted},
	)
	if err != nil {
		t.Fatalf("location-shifting Engine.Analyze() error = %v", err)
	}
	if fullCalls != 1 {
		t.Fatalf("full analysis calls after location-only edit = %d, want 1", fullCalls)
	}
	if shiftedState := findPackageStateForTest(
		t,
		engine.states[scope.CacheKey()].index,
		"example.com/body-recheck/implementation",
		"",
	); shiftedState.types != oldImplementation.types {
		t.Fatal("location-only edit replaced canonical types.Package")
	}
	want, err = AnalyzeScope(
		context.Background(),
		scope,
		map[string][]byte{implementationFile: shifted},
	)
	if err != nil {
		t.Fatalf("fresh location-shifting AnalyzeScope() error = %v", err)
	}
	assertSnapshotsEqual(t, got, want, []string{contractFile, implementationFile})

	softError := []byte(`package implementation

import "example.com/body-recheck/contract"

type Person struct{}
func (Person) Greet() string { return "hello" }

func Use() {
	contract.Missing()
	_ = Person{}.Greet()
}
`)
	got, err = engine.Analyze(
		context.Background(),
		scope,
		NewChangeSet(Change{Path: implementationFile, Kind: ChangeContent}),
		map[string][]byte{implementationFile: softError},
	)
	if err != nil {
		t.Fatalf("soft-error Engine.Analyze() error = %v", err)
	}
	if fullCalls != 1 {
		t.Fatalf("full analysis calls after soft type error = %d, want 1", fullCalls)
	}
	want, err = AnalyzeScope(
		context.Background(),
		scope,
		map[string][]byte{implementationFile: softError},
	)
	if err != nil {
		t.Fatalf("fresh soft-error AnalyzeScope() error = %v", err)
	}
	assertSnapshotsEqual(t, got, want, []string{contractFile, implementationFile})
}

func TestEngineAPIEditRechecksReverseClosureAndPreservesUnrelatedState(t *testing.T) {
	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/api-recheck

go 1.26
`)
	contractFile := filepath.Join(moduleDir, "contract", "contract.go")
	writeAnalysisTestFile(t, contractFile, `package contract

type Runner interface { Run() }
`)
	contractTestFile := filepath.Join(moduleDir, "contract", "contract_test.go")
	writeAnalysisTestFile(t, contractTestFile, `package contract

type testRunner struct{}
func (testRunner) Run() {}
`)
	consumerFile := filepath.Join(moduleDir, "consumer", "consumer.go")
	writeAnalysisTestFile(t, consumerFile, `package consumer

import "example.com/api-recheck/contract"

func Use(value contract.Runner) { value.Run() }
`)
	unrelatedFile := filepath.Join(moduleDir, "unrelated", "unrelated.go")
	writeAnalysisTestFile(t, unrelatedFile, `package unrelated

func Value() int { return 1 }
`)

	scope := ModuleAnalysisScope(moduleDir)
	engine := NewEngine()
	fullCalls := 0
	engine.analyze = func(
		ctx context.Context,
		scope AnalysisScope,
		overlays map[string][]byte,
	) (scopeAnalysis, error) {
		fullCalls++
		return analyzeScope(ctx, scope, overlays, true)
	}
	if _, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil); err != nil {
		t.Fatalf("prime Engine.Analyze() error = %v", err)
	}
	oldIndex := engine.states[scope.CacheKey()].index
	oldBase := findPackageStateForTest(t, oldIndex, "example.com/api-recheck/contract", "")
	oldInternal := findPackageStateForTest(
		t,
		oldIndex,
		"example.com/api-recheck/contract",
		"example.com/api-recheck/contract",
	)
	oldConsumer := findPackageStateForTest(t, oldIndex, "example.com/api-recheck/consumer", "")
	oldUnrelated := findPackageStateForTest(t, oldIndex, "example.com/api-recheck/unrelated", "")

	changed := []byte(`package contract

type Runner interface {
	Run(int)
}
`)
	overlays := map[string][]byte{contractFile: changed}
	got, err := engine.Analyze(
		context.Background(),
		scope,
		NewChangeSet(Change{Path: contractFile, Kind: ChangeContent}),
		overlays,
	)
	if err != nil {
		t.Fatalf("incremental Engine.Analyze() error = %v", err)
	}
	if fullCalls != 1 {
		t.Fatalf("full analysis calls after API edit = %d, want 1", fullCalls)
	}

	newIndex := engine.states[scope.CacheKey()].index
	newBase := findPackageStateForTest(t, newIndex, "example.com/api-recheck/contract", "")
	newInternal := findPackageStateForTest(
		t,
		newIndex,
		"example.com/api-recheck/contract",
		"example.com/api-recheck/contract",
	)
	newConsumer := findPackageStateForTest(t, newIndex, "example.com/api-recheck/consumer", "")
	newUnrelated := findPackageStateForTest(t, newIndex, "example.com/api-recheck/unrelated", "")
	for name, pair := range map[string][2]*packageState{
		"base":     {oldBase, newBase},
		"internal": {oldInternal, newInternal},
		"consumer": {oldConsumer, newConsumer},
	} {
		if pair[0] == pair[1] || pair[0].types == pair[1].types {
			t.Errorf("API edit did not replace %s package state and type universe", name)
		}
	}
	if newUnrelated != oldUnrelated {
		t.Fatal("API edit replaced an unrelated package state")
	}

	want, err := AnalyzeScope(context.Background(), scope, overlays)
	if err != nil {
		t.Fatalf("fresh AnalyzeScope() error = %v", err)
	}
	assertSnapshotsEqual(
		t,
		got,
		want,
		[]string{contractFile, contractTestFile, consumerFile, unrelatedFile},
	)
}

func TestEngineStructureEditUsesFullFallback(t *testing.T) {
	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/structure-recheck

go 1.26
`)
	sourceFile := filepath.Join(moduleDir, "sample.go")
	original := []byte("package sample\n\nfunc Value() int { return 1 }\n")
	writeAnalysisTestFile(t, sourceFile, string(original))

	scope := ModuleAnalysisScope(moduleDir)
	engine := NewEngine()
	fullCalls := 0
	engine.analyze = func(
		ctx context.Context,
		scope AnalysisScope,
		overlays map[string][]byte,
	) (scopeAnalysis, error) {
		fullCalls++
		return analyzeScope(ctx, scope, overlays, true)
	}
	if _, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil); err != nil {
		t.Fatalf("prime Engine.Analyze() error = %v", err)
	}

	changed := append([]byte("//go:build !never\n\n"), original...)
	if _, err := engine.Analyze(
		context.Background(),
		scope,
		NewChangeSet(Change{Path: sourceFile, Kind: ChangeContent}),
		map[string][]byte{sourceFile: changed},
	); err != nil {
		t.Fatalf("fallback Engine.Analyze() error = %v", err)
	}
	if fullCalls != 2 {
		t.Fatalf("full analysis calls after structure edit = %d, want 2", fullCalls)
	}
}

func TestEngineNewMethodIdentityUsesFullFallback(t *testing.T) {
	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/new-method-recheck

go 1.26
`)
	sourceFile := filepath.Join(moduleDir, "sample.go")
	writeAnalysisTestFile(t, sourceFile, `package sample

type Runner interface { Run() }
`)

	scope := ModuleAnalysisScope(moduleDir)
	engine := NewEngine()
	fullCalls := 0
	engine.analyze = func(
		ctx context.Context,
		scope AnalysisScope,
		overlays map[string][]byte,
	) (scopeAnalysis, error) {
		fullCalls++
		return analyzeScope(ctx, scope, overlays, true)
	}
	if _, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil); err != nil {
		t.Fatalf("prime Engine.Analyze() error = %v", err)
	}
	changed := []byte(`package sample

type Runner interface {
	Run()
	Stop()
}
`)
	if _, err := engine.Analyze(
		context.Background(),
		scope,
		NewChangeSet(Change{Path: sourceFile, Kind: ChangeContent}),
		map[string][]byte{sourceFile: changed},
	); err != nil {
		t.Fatalf("new-method Engine.Analyze() error = %v", err)
	}
	if fullCalls != 2 {
		t.Fatalf("full analysis calls after new method identity = %d, want 2", fullCalls)
	}
}

func TestRecheckSourcesReadOnlyOwningCompiledFiles(t *testing.T) {
	t.Parallel()

	fixture := testutil.WriteModule(t, testutil.ModuleSpec{
		ModulePath:          "example.com/recheck-reads",
		PackageCount:        2,
		FilesPerPackage:     2,
		DeclarationsPerFile: 1,
	})
	index := loadScopeIndexForTest(t, ModuleAnalysisScope(fixture.Root))
	plan := index.reloadPlanFor(NewChangeSet(Change{
		Path: fixture.EntryFile,
		Kind: ChangeContent,
	}))
	original, err := os.ReadFile(fixture.EntryFile)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", fixture.EntryFile, err)
	}
	sources := &recheckSourceSet{
		fileSet:      token.NewFileSet(),
		overlays:     map[string][]byte{canonicalChangePath(fixture.EntryFile): original},
		files:        make(map[string]*ast.File),
		fingerprints: make(map[string]sourceFingerprint),
		sourceByPath: make(map[string][]byte),
	}
	var reads []string
	sources.readFile = func(path string) ([]byte, error) {
		reads = append(reads, path)
		// #nosec G304 -- every path comes from the successfully loaded package graph.
		return os.ReadFile(path)
	}
	if err := sources.ensurePackages(context.Background(), index, plan.packageIDs); err != nil {
		t.Fatalf("ensurePackages() error = %v", err)
	}

	wantReads := make([]string, 0)
	wanted := make(map[string]struct{})
	for _, packageID := range plan.packageIDs {
		for _, file := range index.packages[packageID].files {
			if file.path != canonicalChangePath(fixture.EntryFile) {
				wanted[file.path] = struct{}{}
			}
		}
	}
	for path := range wanted {
		wantReads = append(wantReads, path)
	}
	slices.Sort(wantReads)
	if !reflect.DeepEqual(reads, wantReads) {
		t.Fatalf("incremental file reads = %q, want owning compiled files %q", reads, wantReads)
	}
	for _, unrelated := range fixture.PackageFiles[2:] {
		if slices.Contains(reads, canonicalChangePath(unrelated)) {
			t.Fatalf("incremental recheck read unrelated file %q", unrelated)
		}
	}
}

func TestRecheckPackagesHonorsCancellation(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/cancel-recheck

go 1.26
`)
	sourceFile := filepath.Join(moduleDir, "sample.go")
	writeAnalysisTestFile(t, sourceFile, "package sample\n\nfunc Value() int { return 1 }\n")
	index := loadScopeIndexForTest(t, ModuleAnalysisScope(moduleDir))
	plan := index.reloadPlanFor(NewChangeSet(Change{Path: sourceFile, Kind: ChangeContent}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := recheckPackages(ctx, index, plan, map[string][]byte{
		sourceFile: []byte("package sample\n\nfunc Value() int { return 2 }\n"),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("recheckPackages() error = %v, want context.Canceled", err)
	}
}

func TestSourceStructureFingerprintTracksPackageBuildAndImports(t *testing.T) {
	t.Parallel()

	base, err := sourceFingerprintForBytes([]byte(`package sample

import alias "example.com/dependency"
`))
	if err != nil {
		t.Fatalf("sourceFingerprintForBytes(base) error = %v", err)
	}
	aliasOnly, err := sourceFingerprintForBytes([]byte(`package sample

import renamed "example.com/dependency"
`))
	if err != nil {
		t.Fatalf("sourceFingerprintForBytes(alias edit) error = %v", err)
	}
	if aliasOnly.structure != base.structure {
		t.Fatal("import alias edit changed dependency-structure fingerprint")
	}
	for name, source := range map[string][]byte{
		"package": []byte("package renamed\n"),
		"build":   []byte("//go:build darwin\n\npackage sample\n"),
		"import": []byte(`package sample

import "example.com/other"
`),
	} {
		fingerprint, fingerprintErr := sourceFingerprintForBytes(source)
		if fingerprintErr != nil {
			t.Fatalf("sourceFingerprintForBytes(%s edit) error = %v", name, fingerprintErr)
		}
		if fingerprint.structure == base.structure {
			t.Errorf("%s edit did not change structure fingerprint", name)
		}
	}
}

func TestPackageFileStateDoesNotRetainSource(t *testing.T) {
	t.Parallel()

	if reflect.TypeFor[packageFileState]().NumField() == 0 {
		t.Fatal("packageFileState unexpectedly has no metadata fields")
	}
	for field := range reflect.TypeFor[packageFileState]().Fields() {
		if field.Type == reflect.TypeFor[[]byte]() {
			t.Fatalf("packageFileState field %q retains source bytes", field.Name)
		}
	}
}
