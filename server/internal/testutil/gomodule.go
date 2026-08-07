// Package testutil provides deterministic Go workspace fixtures for tests and
// benchmarks.
package testutil

import (
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
)

const fixtureGoVersion = "1.26"

// ModuleSpec describes the size and shape of a generated Go module.
type ModuleSpec struct {
	ModulePath          string
	PackageCount        int
	FilesPerPackage     int
	DeclarationsPerFile int
	IncludeTests        bool
}

// Module describes a generated Go module and its source files.
type Module struct {
	Root         string
	EntryFile    string
	PackageFiles []string
}

// Workspace describes a generated go.work workspace and its modules.
type Workspace struct {
	Root     string
	WorkFile string
	Modules  []Module
}

// WriteModule creates a deterministic Go module in a new temporary directory.
func WriteModule(tb testing.TB, spec ModuleSpec) Module {
	tb.Helper()

	root := tb.TempDir()
	return writeModuleAt(tb, root, spec)
}

// WriteWorkspace creates a deterministic go.work workspace in a new temporary
// directory. Each module is placed in its own child directory.
func WriteWorkspace(tb testing.TB, specs []ModuleSpec) Workspace {
	tb.Helper()

	if len(specs) == 0 {
		tb.Fatal("write workspace: at least one module is required")
	}

	root := tb.TempDir()
	workspace := Workspace{
		Root:     root,
		WorkFile: filepath.Join(root, "go.work"),
		Modules:  make([]Module, 0, len(specs)),
	}

	var workFile strings.Builder
	fmt.Fprintf(&workFile, "go %s\n\nuse (\n", fixtureGoVersion)
	for index, spec := range specs {
		moduleDirectory := fmt.Sprintf("module%03d", index)
		moduleRoot := filepath.Join(root, moduleDirectory)
		workspace.Modules = append(workspace.Modules, writeModuleAt(tb, moduleRoot, spec))
		fmt.Fprintf(&workFile, "\t./%s\n", moduleDirectory)
	}
	workFile.WriteString(")\n")
	mustWriteFile(tb, workspace.WorkFile, []byte(workFile.String()))

	return workspace
}

func writeModuleAt(tb testing.TB, root string, spec ModuleSpec) Module {
	tb.Helper()
	validateModuleSpec(tb, spec)

	if err := os.MkdirAll(root, 0o700); err != nil {
		tb.Fatalf("create module directory %s: %v", root, err)
	}
	mustWriteFile(tb, filepath.Join(root, "go.mod"), []byte(
		"module "+spec.ModulePath+"\n\ngo "+fixtureGoVersion+"\n",
	))

	packageFilesPerPackage := spec.FilesPerPackage
	if spec.IncludeTests {
		packageFilesPerPackage += 2
	}
	result := Module{
		Root:         root,
		PackageFiles: make([]string, 0, spec.PackageCount*packageFilesPerPackage),
	}

	for packageIndex := range spec.PackageCount {
		packageDirectory := filepath.Join(root, packageName(packageIndex))
		if err := os.MkdirAll(packageDirectory, 0o700); err != nil {
			tb.Fatalf("create package directory %s: %v", packageDirectory, err)
		}

		for fileIndex := range spec.FilesPerPackage {
			path := filepath.Join(packageDirectory, fmt.Sprintf("file%03d.go", fileIndex))
			mustWriteGoFile(tb, path, packageSource(spec, packageIndex, fileIndex))
			result.PackageFiles = append(result.PackageFiles, path)
			if result.EntryFile == "" {
				result.EntryFile = path
			}
		}
		if spec.IncludeTests {
			internalPath := filepath.Join(packageDirectory, "internal_test.go")
			mustWriteGoFile(tb, internalPath, internalTestSource(packageIndex))
			result.PackageFiles = append(result.PackageFiles, internalPath)

			externalPath := filepath.Join(packageDirectory, "external_test.go")
			mustWriteGoFile(tb, externalPath, externalTestSource(spec.ModulePath, packageIndex))
			result.PackageFiles = append(result.PackageFiles, externalPath)
		}
	}

	return result
}

