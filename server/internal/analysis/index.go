package analysis

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
)

// ownedSymbolID distinguishes the same logical symbol contributed by distinct
// go/packages variants, such as a production package and its internal tests.
type ownedSymbolID struct {
	packageID string
	symbolID  symbolID
}

// ownedFact associates a fact pointer with the go/packages variant that owns
// its backing storage. Ownership lets replacement remove one variant without
// disturbing identical symbols contributed by test variants.
type ownedFact[T any] struct {
	packageID string
	value     *T
}

// interfaceMethodFact retains the containing interface name because method
// identities alone are insufficient to render "Implements: I.M".
type interfaceMethodFact struct {
	declaration   *methodFact
	interfaceName string
}

// indexedDeclaration is the minimal declaration data needed after extraction.
type indexedDeclaration struct {
	location Location
	showZero bool
}

// factChanges is the exact semantic surface changed by a package replacement.
// Relationship indexes consume it without scanning unrelated packages.
type factChanges struct {
	namedTypes               map[ownedSymbolID]struct{}
	interfaces               map[ownedSymbolID]struct{}
	interfaceMethods         map[ownedSymbolID]struct{}
	concreteMethods          map[ownedSymbolID]struct{}
	namedTypeMethodIDs       map[string]struct{}
	concreteMethodIdentities map[string]struct{}
	lenses                   map[lensKey]struct{}
}

func (c *factChanges) relationshipsChanged() bool {
	return len(c.namedTypes) > 0 || len(c.interfaces) > 0 ||
		len(c.interfaceMethods) > 0 || len(c.concreteMethods) > 0
}

func (c *factChanges) addLens(id symbolID, kind LensKind) {
	if c.lenses == nil {
		c.lenses = make(map[lensKey]struct{})
	}
	c.lenses[lensKey{declaration: id, kind: kind}] = struct{}{}
}

func (c *factChanges) sortedLensKeys() []lensKey {
	// Retain exact capacity on revision hot paths; iterator growth would add
	// temporary allocations even though the final key count is already known.
	keys := slices.AppendSeq(make([]lensKey, 0, len(c.lenses)), maps.Keys(c.lenses))
	slices.SortFunc(keys, func(left, right lensKey) int {
		if left.declaration != right.declaration {
			return cmp.Compare(left.declaration, right.declaration)
		}
		return cmp.Compare(left.kind, right.kind)
	})
	return keys
}

// factIndex is an immutable, package-owned semantic fact index. Top-level maps
// are copied only for categories touched by a replacement; unaffected buckets
// and their backing arrays remain shared with the previous revision.
type factIndex struct {
	packages map[string]*packageFacts

	declarationBuckets map[symbolID][]ownedFact[declarationFact]
	declarations       map[symbolID][]indexedDeclaration
	directUseBuckets   map[symbolID][]ownedFact[directUseFact]
	methodUseBuckets   map[symbolID][]ownedFact[methodUseFact]
	referenceTargets   map[symbolID][]Location

	namedTypes           map[ownedSymbolID]*namedTypeFact
	namedTypesByLocation map[Location][]ownedSymbolID
	namedTypesByMethod   map[string][]ownedSymbolID

	interfaces           map[ownedSymbolID]*interfaceFact
	interfacesByLocation map[Location][]ownedSymbolID
	interfacesByMethod   map[string][]ownedSymbolID

	interfaceMethods           map[ownedSymbolID]interfaceMethodFact
	interfaceMethodsByLocation map[Location][]ownedSymbolID
	interfaceMethodsByIdentity map[string][]ownedSymbolID

	concreteMethods           map[ownedSymbolID]*methodFact
	concreteMethodsByLocation map[Location][]ownedSymbolID
	concreteMethodsByIdentity map[string][]ownedSymbolID
}

// factIndexCapacities keeps full-index construction close to one allocation per
// top-level map even for multi-module workspaces.
type factIndexCapacities struct {
	packages         int
	declarations     int
	namedTypes       int
	namedTypeMethods int
	interfaces       int
	interfaceMethods int
	concreteMethods  int
}

func newFactIndex(factsByPackage map[string]*packageFacts) *factIndex {
	index := emptyFactIndex(factIndexCapacitiesFor(factsByPackage))
	packageIDs := slices.AppendSeq(make([]string, 0, len(factsByPackage)), maps.Keys(factsByPackage))
	slices.Sort(packageIDs)
	for _, packageID := range packageIDs {
		facts := factsByPackage[packageID]
		if facts == nil {
			continue
		}
		index.packages[packageID] = facts
		index.addAllPackageContributions(facts)
	}
	index.rebuildAllAggregates()
	return index
}

