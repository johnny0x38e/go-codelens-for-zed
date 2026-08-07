package analysis

import (
	"errors"
	"fmt"
	"go/types"
	"maps"
	"slices"

	"golang.org/x/tools/go/packages"
)

// packageState is the immutable, reusable portion of one exact package
// variant. It deliberately excludes loader syntax, file sets, type-checker
// maps, and source buffers.
type packageState struct {
	id          string
	packagePath string
	name        string
	forTest     string
	goVersion   string
	files       []packageFileState
	imports     map[string]string
	types       *types.Package
	typesSizes  types.Sizes
	facts       *packageFacts
}

// packageFileState identifies one exact CompiledGoFile. Per-path fingerprints
// live on scopeIndex so test variants do not retain duplicate digests.
type packageFileState struct {
	path string
}

// scopeIndex owns the reusable package graph produced by one coherent
// packages.Load call. Every map and slice is constructed once and thereafter
// treated as read-only.
type scopeIndex struct {
	scope          AnalysisScope
	packages       map[string]*packageState
	fileOwners     map[string][]string
	reverseImports map[string][]string
	importTypes    map[string]*types.Package
	fingerprints   map[string]sourceFingerprint
	facts          *factIndex
	relationships  *relationshipIndex
	changedLenses  []lensKey
}

// reloadPlan identifies exact package variants affected by known content
// edits. A full plan requests the existing complete-analysis fallback.
type reloadPlan struct {
	full       bool
	reason     fallbackReason
	packageIDs []string
}

// packageImporter resolves imports exclusively from a retained, coherent type
// graph. It never reads export data or source from disk.
type packageImporter struct {
	packages map[string]*types.Package
}

var _ types.ImporterFrom = packageImporter{}

func buildScopeIndex(
	scope AnalysisScope,
	loaded []*packages.Package,
	factsByPackage map[string]*packageFacts,
	sourceByPath map[string][]byte,
) (*scopeIndex, error) {
	// First associate each canonical path with syntax from the original load.
	// Test variants may repeat files under distinct FileSets; one matching pair
	// is sufficient because fingerprints depend only on the parsed source.
	states := make(map[string]*packageState, len(loaded))
	fingerprints := make(map[string]sourceFingerprint)
	syntaxByPath := make(map[string]fingerprintSyntax)
	// Build package states only after the path-to-syntax table is complete. This
	// keeps state independent of load traversal order.
	for _, loadedPackage := range loaded {
		if len(sourceByPath) > 0 && len(syntaxByPath) == len(sourceByPath) {
			break
		}
		if loadedPackage == nil || loadedPackage.Fset == nil {
			continue
		}
		for fileIndex, file := range loadedPackage.Syntax {
			if file == nil {
				continue
			}
			path := ""
			// go/packages keeps Syntax aligned with CompiledGoFiles. Prefer that
			// path to avoid materializing token.Position values for test variants.
			if fileIndex < len(loadedPackage.CompiledGoFiles) {
				path = loadedPackage.CompiledGoFiles[fileIndex]
			}
			if _, available := sourceByPath[path]; !available {
				path = canonicalChangePath(path)
			}
			if _, available := sourceByPath[path]; !available {
				path = canonicalChangePath(
					loadedPackage.Fset.PositionFor(file.Package, false).Filename,
				)
			}
			if _, available := sourceByPath[path]; !available {
				continue
			}
			if _, exists := syntaxByPath[path]; !exists {
				syntaxByPath[path] = fingerprintSyntax{
					fileSet: loadedPackage.Fset,
					file:    file,
				}
			}
		}
	}
	for _, loadedPackage := range loaded {
		packageID := loadedPackageID(loadedPackage)
		if packageID == "" {
			return nil, errors.New("loaded package has no ID or package path")
		}
		if _, exists := states[packageID]; exists {
			return nil, fmt.Errorf("duplicate loaded package ID %q", packageID)
		}
		facts := factsByPackage[packageID]
		if facts == nil {
			return nil, fmt.Errorf("package %q has no extracted semantic facts", packageID)
		}

		packagePath := loadedPackage.PkgPath
		if loadedPackage.Types != nil && loadedPackage.Types.Path() != "" {
			packagePath = loadedPackage.Types.Path()
		}
		imports := make(map[string]string, len(loadedPackage.Imports))
		for importPath, importedPackage := range loadedPackage.Imports {
			importID := loadedPackageID(importedPackage)
			if importID == "" {
				importID = importPath
			}
			imports[importPath] = importID
		}
		goVersion := ""
		if loadedPackage.Module != nil {
			goVersion = loadedPackage.Module.GoVersion
		}
		files, err := buildPackageFileStates(
			loadedPackage.CompiledGoFiles,
			sourceByPath,
			fingerprints,
			syntaxByPath,
		)
		if err != nil {
			return nil, fmt.Errorf("index package %q files: %w", packageID, err)
		}
		states[packageID] = &packageState{
			id:          packageID,
			packagePath: packagePath,
			name:        loadedPackage.Name,
			forTest:     loadedPackage.ForTest,
			goVersion:   goVersion,
			files:       files,
			imports:     imports,
			types:       loadedPackage.Types,
			typesSizes:  loadedPackage.TypesSizes,
			facts:       facts,
		}
	}

	return newScopeIndexFromStatesAndFingerprints(scope, states, fingerprints)
}

