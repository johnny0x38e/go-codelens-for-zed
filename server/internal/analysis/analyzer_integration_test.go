package analysis

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/testutil"
)

func TestAnalyzeModuleReturnsLensesForRepositoryFixture(t *testing.T) {
	t.Parallel()

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test filename")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", ".."))
	moduleDir := filepath.Join(repositoryRoot, "examples", "poc")
	mainFile := filepath.Join(moduleDir, "main.go")
	mainURI, err := PathToURI(mainFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", mainFile, err)
	}

	snapshot, err := AnalyzeModule(context.Background(), moduleDir, nil)
	if err != nil {
		t.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
	}
	lenses := snapshot.Lenses(mainURI)
	assertAnalysisLens(t, lenses, "2 implementations", 4)
	assertAnalysisLens(t, lenses, "1 reference", 4)
	assertAnalysisLens(t, lenses, "1 reference", 5)
	assertAnalysisLens(t, lenses, "Implements: Greeter", 8)
	assertAnalysisLens(t, lenses, "2 references", 8)
	assertAnalysisLens(t, lenses, "2 references", 9)
	assertAnalysisLens(t, lenses, "1 reference", 12)
	assertAnalysisLens(t, lenses, "Implements: Greeter", 16)
	assertAnalysisLens(t, lenses, "1 reference", 20)
	assertAnalysisLens(t, lenses, "2 references", 30)
	assertAnalysisLens(t, lenses, "0 references", 34)
}

func TestAnalyzeModuleCountsCrossPackageReferences(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/sample

go 1.26
`)
	contractFile := filepath.Join(moduleDir, "contract", "contract.go")
	writeAnalysisTestFile(t, contractFile, `package contract

type Greeter interface {
	Greet() string
}

func Welcome(g Greeter) string { return g.Greet() }
`)
	appFile := filepath.Join(moduleDir, "app", "app.go")
	writeAnalysisTestFile(t, appFile, `package app

import "example.com/sample/contract"

type Person struct{}

func (Person) Greet() string { return "hi" }

func Use() string {
	var greeter contract.Greeter = Person{}
	return contract.Welcome(greeter)
}
`)

	contractURI, err := PathToURI(contractFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", contractFile, err)
	}
	appURI, err := PathToURI(appFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", appFile, err)
	}
	snapshot, err := AnalyzeModule(context.Background(), moduleDir, nil)
	if err != nil {
		t.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
	}

	assertAnalysisLens(t, snapshot.Lenses(contractURI), "2 references", 2)
	assertAnalysisLens(t, snapshot.Lenses(contractURI), "1 reference", 3)
	assertAnalysisLens(t, snapshot.Lenses(contractURI), "1 reference", 6)
	assertAnalysisLens(t, snapshot.Lenses(appURI), "2 references", 4)
}

func TestAnalyzeModuleMatchesGoplsMethodReferenceFamilies(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/sample

go 1.26
`)
	sourceFile := filepath.Join(moduleDir, "sample.go")
	writeAnalysisTestFile(t, sourceFile, `package sample

type Narrow interface {
	Greet() string
}

type Wide interface {
	Greet() string
	Other()
}

type Partial struct{}
func (Partial) Greet() string { return "partial" }

type Full struct{}
func (Full) Greet() string { return "full" }
func (Full) Other() {}

func ViaNarrow(value Narrow) string { return value.Greet() }
func ViaWide(value Wide) string { return value.Greet() }
func DirectPartial(value Partial) string { return value.Greet() }
func DirectFull(value Full) string { return value.Greet() }
`)

	sourceURI, err := PathToURI(sourceFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", sourceFile, err)
	}
	snapshot, err := AnalyzeModule(context.Background(), moduleDir, nil)
	if err != nil {
		t.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
	}
	lenses := snapshot.Lenses(sourceURI)

	assertAnalysisLens(t, lenses, "4 references", 3)
	assertAnalysisLens(t, lenses, "3 references", 7)
	assertAnalysisLens(t, lenses, "2 references", 12)
	assertAnalysisLens(t, lenses, "3 references", 15)
}