func factIndexCapacitiesFor(factsByPackage map[string]*packageFacts) factIndexCapacities {
	capacities := factIndexCapacities{packages: len(factsByPackage)}
	for _, facts := range factsByPackage {
		if facts == nil {
			continue
		}
		capacities.declarations += len(facts.declarations)
		capacities.namedTypes += len(facts.namedTypes)
		for _, fact := range facts.namedTypes {
			capacities.namedTypeMethods += len(fact.methodIdentities)
		}
		capacities.interfaces += len(facts.interfaces)
		for _, fact := range facts.interfaces {
			capacities.interfaceMethods += len(fact.methods)
		}
		capacities.concreteMethods += len(facts.declaredMethods)
	}
	return capacities
}

func emptyFactIndex(capacities factIndexCapacities) *factIndex {
	return &factIndex{
		packages:                   make(map[string]*packageFacts, capacities.packages),
		declarationBuckets:         make(map[symbolID][]ownedFact[declarationFact], capacities.declarations),
		declarations:               make(map[symbolID][]indexedDeclaration, capacities.declarations),
		directUseBuckets:           make(map[symbolID][]ownedFact[directUseFact]),
		methodUseBuckets:           make(map[symbolID][]ownedFact[methodUseFact]),
		referenceTargets:           make(map[symbolID][]Location),
		namedTypes:                 make(map[ownedSymbolID]*namedTypeFact, capacities.namedTypes),
		namedTypesByLocation:       make(map[Location][]ownedSymbolID, capacities.namedTypes),
		namedTypesByMethod:         make(map[string][]ownedSymbolID, capacities.namedTypeMethods),
		interfaces:                 make(map[ownedSymbolID]*interfaceFact, capacities.interfaces),
		interfacesByLocation:       make(map[Location][]ownedSymbolID, capacities.interfaces),
		interfacesByMethod:         make(map[string][]ownedSymbolID, capacities.interfaceMethods),
		interfaceMethods:           make(map[ownedSymbolID]interfaceMethodFact, capacities.interfaceMethods),
		interfaceMethodsByLocation: make(map[Location][]ownedSymbolID, capacities.interfaceMethods),
		interfaceMethodsByIdentity: make(map[string][]ownedSymbolID, capacities.interfaceMethods),
		concreteMethods:            make(map[ownedSymbolID]*methodFact, capacities.concreteMethods),
		concreteMethodsByLocation:  make(map[Location][]ownedSymbolID, capacities.concreteMethods),
		concreteMethodsByIdentity:  make(map[string][]ownedSymbolID, capacities.concreteMethods),
	}
}

