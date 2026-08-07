package analysis

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"hash"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

var errIncrementalFallback = errors.New("incremental analysis requires a full reload")

// sourceFingerprint separates declaration changes from package-graph changes.
// Both digests are derived from syntax only and never retain source bytes.
type sourceFingerprint struct {
	declaration [sha256.Size]byte
	structure   [sha256.Size]byte
}

type fingerprintWriter struct {
	// header encodes a domain tag and an unsigned length. Length-prefixing
	// prevents different token sequences from hashing to the same byte stream.
	digest hash.Hash
	header [9]byte
}

// fingerprintSyntax couples syntax with the FileSet that defines its token
// offsets. Mixing file sets would make sourceTokenRange invalid.
type fingerprintSyntax struct {
	fileSet *token.FileSet
	file    *ast.File
}

// recheckSourceSet is request-local scratch storage. It reads only the exact
// CompiledGoFiles retained for affected package variants, preferring overlays;
// no directory walk or package reload occurs on this path.
type recheckSourceSet struct {
	fileSet      *token.FileSet
	overlays     map[string][]byte
	files        map[string]*ast.File
	fingerprints map[string]sourceFingerprint
	sourceByPath map[string][]byte
	readFile     func(string) ([]byte, error)
}

// incrementalImporter overlays newly checked packages on the previous coherent
// type universe. missing records an import-cache miss even when go/types can
// continue and return a partial package.
type incrementalImporter struct {
	base         packageImporter
	replacements map[string]*types.Package
	missing      error
}

type relationshipReceiver struct {
	typeValue types.Type
}

type relationshipResolver struct {
	methodsByIdentity map[string][]methodReferenceFact
	receivers         map[symbolID]relationshipReceiver
}

// declarationFingerprint returns a stable digest of declarations and imports
// while excluding function bodies and non-build comments.
func declarationFingerprint(source []byte) ([sha256.Size]byte, error) {
	fingerprint, err := sourceFingerprintForBytes(source)
	return fingerprint.declaration, err
}

func sourceFingerprintForBytes(source []byte) (sourceFingerprint, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(
		fileSet,
		"",
		source,
		parser.AllErrors|parser.ParseComments|parser.SkipObjectResolution,
	)
	if err != nil {
		return sourceFingerprint{}, err
	}
	return sourceFingerprintForFile(fileSet, file, source)
}

func sourceFingerprintForFile(
	fileSet *token.FileSet,
	file *ast.File,
	source []byte,
) (sourceFingerprint, error) {
	if file == nil || file.Name == nil {
		return sourceFingerprint{}, errors.New("parsed source has no package clause")
	}

	// Structure changes can alter package selection or the dependency graph and
	// therefore require packages.Load. Declaration changes can be handled by
	// rechecking the known reverse dependency closure. Function bodies are
	// excluded from both decisions and use the cheapest recheck path.
	structureHash := newFingerprintWriter()
	structureHash.write('p', []byte(file.Name.Name))
	for _, constraint := range buildConstraintComments(file) {
		structureHash.write('b', []byte(constraint))
	}
	importPaths := make([]string, 0, len(file.Imports))
	for _, imported := range file.Imports {
		if imported == nil || imported.Path == nil {
			continue
		}
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return sourceFingerprint{}, fmt.Errorf("decode import path %q: %w", imported.Path.Value, err)
		}
		importPaths = append(importPaths, path)
	}
	slices.Sort(importPaths)
	for _, importPath := range importPaths {
		structureHash.write('i', []byte(importPath))
	}

	declarationHash := newFingerprintWriter()
	declarationHash.write('p', []byte(file.Name.Name))
	for _, declaration := range file.Decls {
		start, end := declaration.Pos(), declaration.End()
		tag := byte('g')
		if declaration, ok := declaration.(*ast.FuncDecl); ok {
			tag = 'f'
			if declaration.Body != nil {
				end = declaration.Body.Lbrace + 1
			}
		}
		declarationSource, err := sourceTokenRange(fileSet, source, start, end)
		if err != nil {
			return sourceFingerprint{}, err
		}
		declarationHash.write(tag, declarationSource)
	}

	var result sourceFingerprint
	declarationHash.sum(&result.declaration)
	structureHash.sum(&result.structure)
	return result, nil
}

