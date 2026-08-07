package analysis

import (
	"go/types"
	"maps"
	"slices"
)

func snapshotFromIndexes(facts *factIndex, relationships *relationshipIndex) Snapshot {
	// Indexed facts may contain the same logical declaration at distinct source
	// locations (notably production and test variants). Preserve each location
	// while sharing its symbol-level reference targets.
	declarationCount := 0
	for _, declarations := range facts.declarations {
		declarationCount += len(declarations)
	}
	if declarationCount == 0 {
		return newSnapshot(make(map[string][]Lens))
	}

	referenceIndexes := make(map[symbolID]int, len(facts.declarations))
	var additionalReferenceIndexes map[symbolID][]int
	referenceLenses := make([]referenceLensData, 0, declarationCount)
	for id, declarations := range facts.declarations {
		for _, declaration := range declarations {
			if _, exists := referenceIndexes[id]; exists {
				if additionalReferenceIndexes == nil {
					additionalReferenceIndexes = make(map[symbolID][]int)
				}
				additionalReferenceIndexes[id] = append(
					additionalReferenceIndexes[id],
					len(referenceLenses),
				)
			} else {
				referenceIndexes[id] = len(referenceLenses)
			}
			referenceLenses = append(referenceLenses, referenceLensData{
				declaration: id,
				location:    declaration.location,
				showZero:    declaration.showZero,
			})
		}
	}
	for key := range facts.interfaces {
		markDeclarationLens(
			referenceLenses,
			referenceIndexes,
			additionalReferenceIndexes,
			key.symbolID,
			declarationLensInterface,
		)
	}
	for key := range facts.namedTypes {
		markDeclarationLens(
			referenceLenses,
			referenceIndexes,
			additionalReferenceIndexes,
			key.symbolID,
			declarationLensNamedType,
		)
	}
	for key := range facts.interfaceMethods {
		markDeclarationLens(
			referenceLenses,
			referenceIndexes,
			additionalReferenceIndexes,
			key.symbolID,
			declarationLensInterfaceMethod,
		)
	}
	for key := range facts.concreteMethods {
		markDeclarationLens(
			referenceLenses,
			referenceIndexes,
			additionalReferenceIndexes,
			key.symbolID,
			declarationLensConcreteMethod,
		)
	}
	for id, targets := range facts.referenceTargets {
		primary, exists := referenceIndexes[id]
		if !exists {
			continue
		}
		referenceLenses[primary].targets = slices.Clone(targets)
		for _, index := range additionalReferenceIndexes[id] {
			referenceLenses[index].targets = slices.Clone(targets)
		}
	}
	return snapshotFromLensData(
		referenceLenses,
		relationships.interfaceTargets,
		relationships.typeInterfaces,
		relationships.interfaceMethodTargets,
		relationships.concreteMethodInterfaces,
		len(facts.packages),
	)
}

// relationshipIndex stores exact implementation edges in both directions and
// pre-materializes their location-based output. Body-only edits reuse the
// entire index; API edits copy and recompute only affected rows and locations.
type relationshipIndex struct {
	typesByInterface map[ownedSymbolID][]ownedSymbolID
	interfacesByType map[ownedSymbolID][]ownedSymbolID

	concreteMethodsByInterfaceMethod map[ownedSymbolID][]ownedSymbolID
	interfaceMethodsByConcreteMethod map[ownedSymbolID][]ownedSymbolID

	interfaceTargets         map[Location][]Location
	typeInterfaces           map[Location][]namedRelationship
	interfaceMethodTargets   map[Location][]Location
	concreteMethodInterfaces map[Location][]namedRelationship
}