func (i *factIndex) withPackageFacts(replacements map[string]*packageFacts) (*factIndex, factChanges, error) {
	if len(replacements) == 0 {
		return i, factChanges{}, nil
	}
	// The index is immutable once published. Start with a shallow copy, then
	// clone only maps whose package-owned contributions actually changed.
	next := *i
	next.packages = maps.Clone(i.packages)
	var changes factChanges
	var declarationLensCandidates map[symbolID]struct{}
	var referenceLensCandidates map[symbolID]struct{}

	packageIDs := slices.AppendSeq(make([]string, 0, len(replacements)), maps.Keys(replacements))
	slices.Sort(packageIDs)

	var declarationsChanged, directUsesChanged, methodUsesChanged bool
	var namedTypesChanged, interfacesChanged, concreteMethodsChanged bool
	for _, packageID := range packageIDs {
		oldFacts := i.packages[packageID]
		newFacts := replacements[packageID]
		if oldFacts == nil {
			return nil, factChanges{}, fmt.Errorf("cannot replace facts for unknown package %q", packageID)
		}
		if newFacts == nil || newFacts.packageID != packageID {
			return nil, factChanges{}, fmt.Errorf("replacement facts do not match package ID %q", packageID)
		}
		next.packages[packageID] = newFacts

		if !sameBackingSlice(oldFacts.declarations, newFacts.declarations) {
			for _, id := range mergedSymbolIDs(
				uniqueDeclarationIDs(oldFacts.declarations),
				uniqueDeclarationIDs(newFacts.declarations),
			) {
				addSymbolID(&declarationLensCandidates, id)
			}
			if !declarationsChanged {
				next.declarationBuckets = maps.Clone(i.declarationBuckets)
				next.declarations = maps.Clone(i.declarations)
				declarationsChanged = true
			}
			next.replaceDeclarationContributions(packageID, oldFacts, newFacts)
		}
		if !sameBackingSlice(oldFacts.directUses, newFacts.directUses) {
			for _, id := range mergedSymbolIDs(
				uniqueDirectUseIDs(oldFacts.directUses),
				uniqueDirectUseIDs(newFacts.directUses),
			) {
				addSymbolID(&referenceLensCandidates, id)
			}
			if !directUsesChanged {
				next.directUseBuckets = maps.Clone(i.directUseBuckets)
				next.referenceTargets = maps.Clone(i.referenceTargets)
				directUsesChanged = true
			}
			next.replaceDirectUseContributions(packageID, oldFacts, newFacts)
		}
		if !sameBackingSlice(oldFacts.methodUses, newFacts.methodUses) {
			for _, id := range mergedSymbolIDs(
				uniqueMethodTargetIDs(oldFacts.methodUses),
				uniqueMethodTargetIDs(newFacts.methodUses),
			) {
				addSymbolID(&referenceLensCandidates, id)
			}
			if !methodUsesChanged {
				next.methodUseBuckets = maps.Clone(i.methodUseBuckets)
				if !directUsesChanged {
					next.referenceTargets = maps.Clone(i.referenceTargets)
				}
				methodUsesChanged = true
			}
			next.replaceMethodUseContributions(packageID, oldFacts, newFacts)
		}
		if !sameBackingSlice(oldFacts.namedTypes, newFacts.namedTypes) {
			for _, key := range append(namedTypeKeys(oldFacts), namedTypeKeys(newFacts)...) {
				changes.addLens(key.symbolID, LensKindImplementations)
			}
			if !namedTypesChanged {
				next.cloneNamedTypeMaps(i)
				namedTypesChanged = true
			}
			next.replaceNamedTypeContributions(packageID, oldFacts, newFacts, &changes)
		}
		if !sameBackingSlice(oldFacts.interfaces, newFacts.interfaces) {
			for _, key := range append(interfaceKeys(oldFacts), interfaceKeys(newFacts)...) {
				changes.addLens(key.symbolID, LensKindImplementations)
			}
			for _, key := range append(interfaceMethodKeys(oldFacts), interfaceMethodKeys(newFacts)...) {
				changes.addLens(key.symbolID, LensKindImplementations)
			}
			if !interfacesChanged {
				next.cloneInterfaceMaps(i)
				interfacesChanged = true
			}
			next.replaceInterfaceContributions(packageID, oldFacts, newFacts, &changes)
		}
		if !sameBackingSlice(oldFacts.declaredMethods, newFacts.declaredMethods) {
			for _, key := range append(concreteMethodKeys(oldFacts), concreteMethodKeys(newFacts)...) {
				changes.addLens(key.symbolID, LensKindImplementations)
			}
			if !concreteMethodsChanged {
				next.cloneConcreteMethodMaps(i)
				concreteMethodsChanged = true
			}
			next.replaceConcreteMethodContributions(packageID, oldFacts, newFacts, &changes)
		}
	}
	for id := range declarationLensCandidates {
		if slices.Equal(i.declarations[id], next.declarations[id]) {
			continue
		}
		changes.addLens(id, LensKindReferences)
		changes.addLens(id, LensKindImplementations)
	}
	for id := range referenceLensCandidates {
		if !slices.Equal(i.referenceTargets[id], next.referenceTargets[id]) {
			changes.addLens(id, LensKindReferences)
		}
	}
	return &next, changes, nil
}

func mergedSymbolIDs(left, right []symbolID) []symbolID {
	result := make([]symbolID, 0, len(left)+len(right))
	result = append(result, left...)
	result = append(result, right...)
	return uniqueSortedSymbolIDs(result)
}

func addSymbolID(values *map[symbolID]struct{}, id symbolID) {
	if *values == nil {
		*values = make(map[symbolID]struct{})
	}
	(*values)[id] = struct{}{}
}

func sameBackingSlice[T any](left, right []T) bool {
	// packageFacts are immutable after publication. Incremental rechecks reuse
	// an unchanged category's exact slice, so backing-array identity is a
	// constant-time semantic revision check; callers must preserve that contract.
	if len(left) != len(right) {
		return false
	}
	return len(left) == 0 || &left[0] == &right[0]
}

