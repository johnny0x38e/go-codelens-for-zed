package analysis

import (
	"cmp"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

// namedRelationship keeps the display name and navigation target together.
// The name cannot be recovered from Location when a snapshot is materialized,
// so retaining this small value avoids keeping go/types objects alive.
type namedRelationship struct {
	name     string
	location Location
}

// referenceLensData is the compact, declaration-oriented input to snapshot
// construction. It deliberately contains no AST or go/types pointers so the
// expensive package graph can be released after a non-incremental analysis.
type referenceLensData struct {
	declaration        symbolID
	location           Location
	showZero           bool
	relationshipLenses declarationLensSet
	targets            []Location
}

// declarationLensSet records the non-reference lenses emitted at a declaration.
// A bitset keeps the common snapshot-building path compact and branch-friendly.
type declarationLensSet uint8

const (
	declarationLensInterface declarationLensSet = 1 << iota
	declarationLensNamedType
	declarationLensInterfaceMethod
	declarationLensConcreteMethod
)

func markDeclarationLens(
	lenses []referenceLensData,
	primaryIndexes map[symbolID]int,
	additionalIndexes map[symbolID][]int,
	id symbolID,
	kind declarationLensSet,
) {
	primary, exists := primaryIndexes[id]
	if !exists {
		return
	}
	lenses[primary].relationshipLenses |= kind
	if additionalIndexes == nil {
		return
	}
	for _, index := range additionalIndexes[id] {
		lenses[index].relationshipLenses |= kind
	}
}

// loadedScope owns the only source copy retained from packages.Load. The source
// bytes are reused for UTF-16 location conversion instead of rereading files.
type loadedScope struct {
	packages     []*packages.Package
	sourceByPath map[string][]byte
}

// scopeAnalysis optionally carries the immutable indexes required by Engine.
// One-shot callers receive only the compact Snapshot.
type scopeAnalysis struct {
	snapshot Snapshot
	index    *scopeIndex
}

// sourceParser captures the exact bytes parsed by go/packages. ParseFile may be
// called concurrently, hence the mutex around sourceByPath.
type sourceParser struct {
	mu           sync.Mutex
	sourceByPath map[string][]byte
}

// implementationIndexThreshold is the crossover between a direct Cartesian
// scan and building method posting lists. Below this many candidate pairs, the
// allocation-free direct path is generally cheaper than constructing an index.
const implementationIndexThreshold = 1024

// AnalyzeModule loads all packages in moduleDir and computes exact named type
// implementation relationships and declaration references, including tests.
// Callers must not mutate overlays until AnalyzeModule returns.
func AnalyzeModule(ctx context.Context, moduleDir string, overlays map[string][]byte) (Snapshot, error) {
	return AnalyzeScope(ctx, ModuleAnalysisScope(moduleDir), overlays)
}

// AnalyzeScope loads all packages and test variants in scope together and
// computes relationships across every included module. Callers must not mutate
// overlays until AnalyzeScope returns.
func AnalyzeScope(ctx context.Context, scope AnalysisScope, overlays map[string][]byte) (Snapshot, error) {
	result, err := analyzeScope(ctx, scope, overlays, false)
	return result.snapshot, err
}

func analyzeScope(ctx context.Context, scope AnalysisScope, overlays map[string][]byte, retainIndex bool) (scopeAnalysis, error) {
	absoluteOverlays, err := canonicalOverlays(overlays)
	if err != nil {
		return scopeAnalysis{}, err
	}
	loaded, err := loadScopePackages(ctx, scope, absoluteOverlays)
	if err != nil {
		return scopeAnalysis{}, err
	}

	packagesToInspect := packagesForAnalysis(loaded.packages)
	facts, err := extractPackageFacts(packagesToInspect, newLocationResolver(loaded.sourceByPath))
	if err != nil {
		return scopeAnalysis{}, err
	}
	result := scopeAnalysis{}
	if retainIndex {
		// Incremental mode pays the one-time indexing cost so later edits can
		// copy and recompute only package-owned buckets and relationship rows.
		result.index, err = buildScopeIndex(scope, packagesToInspect, facts, loaded.sourceByPath)
		if err != nil {
			return scopeAnalysis{}, err
		}
		result.snapshot = snapshotFromIndexes(result.index.facts, result.index.relationships)
	} else {
		// One-shot analysis avoids retaining the type graph and its indexes.
		result.snapshot = snapshotFromFacts(facts)
	}
	return result, nil
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

	return loadedScope{
		packages:     loaded,
		sourceByPath: parser.sourceByPath,
	}, nil
}

func analyzeLoadedPackages(loaded []*packages.Package, sourceByPath map[string][]byte) (Snapshot, error) {
	locations := newLocationResolver(sourceByPath)
	facts, err := extractPackageFacts(loaded, locations)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotFromFacts(facts), nil
}

func snapshotFromFacts(factsByPackage map[string]*packageFacts) Snapshot {
	// Count before allocating: a workspace may contain thousands of package
	// variants, and repeated slice growth here would amplify peak memory.
	var declarationCount, interfaceCount, interfaceMethodCount, concreteTypeCount, concreteMethodCount int
	for _, facts := range factsByPackage {
		declarationCount += len(facts.declarations)
		interfaceCount += len(facts.interfaces)
		for _, declaration := range facts.interfaces {
			interfaceMethodCount += len(declaration.methods)
		}
		concreteTypeCount += len(facts.namedTypes)
		concreteMethodCount += len(facts.declaredMethods)
	}
	if declarationCount == 0 {
		return newSnapshot(make(map[string][]Lens))
	}

	referenceIndexes := make(map[symbolID]int, declarationCount)
	var additionalReferenceIndexes map[symbolID][]int
	referenceLenses := make([]referenceLensData, 0, declarationCount)
	for _, facts := range factsByPackage {
		for _, declaration := range facts.declarations {
			primary, exists := referenceIndexes[declaration.id]
			if exists {
				if referenceLenses[primary].location == declaration.location {
					referenceLenses[primary].showZero = referenceLenses[primary].showZero ||
						declaration.showZero
					continue
				}
				duplicate := false
				if additionalReferenceIndexes != nil {
					for _, index := range additionalReferenceIndexes[declaration.id] {
						if referenceLenses[index].location == declaration.location {
							referenceLenses[index].showZero = referenceLenses[index].showZero ||
								declaration.showZero
							duplicate = true
							break
						}
					}
				}
				if duplicate {
					continue
				}
				if additionalReferenceIndexes == nil {
					additionalReferenceIndexes = make(map[symbolID][]int)
				}
				additionalReferenceIndexes[declaration.id] = append(
					additionalReferenceIndexes[declaration.id],
					len(referenceLenses),
				)
			} else {
				referenceIndexes[declaration.id] = len(referenceLenses)
			}
			referenceLenses = append(referenceLenses, referenceLensData{
				declaration: declaration.id,
				location:    declaration.location,
				showZero:    declaration.showZero,
			})
		}
	}
	for _, facts := range factsByPackage {
		for _, declaration := range facts.interfaces {
			markDeclarationLens(
				referenceLenses,
				referenceIndexes,
				additionalReferenceIndexes,
				declaration.id,
				declarationLensInterface,
			)
			for _, method := range declaration.methods {
				markDeclarationLens(
					referenceLenses,
					referenceIndexes,
					additionalReferenceIndexes,
					method.id,
					declarationLensInterfaceMethod,
				)
			}
		}
		for _, declaration := range facts.namedTypes {
			markDeclarationLens(
				referenceLenses,
				referenceIndexes,
				additionalReferenceIndexes,
				declaration.id,
				declarationLensNamedType,
			)
		}
		for _, method := range facts.declaredMethods {
			markDeclarationLens(
				referenceLenses,
				referenceIndexes,
				additionalReferenceIndexes,
				method.id,
				declarationLensConcreteMethod,
			)
		}
	}
	for _, facts := range factsByPackage {
		for _, use := range facts.directUses {
			if primary, exists := referenceIndexes[use.id]; exists {
				referenceLenses[primary].targets = append(referenceLenses[primary].targets, use.location)
				if additionalReferenceIndexes != nil {
					for _, index := range additionalReferenceIndexes[use.id] {
						referenceLenses[index].targets = append(referenceLenses[index].targets, use.location)
					}
				}
			}
		}
		for _, use := range facts.methodUses {
			for _, methodID := range use.matchedMethods {
				if primary, exists := referenceIndexes[methodID]; exists {
					referenceLenses[primary].targets = append(referenceLenses[primary].targets, use.location)
					if additionalReferenceIndexes != nil {
						for _, index := range additionalReferenceIndexes[methodID] {
							referenceLenses[index].targets = append(referenceLenses[index].targets, use.location)
						}
					}
				}
			}
		}
	}
	interfaceTargets := make(map[Location][]Location, interfaceCount)
	typeInterfaces := make(map[Location][]namedRelationship, concreteTypeCount)
	collectPackageFactImplementationRelationships(
		factsByPackage,
		interfaceCount,
		concreteTypeCount,
		interfaceTargets,
		typeInterfaces,
	)
	interfaceMethodTargets := make(map[Location][]Location, interfaceMethodCount)
	concreteMethodInterfaces := make(map[Location][]namedRelationship, concreteMethodCount)
	concreteMethodsByIdentity := make(map[string][]methodFact, concreteMethodCount)
	for _, facts := range factsByPackage {
		for _, method := range facts.declaredMethods {
			concreteMethodsByIdentity[method.identity] = append(concreteMethodsByIdentity[method.identity], method)
		}
	}
	for _, facts := range factsByPackage {
		for _, interfaceDeclaration := range facts.interfaces {
			for _, interfaceMethod := range interfaceDeclaration.methods {
				for _, concreteMethod := range concreteMethodsByIdentity[interfaceMethod.identity] {
					if !types.Identical(interfaceMethod.function.Type(), concreteMethod.function.Type()) {
						continue
					}
					interfaceMethodTargets[interfaceMethod.location] = append(
						interfaceMethodTargets[interfaceMethod.location],
						concreteMethod.location,
					)
					concreteMethodInterfaces[concreteMethod.location] = append(
						concreteMethodInterfaces[concreteMethod.location],
						namedRelationship{
							name:     interfaceDeclaration.name + "." + interfaceMethod.id.name(),
							location: interfaceMethod.location,
						},
					)
				}
			}
		}
	}

	return snapshotFromLensData(
		referenceLenses,
		interfaceTargets,
		typeInterfaces,
		interfaceMethodTargets,
		concreteMethodInterfaces,
		len(factsByPackage),
	)
}

func snapshotFromLensData(
	referenceLenses []referenceLensData,
	interfaceTargets map[Location][]Location,
	typeInterfaces map[Location][]namedRelationship,
	interfaceMethodTargets map[Location][]Location,
	concreteMethodInterfaces map[Location][]namedRelationship,
	documentCapacity int,
) Snapshot {
	// Determine exact per-document lens capacities before materializing the
	// immutable Snapshot. This avoids repeated growth and copying of Lens slices
	// while preserving the same duplicate-location rules as emission below.
	lensCounts := make(map[string]int, documentCapacity)
	for _, data := range referenceLenses {
		count := 0
		if len(data.targets) > 0 || data.showZero {
			count++
		}
		if data.relationshipLenses&declarationLensInterface != 0 {
			count++
		}
		if data.relationshipLenses&declarationLensNamedType != 0 &&
			len(typeInterfaces[data.location]) > 0 {
			count++
		}
		if data.relationshipLenses&declarationLensInterfaceMethod != 0 {
			count++
		}
		if data.relationshipLenses&declarationLensConcreteMethod != 0 &&
			len(concreteMethodInterfaces[data.location]) > 0 {
			count++
		}
		if count > 0 {
			lensCounts[data.location.URI] += count
		}
	}

	lensesByURI := make(map[string][]Lens, len(lensCounts))
	keysByURI := make(map[string][]lensKey, len(lensCounts))
	for uri, count := range lensCounts {
		lensesByURI[uri] = make([]Lens, 0, count)
		keysByURI[uri] = make([]lensKey, 0, count)
	}
	for _, data := range referenceLenses {
		targets := uniqueSortedLocations(data.targets)
		if len(targets) > 0 || data.showZero {
			lensesByURI[data.location.URI] = append(lensesByURI[data.location.URI], Lens{
				Range:   data.location.Range,
				Title:   referenceCountTitle(len(targets)),
				Targets: targets,
				Kind:    LensKindReferences,
			})
			keysByURI[data.location.URI] = append(keysByURI[data.location.URI], lensKey{
				declaration: data.declaration,
				kind:        LensKindReferences,
			})
		}
		if data.relationshipLenses&declarationLensInterface != 0 {
			implementationTargets := uniqueSortedLocations(interfaceTargets[data.location])
			lensesByURI[data.location.URI] = append(lensesByURI[data.location.URI], Lens{
				Range:   data.location.Range,
				Title:   implementationCountTitle(len(implementationTargets)),
				Targets: implementationTargets,
				Kind:    LensKindImplementations,
			})
			keysByURI[data.location.URI] = append(keysByURI[data.location.URI], lensKey{
				declaration: data.declaration,
				kind:        LensKindImplementations,
			})
		}
		if data.relationshipLenses&declarationLensNamedType != 0 {
			appendNamedRelationshipLens(
				lensesByURI,
				keysByURI,
				lensKey{declaration: data.declaration, kind: LensKindImplementations},
				data.location,
				typeInterfaces[data.location],
			)
		}
		if data.relationshipLenses&declarationLensInterfaceMethod != 0 {
			implementationTargets := uniqueSortedLocations(interfaceMethodTargets[data.location])
			lensesByURI[data.location.URI] = append(lensesByURI[data.location.URI], Lens{
				Range:   data.location.Range,
				Title:   implementationCountTitle(len(implementationTargets)),
				Targets: implementationTargets,
				Kind:    LensKindImplementations,
			})
			keysByURI[data.location.URI] = append(keysByURI[data.location.URI], lensKey{
				declaration: data.declaration,
				kind:        LensKindImplementations,
			})
		}
		if data.relationshipLenses&declarationLensConcreteMethod != 0 {
			appendNamedRelationshipLens(
				lensesByURI,
				keysByURI,
				lensKey{declaration: data.declaration, kind: LensKindImplementations},
				data.location,
				concreteMethodInterfaces[data.location],
			)
		}
	}

	for uri := range lensesByURI {
		sort.Sort(keyedLensSorter{lenses: lensesByURI[uri], keys: keysByURI[uri]})
	}
	return newKeyedSnapshot(lensesByURI, keysByURI)
}

type keyedLensSorter struct {
	lenses []Lens
	keys   []lensKey
}

func (s keyedLensSorter) Len() int {
	return len(s.lenses)
}

func (s keyedLensSorter) Less(left, right int) bool {
	return compareLenses(s.lenses[left], s.lenses[right]) < 0
}

func (s keyedLensSorter) Swap(left, right int) {
	s.lenses[left], s.lenses[right] = s.lenses[right], s.lenses[left]
	s.keys[left], s.keys[right] = s.keys[right], s.keys[left]
}

func compareLenses(left, right Lens) int {
	if left.Range.Start != right.Range.Start {
		return comparePositions(left.Range.Start, right.Range.Start)
	}
	if left.Kind != right.Kind {
		return cmp.Compare(left.Kind, right.Kind)
	}
	return cmp.Compare(left.Title, right.Title)
}

func appendNamedRelationshipLens(
	lensesByURI map[string][]Lens,
	keysByURI map[string][]lensKey,
	key lensKey,
	location Location,
	relationships []namedRelationship,
) {
	lens, ok := namedRelationshipLens(location, relationships)
	if !ok {
		return
	}
	lensesByURI[location.URI] = append(lensesByURI[location.URI], lens)
	keysByURI[location.URI] = append(keysByURI[location.URI], key)
}

func namedRelationshipLens(location Location, relationships []namedRelationship) (Lens, bool) {
	relationships = uniqueSortedRelationships(relationships)
	if len(relationships) == 0 {
		return Lens{}, false
	}
	if len(relationships) == 1 {
		relationship := relationships[0]
		return Lens{
			Range:   location.Range,
			Title:   "Implements: " + relationship.name,
			Targets: []Location{relationship.location},
			Kind:    LensKindImplementations,
		}, true
	}
	names := make([]string, 0, len(relationships))
	targets := make([]Location, 0, len(relationships))
	for _, relationship := range relationships {
		names = append(names, relationship.name)
		targets = append(targets, relationship.location)
	}
	return Lens{
		Range:   location.Range,
		Title:   "Implements: " + strings.Join(names, ", "),
		Targets: uniqueSortedLocations(targets),
		Kind:    LensKindImplementations,
	}, true
}

func collectPackageFactImplementationRelationships(
	factsByPackage map[string]*packageFacts,
	interfaceCount, concreteTypeCount int,
	interfaceTargets map[Location][]Location,
	typeInterfaces map[Location][]namedRelationship,
) {
	if interfaceCount == 0 || concreteTypeCount == 0 {
		return
	}
	// Avoid multiplying the counts directly: division preserves the same
	// threshold decision without an overflow edge case on very large inputs.
	if interfaceCount <= implementationIndexThreshold/concreteTypeCount {
		for _, interfaceFacts := range factsByPackage {
			for _, interfaceDeclaration := range interfaceFacts.interfaces {
				interfaceDeclaration.interfaceType.Complete()
				if interfaceDeclaration.interfaceType.NumMethods() == 0 {
					continue
				}
				for _, concreteFacts := range factsByPackage {
					for _, candidate := range concreteFacts.namedTypes {
						if !types.Implements(candidate.named, interfaceDeclaration.interfaceType) &&
							!types.Implements(candidate.pointer, interfaceDeclaration.interfaceType) {
							continue
						}
						interfaceTargets[interfaceDeclaration.location] = append(
							interfaceTargets[interfaceDeclaration.location],
							candidate.location,
						)
						typeInterfaces[candidate.location] = append(
							typeInterfaces[candidate.location],
							namedRelationship{
								name:     interfaceDeclaration.name,
								location: interfaceDeclaration.location,
							},
						)
					}
				}
			}
		}
		return
	}

	interfaces := make([]*interfaceFact, 0, interfaceCount)
	concreteTypes := make([]*namedTypeFact, 0, concreteTypeCount)
	for _, facts := range factsByPackage {
		for index := range facts.interfaces {
			interfaces = append(interfaces, &facts.interfaces[index])
		}
		for index := range facts.namedTypes {
			concreteTypes = append(concreteTypes, &facts.namedTypes[index])
		}
	}
	collectPackageFactImplementationRelationshipsIndexed(
		interfaces,
		concreteTypes,
		interfaceTargets,
		typeInterfaces,
	)
}

func collectPackageFactImplementationRelationshipsIndexed(
	interfaces []*interfaceFact,
	concreteTypes []*namedTypeFact,
	interfaceTargets map[Location][]Location,
	typeInterfaces map[Location][]namedRelationship,
) {
	// Build posting lists only for identities that an interface actually
	// requires; indexing every method in a large workspace wastes memory.
	requiredMethodIDs := make(map[string]struct{})
	for _, interfaceDeclaration := range interfaces {
		interfaceDeclaration.interfaceType.Complete()
		for method := range interfaceDeclaration.interfaceType.Methods() {
			requiredMethodIDs[method.Id()] = struct{}{}
		}
	}
	candidateIndexesByMethodID := make(map[string][]int, len(requiredMethodIDs))
	for candidateIndex, candidate := range concreteTypes {
		for _, methodIdentity := range candidate.methodIdentities {
			if _, required := requiredMethodIDs[methodIdentity]; required {
				candidateIndexesByMethodID[methodIdentity] = append(
					candidateIndexesByMethodID[methodIdentity],
					candidateIndex,
				)
			}
		}
	}
	for _, interfaceDeclaration := range interfaces {
		interfaceType := interfaceDeclaration.interfaceType
		if interfaceType.NumMethods() == 0 {
			continue
		}
		// Every real implementation must contain every required method. The
		// smallest posting list is therefore a safe candidate superset and
		// minimizes the number of exact go/types checks below.
		var candidateIndexes []int
		for method := range interfaceType.Methods() {
			indexes := candidateIndexesByMethodID[method.Id()]
			if len(indexes) == 0 {
				candidateIndexes = nil
				break
			}
			if candidateIndexes == nil || len(indexes) < len(candidateIndexes) {
				candidateIndexes = indexes
			}
		}
		for _, candidateIndex := range candidateIndexes {
			candidate := concreteTypes[candidateIndex]
			if !types.Implements(candidate.named, interfaceType) &&
				!types.Implements(candidate.pointer, interfaceType) {
				continue
			}
			interfaceTargets[interfaceDeclaration.location] = append(
				interfaceTargets[interfaceDeclaration.location],
				candidate.location,
			)
			typeInterfaces[candidate.location] = append(
				typeInterfaces[candidate.location],
				namedRelationship{
					name:     interfaceDeclaration.name,
					location: interfaceDeclaration.location,
				},
			)
		}
	}
}

func collectFactImplementationRelationships(
	interfaces []interfaceFact,
	concreteTypes []namedTypeFact,
	interfaceTargets map[Location][]Location,
	typeInterfaces map[Location][]namedRelationship,
) {
	if len(interfaces) == 0 || len(concreteTypes) == 0 {
		return
	}
	// The direct path avoids index allocations for small scopes. The indexed
	// path bounds exact type checks for large interface/type cross-products.
	if len(interfaces) <= implementationIndexThreshold/len(concreteTypes) {
		collectFactImplementationRelationshipsDirect(
			interfaces,
			concreteTypes,
			interfaceTargets,
			typeInterfaces,
		)
		return
	}
	collectFactImplementationRelationshipsIndexed(
		interfaces,
		concreteTypes,
		interfaceTargets,
		typeInterfaces,
	)
}

func collectFactImplementationRelationshipsDirect(
	interfaces []interfaceFact,
	concreteTypes []namedTypeFact,
	interfaceTargets map[Location][]Location,
	typeInterfaces map[Location][]namedRelationship,
) {
	for _, interfaceDeclaration := range interfaces {
		interfaceDeclaration.interfaceType.Complete()
		if interfaceDeclaration.interfaceType.NumMethods() == 0 {
			continue
		}
		for _, candidate := range concreteTypes {
			if !types.Implements(candidate.named, interfaceDeclaration.interfaceType) &&
				!types.Implements(candidate.pointer, interfaceDeclaration.interfaceType) {
				continue
			}
			interfaceTargets[interfaceDeclaration.location] = append(
				interfaceTargets[interfaceDeclaration.location],
				candidate.location,
			)
			typeInterfaces[candidate.location] = append(typeInterfaces[candidate.location], namedRelationship{
				name:     interfaceDeclaration.name,
				location: interfaceDeclaration.location,
			})
		}
	}
}

func collectFactImplementationRelationshipsIndexed(
	interfaces []interfaceFact,
	concreteTypes []namedTypeFact,
	interfaceTargets map[Location][]Location,
	typeInterfaces map[Location][]namedRelationship,
) {
	// Restrict posting lists to identities that can participate in a match.
	requiredMethodIDs := make(map[string]struct{})
	for _, interfaceDeclaration := range interfaces {
		interfaceDeclaration.interfaceType.Complete()
		for method := range interfaceDeclaration.interfaceType.Methods() {
			requiredMethodIDs[method.Id()] = struct{}{}
		}
	}
	candidateIndexesByMethodID := make(map[string][]int, len(requiredMethodIDs))
	for candidateIndex, candidate := range concreteTypes {
		for _, methodIdentity := range candidate.methodIdentities {
			if _, required := requiredMethodIDs[methodIdentity]; required {
				candidateIndexesByMethodID[methodIdentity] = append(
					candidateIndexesByMethodID[methodIdentity],
					candidateIndex,
				)
			}
		}
	}
	for _, interfaceDeclaration := range interfaces {
		interfaceType := interfaceDeclaration.interfaceType
		if interfaceType.NumMethods() == 0 {
			continue
		}
		// Selecting the rarest required method is equivalent to the first step
		// of an intersection, but avoids allocating an intermediate set. Exact
		// types.Implements remains the authority for promoted methods and signatures.
		var candidateIndexes []int
		for method := range interfaceType.Methods() {
			indexes := candidateIndexesByMethodID[method.Id()]
			if len(indexes) == 0 {
				candidateIndexes = nil
				break
			}
			if candidateIndexes == nil || len(indexes) < len(candidateIndexes) {
				candidateIndexes = indexes
			}
		}
		for _, candidateIndex := range candidateIndexes {
			candidate := concreteTypes[candidateIndex]
			if !types.Implements(candidate.named, interfaceType) &&
				!types.Implements(candidate.pointer, interfaceType) {
				continue
			}
			interfaceTargets[interfaceDeclaration.location] = append(
				interfaceTargets[interfaceDeclaration.location],
				candidate.location,
			)
			typeInterfaces[candidate.location] = append(typeInterfaces[candidate.location], namedRelationship{
				name:     interfaceDeclaration.name,
				location: interfaceDeclaration.location,
			})
		}
	}
}

func (p *sourceParser) parseFile(fileSet *token.FileSet, filename string, source []byte) (*ast.File, error) {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("make parsed filename absolute: %w", err)
	}

	// packages.Load owns source for the duration of this call and does not
	// mutate it after parsing. Retaining the slice avoids reading the same file
	// again solely to translate byte columns to UTF-16 LSP positions.
	p.mu.Lock()
	p.sourceByPath[filepath.Clean(absolute)] = source
	p.mu.Unlock()

	const mode = parser.AllErrors | parser.ParseComments
	return parser.ParseFile(fileSet, filename, source, mode)
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

func uniqueSortedRelationships(relationships []namedRelationship) []namedRelationship {
	slices.SortFunc(relationships, func(left, right namedRelationship) int {
		if left.name != right.name {
			return cmp.Compare(left.name, right.name)
		}

		return compareLocations(left.location, right.location)
	})

	result := relationships[:0]
	for _, relationship := range relationships {
		if len(result) > 0 && result[len(result)-1] == relationship {
			continue
		}
		result = append(result, relationship)
	}

	clear(relationships[len(result):])
	return result
}

func typeOrPointerImplements(candidate types.Type, interfaceType *types.Interface) bool {
	if candidate == nil || interfaceType == nil {
		return false
	}
	if types.Implements(candidate, interfaceType) {
		return true
	}
	if _, isPointer := candidate.Underlying().(*types.Pointer); isPointer {
		return false
	}

	return types.Implements(types.NewPointer(candidate), interfaceType)
}

func implementationCountTitle(count int) string {
	if count == 1 {
		return "1 implementation"
	}

	return fmt.Sprintf("%d implementations", count)
}

func referenceCountTitle(count int) string {
	if count == 1 {
		return "1 reference"
	}

	return fmt.Sprintf("%d references", count)
}

func uniqueSortedLocations(locations []Location) []Location {
	if len(locations) == 0 {
		// LSP navigation arguments use an empty JSON array, not null.
		return []Location{}
	}
	slices.SortFunc(locations, compareLocations)

	result := locations[:0]
	for _, location := range locations {
		if len(result) > 0 && result[len(result)-1] == location {
			continue
		}
		result = append(result, location)
	}

	clear(locations[len(result):])
	return result
}

func compareLocations(left, right Location) int {
	if left.URI != right.URI {
		return cmp.Compare(left.URI, right.URI)
	}
	if left.Range.Start != right.Range.Start {
		return comparePositions(left.Range.Start, right.Range.Start)
	}

	return comparePositions(left.Range.End, right.Range.End)
}

func comparePositions(left, right Position) int {
	if left.Line != right.Line {
		return cmp.Compare(left.Line, right.Line)
	}

	return cmp.Compare(left.Character, right.Character)
}
