package analysis

import (
	"reflect"
	"testing"
)

func TestFactIndexReplacesOnlyOwnedPackageContributions(t *testing.T) {
	t.Parallel()

	declarationID := newSymbolID("example.com/index", "", "Value", symbolFunction)
	methodID := newSymbolID("example.com/index", "type:Value", "Run", symbolMethod)
	declarationLocation := benchmarkLocation("declaration", 0)
	oldUseLocation := benchmarkLocation("use", 1)
	peerUseLocation := benchmarkLocation("use", 2)
	newUseLocation := benchmarkLocation("use", 3)

	oldOwner := &packageFacts{
		packageID:   "example.com/index",
		packagePath: "example.com/index",
		declarations: []declarationFact{{
			id:       declarationID,
			location: declarationLocation,
		}},
		directUses: []directUseFact{{id: declarationID, location: oldUseLocation}},
		methodUses: []methodUseFact{{
			matchedMethods: []symbolID{methodID},
			location:       oldUseLocation,
		}},
	}
	peer := &packageFacts{
		packageID:   "example.com/index [example.com/index.test]",
		packagePath: "example.com/index",
		declarations: []declarationFact{{
			id:       declarationID,
			location: declarationLocation,
			showZero: true,
		}},
		directUses: []directUseFact{{id: declarationID, location: peerUseLocation}},
	}

	index := newFactIndex(map[string]*packageFacts{
		oldOwner.packageID: oldOwner,
		peer.packageID:     peer,
	})
	if got := index.declarations[declarationID]; len(got) != 1 || !got[0].showZero {
		t.Fatalf("merged declaration facts = %#v, want one show-zero declaration", got)
	}
	if got, want := index.referenceTargets[declarationID], []Location{
		oldUseLocation,
		peerUseLocation,
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("initial reference targets = %#v, want %#v", got, want)
	}
	if got, want := index.referenceTargets[methodID], []Location{oldUseLocation}; !reflect.DeepEqual(got, want) {
		t.Fatalf("initial method targets = %#v, want %#v", got, want)
	}

	replacement := &packageFacts{
		packageID:    oldOwner.packageID,
		packagePath:  oldOwner.packagePath,
		declarations: oldOwner.declarations,
		directUses:   []directUseFact{{id: declarationID, location: newUseLocation}},
	}
	next, changes, err := index.withPackageFacts(map[string]*packageFacts{
		replacement.packageID: replacement,
	})
	if err != nil {
		t.Fatalf("withPackageFacts() error = %v", err)
	}
	if changes.relationshipsChanged() {
		t.Fatal("use-only replacement reported relationship changes")
	}
	if changes.namedTypes != nil || changes.interfaces != nil ||
		changes.interfaceMethods != nil || changes.concreteMethods != nil ||
		changes.namedTypeMethodIDs != nil || changes.concreteMethodIdentities != nil {
		t.Fatalf("use-only replacement allocated semantic change sets: %#v", changes)
	}
	if got, want := next.referenceTargets[declarationID], []Location{
		peerUseLocation,
		newUseLocation,
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("updated reference targets = %#v, want %#v", got, want)
	}
	if _, exists := next.referenceTargets[methodID]; exists {
		t.Fatalf("removed method-use target %q remains indexed", methodID.name())
	}
	if got, want := index.referenceTargets[declarationID], []Location{
		oldUseLocation,
		peerUseLocation,
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("previous immutable index changed: got %#v, want %#v", got, want)
	}
}