func (i *factIndex) addAllPackageContributions(facts *packageFacts) {
	packageID := facts.packageID
	for index := range facts.declarations {
		fact := &facts.declarations[index]
		i.declarationBuckets[fact.id] = append(
			i.declarationBuckets[fact.id],
			ownedFact[declarationFact]{packageID, fact},
		)
	}
	for index := range facts.directUses {
		fact := &facts.directUses[index]
		i.directUseBuckets[fact.id] = append(i.directUseBuckets[fact.id], ownedFact[directUseFact]{packageID, fact})
	}
	for index := range facts.methodUses {
		fact := &facts.methodUses[index]
		for _, targetID := range fact.matchedMethods {
			i.methodUseBuckets[targetID] = append(
				i.methodUseBuckets[targetID],
				ownedFact[methodUseFact]{packageID, fact},
			)
		}
	}
	for index := range facts.namedTypes {
		fact := &facts.namedTypes[index]
		key := ownedSymbolID{packageID, fact.id}
		i.namedTypes[key] = fact
		i.namedTypesByLocation[fact.location] = append(i.namedTypesByLocation[fact.location], key)
		for _, identity := range fact.methodIdentities {
			i.namedTypesByMethod[identity] = append(i.namedTypesByMethod[identity], key)
		}
	}
	for index := range facts.interfaces {
		fact := &facts.interfaces[index]
		key := ownedSymbolID{packageID, fact.id}
		i.interfaces[key] = fact
		i.interfacesByLocation[fact.location] = append(i.interfacesByLocation[fact.location], key)
		fact.interfaceType.Complete()
		for method := range fact.interfaceType.Methods() {
			i.interfacesByMethod[method.Id()] = append(i.interfacesByMethod[method.Id()], key)
		}
		for methodIndex := range fact.methods {
			method := &fact.methods[methodIndex]
			methodKey := ownedSymbolID{packageID, method.id}
			i.interfaceMethods[methodKey] = interfaceMethodFact{method, fact.name}
			i.interfaceMethodsByLocation[method.location] = append(
				i.interfaceMethodsByLocation[method.location],
				methodKey,
			)
			i.interfaceMethodsByIdentity[method.identity] = append(
				i.interfaceMethodsByIdentity[method.identity],
				methodKey,
			)
		}
	}
	for index := range facts.declaredMethods {
		fact := &facts.declaredMethods[index]
		key := ownedSymbolID{packageID, fact.id}
		i.concreteMethods[key] = fact
		i.concreteMethodsByLocation[fact.location] = append(i.concreteMethodsByLocation[fact.location], key)
		i.concreteMethodsByIdentity[fact.identity] = append(i.concreteMethodsByIdentity[fact.identity], key)
	}
}

func (i *factIndex) rebuildAllAggregates() {
	for id := range i.declarationBuckets {
		i.rebuildDeclarations(id)
	}
	referenceIDs := make(map[symbolID]struct{}, len(i.directUseBuckets)+len(i.methodUseBuckets))
	for id := range i.directUseBuckets {
		referenceIDs[id] = struct{}{}
	}
	for id := range i.methodUseBuckets {
		referenceIDs[id] = struct{}{}
	}
	for id := range referenceIDs {
		i.rebuildReferenceTargets(id)
	}
	i.sortOwnedKeyBuckets()
}

func (i *factIndex) replaceDeclarationContributions(packageID string, oldFacts, newFacts *packageFacts) {
	oldKeys := uniqueDeclarationIDs(oldFacts.declarations)
	additions := groupOwnedFacts(
		packageID,
		newFacts.declarations,
		func(fact *declarationFact) symbolID { return fact.id },
	)
	affected := replaceOwnedFactBuckets(i.declarationBuckets, packageID, oldKeys, additions)
	for _, id := range affected {
		i.rebuildDeclarations(id)
	}
}

func (i *factIndex) replaceDirectUseContributions(packageID string, oldFacts, newFacts *packageFacts) {
	oldKeys := uniqueDirectUseIDs(oldFacts.directUses)
	additions := groupOwnedFacts(
		packageID,
		newFacts.directUses,
		func(fact *directUseFact) symbolID { return fact.id },
	)
	affected := replaceOwnedFactBuckets(i.directUseBuckets, packageID, oldKeys, additions)
	for _, id := range affected {
		i.rebuildReferenceTargets(id)
	}
}

func (i *factIndex) replaceMethodUseContributions(packageID string, oldFacts, newFacts *packageFacts) {
	oldKeys := uniqueMethodTargetIDs(oldFacts.methodUses)
	additions := make(map[symbolID][]ownedFact[methodUseFact])
	for index := range newFacts.methodUses {
		fact := &newFacts.methodUses[index]
		for _, id := range fact.matchedMethods {
			additions[id] = append(additions[id], ownedFact[methodUseFact]{packageID, fact})
		}
	}
	affected := replaceOwnedFactBuckets(i.methodUseBuckets, packageID, oldKeys, additions)
	for _, id := range affected {
		i.rebuildReferenceTargets(id)
	}
}

