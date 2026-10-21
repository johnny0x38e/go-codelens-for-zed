package analysis

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
)

// ErrNoModule reports that no go.mod exists between a document and its
// workspace boundary.
var ErrNoModule = errors.New("document is not inside a Go module")

// AnalysisScope describes the Go packages that must be loaded together to
// answer semantic queries for a document. It is either one module or every
// module listed by an applicable go.work file.
type AnalysisScope struct {
	Root        string
	WorkFile    string
	ModuleRoots []string
	Patterns    []string
}

// CacheKey identifies all documents that share one semantic snapshot.
func (s AnalysisScope) CacheKey() string {
	if s.WorkFile != "" {
		return s.WorkFile
	}
	return s.Root
}

// ModuleAnalysisScope returns a scope containing only moduleDir.
func ModuleAnalysisScope(moduleDir string) AnalysisScope {
	absolute, err := filepath.Abs(moduleDir)
	if err != nil {
		absolute = moduleDir
	}
	absolute = filepath.Clean(absolute)
	return AnalysisScope{
		Root:        absolute,
		ModuleRoots: []string{absolute},
		Patterns:    []string{"./..."},
	}
}

// FindModuleRoot finds the closest go.mod above filename without walking above
// workspaceRoot. An empty workspace root allows walking to the filesystem root.
func FindModuleRoot(filename, workspaceRoot string) (string, error) {
	filename, err := filepath.Abs(filename)
	if err != nil {
		return "", fmt.Errorf("make filename absolute: %w", err)
	}
	dir := filepath.Dir(filename)

	boundary := ""
	if workspaceRoot != "" {
		boundary, err = filepath.Abs(workspaceRoot)
		if err != nil {
			return "", fmt.Errorf("make workspace root absolute: %w", err)
		}
		boundary = filepath.Clean(boundary)
		if !pathWithin(dir, boundary) {
			return "", ErrNoModule
		}
	}

	for {
		if info, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil && !info.IsDir() {
			return dir, nil
		}
		if boundary != "" && dir == boundary {
			return "", ErrNoModule
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNoModule
		}
		dir = parent
	}
}

// FindAnalysisScope finds the closest module containing filename and expands
// it to the nearest containing go.work when that work file lists the module.
// It never searches above workspaceRoot when a workspace boundary is supplied.
func FindAnalysisScope(filename, workspaceRoot string) (AnalysisScope, error) {
	moduleRoot, err := FindModuleRoot(filename, workspaceRoot)
	if err != nil {
		return AnalysisScope{}, err
	}
	moduleScope := ModuleAnalysisScope(moduleRoot)

	workFile, err := findWorkFile(moduleRoot, workspaceRoot)
	if err != nil {
		return AnalysisScope{}, err
	}
	if workFile == "" {
		return moduleScope, nil
	}
	moduleRoots, err := parseWorkModuleRoots(workFile)
	if err != nil {
		return AnalysisScope{}, err
	}
	if !containsPath(moduleRoots, moduleRoot) {
		return moduleScope, nil
	}

	root := filepath.Dir(workFile)
	patterns := make([]string, 0, len(moduleRoots))
	for _, rootModule := range moduleRoots {
		relative, err := filepath.Rel(root, rootModule)
		if err != nil {
			return AnalysisScope{}, fmt.Errorf("make go.work module %q relative: %w", rootModule, err)
		}
		if relative == "." {
			patterns = append(patterns, "./...")
			continue
		}
		pattern := filepath.ToSlash(filepath.Join(relative, "..."))
		if !strings.HasPrefix(pattern, ".") {
			pattern = "./" + pattern
		}
		patterns = append(patterns, pattern)
	}

	return AnalysisScope{
		Root:        root,
		WorkFile:    workFile,
		ModuleRoots: moduleRoots,
		Patterns:    patterns,
	}, nil
}

func findWorkFile(moduleRoot, workspaceRoot string) (string, error) {
	dir := filepath.Clean(moduleRoot)
	boundary := ""
	if workspaceRoot != "" {
		absolute, err := filepath.Abs(workspaceRoot)
		if err != nil {
			return "", fmt.Errorf("make workspace root absolute: %w", err)
		}
		boundary = filepath.Clean(absolute)
		if !pathWithin(dir, boundary) {
			return "", nil
		}
	}

	for {
		candidate := filepath.Join(dir, "go.work")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
		if boundary != "" && dir == boundary {
			return "", nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

func parseWorkModuleRoots(workFile string) ([]string, error) {
	content, err := os.ReadFile(
		workFile,
	) //nolint:gosec // The path is a go.work file discovered within the workspace boundary.
	if err != nil {
		return nil, fmt.Errorf("read go.work %q: %w", workFile, err)
	}
	parsed, err := modfile.ParseWork(workFile, content, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.work %q: %w", workFile, err)
	}

	workRoot := filepath.Dir(workFile)
	seen := make(map[string]struct{}, len(parsed.Use))
	moduleRoots := make([]string, 0, len(parsed.Use))
	for _, use := range parsed.Use {
		moduleRoot := use.Path
		if !filepath.IsAbs(moduleRoot) {
			moduleRoot = filepath.Join(workRoot, moduleRoot)
		}
		absolute, err := filepath.Abs(moduleRoot)
		if err != nil {
			return nil, fmt.Errorf("make go.work use path %q absolute: %w", use.Path, err)
		}
		absolute = filepath.Clean(absolute)
		if _, ok := seen[absolute]; ok {
			continue
		}
		seen[absolute] = struct{}{}
		moduleRoots = append(moduleRoots, absolute)
	}

	slices.Sort(moduleRoots)
	return moduleRoots, nil
}

func containsPath(paths []string, target string) bool {
	for _, path := range paths {
		if samePath(path, target) {
			return true
		}
	}

	return false
}

func pathWithin(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