func buildConstraintComments(file *ast.File) []string {
	var constraints []string
	for _, group := range file.Comments {
		if group == nil || group.End() > file.Package {
			continue
		}
		for _, comment := range group.List {
			if comment == nil || !strings.HasPrefix(comment.Text, "//") {
				continue
			}
			text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			if strings.HasPrefix(text, "go:build") || strings.HasPrefix(text, "+build") {
				constraints = append(constraints, text)
			}
		}
	}
	return constraints
}

func sourceTokenRange(
	fileSet *token.FileSet,
	source []byte,
	start token.Pos,
	end token.Pos,
) ([]byte, error) {
	startFile := fileSet.File(start)
	endFile := fileSet.File(end)
	if startFile == nil || startFile != endFile {
		return nil, errors.New("source token range does not belong to one parsed file")
	}
	startOffset := int(start) - startFile.Base()
	endOffset := int(end) - startFile.Base()
	if startOffset < 0 || endOffset < startOffset || endOffset > len(source) {
		return nil, fmt.Errorf(
			"source token range [%d,%d) is outside content of length %d",
			startOffset,
			endOffset,
			len(source),
		)
	}
	return source[startOffset:endOffset], nil
}

func newFingerprintWriter() fingerprintWriter {
	return fingerprintWriter{digest: sha256.New()}
}

func (w *fingerprintWriter) write(tag byte, value []byte) {
	w.header[0] = tag
	binary.BigEndian.PutUint64(w.header[1:], uint64(len(value)))
	_, _ = w.digest.Write(w.header[:])
	_, _ = w.digest.Write(value)
}

func (w *fingerprintWriter) sum(destination *[sha256.Size]byte) {
	w.digest.Sum(destination[:0])
}