func (i *factIndex) rebuildDeclarations(id symbolID) {
	bucket := i.declarationBuckets[id]
	if len(bucket) == 0 {
		delete(i.declarations, id)
		return
	}
	result := make([]indexedDeclaration, 0, len(bucket))
	for _, owned := range bucket {
		declaration := indexedDeclaration{
			location: owned.value.location,
			showZero: owned.value.showZero,
		}
		duplicate := false
		for index := range result {
			if result[index].location == declaration.location {
				result[index].showZero = result[index].showZero || declaration.showZero
				duplicate = true
				break
			}
		}
		if !duplicate {
			result = append(result, declaration)
		}
	}
	slices.SortFunc(result, func(left, right indexedDeclaration) int {
		return compareLocations(left.location, right.location)
	})
	i.declarations[id] = result
}

func (i *factIndex) rebuildReferenceTargets(id symbolID) {
	capacity := len(i.directUseBuckets[id]) + len(i.methodUseBuckets[id])
	if capacity == 0 {
		delete(i.referenceTargets, id)
		return
	}
	targets := make([]Location, 0, capacity)
	for _, owned := range i.directUseBuckets[id] {
		targets = append(targets, owned.value.location)
	}
	for _, owned := range i.methodUseBuckets[id] {
		targets = append(targets, owned.value.location)
	}
	i.referenceTargets[id] = uniqueSortedLocations(targets)
}

func (i *factIndex) cloneNamedTypeMaps(previous *factIndex) {
	i.namedTypes = maps.Clone(previous.namedTypes)
	i.namedTypesByLocation = maps.Clone(previous.namedTypesByLocation)
	i.namedTypesByMethod = maps.Clone(previous.namedTypesByMethod)
}

func (i *factIndex) cloneInterfaceMaps(previous *factIndex) {
	i.interfaces = maps.Clone(previous.interfaces)
	i.interfacesByLocation = maps.Clone(previous.interfacesByLocation)
	i.interfacesByMethod = maps.Clone(previous.interfacesByMethod)
	i.interfaceMethods = maps.Clone(previous.interfaceMethods)
	i.interfaceMethodsByLocation = maps.Clone(previous.interfaceMethodsByLocation)
	i.interfaceMethodsByIdentity = maps.Clone(previous.interfaceMethodsByIdentity)
}

func (i *factIndex) cloneConcreteMethodMaps(previous *factIndex) {
	i.concreteMethods = maps.Clone(previous.concreteMethods)
	i.concreteMethodsByLocation = maps.Clone(previous.concreteMethodsByLocation)
	i.concreteMethodsByIdentity = maps.Clone(previous.concreteMethodsByIdentity)
}

func (i *factIndex) replaceNamedTypeContributions(
	packageID string,
	oldFacts, newFacts *packageFacts,
	changes *factChanges,
) {
	oldKeys := namedTypeKeys(oldFacts)
	for _, key := range oldKeys {
		addOwnedSymbolID(&changes.namedTypes, key)
		delete(i.namedTypes, key)
	}
	for index := range newFacts.namedTypes {
		fact := &newFacts.namedTypes[index]
		key := ownedSymbolID{packageID, fact.id}
		addOwnedSymbolID(&changes.namedTypes, key)
		i.namedTypes[key] = fact
	}
	replaceOwnedKeyBuckets(
		i.namedTypesByLocation,
		packageID,
		namedTypeLocations(oldFacts),
		namedTypeKeysByLocation(newFacts),
	)
	replaceOwnedKeyBuckets(
		i.namedTypesByMethod,
		packageID,
		namedTypeMethodIDs(oldFacts),
		namedTypeKeysByMethod(newFacts),
	)
	for _, identity := range append(namedTypeMethodIDs(oldFacts), namedTypeMethodIDs(newFacts)...) {
		addString(&changes.namedTypeMethodIDs, identity)
	}
}

