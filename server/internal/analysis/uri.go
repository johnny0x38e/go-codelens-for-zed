package analysis

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

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

// URIToPath converts an absolute file URI to a cleaned absolute path.
func URIToPath(rawURI string) (string, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return "", fmt.Errorf("parse document URI: %w", err)
	}
	if parsed.Scheme != "file" {
		return "", fmt.Errorf("unsupported URI scheme %q", parsed.Scheme)
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return "", fmt.Errorf("unsupported file URI host %q", parsed.Host)
	}
	path, err := filepath.Abs(filepath.FromSlash(parsed.Path))
	if err != nil {
		return "", fmt.Errorf("make file URI path absolute: %w", err)
	}
	return filepath.Clean(path), nil
}

// PathToURI converts a local path to its canonical file URI representation.
func PathToURI(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Clean(absolute))}).String(), nil
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
	content, err := os.ReadFile(workFile) //nolint:gosec // The path is a go.work file discovered within the workspace boundary.
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
	sort.Strings(moduleRoots)
	return moduleRoots, nil
}

func containsPath(paths []string, target string) bool {
	target = filepath.Clean(target)
	for _, path := range paths {
		if filepath.Clean(path) == target {
			return true
		}
	}
	return false
}

func pathWithin(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// locationResolver owns source bytes and canonical URIs for one analysis.
// Keeping this cache analysis-scoped avoids repeated disk reads without
// retaining source content in the long-lived Snapshot.
type locationResolver struct {
	overlays map[string][]byte
	files    map[string]locationFile
	readFile func(string) ([]byte, error)
}

type locationFile struct {
	content []byte
	uri     string
}

func newLocationResolver(overlays map[string][]byte) *locationResolver {
	return &locationResolver{
		overlays: overlays,
		files:    make(map[string]locationFile),
		readFile: os.ReadFile,
	}
}

func (r *locationResolver) identLocation(fileSet *token.FileSet, ident *ast.Ident) (Location, error) {
	start := fileSet.PositionFor(ident.Pos(), false)
	end := fileSet.PositionFor(ident.End(), false)
	filename, err := filepath.Abs(start.Filename)
	if err != nil {
		return Location{}, fmt.Errorf("make declaration filename absolute: %w", err)
	}
	filename = filepath.Clean(filename)

	file, ok := r.files[filename]
	if !ok {
		content, overlaid := r.overlays[filename]
		if !overlaid {
			content, err = r.readFile(filename)
		}
		if err != nil {
			return Location{}, fmt.Errorf("read declaration source %q: %w", filename, err)
		}
		uri, uriErr := PathToURI(filename)
		if uriErr != nil {
			return Location{}, uriErr
		}
		file = locationFile{content: content, uri: uri}
		r.files[filename] = file
	}
	startPosition, err := positionForTokenPosition(file.content, start)
	if err != nil {
		return Location{}, err
	}
	endPosition, err := positionForTokenPosition(file.content, end)
	if err != nil {
		return Location{}, err
	}
	return Location{
		URI: file.uri,
		Range: Range{
			Start: startPosition,
			End:   endPosition,
		},
	}, nil
}

func positionForTokenPosition(content []byte, position token.Position) (Position, error) {
	if position.Offset < 0 || position.Offset > len(content) {
		return Position{}, fmt.Errorf("source offset %d is outside content of length %d", position.Offset, len(content))
	}
	if position.Line < 1 || position.Column < 1 {
		return Position{}, fmt.Errorf("invalid token position at line %d column %d", position.Line, position.Column)
	}
	lineStart := position.Offset - position.Column + 1
	if lineStart < 0 {
		return Position{}, fmt.Errorf("token column %d exceeds source offset %d", position.Column, position.Offset)
	}
	return Position{
		Line:      position.Line - 1,
		Character: utf16Length(content[lineStart:position.Offset]),
	}, nil
}

func utf16Length(content []byte) int {
	asciiLength := 0
	for asciiLength < len(content) && content[asciiLength] < utf8.RuneSelf {
		asciiLength++
	}
	length := asciiLength
	content = content[asciiLength:]
	for len(content) > 0 {
		r, size := utf8.DecodeRune(content)
		content = content[size:]
		if r > 0xffff {
			length += 2
		} else {
			length++
		}
	}
	return length
}