func validateModuleSpec(tb testing.TB, spec ModuleSpec) {
	tb.Helper()

	if err := module.CheckPath(spec.ModulePath); err != nil {
		tb.Fatalf("invalid module path %q: %v", spec.ModulePath, err)
	}
	if spec.PackageCount <= 0 {
		tb.Fatalf("invalid package count %d: must be positive", spec.PackageCount)
	}
	if spec.FilesPerPackage <= 0 {
		tb.Fatalf("invalid files per package %d: must be positive", spec.FilesPerPackage)
	}
	if spec.DeclarationsPerFile <= 0 {
		tb.Fatalf("invalid declarations per file %d: must be positive", spec.DeclarationsPerFile)
	}
}

func packageSource(spec ModuleSpec, packageIndex, fileIndex int) string {
	var source strings.Builder
	fmt.Fprintf(&source, "package %s\n", packageName(packageIndex))
	if packageIndex > 0 {
		fmt.Fprintf(
			&source,
			"\nimport previous %q\n",
			fmt.Sprintf("%s/%s", spec.ModulePath, packageName(0)),
		)
	}

	for declarationIndex := range spec.DeclarationsPerFile {
		identity := declarationIdentity(packageIndex, fileIndex, declarationIndex)
		fmt.Fprintf(&source, `
type Interface%s interface {
	Method%s() int
}

type Value%s struct{}

func (Value%s) Method%s() int {
	return Function%s()
}

type Pointer%s struct{}

func (*Pointer%s) Method%s() int {
	return Function%s()
}

func Function%s() int {
	return %d
}

var Reference%s = Function%s
`, identity, identity, identity, identity, identity, identity, identity, identity, identity, identity,
			identity, packageIndex+fileIndex+declarationIndex, identity, identity)

		if packageIndex == 0 {
			continue
		}
		previousIdentity := declarationIdentity(0, fileIndex, declarationIndex)
		fmt.Fprintf(&source, `
var _ previous.Interface%s = Value%s{}
var _ previous.Interface%s = (*Pointer%s)(nil)

func (Value%s) Method%s() int {
	return previous.Function%s()
}

func (*Pointer%s) Method%s() int {
	return previous.Function%s()
}
`, previousIdentity, identity, previousIdentity, identity, identity, previousIdentity, previousIdentity,
			identity, previousIdentity, previousIdentity)
	}

	return source.String()
}

func internalTestSource(packageIndex int) string {
	identity := declarationIdentity(packageIndex, 0, 0)
	return fmt.Sprintf(`package %s

import "testing"

func TestInternalPackage(t *testing.T) {
	if got := Function%s(); got < 0 {
		t.Fatalf("Function%s() = %%d, want a non-negative value", got)
	}
}
`, packageName(packageIndex), identity, identity)
}

func externalTestSource(modulePath string, packageIndex int) string {
	identity := declarationIdentity(packageIndex, 0, 0)
	return fmt.Sprintf(`package %s_test

import (
	"testing"

	subject %q
)

func TestExternalPackage(t *testing.T) {
	if got := subject.Function%s(); got < 0 {
		t.Fatalf("Function%s() = %%d, want a non-negative value", got)
	}
}
`, packageName(packageIndex), fmt.Sprintf("%s/%s", modulePath, packageName(packageIndex)), identity, identity)
}

func packageName(index int) string {
	return fmt.Sprintf("pkg%03d", index)
}

func declarationIdentity(packageIndex, fileIndex, declarationIndex int) string {
	return fmt.Sprintf("%03d_%03d_%03d", packageIndex, fileIndex, declarationIndex)
}

func mustWriteGoFile(tb testing.TB, path, source string) {
	tb.Helper()

	formatted, err := format.Source([]byte(source))
	if err != nil {
		tb.Fatalf("format generated Go source %s: %v\n%s", path, err, source)
	}
	mustWriteFile(tb, path, formatted)
}

func mustWriteFile(tb testing.TB, path string, content []byte) {
	tb.Helper()

	if err := os.WriteFile(path, content, 0o600); err != nil {
		tb.Fatalf("write fixture file %s: %v", path, err)
	}
}