func (i *factIndex) replaceInterfaceContributions(
	packageID string,
	oldFacts, newFacts *packageFacts,
	changes *factChanges,
) {
	for _, key := range interfaceKeys(oldFacts) {
		addOwnedSymbolID(&changes.interfaces, key)
		delete(i.interfaces, key)
	}
	for _, key := range interfaceMethodKeys(oldFacts) {
		addOwnedSymbolID(&changes.interfaceMethods, key)
		delete(i.interfaceMethods, key)
	}
	for index := range newFacts.interfaces {
		fact := &newFacts.interfaces[index]
		key := ownedSymbolID{packageID, fact.id}
		addOwnedSymbolID(&changes.interfaces, key)
		i.interfaces[key] = fact
		for methodIndex := range fact.methods {
			method := &fact.methods[methodIndex]
			methodKey := ownedSymbolID{packageID, method.id}
			addOwnedSymbolID(&changes.interfaceMethods, methodKey)
			i.interfaceMethods[methodKey] = interfaceMethodFact{method, fact.name}
		}
	}
	replaceOwnedKeyBuckets(
		i.interfacesByLocation,
		packageID,
		interfaceLocations(oldFacts),
		interfaceKeysByLocation(newFacts),
	)
	replaceOwnedKeyBuckets(
		i.interfacesByMethod,
		packageID,
		interfaceRequiredMethods(oldFacts),
		interfaceKeysByMethod(newFacts),
	)
	replaceOwnedKeyBuckets(
		i.interfaceMethodsByLocation,
		packageID,
		interfaceMethodLocations(oldFacts),
		interfaceMethodKeysByLocation(newFacts),
	)
	replaceOwnedKeyBuckets(
		i.interfaceMethodsByIdentity,
		packageID,
		interfaceMethodIdentities(oldFacts),
		interfaceMethodKeysByIdentity(newFacts),
	)
}

func (i *factIndex) replaceConcreteMethodContributions(
	packageID string,
	oldFacts, newFacts *packageFacts,
	changes *factChanges,
) {
	for _, key := range concreteMethodKeys(oldFacts) {
		addOwnedSymbolID(&changes.concreteMethods, key)
		delete(i.concreteMethods, key)
	}
	for index := range newFacts.declaredMethods {
		fact := &newFacts.declaredMethods[index]
		key := ownedSymbolID{packageID, fact.id}
		addOwnedSymbolID(&changes.concreteMethods, key)
		addString(&changes.concreteMethodIdentities, fact.identity)
		i.concreteMethods[key] = fact
	}
	for _, fact := range oldFacts.declaredMethods {
		addString(&changes.concreteMethodIdentities, fact.identity)
	}
	replaceOwnedKeyBuckets(i.concreteMethodsByLocation,
		packageID,
		concreteMethodLocations(oldFacts),
		concreteMethodKeysByLocation(newFacts),
	)
	replaceOwnedKeyBuckets(i.concreteMethodsByIdentity,
		packageID,
		concreteMethodIdentities(oldFacts),
		concreteMethodKeysByIdentity(newFacts),
	)
}

func groupOwnedFacts[K comparable, T any](packageID string, facts []T, key func(*T) K) map[K][]ownedFact[T] {
	result := make(map[K][]ownedFact[T])
	for index := range facts {
		fact := &facts[index]
		factKey := key(fact)
		result[factKey] = append(result[factKey], ownedFact[T]{packageID, fact})
	}
	return result
}

func replaceOwnedFactBuckets[K comparable, T any](
	buckets map[K][]ownedFact[T],
	packageID string,
	oldKeys []K,
	additions map[K][]ownedFact[T],
) []K {
	// Only buckets named by the old or new package contribution can change.
	// Unaffected buckets keep their backing arrays shared with the prior index.
	affected := append([]K(nil), oldKeys...)
	seen := make(map[K]struct{}, len(oldKeys)+len(additions))
	for _, key := range oldKeys {
		seen[key] = struct{}{}
	}
	for key := range additions {
		if _, ok := seen[key]; !ok {
			affected = append(affected, key)
			seen[key] = struct{}{}
		}
	}
	for _, key := range affected {
		current := buckets[key]
		next := make([]ownedFact[T], 0, len(current)+len(additions[key]))
		for _, fact := range current {
			if fact.packageID != packageID {
				next = append(next, fact)
			}
		}
		next = append(next, additions[key]...)
		if len(next) == 0 {
			delete(buckets, key)
		} else {
			buckets[key] = next
		}
	}
	return affected
}

func replaceOwnedKeyBuckets[K comparable](
	buckets map[K][]ownedSymbolID,
	packageID string,
	oldKeys []K,
	additions map[K][]ownedSymbolID,
) {
	// Rebuild each affected posting list by filtering the replaced owner and
	// appending its new entries. Other package variants remain untouched.
	seen := make(map[K]struct{}, len(oldKeys)+len(additions))
	for _, key := range oldKeys {
		seen[key] = struct{}{}
	}
	for key := range additions {
		seen[key] = struct{}{}
	}
	for key := range seen {
		current := buckets[key]
		next := make([]ownedSymbolID, 0, len(current)+len(additions[key]))
		for _, candidate := range current {
			if candidate.packageID != packageID {
				next = append(next, candidate)
			}
		}
		next = append(next, additions[key]...)
		sortOwnedSymbolIDs(next)
		if len(next) == 0 {
			delete(buckets, key)
		} else {
			buckets[key] = slices.Compact(next)
		}
	}
}