func newScopeIndexFromStates(scope AnalysisScope, states map[string]*packageState) (*scopeIndex, error) {
	return newScopeIndexFromStatesAndFingerprints(scope, states, nil)
}

func newScopeIndexFromStatesAndFingerprints(
	scope AnalysisScope,
	states map[string]*packageState,
	fingerprints map[string]sourceFingerprint,
) (*scopeIndex, error) {
	index, err := newScopeIndexMetadata(scope, states, fingerprints)
	if err != nil {
		return nil, err
	}
	factsByPackage := make(map[string]*packageFacts, len(index.packages))
	for packageID, state := range index.packages {
		factsByPackage[packageID] = state.facts
	}
	index.facts = newFactIndex(factsByPackage)
	index.relationships = newRelationshipIndex(index.facts)
	return index, nil
}

func newScopeIndexMetadata(
	scope AnalysisScope,
	states map[string]*packageState,
	fingerprints map[string]sourceFingerprint,
) (*scopeIndex, error) {
	packagesByID := make(map[string]*packageState, len(states))
	for packageID, state := range states {
		if state == nil {
			return nil, fmt.Errorf("package %q has nil state", packageID)
		}
		if packageID == "" || state.id != packageID {
			return nil, fmt.Errorf("package state key %q does not match ID %q", packageID, state.id)
		}
		packagesByID[packageID] = state
	}

	// Incremental type checking is correct only when every retained import edge
	// points at the exact *types.Package instance used by its importer.
	if err := validateTypeUniverse(packagesByID); err != nil {
		return nil, err
	}
	index := &scopeIndex{
		scope:          cloneAnalysisScope(scope),
		packages:       packagesByID,
		fileOwners:     make(map[string][]string),
		reverseImports: make(map[string][]string),
		importTypes:    make(map[string]*types.Package),
		fingerprints:   fingerprints,
	}
	packageIDs := sortedPackageIDs(packagesByID)
	for _, packageID := range packageIDs {
		state := packagesByID[packageID]
		for _, file := range state.files {
			index.fileOwners[file.path] = append(index.fileOwners[file.path], packageID)
		}
		for _, importedID := range state.imports {
			if packagesByID[importedID] != nil {
				index.reverseImports[importedID] = append(
					index.reverseImports[importedID],
					packageID,
				)
			}
		}
	}
	canonicalizeStringLists(index.fileOwners)
	canonicalizeStringLists(index.reverseImports)
	if err := index.buildImportTypes(packageIDs); err != nil {
		return nil, err
	}
	return index, nil
}

func buildPackageFileStates(
	paths []string,
	sourceByPath map[string][]byte,
	fingerprints map[string]sourceFingerprint,
	syntaxByPath map[string]fingerprintSyntax,
) ([]packageFileState, error) {
	canonicalPaths := canonicalSortedPaths(paths)
	files := make([]packageFileState, 0, len(canonicalPaths))
	for _, path := range canonicalPaths {
		_, exists := fingerprints[path]
		if !exists {
			source, available := sourceByPath[path]
			if !available {
				return nil, fmt.Errorf("parsed source for %q is unavailable", path)
			}
			syntax, syntaxAvailable := syntaxByPath[path]
			if !syntaxAvailable || syntax.fileSet == nil || syntax.file == nil {
				return nil, fmt.Errorf("parsed syntax for %q is unavailable", path)
			}
			var err error
			fingerprint, err := sourceFingerprintForFile(syntax.fileSet, syntax.file, source)
			if err != nil {
				return nil, fmt.Errorf("fingerprint %q: %w", path, err)
			}
			fingerprints[path] = fingerprint
		}
		files = append(files, packageFileState{path: path})
	}
	return files, nil
}