func newRelationshipIndex(facts *factIndex) *relationshipIndex {
	// Forward rows are computed exactly once; reverse rows are populated at the
	// same time so later incremental updates never need a global reverse scan.
	index := &relationshipIndex{
		typesByInterface:                 make(map[ownedSymbolID][]ownedSymbolID, len(facts.interfaces)),
		interfacesByType:                 make(map[ownedSymbolID][]ownedSymbolID, len(facts.namedTypes)),
		concreteMethodsByInterfaceMethod: make(map[ownedSymbolID][]ownedSymbolID, len(facts.interfaceMethods)),
		interfaceMethodsByConcreteMethod: make(map[ownedSymbolID][]ownedSymbolID, len(facts.concreteMethods)),
		interfaceTargets:                 make(map[Location][]Location, len(facts.interfaces)),
		typeInterfaces:                   make(map[Location][]namedRelationship, len(facts.namedTypes)),
		interfaceMethodTargets:           make(map[Location][]Location, len(facts.interfaceMethods)),
		concreteMethodInterfaces:         make(map[Location][]namedRelationship, len(facts.concreteMethods)),
	}
	interfaceKeys := mapKeys(facts.interfaces)
	sortOwnedSymbolIDs(interfaceKeys)
	for _, interfaceKey := range interfaceKeys {
		typeKeys := implementationCandidates(facts, interfaceKey)
		if len(typeKeys) == 0 {
			continue
		}
		index.typesByInterface[interfaceKey] = typeKeys
		for _, typeKey := range typeKeys {
			index.interfacesByType[typeKey] = append(index.interfacesByType[typeKey], interfaceKey)
		}
	}
	methodKeys := mapKeys(facts.interfaceMethods)
	sortOwnedSymbolIDs(methodKeys)
	for _, methodKey := range methodKeys {
		concreteKeys := methodImplementationCandidates(facts, methodKey)
		if len(concreteKeys) == 0 {
			continue
		}
		index.concreteMethodsByInterfaceMethod[methodKey] = concreteKeys
		for _, concreteKey := range concreteKeys {
			index.interfaceMethodsByConcreteMethod[concreteKey] = append(
				index.interfaceMethodsByConcreteMethod[concreteKey],
				methodKey,
			)
		}
	}
	for key, values := range index.interfacesByType {
		sortOwnedSymbolIDs(values)
		index.interfacesByType[key] = slices.Compact(values)
	}
	for key, values := range index.interfaceMethodsByConcreteMethod {
		sortOwnedSymbolIDs(values)
		index.interfaceMethodsByConcreteMethod[key] = slices.Compact(values)
	}
	index.rebuildAllOutputs(facts)
	return index
}

func (i *relationshipIndex) withFactChanges(oldFacts, newFacts *factIndex, changes *factChanges) *relationshipIndex {
	if !changes.relationshipsChanged() {
		return i
	}
	// Published relationship indexes are immutable. Each family below clones
	// only when its dependency keys changed, preserving all unrelated maps.
	next := *i
	affectedInterfaces := maps.Clone(changes.interfaces)
	for identity := range changes.namedTypeMethodIDs {
		for _, key := range oldFacts.interfacesByMethod[identity] {
			addOwnedSymbolID(&affectedInterfaces, key)
		}
		for _, key := range newFacts.interfacesByMethod[identity] {
			addOwnedSymbolID(&affectedInterfaces, key)
		}
	}
	if len(affectedInterfaces) > 0 {
		next.typesByInterface = maps.Clone(i.typesByInterface)
		next.interfacesByType = maps.Clone(i.interfacesByType)
		next.interfaceTargets = maps.Clone(i.interfaceTargets)
		next.typeInterfaces = maps.Clone(i.typeInterfaces)
		next.updateImplementationRows(oldFacts, newFacts, affectedInterfaces, changes)
	}

	affectedMethods := maps.Clone(changes.interfaceMethods)
	for identity := range changes.concreteMethodIdentities {
		for _, key := range oldFacts.interfaceMethodsByIdentity[identity] {
			addOwnedSymbolID(&affectedMethods, key)
		}
		for _, key := range newFacts.interfaceMethodsByIdentity[identity] {
			addOwnedSymbolID(&affectedMethods, key)
		}
	}
	if len(affectedMethods) > 0 {
		next.concreteMethodsByInterfaceMethod = maps.Clone(i.concreteMethodsByInterfaceMethod)
		next.interfaceMethodsByConcreteMethod = maps.Clone(i.interfaceMethodsByConcreteMethod)
		next.interfaceMethodTargets = maps.Clone(i.interfaceMethodTargets)
		next.concreteMethodInterfaces = maps.Clone(i.concreteMethodInterfaces)
		next.updateMethodRows(oldFacts, newFacts, affectedMethods, changes)
	}
	return &next
}