func (i *factIndex) sortOwnedKeyBuckets() {
	for _, buckets := range []map[Location][]ownedSymbolID{
		i.namedTypesByLocation,
		i.interfacesByLocation,
		i.interfaceMethodsByLocation,
		i.concreteMethodsByLocation,
	} {
		for key, values := range buckets {
			sortOwnedSymbolIDs(values)
			buckets[key] = slices.Compact(values)
		}
	}
	for _, buckets := range []map[string][]ownedSymbolID{
		i.namedTypesByMethod,
		i.interfacesByMethod,
		i.interfaceMethodsByIdentity,
		i.concreteMethodsByIdentity,
	} {
		for key, values := range buckets {
			sortOwnedSymbolIDs(values)
			buckets[key] = slices.Compact(values)
		}
	}
}

func sortOwnedSymbolIDs(values []ownedSymbolID) {
	slices.SortFunc(values, func(left, right ownedSymbolID) int {
		if left.packageID != right.packageID {
			return cmp.Compare(left.packageID, right.packageID)
		}
		return cmp.Compare(left.symbolID, right.symbolID)
	})
}

func uniqueDeclarationIDs(facts []declarationFact) []symbolID {
	result := make([]symbolID, 0, len(facts))
	for _, fact := range facts {
		result = append(result, fact.id)
	}
	return uniqueSortedSymbolIDs(result)
}

func uniqueDirectUseIDs(facts []directUseFact) []symbolID {
	result := make([]symbolID, 0, len(facts))
	for _, fact := range facts {
		result = append(result, fact.id)
	}
	return uniqueSortedSymbolIDs(result)
}

func uniqueMethodTargetIDs(facts []methodUseFact) []symbolID {
	var result []symbolID
	for _, fact := range facts {
		result = append(result, fact.matchedMethods...)
	}
	return uniqueSortedSymbolIDs(result)
}

func namedTypeKeys(facts *packageFacts) []ownedSymbolID {
	result := make([]ownedSymbolID, 0, len(facts.namedTypes))
	for _, declaration := range facts.namedTypes {
		result = append(result, ownedSymbolID{facts.packageID, declaration.id})
	}
	return result
}

func namedTypeLocations(facts *packageFacts) []Location {
	result := make([]Location, 0, len(facts.namedTypes))
	for _, declaration := range facts.namedTypes {
		result = append(result, declaration.location)
	}
	return uniqueLocationKeys(result)
}

func namedTypeMethodIDs(facts *packageFacts) []string {
	var result []string
	for _, declaration := range facts.namedTypes {
		result = append(result, declaration.methodIdentities...)
	}
	return uniqueStringKeys(result)
}

func interfaceKeys(facts *packageFacts) []ownedSymbolID {
	result := make([]ownedSymbolID, 0, len(facts.interfaces))
	for _, declaration := range facts.interfaces {
		result = append(result, ownedSymbolID{facts.packageID, declaration.id})
	}
	return result
}

func interfaceLocations(facts *packageFacts) []Location {
	result := make([]Location, 0, len(facts.interfaces))
	for _, declaration := range facts.interfaces {
		result = append(result, declaration.location)
	}
	return uniqueLocationKeys(result)
}

func interfaceRequiredMethods(facts *packageFacts) []string {
	var result []string
	for _, declaration := range facts.interfaces {
		declaration.interfaceType.Complete()
		for method := range declaration.interfaceType.Methods() {
			result = append(result, method.Id())
		}
	}
	return uniqueStringKeys(result)
}

func interfaceMethodKeys(facts *packageFacts) []ownedSymbolID {
	var result []ownedSymbolID
	for _, declaration := range facts.interfaces {
		for _, method := range declaration.methods {
			result = append(result, ownedSymbolID{facts.packageID, method.id})
		}
	}
	return result
}

func interfaceMethodLocations(facts *packageFacts) []Location {
	var result []Location
	for _, declaration := range facts.interfaces {
		for _, method := range declaration.methods {
			result = append(result, method.location)
		}
	}
	return uniqueLocationKeys(result)
}

