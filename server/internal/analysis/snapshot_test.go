package analysis

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/testutil"
)

func TestApplyLensChangesCopiesOnlyTouchedURIs(t *testing.T) {
	t.Parallel()

	firstID := newSymbolID("example.com/snapshot", "", "First", symbolFunction, 0)
	secondID := newSymbolID("example.com/snapshot", "", "Second", symbolFunction, 0)
	firstDeclaration := benchmarkLocation("first-declaration", 0)
	secondDeclaration := benchmarkLocation("second-declaration", 0)
	firstOldUse := benchmarkLocation("first-use", 1)
	firstNewUse := benchmarkLocation("first-use", 2)
	secondUse := benchmarkLocation("second-use", 1)

	oldPackage := &packageFacts{
		packageID:   "example.com/snapshot",
		packagePath: "example.com/snapshot",
		declarations: []declarationFact{
			{id: firstID, location: firstDeclaration},
			{id: secondID, location: secondDeclaration},
		},
		directUses: []directUseFact{
			{id: firstID, location: firstOldUse},
			{id: secondID, location: secondUse},
		},
	}
	oldFacts := newFactIndex(map[string]*packageFacts{oldPackage.packageID: oldPackage})
	relationships := newRelationshipIndex(oldFacts)
	oldSnapshot := snapshotFromIndexes(oldFacts, relationships)
	oldFirst := oldSnapshot.Lenses(firstDeclaration.URI)
	oldSecond := oldSnapshot.Lenses(secondDeclaration.URI)
	oldFirstCopy := cloneLensesForTest(oldFirst)
	oldSecondCopy := cloneLensesForTest(oldSecond)

	newPackage := &packageFacts{
		packageID:    oldPackage.packageID,
		packagePath:  oldPackage.packagePath,
		declarations: oldPackage.declarations,
		directUses: []directUseFact{
			{id: firstID, location: firstNewUse},
			{id: secondID, location: secondUse},
		},
	}
	newFacts, changes, err := oldFacts.withPackageFacts(map[string]*packageFacts{
		newPackage.packageID: newPackage,
	})
	if err != nil {
		t.Fatalf("withPackageFacts() error = %v", err)
	}
	newSnapshot := applyLensChanges(
		oldSnapshot,
		oldFacts,
		newFacts,
		relationships,
		changes.sortedLensKeys(),
	)

	if !reflect.DeepEqual(oldSnapshot.Lenses(firstDeclaration.URI), oldFirstCopy) ||
		!reflect.DeepEqual(oldSnapshot.Lenses(secondDeclaration.URI), oldSecondCopy) {
		t.Fatal("applying Lens changes mutated the previous immutable Snapshot")
	}
	newFirst := newSnapshot.Lenses(firstDeclaration.URI)
	newSecond := newSnapshot.Lenses(secondDeclaration.URI)
	if len(oldFirst) == 0 || len(newFirst) == 0 || &oldFirst[0] == &newFirst[0] {
		t.Fatal("touched URI reused its previous Lens backing slice")
	}
	if len(oldSecond) == 0 || len(newSecond) == 0 || &oldSecond[0] != &newSecond[0] {
		t.Fatal("untouched URI did not reuse its previous Lens backing slice")
	}
	if got, want := newFirst[0].Targets, []Location{firstNewUse}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updated reference targets = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(newSecond, oldSecond) {
		t.Fatalf("untouched lenses = %#v, want %#v", newSecond, oldSecond)
	}
}

func cloneLensesForTest(lenses []Lens) []Lens {
	result := make([]Lens, len(lenses))
	for index, lens := range lenses {
		result[index] = lens
		result[index].Targets = append([]Location(nil), lens.Targets...)
	}
	return result
}

func TestIncrementalLifetimeRemainsBounded(t *testing.T) {
	fixture := testutil.WriteModule(t, testutil.ModuleSpec{
		ModulePath:          "example.com/bounded-lifetime",
		PackageCount:        8,
		FilesPerPackage:     1,
		DeclarationsPerFile: 2,
	})
	source, err := os.ReadFile(fixture.EntryFile)
	if err != nil {
		t.Fatalf("read entry file: %v", err)
	}
	scope := ModuleAnalysisScope(fixture.Root)
	engine := NewEngine()
	if _, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil); err != nil {
		t.Fatalf("prime Engine.Analyze() error = %v", err)
	}

	var stable snapshotLifetimeCounts
	for revision := 1; revision <= 1_000; revision++ {
		marker := "A"
		if revision%2 == 0 {
			marker = "B"
		}
		overlay := append(append([]byte(nil), source...), "\n// bounded edit "+marker+"\n"...)
		if _, err := engine.Analyze(
			context.Background(),
			scope,
			NewChangeSet(Change{Path: fixture.EntryFile, Kind: ChangeContent}),
			map[string][]byte{fixture.EntryFile: overlay},
		); err != nil {
			t.Fatalf("Engine.Analyze() revision %d error = %v", revision, err)
		}
		counts := currentSnapshotLifetimeCounts(engine, scope.CacheKey())
		if revision == 1 {
			stable = counts
			continue
		}
		if counts != stable {
			t.Fatalf("revision %d retained counts = %#v, want stable %#v", revision, counts, stable)
		}
	}
}

type snapshotLifetimeCounts struct {
	packages             int
	declarationBuckets   int
	referenceBuckets     int
	semanticDeclarations int
	snapshotURIs         int
	snapshotLenses       int
}

func currentSnapshotLifetimeCounts(engine *Engine, cacheKey string) snapshotLifetimeCounts {
	engine.mu.RLock()
	defer engine.mu.RUnlock()

	state := engine.states[cacheKey]
	counts := snapshotLifetimeCounts{
		packages:           len(state.index.packages),
		declarationBuckets: len(state.index.facts.declarations),
		referenceBuckets:   len(state.index.facts.referenceTargets),
		snapshotURIs:       len(state.snapshot.lensMap()),
	}
	counts.semanticDeclarations = len(state.index.facts.namedTypes) +
		len(state.index.facts.interfaces) +
		len(state.index.facts.interfaceMethods) +
		len(state.index.facts.concreteMethods)
	for _, lenses := range state.snapshot.lensMap() {
		counts.snapshotLenses += len(lenses)
	}
	return counts
}