func TestAnalyzeModuleListsEveryImplementedInterface(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/sample

go 1.26
`)
	sourceFile := filepath.Join(moduleDir, "sample.go")
	writeAnalysisTestFile(t, sourceFile, `package sample

type Alpha interface {
	Run()
}

type Beta interface {
	Run()
}

type Worker struct{}

func (Worker) Run() {}
`)

	sourceURI, err := PathToURI(sourceFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", sourceFile, err)
	}
	snapshot, err := AnalyzeModule(context.Background(), moduleDir, nil)
	if err != nil {
		t.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
	}
	lenses := snapshot.Lenses(sourceURI)

	assertAnalysisLens(t, lenses, "Implements: Alpha, Beta", 10)
	assertAnalysisLens(t, lenses, "Implements: Alpha.Run, Beta.Run", 12)
}

func TestAnalyzeModuleDoesNotMatchUnrelatedMethodIdentities(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/sample

go 1.26
`)
	sourceFile := filepath.Join(moduleDir, "sample.go")
	writeAnalysisTestFile(t, sourceFile, `package sample

type Runner interface { Run() }
type Worker struct{}
func (Worker) Run() {}
func (Worker) Stop() {}
func (Worker) Pause() {}
`)

	sourceURI, err := PathToURI(sourceFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", sourceFile, err)
	}
	snapshot, err := AnalyzeModule(context.Background(), moduleDir, nil)
	if err != nil {
		t.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
	}
	lenses := snapshot.Lenses(sourceURI)

	assertAnalysisLens(t, lenses, "1 implementation", 2)
	assertAnalysisLens(t, lenses, "Implements: Runner.Run", 4)
}

func TestAnalyzeModuleIncludesTestsWithoutDuplicateProductionResults(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/sample

go 1.26
`)
	contractFile := filepath.Join(moduleDir, "contract.go")
	writeAnalysisTestFile(t, contractFile, `package sample

type Greeter interface {
	Greet() string
}

type ProductionGreeter struct{}

func (ProductionGreeter) Greet() string { return "production" }

func Welcome(g Greeter) string { return g.Greet() }
`)
	internalTestFile := filepath.Join(moduleDir, "contract_test.go")
	writeAnalysisTestFile(t, internalTestFile, `package sample

import "testing"

type internalGreeter struct{}

func (internalGreeter) Greet() string { return "internal" }

func TestInternal(t *testing.T) {
	_ = Welcome(internalGreeter{})
}
`)
	externalTestFile := filepath.Join(moduleDir, "external_test.go")
	writeAnalysisTestFile(t, externalTestFile, `package sample_test

import (
	"testing"

	sample "example.com/sample"
)

type externalGreeter struct{}

func (externalGreeter) Greet() string { return "external" }

func TestExternal(t *testing.T) {
	_ = sample.Welcome(externalGreeter{})
}
`)

	contractURI, err := PathToURI(contractFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", contractFile, err)
	}
	internalTestURI, err := PathToURI(internalTestFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", internalTestFile, err)
	}
	externalTestURI, err := PathToURI(externalTestFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", externalTestFile, err)
	}

	snapshot, err := AnalyzeModule(context.Background(), moduleDir, nil)
	if err != nil {
		t.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
	}
	contractLenses := snapshot.Lenses(contractURI)
	greeterImplementations := requireAnalysisLens(t, contractLenses, "3 implementations", 2)
	if len(greeterImplementations.Targets) != 3 {
		t.Fatalf("Greeter implementation targets = %#v, want 3 unique targets", greeterImplementations.Targets)
	}
	_ = requireAnalysisLens(t, contractLenses, "3 implementations", 3)
	welcomeReferences := requireAnalysisLens(t, contractLenses, "2 references", 10)
	if len(welcomeReferences.Targets) != 2 {
		t.Fatalf("Welcome reference targets = %#v, want 2 unique targets", welcomeReferences.Targets)
	}
	assertAnalysisLensCount(t, contractLenses, "1 reference", 2, 1)
	assertAnalysisLensCount(t, contractLenses, "3 implementations", 2, 1)
	assertAnalysisLens(t, snapshot.Lenses(internalTestURI), "Implements: Greeter", 4)
	assertAnalysisLens(t, snapshot.Lenses(internalTestURI), "Implements: Greeter.Greet", 6)
	assertAnalysisLens(t, snapshot.Lenses(externalTestURI), "Implements: Greeter", 8)
	assertAnalysisLens(t, snapshot.Lenses(externalTestURI), "Implements: Greeter.Greet", 10)
}

func TestAnalyzeModulePreservesComplexFactSemantics(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/complex-facts

go 1.26
`)
	contractFile := filepath.Join(moduleDir, "contract", "contract.go")
	writeAnalysisTestFile(t, contractFile, `package contract

type Public interface {
	Read() int
}

type private interface {
	hidden()
}

type Box[T any] struct {
	Value T
}

type Alias = Box[int]

func Identity[U any](value U) U {
	return value
}
`)
	implementationFile := filepath.Join(moduleDir, "implementation", "implementation.go")
	writeAnalysisTestFile(t, implementationFile, `package implementation

import "example.com/complex-facts/contract"

type Embedded struct{}

func (Embedded) Read() int { return 1 }

type Wrapper struct {
	Embedded
}

type Pointer struct{}

func (*Pointer) Read() int { return 2 }

type localPrivate interface {
	hidden()
}

type hiddenImpl struct{}

func (hiddenImpl) hidden() {}

var _ contract.Public = Wrapper{}
var _ contract.Public = (*Pointer)(nil)
var _ localPrivate = hiddenImpl{}

var AliasValue contract.Alias

func ReadAlias() int {
	AliasValue.Value = contract.Identity(1)
	return AliasValue.Value
}
`)

	snapshot, err := AnalyzeModule(context.Background(), moduleDir, nil)
	if err != nil {
		t.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
	}
	contractURI, err := PathToURI(contractFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", contractFile, err)
	}
	implementationURI, err := PathToURI(implementationFile)
	if err != nil {
		t.Fatalf("PathToURI(%q) error = %v", implementationFile, err)
	}

	contractLenses := snapshot.Lenses(contractURI)
	assertAnalysisLens(t, contractLenses, "3 implementations", 2)
	assertAnalysisLens(t, contractLenses, "2 implementations", 3)
	assertAnalysisLens(t, contractLenses, "0 implementations", 6)
	assertAnalysisLens(t, contractLenses, "0 implementations", 7)
	assertAnalysisLens(t, contractLenses, "2 references", 11)
	assertAnalysisLens(t, contractLenses, "1 reference", 14)
	assertAnalysisLens(t, contractLenses, "2 references", 16)

	implementationLenses := snapshot.Lenses(implementationURI)
	assertAnalysisLens(t, implementationLenses, "Implements: Public", 4)
	assertAnalysisLens(t, implementationLenses, "Implements: Public", 8)
	assertAnalysisLens(t, implementationLenses, "Implements: Public", 12)
	assertAnalysisLens(t, implementationLenses, "1 implementation", 16)
	assertAnalysisLens(t, implementationLenses, "Implements: localPrivate", 20)
	for _, lens := range implementationLenses {
		if lens.Title == "Implements: private" {
			t.Fatalf("private method identity crossed package boundary: %#v", lens)
		}
	}
}

