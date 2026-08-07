package testutil

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWriteModuleCreatesDeterministicPackagesAndTests(t *testing.T) {
	module := WriteModule(t, ModuleSpec{
		ModulePath:          "example.com/fixture",
		PackageCount:        2,
		FilesPerPackage:     2,
		DeclarationsPerFile: 2,
		IncludeTests:        true,
	})

	if module.Root == "" {
		t.Fatal("Root is empty")
	}
	if module.EntryFile == "" {
		t.Fatal("EntryFile is empty")
	}
	if got, want := len(module.PackageFiles), 8; got != want {
		t.Fatalf("len(PackageFiles) = %d, want %d", got, want)
	}
	if !slices.Contains(module.PackageFiles, module.EntryFile) {
		t.Fatalf("PackageFiles does not contain EntryFile %q", module.EntryFile)
	}

	goMod := readFixtureFile(t, filepath.Join(module.Root, "go.mod"))
	if goMod != "module example.com/fixture\n\ngo 1.26\n" {
		t.Fatalf("unexpected go.mod:\n%s", goMod)
	}

	entry := readFixtureFile(t, module.EntryFile)
	for _, fragment := range []string{
		"type Interface000_000_000 interface",
		"func (Value000_000_000) Method000_000_000() int",
		"func (*Pointer000_000_000) Method000_000_000() int",
		"var Reference000_000_000 = Function000_000_000",
	} {
		if !strings.Contains(entry, fragment) {
			t.Errorf("entry file does not contain %q", fragment)
		}
	}

	dependent := readFixtureFile(t, filepath.Join(module.Root, "pkg001", "file000.go"))
	for _, fragment := range []string{
		"previous \"example.com/fixture/pkg000\"",
		"var _ previous.Interface000_000_000 = Value001_000_000{}",
		"return previous.Function000_000_000()",
	} {
		if !strings.Contains(dependent, fragment) {
			t.Errorf("dependent package file does not contain %q", fragment)
		}
	}

	internalTest := readFixtureFile(t, filepath.Join(module.Root, "pkg000", "internal_test.go"))
	if !strings.Contains(internalTest, "package pkg000") ||
		!strings.Contains(internalTest, "Function000_000_000()") {
		t.Fatalf("unexpected internal test:\n%s", internalTest)
	}
	externalTest := readFixtureFile(t, filepath.Join(module.Root, "pkg000", "external_test.go"))
	if !strings.Contains(externalTest, "package pkg000_test") ||
		!strings.Contains(externalTest, "subject.Function000_000_000()") {
		t.Fatalf("unexpected external test:\n%s", externalTest)
	}
}

func TestWriteWorkspaceCreatesGoWorkAndIndependentModules(t *testing.T) {
	workspace := WriteWorkspace(t, []ModuleSpec{
		{
			ModulePath:          "example.com/first",
			PackageCount:        1,
			FilesPerPackage:     1,
			DeclarationsPerFile: 1,
		},
		{
			ModulePath:          "example.com/second",
			PackageCount:        1,
			FilesPerPackage:     1,
			DeclarationsPerFile: 1,
		},
	})

	if got, want := len(workspace.Modules), 2; got != want {
		t.Fatalf("len(Modules) = %d, want %d", got, want)
	}
	if workspace.WorkFile != filepath.Join(workspace.Root, "go.work") {
		t.Fatalf("WorkFile = %q, want a go.work beneath Root", workspace.WorkFile)
	}

	goWork := readFixtureFile(t, workspace.WorkFile)
	for _, fragment := range []string{"go 1.26", "./module000", "./module001"} {
		if !strings.Contains(goWork, fragment) {
			t.Errorf("go.work does not contain %q:\n%s", fragment, goWork)
		}
	}
	if workspace.Modules[0].Root == workspace.Modules[1].Root {
		t.Fatalf("workspace modules share Root %q", workspace.Modules[0].Root)
	}
}

func readFixtureFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path) //nolint:gosec // The caller constructs paths beneath testing.T.TempDir.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