func implementationCandidates(facts *factIndex, interfaceKey ownedSymbolID) []ownedSymbolID {
	declaration := facts.interfaces[interfaceKey]
	if declaration == nil || declaration.interfaceType == nil {
		return nil
	}
	interfaceType := declaration.interfaceType.Complete()
	if interfaceType.NumMethods() == 0 {
		return nil
	}
	// An implementation must appear in every required-method posting list. The
	// smallest list is therefore a safe superset and minimizes exact checks
	// without allocating a set intersection.
	var candidates []ownedSymbolID
	for method := range interfaceType.Methods() {
		bucket := facts.namedTypesByMethod[method.Id()]
		if len(bucket) == 0 {
			return nil
		}
		if candidates == nil || len(bucket) < len(candidates) {
			candidates = bucket
		}
	}
	// Posting lists are only a prefilter. types.Implements remains authoritative
	// for signatures, pointer method sets, aliases, and promoted methods.
	result := make([]ownedSymbolID, 0, len(candidates))
	for _, candidateKey := range candidates {
		candidate := facts.namedTypes[candidateKey]
		if candidate == nil ||
			(!types.Implements(candidate.named, interfaceType) &&
				!types.Implements(candidate.pointer, interfaceType)) {
			continue
		}
		result = append(result, candidateKey)
	}
	sortOwnedSymbolIDs(result)
	return slices.Compact(result)
}

func methodImplementationCandidates(facts *factIndex, methodKey ownedSymbolID) []ownedSymbolID {
	interfaceMethod, exists := facts.interfaceMethods[methodKey]
	if !exists || interfaceMethod.declaration == nil {
		return nil
	}
	// Id() narrows by package-aware method name; Identical below verifies the
	// complete signature before an edge is published.
	candidates := facts.concreteMethodsByIdentity[interfaceMethod.declaration.identity]
	result := make([]ownedSymbolID, 0, len(candidates))
	for _, candidateKey := range candidates {
		candidate := facts.concreteMethods[candidateKey]
		if candidate == nil || !types.Identical(
			interfaceMethod.declaration.function.Type(),
			candidate.function.Type(),
		) {
			continue
		}
		result = append(result, candidateKey)
	}
	sortOwnedSymbolIDs(result)
	return slices.Compact(result)
}

func (i *relationshipIndex) updateImplementationRows(oldFacts, newFacts *factIndex, affected map[ownedSymbolID]struct{}, changes *factChanges) {
	interfaceLocations := make(map[Location]struct{})
	typeLocations := make(map[Location]struct{})
	keys := mapKeys(affected)
	sortOwnedSymbolIDs(keys)
	for _, interfaceKey := range keys {
		changes.addLens(interfaceKey.symbolID, LensKindImplementations)
		oldTypes := i.typesByInterface[interfaceKey]
		if oldDeclaration := oldFacts.interfaces[interfaceKey]; oldDeclaration != nil {
			interfaceLocations[oldDeclaration.location] = struct{}{}
		}
		for _, typeKey := range oldTypes {
			changes.addLens(typeKey.symbolID, LensKindImplementations)
			if declaration := oldFacts.namedTypes[typeKey]; declaration != nil {
				typeLocations[declaration.location] = struct{}{}
			}
		}

		newTypes := implementationCandidates(newFacts, interfaceKey)
		if newDeclaration := newFacts.interfaces[interfaceKey]; newDeclaration != nil {
			interfaceLocations[newDeclaration.location] = struct{}{}
		}
		for _, typeKey := range newTypes {
			changes.addLens(typeKey.symbolID, LensKindImplementations)
			if declaration := newFacts.namedTypes[typeKey]; declaration != nil {
				typeLocations[declaration.location] = struct{}{}
			}
		}
		i.replaceImplementationRow(interfaceKey, oldTypes, newTypes)
	}
	for location := range interfaceLocations {
		i.rebuildInterfaceTargets(newFacts, location)
	}
	for location := range typeLocations {
		i.rebuildTypeInterfaces(newFacts, location)
	}
}