func (i *scopeIndex) buildImportTypes(packageIDs []string) error {
	// Prefer production packages for direct path lookup. Test variants often use
	// the same package path but belong to a different augmented type universe.
	for _, packageID := range packageIDs {
		state := i.packages[packageID]
		if state.forTest != "" || state.types == nil || state.packagePath == "" {
			continue
		}
		if existing := i.importTypes[state.packagePath]; existing != nil && existing != state.types {
			return fmt.Errorf("multiple production type packages have path %q", state.packagePath)
		}
		i.importTypes[state.packagePath] = state.types
	}

	// Retain transitive external imports too. The incremental importer must never
	// read export data from disk, even when checking a deep dependent.
	visited := make(map[*types.Package]struct{}, len(i.packages))
	var retainImports func(*types.Package)
	retainImports = func(current *types.Package) {
		if current == nil {
			return
		}
		if _, seen := visited[current]; seen {
			return
		}
		visited[current] = struct{}{}
		for _, imported := range current.Imports() {
			if imported == nil {
				continue
			}
			if _, exists := i.importTypes[imported.Path()]; !exists {
				i.importTypes[imported.Path()] = imported
			}
			retainImports(imported)
		}
	}
	for _, packageID := range packageIDs {
		retainImports(i.packages[packageID].types)
	}
	return nil
}

func validateTypeUniverse(states map[string]*packageState) error {
	for _, packageID := range sortedPackageIDs(states) {
		state := states[packageID]
		if state.types == nil {
			continue
		}
		directImports := make(map[string]*types.Package, len(state.types.Imports()))
		for _, imported := range state.types.Imports() {
			if imported != nil {
				directImports[imported.Path()] = imported
			}
		}
		for importPath, importedID := range state.imports {
			importedState := states[importedID]
			if importedState == nil || importedState.types == nil {
				continue
			}
			if directImports[importPath] != importedState.types {
				return fmt.Errorf(
					"package %q imports %q from a different type universe",
					packageID,
					importPath,
				)
			}
		}
	}
	return nil
}

func (i *scopeIndex) importer() packageImporter {
	return packageImporter{packages: i.importTypes}
}

func (i packageImporter) Import(path string) (*types.Package, error) {
	if imported := i.packages[path]; imported != nil {
		return imported, nil
	}
	return nil, fmt.Errorf("package %q is not available in the retained type graph", path)
}

func (i packageImporter) ImportFrom(path, _ string, _ types.ImportMode) (*types.Package, error) {
	return i.Import(path)
}

func (i *scopeIndex) reloadPlanFor(changes ChangeSet) reloadPlan {
	if changes.Empty() {
		return reloadPlan{}
	}

	// Creation, deletion, and metadata edits can change build selection. Only
	// content edits to files with known owners are eligible for local rechecks.
	packageIDs := make([]string, 0, changes.len())
	for changeIndex := 0; changeIndex < changes.len(); changeIndex++ {
		change := changes.at(changeIndex)
		switch change.Kind {
		case ChangeMetadata:
			return reloadPlan{full: true, reason: fallbackModuleMetadata}
		case ChangeCreated:
			return reloadPlan{full: true, reason: fallbackFileCreated}
		case ChangeDeleted:
			return reloadPlan{full: true, reason: fallbackFileDeleted}
		case ChangeContent:
			// Continue below and resolve exact package variants.
		default:
			return reloadPlan{full: true, reason: fallbackRetainedIndex}
		}
		owners := i.fileOwners[change.Path]
		if len(owners) == 0 {
			return reloadPlan{full: true, reason: fallbackUnknownFileOwner}
		}
		packageIDs = append(packageIDs, owners...)
	}
	slices.Sort(packageIDs)
	packageIDs = slices.Compact(packageIDs)
	return reloadPlan{packageIDs: packageIDs}
}

