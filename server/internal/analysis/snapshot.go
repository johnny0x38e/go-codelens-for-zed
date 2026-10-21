package analysis

import (
	"maps"
	"sort"
)

// applyLensChanges produces a new immutable Snapshot by replacing only lenses
// identified by changedKeys. Untouched URI slices and target slices remain
// shared with previous; callers must continue treating both snapshots as
// immutable.
func applyLensChanges(
	previous Snapshot,
	oldFacts, newFacts *factIndex,
	relationships *relationshipIndex,
	changedKeys []lensKey,
) Snapshot {
	if len(changedKeys) == 0 {
		return previous
	}
	if previous.data == nil || previous.data.keysByURI == nil {
		return snapshotFromIndexes(newFacts, relationships)
	}

	touchedURIs := make(map[string]struct{})
	for _, key := range changedKeys {
		addDeclarationURIs(touchedURIs, oldFacts.declarations[key.declaration])
		addDeclarationURIs(touchedURIs, newFacts.declarations[key.declaration])
	}
	if len(touchedURIs) == 0 {
		return previous
	}

	lensesByURI := maps.Clone(previous.data.lensesByURI)
	keysByURI := maps.Clone(previous.data.keysByURI)
	changed := make(map[lensKey]struct{}, len(changedKeys))
	for _, key := range changedKeys {
		changed[key] = struct{}{}
	}

	for uri := range touchedURIs {
		oldLenses := previous.data.lensesByURI[uri]
		oldKeys := previous.data.keysByURI[uri]
		if len(oldLenses) != len(oldKeys) {
			return snapshotFromIndexes(newFacts, relationships)
		}

		// Filter obsolete keys, append their current representations, then sort
		// once. Untouched URI slices remain shared with the prior snapshot.
		lenses := make([]Lens, 0, len(oldLenses)+len(changedKeys))
		keys := make([]lensKey, 0, cap(lenses))
		for index, key := range oldKeys {
			if _, replace := changed[key]; replace {
				continue
			}
			lenses = append(lenses, oldLenses[index])
			keys = append(keys, key)
		}
		for _, key := range changedKeys {
			lenses, keys = appendLensesForKey(lenses, keys, uri, key, newFacts, relationships)
		}
		if len(lenses) == 0 {
			delete(lensesByURI, uri)
			delete(keysByURI, uri)
			continue
		}
		sort.Sort(keyedLensSorter{lenses: lenses, keys: keys})
		lensesByURI[uri] = lenses
		keysByURI[uri] = keys
	}

	return newKeyedSnapshot(lensesByURI, keysByURI)
}

func addDeclarationURIs(uris map[string]struct{}, declarations []indexedDeclaration) {
	for _, declaration := range declarations {
		uris[declaration.location.URI] = struct{}{}
	}
}

func appendLensesForKey(
	lenses []Lens,
	keys []lensKey,
	uri string,
	key lensKey,
	facts *factIndex,
	relationships *relationshipIndex,
) ([]Lens, []lensKey) {
	for _, declaration := range facts.declarations[key.declaration] {
		if declaration.location.URI != uri {
			continue
		}
		switch key.kind {
		case LensKindReferences:
			targets := facts.referenceTargets[key.declaration]
			if len(targets) == 0 && !declaration.showZero {
				continue
			}
			targets = nonNilLocations(targets)
			lenses = append(lenses, Lens{
				Range:   declaration.location.Range,
				Title:   referenceCountTitle(len(targets)),
				Targets: targets,
				Kind:    LensKindReferences,
			})
			keys = append(keys, key)
		case LensKindImplementations:
			lenses, keys = appendImplementationLenses(
				lenses,
				keys,
				key,
				declaration.location,
				facts,
				relationships,
			)
		}
	}
	return lenses, keys
}

func appendImplementationLenses(
	lenses []Lens,
	keys []lensKey,
	key lensKey,
	location Location,
	facts *factIndex,
	relationships *relationshipIndex,
) ([]Lens, []lensKey) {
	if containsSymbolID(facts.interfacesByLocation[location], key.declaration) {
		targets := nonNilLocations(relationships.interfaceTargets[location])
		lenses = append(lenses, Lens{
			Range:   location.Range,
			Title:   implementationCountTitle(len(targets)),
			Targets: targets,
			Kind:    LensKindImplementations,
		})
		keys = append(keys, key)
	}
	if containsSymbolID(facts.namedTypesByLocation[location], key.declaration) {
		if lens, ok := namedRelationshipLens(location, relationships.typeInterfaces[location]); ok {
			lenses = append(lenses, lens)
			keys = append(keys, key)
		}
	}
	if containsSymbolID(facts.interfaceMethodsByLocation[location], key.declaration) {
		targets := nonNilLocations(relationships.interfaceMethodTargets[location])
		lenses = append(lenses, Lens{
			Range:   location.Range,
			Title:   implementationCountTitle(len(targets)),
			Targets: targets,
			Kind:    LensKindImplementations,
		})
		keys = append(keys, key)
	}
	if containsSymbolID(facts.concreteMethodsByLocation[location], key.declaration) {
		if lens, ok := namedRelationshipLens(location, relationships.concreteMethodInterfaces[location]); ok {
			lenses = append(lenses, lens)
			keys = append(keys, key)
		}
	}
	return lenses, keys
}

func containsSymbolID(keys []ownedSymbolID, id symbolID) bool {
	for _, key := range keys {
		if key.symbolID == id {
			return true
		}
	}
	return false
}

func nonNilLocations(locations []Location) []Location {
	if locations == nil {
		return []Location{}
	}
	return locations
}