func TestAnalyzeModuleScaleFixtureProducesStableSnapshot(t *testing.T) {
	t.Parallel()

	fixture := testutil.WriteModule(t, testutil.ModuleSpec{
		ModulePath:          "example.com/scale-correctness",
		PackageCount:        2,
		FilesPerPackage:     2,
		DeclarationsPerFile: 2,
		IncludeTests:        true,
	})
	uris := make([]string, 0, len(fixture.PackageFiles))
	for _, path := range fixture.PackageFiles {
		uri, err := PathToURI(path)
		if err != nil {
			t.Fatalf("PathToURI(%q) error = %v", path, err)
		}
		uris = append(uris, uri)
	}

	first, err := AnalyzeModule(context.Background(), fixture.Root, nil)
	if err != nil {
		t.Fatalf("first AnalyzeModule(%q) error = %v", fixture.Root, err)
	}
	second, err := AnalyzeModule(context.Background(), fixture.Root, nil)
	if err != nil {
		t.Fatalf("second AnalyzeModule(%q) error = %v", fixture.Root, err)
	}

	firstDigest := snapshotDigestForTest(first, uris)
	secondDigest := snapshotDigestForTest(second, uris)
	if firstDigest != secondDigest {
		t.Fatalf("snapshot digest changed between identical analyses: first %x, second %x", firstDigest, secondDigest)
	}
	if len(first.Lenses(uris[0])) == 0 {
		t.Fatal("generated entry file has no Code Lenses")
	}
}

func snapshotDigestForTest(snapshot Snapshot, uris []string) [sha256.Size]byte {
	var encoded []byte
	for _, uri := range uris {
		for _, lens := range snapshot.Lenses(uri) {
			encoded = fmt.Appendf(encoded, "%s|%v|%s|%d\n", uri, lens.Range, lens.Title, len(lens.Targets))
			for _, target := range lens.Targets {
				encoded = fmt.Appendf(encoded, "%s|%v\n", target.URI, target.Range)
			}
		}
	}

	return sha256.Sum256(encoded)
}

func assertAnalysisLens(t *testing.T, lenses []Lens, title string, line int) {
	t.Helper()
	_ = requireAnalysisLens(t, lenses, title, line)
}

func requireAnalysisLens(t *testing.T, lenses []Lens, title string, line int) Lens {
	t.Helper()
	for _, lens := range lenses {
		if lens.Title == title && lens.Range.Start.Line == line {
			return lens
		}
	}
	t.Fatalf("no lens titled %q on line %d in %#v", title, line, lenses)
	return Lens{}
}

func assertAnalysisLensCount(t *testing.T, lenses []Lens, title string, line, want int) {
	t.Helper()
	got := 0
	for _, lens := range lenses {
		if lens.Title == title && lens.Range.Start.Line == line {
			got++
		}
	}
	if got != want {
		t.Fatalf("lens count for %q on line %d = %d, want %d in %#v", title, line, got, want, lenses)
	}
}

func writeAnalysisTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("os.MkdirAll(%q) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}
