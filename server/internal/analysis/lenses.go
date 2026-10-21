package analysis

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// namedRelationship keeps the display name and navigation target together.
// The name cannot be recovered from Location when a snapshot is materialized,
// so retaining this small value avoids keeping go/types objects alive.
type namedRelationship struct {
	name     string
	location Location
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

func uniqueSortedRelationships(relationships []namedRelationship) []namedRelationship {
	slices.SortFunc(relationships, func(left, right namedRelationship) int {
		if left.name != right.name {
			return cmp.Compare(left.name, right.name)
		}

		return compareLocations(left.location, right.location)
	})

	return slices.Compact(relationships)
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

	return slices.Compact(locations)
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