func interfaceMethodIdentities(facts *packageFacts) []string {
	var result []string
	for _, declaration := range facts.interfaces {
		for _, method := range declaration.methods {
			result = append(result, method.identity)
		}
	}
	return uniqueStringKeys(result)
}

func concreteMethodKeys(facts *packageFacts) []ownedSymbolID {
	result := make([]ownedSymbolID, 0, len(facts.declaredMethods))
	for _, method := range facts.declaredMethods {
		result = append(result, ownedSymbolID{facts.packageID, method.id})
	}
	return result
}

func concreteMethodLocations(facts *packageFacts) []Location {
	result := make([]Location, 0, len(facts.declaredMethods))
	for _, method := range facts.declaredMethods {
		result = append(result, method.location)
	}
	return uniqueLocationKeys(result)
}

func concreteMethodIdentities(facts *packageFacts) []string {
	result := make([]string, 0, len(facts.declaredMethods))
	for _, method := range facts.declaredMethods {
		result = append(result, method.identity)
	}
	return uniqueStringKeys(result)
}

func namedTypeKeysByLocation(facts *packageFacts) map[Location][]ownedSymbolID {
	result := make(map[Location][]ownedSymbolID)
	for _, declaration := range facts.namedTypes {
		key := ownedSymbolID{facts.packageID, declaration.id}
		result[declaration.location] = append(result[declaration.location], key)
	}
	return result
}

func namedTypeKeysByMethod(facts *packageFacts) map[string][]ownedSymbolID {
	result := make(map[string][]ownedSymbolID)
	for _, declaration := range facts.namedTypes {
		key := ownedSymbolID{facts.packageID, declaration.id}
		for _, identity := range declaration.methodIdentities {
			result[identity] = append(result[identity], key)
		}
	}
	return result
}

func interfaceKeysByLocation(facts *packageFacts) map[Location][]ownedSymbolID {
	result := make(map[Location][]ownedSymbolID)
	for _, declaration := range facts.interfaces {
		key := ownedSymbolID{facts.packageID, declaration.id}
		result[declaration.location] = append(result[declaration.location], key)
	}
	return result
}

func interfaceKeysByMethod(facts *packageFacts) map[string][]ownedSymbolID {
	result := make(map[string][]ownedSymbolID)
	for _, declaration := range facts.interfaces {
		key := ownedSymbolID{facts.packageID, declaration.id}
		declaration.interfaceType.Complete()
		for method := range declaration.interfaceType.Methods() {
			result[method.Id()] = append(result[method.Id()], key)
		}
	}
	return result
}

func interfaceMethodKeysByLocation(facts *packageFacts) map[Location][]ownedSymbolID {
	result := make(map[Location][]ownedSymbolID)
	for _, declaration := range facts.interfaces {
		for _, method := range declaration.methods {
			key := ownedSymbolID{facts.packageID, method.id}
			result[method.location] = append(result[method.location], key)
		}
	}
	return result
}

func interfaceMethodKeysByIdentity(facts *packageFacts) map[string][]ownedSymbolID {
	result := make(map[string][]ownedSymbolID)
	for _, declaration := range facts.interfaces {
		for _, method := range declaration.methods {
			key := ownedSymbolID{facts.packageID, method.id}
			result[method.identity] = append(result[method.identity], key)
		}
	}
	return result
}

func concreteMethodKeysByLocation(facts *packageFacts) map[Location][]ownedSymbolID {
	result := make(map[Location][]ownedSymbolID)
	for _, method := range facts.declaredMethods {
		key := ownedSymbolID{facts.packageID, method.id}
		result[method.location] = append(result[method.location], key)
	}
	return result
}

func concreteMethodKeysByIdentity(facts *packageFacts) map[string][]ownedSymbolID {
	result := make(map[string][]ownedSymbolID)
	for _, method := range facts.declaredMethods {
		key := ownedSymbolID{facts.packageID, method.id}
		result[method.identity] = append(result[method.identity], key)
	}
	return result
}

func uniqueLocationKeys(values []Location) []Location {
	slices.SortFunc(values, compareLocations)
	return slices.Compact(values)
}

func uniqueStringKeys(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}

func addOwnedSymbolID(values *map[ownedSymbolID]struct{}, value ownedSymbolID) {
	if *values == nil {
		*values = make(map[ownedSymbolID]struct{})
	}
	(*values)[value] = struct{}{}
}

func addString(values *map[string]struct{}, value string) {
	if *values == nil {
		*values = make(map[string]struct{})
	}
	(*values)[value] = struct{}{}
}