// recheckPackages incrementally analyzes a known content-only reload plan.
// Typed fallback errors are intentionally handled by Engine with one complete
// packages.Load; cancellation and unexpected errors are returned directly.
func recheckPackages(ctx context.Context, index *scopeIndex, plan reloadPlan, overlays map[string][]byte) (*scopeIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
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
		sourceByPath: make(map[string][]byte),
		readFile:     os.ReadFile,
	}
	if err := sources.ensurePackages(ctx, index, plan.packageIDs); err != nil {
		return nil, err
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
			return incrementalFallbackf(fallbackRetainedIndex, "package %q is absent from the retained index", packageID)
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
			return false, incrementalFallbackf(fallbackRetainedIndex, "package %q is absent from the retained index", packageID)
		}
		for _, file := range state.files {
			current, available := sources.fingerprints[file.path]
			if !available {
				return false, incrementalFallbackf(fallbackRetainedIndex, "compiled file %q was not reparsed", file.path)
			}
			previous, retained := index.fingerprints[file.path]
			if !retained {
				return false, incrementalFallbackf(fallbackRetainedIndex, "compiled file %q has no retained fingerprint", file.path)
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

func recheckBodies(ctx context.Context, index *scopeIndex, plan reloadPlan, sources *recheckSourceSet) (*scopeIndex, error) {
	// Dependencies are checked before dependents so every import resolves to a
	// coherent retained package. Body-only changes intentionally keep the old
	// declaration type objects and rebuild only uses and source locations.
	packageIDs, err := orderRecheckPackageIDs(index, plan.packageIDs)
	if err != nil {
		return nil, err
	}
	checked := make([]*packages.Package, 0, len(packageIDs))
	importer := &incrementalImporter{base: index.importer()}
	for _, packageID := range packageIDs {
		loadedPackage, err := checkPackageState(ctx, index.packages[packageID], sources, importer)
		if err != nil {
			return nil, err
		}
		checked = append(checked, loadedPackage)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	retained := packageFactsFromIndex(index)
	transient, err := extractPackageFactsWithRetained(
		checked,
		newLocationResolver(sources.sourceByPath),
		retained,
	)
	if err != nil {
		return nil, incrementalFallbackf(fallbackIncompletePackage, "extract body facts: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolver := newRelationshipResolver(retained, index.packages)
	replacements := make(map[string]*packageState, len(packageIDs))
	for _, packageID := range packageIDs {
		state := index.packages[packageID]
		newFacts := transient[packageID]
		if newFacts == nil {
			return nil, incrementalFallbackf(fallbackIncompletePackage, "body recheck produced no facts for %q", packageID)
		}
		newFacts, _, err = rematchPackageMethodUses(newFacts, resolver, nil)
		if err != nil {
			return nil, err
		}
		mergedFacts, err := mergeBodyFacts(state.facts, newFacts)
		if err != nil {
			return nil, err
		}
		replacement := *state
		replacement.facts = mergedFacts
		replacements[packageID] = &replacement
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	next, err := index.withPackageStatesAndFingerprints(replacements, sources.fingerprints)
	if err != nil {
		return nil, incrementalFallbackf(fallbackPackageGraph, "commit body recheck: %v", err)
	}
	return next, nil
}

func mergeBodyFacts(oldFacts, transient *packageFacts) (*packageFacts, error) {
	// Declaration count and stable IDs prove that the fingerprint classifier did
	// not misclassify an API edit. Each semantic category is then copy-on-write:
	// unchanged slices retain identity for factIndex's O(1) revision checks.
	if oldFacts == nil || transient == nil ||
		oldFacts.packageID != transient.packageID ||
		len(oldFacts.declarations) != len(transient.declarations) {
		return nil, incrementalFallbackf(fallbackIncompletePackage, "body recheck changed declaration ownership")
	}
	locations := make(map[symbolID]Location, len(transient.declarations))
	for declarationIndex, declaration := range transient.declarations {
		if oldFacts.declarations[declarationIndex].id != declaration.id {
			return nil, incrementalFallbackf(fallbackSemanticUncertainty, "body recheck changed declaration identities")
		}
		locations[declaration.id] = declaration.location
	}

	merged := *oldFacts
	merged.directUses = transient.directUses
	merged.methodUses = transient.methodUses
	merged.relationshipSensitive = transient.relationshipSensitive
	declarationsCloned := false
	for declarationIndex, declaration := range oldFacts.declarations {
		location, available := locations[declaration.id]
		if !available {
			return nil, incrementalFallbackf(fallbackSemanticUncertainty, "body recheck lost declaration %q", declaration.id.name())
		}
		if declaration.location == location {
			continue
		}
		if !declarationsCloned {
			merged.declarations = slices.Clone(oldFacts.declarations)
			declarationsCloned = true
		}
		merged.declarations[declarationIndex].location = location
	}
	namedTypesCloned := false
	for namedIndex, named := range oldFacts.namedTypes {
		location, available := locations[named.id]
		if !available || named.location == location {
			continue
		}
		if !namedTypesCloned {
			merged.namedTypes = slices.Clone(oldFacts.namedTypes)
			namedTypesCloned = true
		}
		merged.namedTypes[namedIndex].location = location
	}
	interfacesCloned := false
	for interfaceIndex, declaration := range oldFacts.interfaces {
		location, available := locations[declaration.id]
		interfaceChanged := available && declaration.location != location
		methodsChanged := false
		for _, method := range declaration.methods {
			methodLocation, methodAvailable := locations[method.id]
			methodsChanged = methodsChanged || methodAvailable && method.location != methodLocation
		}
		if !interfaceChanged && !methodsChanged {
			continue
		}
		if !interfacesCloned {
			merged.interfaces = slices.Clone(oldFacts.interfaces)
			interfacesCloned = true
		}
		if interfaceChanged {
			merged.interfaces[interfaceIndex].location = location
		}
		if methodsChanged {
			merged.interfaces[interfaceIndex].methods = slices.Clone(declaration.methods)
			for methodIndex, method := range declaration.methods {
				if methodLocation, ok := locations[method.id]; ok {
					merged.interfaces[interfaceIndex].methods[methodIndex].location = methodLocation
				}
			}
		}
	}
	declaredMethodsCloned := false
	for methodIndex, method := range oldFacts.declaredMethods {
		location, available := locations[method.id]
		if !available || method.location == location {
			continue
		}
		if !declaredMethodsCloned {
			merged.declaredMethods = slices.Clone(oldFacts.declaredMethods)
			declaredMethodsCloned = true
		}
		merged.declaredMethods[methodIndex].location = location
	}
	return &merged, nil
}

func recheckAPIClosure(ctx context.Context, index *scopeIndex, plan reloadPlan, sources *recheckSourceSet) (*scopeIndex, error) {
	// An exported declaration can change the meaning of every transitive reverse
	// dependent. Recheck exactly that closure, in dependency order, while all
	// unrelated packages continue to use the retained type universe.
	closure, err := orderRecheckPackageIDs(index, index.reverseClosure(plan.packageIDs))
	if err != nil {
		return nil, err
	}
	if len(closure) == 0 {
		return nil, incrementalFallbackf(fallbackPackageGraph, "API reverse closure is empty")
	}
	if err := sources.ensurePackages(ctx, index, closure); err != nil {
		return nil, err
	}
	for _, packageID := range closure {
		state := index.packages[packageID]
		for _, file := range state.files {
			previous, retained := index.fingerprints[file.path]
			if !retained {
				return nil, incrementalFallbackf(fallbackRetainedIndex, "compiled file %q has no retained fingerprint", file.path)
			}
			if sources.fingerprints[file.path].structure != previous.structure {
				return nil, incrementalFallbackf(
					fallbackReasonForStructureChange(state, sources.files[file.path]),
					"package, build, or import structure changed in %q",
					file.path,
				)
			}
		}
	}

	importer := &incrementalImporter{
		base:         index.importer(),
		replacements: make(map[string]*types.Package),
	}
	checked := make([]*packages.Package, 0, len(closure))
	replacements := make(map[string]*packageState, len(closure))
	for _, packageID := range closure {
		state := index.packages[packageID]
		loadedPackage, err := checkPackageState(ctx, state, sources, importer)
		if err != nil {
			return nil, err
		}
		checked = append(checked, loadedPackage)
		replacement := *state
		replacement.types = loadedPackage.Types
		replacements[packageID] = &replacement
		if state.forTest == "" {
			importer.replacements[state.packagePath] = loadedPackage.Types
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	closureSet := make(map[string]struct{}, len(closure))
	for _, packageID := range closure {
		closureSet[packageID] = struct{}{}
	}
	retainedOutside := packageFactsFromIndexExcluding(index, closureSet)
	newFacts, err := extractPackageFactsWithRetained(
		checked,
		newLocationResolver(sources.sourceByPath),
		retainedOutside,
	)
	if err != nil {
		return nil, incrementalFallbackf(fallbackIncompletePackage, "extract API-closure facts: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, packageID := range closure {
		facts := newFacts[packageID]
		if facts == nil {
			return nil, incrementalFallbackf(fallbackIncompletePackage, "API recheck produced no facts for %q", packageID)
		}
		replacements[packageID].facts = facts
	}

	oldFacts := packageFactsFromIndex(index)
	affectedMethods := methodIdentitiesForPackages(oldFacts, closureSet)
	for identity := range methodIdentitiesForPackages(newFacts, nil) {
		affectedMethods[identity] = struct{}{}
	}
	// Uses of an entirely new method identity were never candidates in the old
	// fact set. Discovering them would require scanning all syntax, so fall back
	// instead of risking an incomplete reference count.
	oldMethods := methodIdentitiesForPackages(oldFacts, nil)
	for identity := range methodIdentitiesForPackages(newFacts, nil) {
		if _, existed := oldMethods[identity]; !existed {
			return nil, incrementalFallbackf(fallbackSemanticUncertainty, "new method identity %q requires full-scope use discovery", identity)
		}
	}

	combinedStates := make(map[string]*packageState, len(index.packages))
	maps.Copy(combinedStates, index.packages)
	combinedFacts := make(map[string]*packageFacts, len(index.packages))
	maps.Copy(combinedFacts, oldFacts)
	for packageID, replacement := range replacements {
		combinedStates[packageID] = replacement
		combinedFacts[packageID] = replacement.facts
	}
	// Rematch affected method selections across the whole scope. The operation
	// scans compact retained use facts, not source files or types.Info maps.
	resolver := newRelationshipResolver(combinedFacts, combinedStates)
	for _, packageID := range sortedPackageIDs(combinedStates) {
		facts, changed, rematchErr := rematchPackageMethodUses(
			combinedFacts[packageID],
			resolver,
			affectedMethods,
		)
		if rematchErr != nil {
			return nil, rematchErr
		}
		if !changed {
			continue
		}
		combinedFacts[packageID] = facts
		if replacement := replacements[packageID]; replacement != nil {
			replacement.facts = facts
			continue
		}
		replacement := *index.packages[packageID]
		replacement.facts = facts
		replacements[packageID] = &replacement
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	next, err := index.withPackageStatesAndFingerprints(replacements, sources.fingerprints)
	if err != nil {
		return nil, incrementalFallbackf(fallbackPackageGraph, "commit API recheck: %v", err)
	}
	return next, nil
}

func checkPackageState(ctx context.Context, state *packageState, sources *recheckSourceSet, importer types.Importer) (*packages.Package, error) {
	if state == nil {
		return nil, incrementalFallbackf(fallbackIncompletePackage, "cannot check a nil package state")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files := make([]*ast.File, 0, len(state.files))
	compiledGoFiles := make([]string, 0, len(state.files))
	for _, file := range state.files {
		parsed := sources.files[file.path]
		if parsed == nil {
			return nil, incrementalFallbackf(fallbackRetainedIndex, "compiled file %q was not parsed", file.path)
		}
		files = append(files, parsed)
		compiledGoFiles = append(compiledGoFiles, file.path)
	}
	info := &types.Info{
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	// go/types can return useful Defs, Uses, and Selections alongside ordinary
	// source errors (for example during an incomplete edit), matching editor
	// tooling behavior. Missing retained imports are detected separately below.
	config := types.Config{
		Importer:  importer,
		Sizes:     state.typesSizes,
		GoVersion: checkerGoVersion(state.goVersion),
		Error:     func(error) {},
	}
	checkedTypes, checkErr := config.Check(state.packagePath, sources.fileSet, files, info)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tracked, ok := importer.(*incrementalImporter); ok && tracked.missing != nil {
		return nil, incrementalFallbackf(fallbackUncachedImport, "resolve package %q imports: %v", state.id, tracked.missing)
	}
	if checkedTypes == nil {
		return nil, incrementalFallbackf(fallbackIncompletePackage, "type check package %q produced no package: %v", state.id, checkErr)
	}
	return &packages.Package{
		ID:              state.id,
		Name:            state.name,
		PkgPath:         state.packagePath,
		ForTest:         state.forTest,
		CompiledGoFiles: compiledGoFiles,
		Types:           checkedTypes,
		Fset:            sources.fileSet,
		Syntax:          files,
		TypesInfo:       info,
		TypesSizes:      state.typesSizes,
	}, nil
}

func checkerGoVersion(version string) string {
	if version == "" || strings.HasPrefix(version, "go") {
		return version
	}
	return "go" + version
}

func (i *incrementalImporter) Import(path string) (*types.Package, error) {
	if replacement := i.replacements[path]; replacement != nil {
		return replacement, nil
	}
	imported, err := i.base.Import(path)
	if err != nil && i.missing == nil {
		i.missing = err
	}
	return imported, err
}

func (i *incrementalImporter) ImportFrom(
	path string,
	_ string,
	_ types.ImportMode,
) (*types.Package, error) {
	return i.Import(path)
}

func orderRecheckPackageIDs(index *scopeIndex, packageIDs []string) ([]string, error) {
	// A deterministic DFS emits dependencies before dependents. Test variants
	// also depend on their production package even when that edge is not present
	// in the ordinary import map.
	included := make(map[string]struct{}, len(packageIDs))
	for _, packageID := range packageIDs {
		if index.packages[packageID] != nil {
			included[packageID] = struct{}{}
		}
	}
	productionByPath := make(map[string]string)
	for packageID, state := range index.packages {
		if state.forTest == "" {
			productionByPath[state.packagePath] = packageID
		}
	}
	ordered := make([]string, 0, len(included))
	visitState := make(map[string]uint8, len(included))
	cyclePackage := ""
	var visit func(string)
	visit = func(packageID string) {
		switch visitState[packageID] {
		case 1:
			cyclePackage = packageID
			return
		case 2:
			return
		}
		visitState[packageID] = 1
		state := index.packages[packageID]
		dependencies := make([]string, 0, len(state.imports)+1)
		for _, importedID := range state.imports {
			if _, ok := included[importedID]; ok {
				dependencies = append(dependencies, importedID)
			}
		}
		if state.forTest != "" {
			if baseID := productionByPath[state.forTest]; baseID != "" {
				if _, ok := included[baseID]; ok {
					dependencies = append(dependencies, baseID)
				}
			}
		}
		slices.Sort(dependencies)
		dependencies = slices.Compact(dependencies)
		for _, dependencyID := range dependencies {
			visit(dependencyID)
			if cyclePackage != "" {
				return
			}
		}
		visitState[packageID] = 2
		ordered = append(ordered, packageID)
	}
	for _, packageID := range sortedSetKeys(included) {
		visit(packageID)
		if cyclePackage != "" {
			return nil, incrementalFallbackf(
				fallbackPackageGraph,
				"retained package graph contains a cycle through %q",
				cyclePackage,
			)
		}
	}
	return ordered, nil
}

func packageFactsFromIndex(index *scopeIndex) map[string]*packageFacts {
	return packageFactsFromIndexExcluding(index, nil)
}

func packageFactsFromIndexExcluding(index *scopeIndex, excluded map[string]struct{}) map[string]*packageFacts {
	result := make(map[string]*packageFacts, len(index.packages)-len(excluded))
	for packageID, state := range index.packages {
		if _, skip := excluded[packageID]; skip {
			continue
		}
		result[packageID] = state.facts
	}
	return result
}

func methodIdentitiesForPackages(factsByPackage map[string]*packageFacts, included map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{})
	for packageID, facts := range factsByPackage {
		if included != nil {
			if _, ok := included[packageID]; !ok {
				continue
			}
		}
		if facts == nil {
			continue
		}
		for _, declaration := range facts.interfaces {
			for _, method := range declaration.methods {
				result[method.identity] = struct{}{}
			}
		}
		for _, method := range facts.declaredMethods {
			result[method.identity] = struct{}{}
		}
	}
	return result
}

func newRelationshipResolver(factsByPackage map[string]*packageFacts, states map[string]*packageState) *relationshipResolver {
	resolver := &relationshipResolver{
		methodsByIdentity: make(map[string][]methodReferenceFact),
		receivers:         make(map[symbolID]relationshipReceiver),
	}
	// Prefer production declarations before test variants when logical symbol
	// IDs coincide. This keeps receiver identity deterministic without merging
	// incompatible go/types universes.
	packageIDs := sortedPackageIDs(states)
	slices.SortStableFunc(packageIDs, func(left, right string) int {
		leftTest := states[left].forTest != ""
		rightTest := states[right].forTest != ""
		switch {
		case leftTest == rightTest:
			return strings.Compare(left, right)
		case leftTest:
			return 1
		default:
			return -1
		}
	})
	for _, packageID := range packageIDs {
		facts := factsByPackage[packageID]
		if facts == nil {
			continue
		}
		for _, named := range facts.namedTypes {
			if _, exists := resolver.receivers[named.id]; !exists {
				resolver.receivers[named.id] = relationshipReceiver{typeValue: named.named}
			}
		}
		for _, declaration := range facts.interfaces {
			if _, exists := resolver.receivers[declaration.id]; !exists {
				resolver.receivers[declaration.id] = relationshipReceiver{
					typeValue: declaration.interfaceType,
				}
			}
			for _, method := range declaration.methods {
				resolver.methodsByIdentity[method.identity] = append(
					resolver.methodsByIdentity[method.identity],
					methodReferenceFact{
						id:            method.id,
						function:      method.function,
						interfaceType: declaration.interfaceType,
					},
				)
			}
		}
		for _, method := range facts.declaredMethods {
			resolver.methodsByIdentity[method.identity] = append(
				resolver.methodsByIdentity[method.identity],
				methodReferenceFact{
					id:               method.id,
					function:         method.function,
					concreteReceiver: method.receiver,
				},
			)
		}
	}
	return resolver
}

func rematchPackageMethodUses(facts *packageFacts, resolver *relationshipResolver, affected map[string]struct{}) (*packageFacts, bool, error) {
	if facts == nil || len(facts.methodUses) == 0 {
		return facts, false, nil
	}
	var updated []methodUseFact
	for useIndex, use := range facts.methodUses {
		if affected != nil {
			if _, relevant := affected[use.methodIdentity]; !relevant {
				continue
			}
		}
		matched, err := resolver.matchMethodUse(use)
		if err != nil {
			return nil, false, err
		}
		if slices.Equal(matched, use.matchedMethods) {
			continue
		}
		if updated == nil {
			updated = slices.Clone(facts.methodUses)
		}
		updated[useIndex].matchedMethods = matched
	}
	if updated == nil {
		return facts, false, nil
	}
	replacement := *facts
	replacement.methodUses = updated
	return &replacement, true, nil
}

func (r *relationshipResolver) matchMethodUse(use methodUseFact) ([]symbolID, error) {
	candidates := r.methodsByIdentity[use.methodIdentity]
	if len(candidates) == 0 {
		return nil, nil
	}
	if use.unstableReceiver || !use.receiverID.valid() {
		return nil, incrementalFallbackf(fallbackSemanticUncertainty, "method use %q has no stable receiver", use.methodIdentity)
	}
	receiver, available := r.receivers[use.receiverID]
	if !available || receiver.typeValue == nil {
		return nil, incrementalFallbackf(fallbackSemanticUncertainty, "method use %q receiver is unavailable", use.methodIdentity)
	}
	selectedReceiver := receiver.typeValue
	if use.receiverKind == receiverPointer {
		if _, pointer := selectedReceiver.(*types.Pointer); !pointer {
			selectedReceiver = types.NewPointer(selectedReceiver)
		}
	}
	usedMethod := methodByIdentity(selectedReceiver, use.methodIdentity)
	if usedMethod == nil {
		return nil, incrementalFallbackf(fallbackSemanticUncertainty, "method %q is absent from its retained receiver", use.methodIdentity)
	}
	usedID, _ := methodSymbolID(usedMethod)
	matched := make([]symbolID, 0, len(candidates))
	for _, declaration := range candidates {
		if declaration.id == usedID || declaration.function == nil ||
			!types.Identical(declaration.function.Type(), usedMethod.Type()) ||
			!methodReferenceFactMatchesReceiver(declaration, selectedReceiver) {
			continue
		}
		matched = append(matched, declaration.id)
	}
	return uniqueSortedSymbolIDs(matched), nil
}

func methodByIdentity(receiver types.Type, identity string) *types.Func {
	methodSet := types.NewMethodSet(receiver)
	for selection := range methodSet.Methods() {
		function, ok := selection.Obj().(*types.Func)
		if ok && function.Id() == identity {
			return function
		}
	}
	return nil
}

func methodReferenceFactMatchesReceiver(declaration methodReferenceFact, selectedReceiver types.Type) bool {
	selectedInterface, receiverIsInterface := selectedReceiver.Underlying().(*types.Interface)
	if receiverIsInterface {
		selectedInterface.Complete()
		if declaration.interfaceType != nil {
			return true
		}
		return typeOrPointerImplements(declaration.concreteReceiver, selectedInterface)
	}
	if declaration.interfaceType == nil {
		return false
	}
	return typeOrPointerImplements(selectedReceiver, declaration.interfaceType)
}

func incrementalFallbackf(reason fallbackReason, format string, arguments ...any) error {
	return &incrementalFallbackError{
		reason: reason,
		detail: fmt.Sprintf(format, arguments...),
	}
}
