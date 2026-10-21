package analysis

import (
	"go/types"
	"sort"
)

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