func (i *scopeIndex) reverseClosure(seeds []string) []string {
	if len(seeds) == 0 {
		return nil
	}

	// Breadth-first traversal finds the transitive dependent set without
	// recursion; ordering is imposed separately by the import postorder below.
	closure := make(map[string]struct{}, len(seeds))
	queue := make([]string, 0, len(seeds))
	for _, packageID := range seeds {
		if i.packages[packageID] == nil {
			continue
		}
		if _, exists := closure[packageID]; exists {
			continue
		}
		closure[packageID] = struct{}{}
		queue = append(queue, packageID)
	}
	slices.Sort(queue)
	for head := 0; head < len(queue); head++ {
		for _, dependentID := range i.reverseImports[queue[head]] {
			if _, exists := closure[dependentID]; exists {
				continue
			}
			closure[dependentID] = struct{}{}
			queue = append(queue, dependentID)
		}
	}

	// Postorder traversal over imports emits dependencies before dependents.
	orderedIDs := make([]string, 0, len(closure))
	visitState := make(map[string]uint8, len(closure))
	var visit func(string)
	visit = func(packageID string) {
		switch visitState[packageID] {
		case 1, 2:
			return
		}
		visitState[packageID] = 1
		state := i.packages[packageID]
		dependencies := make([]string, 0, len(state.imports))
		for _, importedID := range state.imports {
			if _, included := closure[importedID]; included {
				dependencies = append(dependencies, importedID)
			}
		}
		slices.Sort(dependencies)
		dependencies = slices.Compact(dependencies)
		for _, importedID := range dependencies {
			visit(importedID)
		}
		visitState[packageID] = 2
		orderedIDs = append(orderedIDs, packageID)
	}
	for _, packageID := range sortedSetKeys(closure) {
		visit(packageID)
	}
	return orderedIDs
}

func (i *scopeIndex) withPackageStates(replacements map[string]*packageState) (*scopeIndex, error) {
	return i.withPackageStatesAndFingerprints(replacements, nil)
}

func (i *scopeIndex) withPackageStatesAndFingerprints(
	replacements map[string]*packageState,
	updates map[string]sourceFingerprint,
) (*scopeIndex, error) {
	// Construct the next revision off to the side. Published scope indexes are
	// immutable, so readers can use an old snapshot without synchronization
	// while this copy-on-write update is assembled.
	states := make(map[string]*packageState, len(i.packages))
	maps.Copy(states, i.packages)
	for packageID, replacement := range replacements {
		if states[packageID] == nil {
			return nil, fmt.Errorf("cannot replace unknown package %q", packageID)
		}
		if replacement == nil || replacement.id != packageID {
			return nil, fmt.Errorf("replacement key %q does not match its package state", packageID)
		}
		states[packageID] = replacement
	}
	fingerprints := i.fingerprints
	if len(updates) > 0 {
		fingerprints = make(map[string]sourceFingerprint, len(i.fingerprints)+len(updates))
		maps.Copy(fingerprints, i.fingerprints)
		maps.Copy(fingerprints, updates)
	}
	next, err := newScopeIndexMetadata(i.scope, states, fingerprints)
	if err != nil {
		return nil, err
	}
	replacementFacts := make(map[string]*packageFacts, len(replacements))
	for packageID, replacement := range replacements {
		replacementFacts[packageID] = replacement.facts
	}
	// factChanges is the bridge between package replacement and precise lens
	// invalidation: only touched relationship rows and URI slices are rebuilt.
	updatedFacts, changes, err := i.facts.withPackageFacts(replacementFacts)
	if err != nil {
		return nil, err
	}
	next.facts = updatedFacts
	next.relationships = i.relationships.withFactChanges(i.facts, next.facts, &changes)
	next.changedLenses = changes.sortedLensKeys()
	return next, nil
}

func loadedPackageID(loadedPackage *packages.Package) string {
	if loadedPackage == nil {
		return ""
	}
	if loadedPackage.ID != "" {
		return loadedPackage.ID
	}
	return loadedPackage.PkgPath
}

func canonicalSortedPaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path != "" {
			result = append(result, canonicalChangePath(path))
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func cloneAnalysisScope(scope AnalysisScope) AnalysisScope {
	scope.ModuleRoots = slices.Clone(scope.ModuleRoots)
	scope.Patterns = slices.Clone(scope.Patterns)
	return scope
}

func sortedPackageIDs(states map[string]*packageState) []string {
	result := make([]string, 0, len(states))
	for packageID := range states {
		result = append(result, packageID)
	}
	slices.Sort(result)
	return result
}

func sortedSetKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func canonicalizeStringLists(lists map[string][]string) {
	for key, values := range lists {
		slices.Sort(values)
		lists[key] = slices.Compact(values)
	}
}
