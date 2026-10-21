package analysis

import (
	"go/types"
)

// implementationIndexThreshold is the crossover between a direct Cartesian
// scan and building method posting lists. Below this many candidate pairs, the
// allocation-free direct path is generally cheaper than constructing an index.
const implementationIndexThreshold = 1024

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
