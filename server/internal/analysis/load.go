package analysis

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

// loadedScope owns the only source copy retained from packages.Load. The source
// bytes are reused for UTF-16 location conversion instead of rereading files.
type loadedScope struct {
	packages     []*packages.Package
	sourceByPath map[string][]byte
}

// sourceParser captures the exact bytes parsed by go/packages. ParseFile may be
// called concurrently, hence the mutex around sourceByPath.
type sourceParser struct {
	mu           sync.Mutex
	sourceByPath map[string][]byte
	parseErr     error
}

func loadScopePackages(ctx context.Context, scope AnalysisScope, overlays map[string][]byte) (loadedScope, error) {
	environment := os.Environ()
	if scope.WorkFile != "" {
		environment = append(environment, "GOWORK="+scope.WorkFile)
	} else {
		environment = append(environment, "GOWORK=off")
	}

	parser := &sourceParser{sourceByPath: make(map[string][]byte)}
	loaded, err := packages.Load(&packages.Config{
		Context:   ctx,
		Dir:       scope.Root,
		Mode:      packages.LoadSyntax | packages.NeedForTest | packages.NeedModule,
		Tests:     true,
		Overlay:   overlays,
		Env:       environment,
		ParseFile: parser.parseFile,
	}, scope.Patterns...)
	if err != nil {
		return loadedScope{}, fmt.Errorf("load Go analysis scope: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return loadedScope{}, err
	}
	if parser.parseErr != nil {
		return loadedScope{}, fmt.Errorf("parse Go analysis scope: %w", parser.parseErr)
	}

	return loadedScope{
		packages:     loaded,
		sourceByPath: parser.sourceByPath,
	}, nil
}

func (p *sourceParser) parseFile(fileSet *token.FileSet, filename string, source []byte) (*ast.File, error) {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("make parsed filename absolute: %w", err)
	}

	// packages.Load owns source for the duration of this call and does not
	// mutate it after parsing. Retaining the slice avoids reading the same file
	// again solely to translate byte columns to UTF-16 LSP positions.
	const mode = parser.AllErrors | parser.ParseComments
	file, parseErr := parser.ParseFile(fileSet, filename, source, mode)
	p.mu.Lock()
	p.sourceByPath[filepath.Clean(absolute)] = source
	if p.parseErr == nil {
		p.parseErr = parseErr
	}
	p.mu.Unlock()
	return file, parseErr
}

func canonicalOverlays(overlays map[string][]byte) (map[string][]byte, error) {
	result := make(map[string][]byte, len(overlays))

	for path, content := range overlays {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("make overlay path absolute: %w", err)
		}
		result[filepath.Clean(absolute)] = content
	}

	return result, nil
}

func packagesForAnalysis(loaded []*packages.Package) []*packages.Package {
	result := loaded[:0]
	for _, loadedPackage := range loaded {
		// packages.Load synthesizes a package main ending in .test to run each
		// test binary. Its generated references are not part of the user's code.
		if loadedPackage.ForTest == "" && loadedPackage.Name == "main" &&
			strings.HasSuffix(loadedPackage.PkgPath, ".test") {
			continue
		}
		result = append(result, loadedPackage)
	}

	clear(loaded[len(result):])

	return result
}
