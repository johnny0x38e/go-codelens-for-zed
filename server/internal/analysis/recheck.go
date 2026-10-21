package analysis

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
)

var errIncrementalFallback = errors.New("incremental analysis requires a full reload")

// recheckSourceSet is request-local scratch storage. It reads only the exact
// CompiledGoFiles retained for affected package variants, preferring overlays;
// no directory walk or package reload occurs on this path.
type recheckSourceSet struct {
	fileSet      *token.FileSet
	overlays     map[string][]byte
	files        map[string]*ast.File
	fingerprints map[string]sourceFingerprint
	identities   map[string]fileIdentity
	sourceByPath map[string][]byte
	readFile     func(string) ([]byte, error)
}

// recheckPackages incrementally analyzes a known content-only reload plan.
// Typed fallback errors are intentionally handled by Engine with one complete
// packages.Load; cancellation and unexpected errors are returned directly.
func recheckPackages(
	ctx context.Context,
	index *scopeIndex,
	plan reloadPlan,
	overlays map[string][]byte,
) (*scopeIndex, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if index == nil || plan.full || len(plan.packageIDs) == 0 {
		return nil, incrementalFallbackf(fallbackRetainedIndex, "reload plan is not incrementally reusable")
	}
	absoluteOverlays, err := canonicalOverlays(overlays)
	if err != nil {
		return nil, err
	}
	sources := &recheckSourceSet{
		fileSet:      token.NewFileSet(),
		overlays:     absoluteOverlays,
		files:        make(map[string]*ast.File),
		fingerprints: make(map[string]sourceFingerprint),
		identities:   make(map[string]fileIdentity),
		sourceByPath: make(map[string][]byte),
		readFile:     os.ReadFile,
	}
	if sourceErr := sources.ensurePackages(ctx, index, plan.packageIDs); sourceErr != nil {
		return nil, sourceErr
	}
	apiChange, err := classifyReloadPlan(index, plan, sources)
	if err != nil {
		return nil, err
	}
	if apiChange {
		return recheckAPIClosure(ctx, index, plan, sources)
	}
	return recheckBodies(ctx, index, plan, sources)
}

func (s *recheckSourceSet) ensurePackages(ctx context.Context, index *scopeIndex, packageIDs []string) error {
	// Multiple test variants commonly share compiled files. Sorting and
	// compacting paths guarantees each source is read, parsed, and hashed once.
	paths := make([]string, 0)
	for _, packageID := range packageIDs {
		state := index.packages[packageID]
		if state == nil {
			return incrementalFallbackf(
				fallbackRetainedIndex,
				"package %q is absent from the retained index",
				packageID,
			)
		}
		for _, file := range state.files {
			paths = append(paths, file.path)
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		if s.files[path] != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Overlay bytes are the editor's source of truth. Disk is consulted only
		// when the exact compiled file has no dirty in-memory version.
		source, available := s.overlays[path]
		if !available {
			var err error
			source, err = s.readFile(path)
			if err != nil {
				return incrementalFallbackf(fallbackSourceUnavailable, "read compiled file %q: %v", path, err)
			}
		}
		// Record the disk identity even for overlaid files: freshness compares
		// what an external writer can change, while the overlay remains the source
		// of truth for content.
		if identity, ok := statFileIdentity(path); ok {
			if uri, uriErr := PathToURI(path); uriErr == nil {
				if s.identities == nil {
					s.identities = make(map[string]fileIdentity)
				}
				s.identities[uri] = identity
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		parsed, err := parser.ParseFile(
			s.fileSet,
			path,
			source,
			parser.AllErrors|parser.ParseComments|parser.SkipObjectResolution,
		)
		if err != nil {
			return incrementalFallbackf(fallbackParseFailure, "parse compiled file %q: %v", path, err)
		}
		fingerprint, err := sourceFingerprintForFile(s.fileSet, parsed, source)
		if err != nil {
			return incrementalFallbackf(fallbackParseFailure, "fingerprint compiled file %q: %v", path, err)
		}
		s.files[path] = parsed
		s.fingerprints[path] = fingerprint
		s.sourceByPath[path] = source
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

func classifyReloadPlan(index *scopeIndex, plan reloadPlan, sources *recheckSourceSet) (bool, error) {
	// A structure mismatch invalidates retained ownership/import metadata. A
	// declaration mismatch is safe only through the API-closure recheck; equal
	// declarations mean the edit is confined to bodies or comments.
	apiChange := false
	for _, packageID := range plan.packageIDs {
		state := index.packages[packageID]
		if state == nil {
			return false, incrementalFallbackf(
				fallbackRetainedIndex,
				"package %q is absent from the retained index",
				packageID,
			)
		}
		for _, file := range state.files {
			current, available := sources.fingerprints[file.path]
			if !available {
				return false, incrementalFallbackf(
					fallbackRetainedIndex,
					"compiled file %q was not reparsed",
					file.path,
				)
			}
			previous, retained := index.fingerprints[file.path]
			if !retained {
				return false, incrementalFallbackf(
					fallbackRetainedIndex,
					"compiled file %q has no retained fingerprint",
					file.path,
				)
			}
			if current.structure != previous.structure {
				return false, incrementalFallbackf(
					fallbackReasonForStructureChange(state, sources.files[file.path]),
					"package, build, or import structure changed in %q",
					file.path,
				)
			}
			apiChange = apiChange || current.declaration != previous.declaration
		}
	}
	return apiChange, nil
}

func fallbackReasonForStructureChange(state *packageState, file *ast.File) fallbackReason {
	if state == nil || file == nil {
		return fallbackRetainedIndex
	}
	for _, imported := range file.Imports {
		if imported == nil || imported.Path == nil {
			continue
		}
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return fallbackParseFailure
		}
		if _, retained := state.imports[path]; !retained {
			return fallbackUncachedImport
		}
	}
	return fallbackSourceStructure
}

func incrementalFallbackf(reason fallbackReason, format string, arguments ...any) error {
	return &incrementalFallbackError{
		reason: reason,
		detail: fmt.Sprintf(format, arguments...),
	}
}