func (i *relationshipIndex) replaceImplementationRow(interfaceKey ownedSymbolID, oldTypes, newTypes []ownedSymbolID) {
	// Update both directions in one operation. Keeping these maps symmetric is
	// required by the location materializers and is cheaper than rebuilding the
	// global inverse relation after every package edit.
	if len(newTypes) == 0 {
		delete(i.typesByInterface, interfaceKey)
	} else {
		i.typesByInterface[interfaceKey] = newTypes
	}
	changedTypes := append(slices.Clone(oldTypes), newTypes...)
	sortOwnedSymbolIDs(changedTypes)
	changedTypes = slices.Compact(changedTypes)
	for _, typeKey := range changedTypes {
		current := i.interfacesByType[typeKey]
		next := make([]ownedSymbolID, 0, len(current)+1)
		for _, candidate := range current {
			if candidate != interfaceKey {
				next = append(next, candidate)
			}
		}
		if slices.Contains(newTypes, typeKey) {
			next = append(next, interfaceKey)
		}
		sortOwnedSymbolIDs(next)
		next = slices.Compact(next)
		if len(next) == 0 {
			delete(i.interfacesByType, typeKey)
		} else {
			i.interfacesByType[typeKey] = next
		}
	}
}

func (i *relationshipIndex) updateMethodRows(oldFacts, newFacts *factIndex, affected map[ownedSymbolID]struct{}, changes *factChanges) {
	interfaceLocations := make(map[Location]struct{})
	concreteLocations := make(map[Location]struct{})
	keys := mapKeys(affected)
	sortOwnedSymbolIDs(keys)
	for _, methodKey := range keys {
		changes.addLens(methodKey.symbolID, LensKindImplementations)
		oldConcrete := i.concreteMethodsByInterfaceMethod[methodKey]
		if oldDeclaration, ok := oldFacts.interfaceMethods[methodKey]; ok {
			interfaceLocations[oldDeclaration.declaration.location] = struct{}{}
		}
		for _, concreteKey := range oldConcrete {
			changes.addLens(concreteKey.symbolID, LensKindImplementations)
			if declaration := oldFacts.concreteMethods[concreteKey]; declaration != nil {
				concreteLocations[declaration.location] = struct{}{}
			}
		}

		newConcrete := methodImplementationCandidates(newFacts, methodKey)
		if newDeclaration, ok := newFacts.interfaceMethods[methodKey]; ok {
			interfaceLocations[newDeclaration.declaration.location] = struct{}{}
		}
		for _, concreteKey := range newConcrete {
			changes.addLens(concreteKey.symbolID, LensKindImplementations)
			if declaration := newFacts.concreteMethods[concreteKey]; declaration != nil {
				concreteLocations[declaration.location] = struct{}{}
			}
		}
		i.replaceMethodRow(methodKey, oldConcrete, newConcrete)
	}
	for location := range interfaceLocations {
		i.rebuildInterfaceMethodTargets(newFacts, location)
	}
	for location := range concreteLocations {
		i.rebuildConcreteMethodInterfaces(newFacts, location)
	}
}

