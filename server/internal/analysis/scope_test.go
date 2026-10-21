package analysis

import (
	"path/filepath"
	"reflect"
	"testing"
)

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
