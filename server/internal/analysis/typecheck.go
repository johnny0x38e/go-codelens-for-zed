package analysis

import (
	"context"
	"go/ast"
	"go/types"
	"maps"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// incrementalImporter overlays newly checked packages on the previous coherent
// type universe. missing records an import-cache miss even when go/types can
// continue and return a partial package.
type incrementalImporter struct {
	base         packageImporter
	replacements map[string]*types.Package
	missing      error
}

func recheckBodies(
	ctx context.Context,
	index *scopeIndex,
	plan reloadPlan,
	sources *recheckSourceSet,
) (*scopeIndex, error) {
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
		loadedPackage, checkErr := checkPackageState(ctx, index.packages[packageID], sources, importer)
		if checkErr != nil {
			return nil, checkErr
		}
		checked = append(checked, loadedPackage)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	retained := index.facts.packages
	transient, err := extractPackageFactsWithRetained(
		checked,
		newLocationResolver(sources.sourceByPath),
		retained,
	)
	if err != nil {
		return nil, incrementalFallbackf(fallbackIncompletePackage, "extract body facts: %v", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	resolver := newRelationshipResolver(retained, index.packages)
	replacements := make(map[string]*packageState, len(packageIDs))
	for _, packageID := range packageIDs {
		state := index.packages[packageID]
		newFacts := transient[packageID]
		if newFacts == nil {
			return nil, incrementalFallbackf(
				fallbackIncompletePackage,
				"body recheck produced no facts for %q",
				packageID,
			)
		}
		newFacts, _, err = rematchPackageMethodUses(newFacts, resolver, nil)
		if err != nil {
			return nil, err
		}
		mergedFacts, mergeErr := mergeBodyFacts(state.facts, newFacts)
		if mergeErr != nil {
			return nil, mergeErr
		}
		replacement := *state
		replacement.facts = mergedFacts
		replacements[packageID] = &replacement
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	next, err := index.withPackageStatesAndFingerprints(
		replacements,
		sources.fingerprints,
		sources.identities,
	)
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
			return nil, incrementalFallbackf(
				fallbackSemanticUncertainty,
				"body recheck lost declaration %q",
				declaration.id.name(),
			)
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

func recheckAPIClosure(
	ctx context.Context,
	index *scopeIndex,
	plan reloadPlan,
	sources *recheckSourceSet,
) (*scopeIndex, error) {
	// An exported declaration can change the meaning of every transitive reverse
	// dependent. Recheck exactly that closure, in dependency order, while all
	// unrelated packages continue to use the retained type universe.
	closure, err := index.reverseClosure(plan.packageIDs)
	if err != nil {
		return nil, err
	}
	if len(closure) == 0 {
		return nil, incrementalFallbackf(fallbackPackageGraph, "API reverse closure is empty")
	}
	if sourceErr := sources.ensurePackages(ctx, index, closure); sourceErr != nil {
		return nil, sourceErr
	}
	if _, classifyErr := classifyReloadPlan(index, reloadPlan{packageIDs: closure}, sources); classifyErr != nil {
		return nil, classifyErr
	}

	importer := &incrementalImporter{
		base:         index.importer(),
		replacements: make(map[string]*types.Package),
	}
	checked := make([]*packages.Package, 0, len(closure))
	replacements := make(map[string]*packageState, len(closure))
	for _, packageID := range closure {
		state := index.packages[packageID]
		loadedPackage, checkErr := checkPackageState(ctx, state, sources, importer)
		if checkErr != nil {
			return nil, checkErr
		}
		checked = append(checked, loadedPackage)
		replacement := *state
		replacement.types = loadedPackage.Types
		replacements[packageID] = &replacement
		if state.forTest == "" {
			importer.replacements[state.packagePath] = loadedPackage.Types
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	closureSet := make(map[string]struct{}, len(closure))
	for _, packageID := range closure {
		closureSet[packageID] = struct{}{}
	}
	retainedOutside := maps.Clone(index.facts.packages)
	for packageID := range closureSet {
		delete(retainedOutside, packageID)
	}
	newFacts, err := extractPackageFactsWithRetained(
		checked,
		newLocationResolver(sources.sourceByPath),
		retainedOutside,
	)
	if err != nil {
		return nil, incrementalFallbackf(fallbackIncompletePackage, "extract API-closure facts: %v", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	for _, packageID := range closure {
		facts := newFacts[packageID]
		if facts == nil {
			return nil, incrementalFallbackf(
				fallbackIncompletePackage,
				"API recheck produced no facts for %q",
				packageID,
			)
		}
		replacements[packageID].facts = facts
	}

	oldFacts := index.facts.packages
	affectedMethods := methodIdentitiesForPackages(oldFacts, closureSet)
	newMethods := methodIdentitiesForPackages(newFacts, nil)
	maps.Copy(affectedMethods, newMethods)
	// Uses of an entirely new method identity were never candidates in the old
	// fact set. Discovering them would require scanning all syntax, so fall back
	// instead of risking an incomplete reference count.
	oldMethods := methodIdentitiesForPackages(oldFacts, nil)
	for identity := range newMethods {
		if _, existed := oldMethods[identity]; !existed {
			return nil, incrementalFallbackf(
				fallbackSemanticUncertainty,
				"new method identity %q requires full-scope use discovery",
				identity,
			)
		}
	}

	combinedStates := maps.Clone(index.packages)
	combinedFacts := maps.Clone(oldFacts)
	for packageID, replacement := range replacements {
		combinedStates[packageID] = replacement
		combinedFacts[packageID] = replacement.facts
	}
	// Rematch affected method selections across the whole scope. The operation
	// scans compact retained use facts, not source files or types.Info maps.
	resolver := newRelationshipResolver(combinedFacts, combinedStates)
	packageIDs := slices.AppendSeq(make([]string, 0, len(combinedStates)), maps.Keys(combinedStates))
	slices.Sort(packageIDs)
	for _, packageID := range packageIDs {
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	next, err := index.withPackageStatesAndFingerprints(
		replacements,
		sources.fingerprints,
		sources.identities,
	)
	if err != nil {
		return nil, incrementalFallbackf(fallbackPackageGraph, "commit API recheck: %v", err)
	}
	return next, nil
}

func checkPackageState(
	ctx context.Context,
	state *packageState,
	sources *recheckSourceSet,
	importer types.Importer,
) (*packages.Package, error) {
	if state == nil {
		return nil, incrementalFallbackf(fallbackIncompletePackage, "cannot check a nil package state")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if tracked, ok := importer.(*incrementalImporter); ok && tracked.missing != nil {
		return nil, incrementalFallbackf(
			fallbackUncachedImport,
			"resolve package %q imports: %v",
			state.id,
			tracked.missing,
		)
	}
	if checkedTypes == nil {
		return nil, incrementalFallbackf(
			fallbackIncompletePackage,
			"type check package %q produced no package: %v",
			state.id,
			checkErr,
		)
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
	roots := slices.AppendSeq(make([]string, 0, len(included)), maps.Keys(included))
	slices.Sort(roots)
	for _, packageID := range roots {
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