func (i *relationshipIndex) replaceMethodRow(methodKey ownedSymbolID, oldConcrete, newConcrete []ownedSymbolID) {
	// Maintain the forward and reverse method edges atomically within the new,
	// unpublished index revision.
	if len(newConcrete) == 0 {
		delete(i.concreteMethodsByInterfaceMethod, methodKey)
	} else {
		i.concreteMethodsByInterfaceMethod[methodKey] = newConcrete
	}
	changedConcrete := append(slices.Clone(oldConcrete), newConcrete...)
	sortOwnedSymbolIDs(changedConcrete)
	changedConcrete = slices.Compact(changedConcrete)
	for _, concreteKey := range changedConcrete {
		current := i.interfaceMethodsByConcreteMethod[concreteKey]
		next := make([]ownedSymbolID, 0, len(current)+1)
		for _, candidate := range current {
			if candidate != methodKey {
				next = append(next, candidate)
			}
		}
		if slices.Contains(newConcrete, concreteKey) {
			next = append(next, methodKey)
		}
		sortOwnedSymbolIDs(next)
		next = slices.Compact(next)
		if len(next) == 0 {
			delete(i.interfaceMethodsByConcreteMethod, concreteKey)
		} else {
			i.interfaceMethodsByConcreteMethod[concreteKey] = next
		}
	}
}

func (i *relationshipIndex) rebuildAllOutputs(facts *factIndex) {
	for location := range facts.interfacesByLocation {
		i.rebuildInterfaceTargets(facts, location)
	}
	for location := range facts.namedTypesByLocation {
		i.rebuildTypeInterfaces(facts, location)
	}
	for location := range facts.interfaceMethodsByLocation {
		i.rebuildInterfaceMethodTargets(facts, location)
	}
	for location := range facts.concreteMethodsByLocation {
		i.rebuildConcreteMethodInterfaces(facts, location)
	}
}

func (i *relationshipIndex) rebuildInterfaceTargets(facts *factIndex, location Location) {
	var targets []Location
	for _, interfaceKey := range facts.interfacesByLocation[location] {
		for _, typeKey := range i.typesByInterface[interfaceKey] {
			if declaration := facts.namedTypes[typeKey]; declaration != nil {
				targets = append(targets, declaration.location)
			}
		}
	}
	if len(targets) == 0 {
		delete(i.interfaceTargets, location)
		return
	}
	i.interfaceTargets[location] = uniqueSortedLocations(targets)
}

func (i *relationshipIndex) rebuildTypeInterfaces(facts *factIndex, location Location) {
	var relationships []namedRelationship
	for _, typeKey := range facts.namedTypesByLocation[location] {
		for _, interfaceKey := range i.interfacesByType[typeKey] {
			if declaration := facts.interfaces[interfaceKey]; declaration != nil {
				relationships = append(relationships, namedRelationship{
					name: declaration.name, location: declaration.location,
				})
			}
		}
	}
	if len(relationships) == 0 {
		delete(i.typeInterfaces, location)
		return
	}
	i.typeInterfaces[location] = uniqueSortedRelationships(relationships)
}

func (i *relationshipIndex) rebuildInterfaceMethodTargets(facts *factIndex, location Location) {
	var targets []Location
	for _, methodKey := range facts.interfaceMethodsByLocation[location] {
		for _, concreteKey := range i.concreteMethodsByInterfaceMethod[methodKey] {
			if declaration := facts.concreteMethods[concreteKey]; declaration != nil {
				targets = append(targets, declaration.location)
			}
		}
	}
	if len(targets) == 0 {
		delete(i.interfaceMethodTargets, location)
		return
	}
	i.interfaceMethodTargets[location] = uniqueSortedLocations(targets)
}

func (i *relationshipIndex) rebuildConcreteMethodInterfaces(facts *factIndex, location Location) {
	var relationships []namedRelationship
	for _, concreteKey := range facts.concreteMethodsByLocation[location] {
		for _, methodKey := range i.interfaceMethodsByConcreteMethod[concreteKey] {
			if declaration, ok := facts.interfaceMethods[methodKey]; ok {
				relationships = append(relationships, namedRelationship{
					name:     declaration.interfaceName + "." + declaration.declaration.id.name(),
					location: declaration.declaration.location,
				})
			}
		}
	}
	if len(relationships) == 0 {
		delete(i.concreteMethodInterfaces, location)
		return
	}
	i.concreteMethodInterfaces[location] = uniqueSortedRelationships(relationships)
}

func mapKeys[K comparable, V any](values map[K]V) []K {
	result := make([]K, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	return result
}
