package analysis

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRelationshipIndexMatchesFullSnapshotAcrossPackageReplacement(t *testing.T) {
	t.Parallel()

	interfaces, namedTypes := benchmarkTypeDeclarations(4, 12)
	interfaceFacts := indexedInterfacePackageFacts(interfaces)
	typeFacts := indexedConcretePackageFacts(namedTypes)
	factsByPackage := map[string]*packageFacts{
		interfaceFacts.packageID: interfaceFacts,
		typeFacts.packageID:      typeFacts,
	}
	facts := newFactIndex(factsByPackage)
	relationships := newRelationshipIndex(facts)
	emptyChanges := factChanges{}
	if got := relationships.withFactChanges(facts, facts, &emptyChanges); got != relationships {
		t.Fatal("body-only change did not reuse the complete relationship index")
	}
	got := snapshotFromIndexes(facts, relationships)
	want := snapshotFromFacts(factsByPackage)
	if !reflect.DeepEqual(got.lensMap(), want.lensMap()) {
		t.Fatalf("initial indexed Snapshot differs from full Snapshot\ngot:  %#v\nwant: %#v", got.lensMap(), want.lensMap())
	}

	replacement := indexedConcretePackageFacts(namedTypes[:5])
	nextFacts, changes, err := facts.withPackageFacts(map[string]*packageFacts{
		replacement.packageID: replacement,
	})
	if err != nil {
		t.Fatalf("withPackageFacts() error = %v", err)
	}
	nextRelationships := relationships.withFactChanges(facts, nextFacts, &changes)
	got = snapshotFromIndexes(nextFacts, nextRelationships)
	want = snapshotFromFacts(map[string]*packageFacts{
		interfaceFacts.packageID: interfaceFacts,
		replacement.packageID:    replacement,
	})
	if !reflect.DeepEqual(got.lensMap(), want.lensMap()) {
		t.Fatalf("updated indexed Snapshot differs from full Snapshot\ngot:  %#v\nwant: %#v", got.lensMap(), want.lensMap())
	}
}

func TestRelationshipIndexMatchesFullAnalyzerComplexFixture(t *testing.T) {
	t.Parallel()

	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/indexed-complex

go 1.26
`)
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "contracts", "contracts.go"), `package contracts

type Box[T any] struct {
	Value T
}

type Reader interface {
	Read(Box[int]) int
}

type ReaderAlias interface {
	Read(Box[int]) int
}

type ReadWriter interface {
	Reader
	Write(Box[string])
}

type private interface {
	hidden()
}
`)
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "implementation", "implementation.go"), `package implementation

import "example.com/indexed-complex/contracts"

type Worker struct{}

func (Worker) Read(value contracts.Box[int]) int { return value.Value }
func (*Worker) Write(contracts.Box[string])       {}

type localPrivate interface {
	hidden()
}

type hiddenWorker struct{}

func (hiddenWorker) hidden() {}

func use(reader contracts.Reader) int {
	return reader.Read(contracts.Box[int]{Value: 1})
}

var _ contracts.Reader = Worker{}
var _ contracts.ReaderAlias = Worker{}
var _ contracts.ReadWriter = (*Worker)(nil)
var _ localPrivate = hiddenWorker{}
`)
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "implementation", "implementation_test.go"), `package implementation

import "example.com/indexed-complex/contracts"

type internalWorker struct{}

func (internalWorker) Read(value contracts.Box[int]) int { return value.Value }
`)
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "implementation", "external_test.go"), `package implementation_test

import "example.com/indexed-complex/contracts"

type externalWorker struct{}

func (externalWorker) Read(value contracts.Box[int]) int { return value.Value }
`)

	full, err := analyzeScope(context.Background(), ModuleAnalysisScope(moduleDir), nil, false)
	if err != nil {
		t.Fatalf("full analyzeScope() error = %v", err)
	}
	indexed, err := analyzeScope(context.Background(), ModuleAnalysisScope(moduleDir), nil, true)
	if err != nil {
		t.Fatalf("indexed analyzeScope() error = %v", err)
	}
	if indexed.index == nil || indexed.index.facts == nil || indexed.index.relationships == nil {
		t.Fatal("indexed analyzeScope() did not retain fact and relationship indexes")
	}
	if !reflect.DeepEqual(indexed.snapshot.lensMap(), full.snapshot.lensMap()) {
		t.Fatalf(
			"indexed complex Snapshot differs from full Snapshot\nindexed: %#v\nfull:    %#v",
			indexed.snapshot.lensMap(),
			full.snapshot.lensMap(),
		)
	}
}

func indexedInterfacePackageFacts(interfaces []interfaceFact) *packageFacts {
	facts := &packageFacts{
		packageID:   "example.com/indexed/contracts",
		packagePath: "example.com/indexed/contracts",
		interfaces:  interfaces,
	}
	for index := range facts.interfaces {
		declaration := &facts.interfaces[index]
		declaration.id = newSymbolID(facts.packagePath, "", declaration.name, symbolType, 0)
		facts.declarations = append(facts.declarations, declarationFact{
			id:       declaration.id,
			location: declaration.location,
		})
		for methodIndex := range declaration.interfaceType.NumExplicitMethods() {
			function := declaration.interfaceType.ExplicitMethod(methodIndex)
			method := newMethodFact(
				newSymbolID(facts.packagePath, "type:"+declaration.name, function.Name(), symbolMethod, 0),
				function,
				benchmarkLocation("interface-method", index*10+methodIndex),
			)
			declaration.methods = append(declaration.methods, method)
			facts.declarations = append(facts.declarations, declarationFact{
				id:       method.id,
				location: method.location,
			})
		}
	}
	return facts
}

func indexedConcretePackageFacts(namedTypes []namedTypeFact) *packageFacts {
	facts := &packageFacts{
		packageID:   "example.com/indexed/implementations",
		packagePath: "example.com/indexed/implementations",
		namedTypes:  namedTypes,
	}
	for index := range facts.namedTypes {
		declaration := &facts.namedTypes[index]
		declaration.id = newSymbolID(facts.packagePath, "", declaration.name, symbolType, 0)
		facts.declarations = append(facts.declarations, declarationFact{
			id:       declaration.id,
			location: declaration.location,
		})
		for methodIndex := range declaration.named.NumMethods() {
			function := declaration.named.Method(methodIndex)
			method := newMethodFact(
				newSymbolID(
					facts.packagePath,
					"type:"+declaration.name,
					function.Name(),
					symbolMethod,
					0,
				),
				function,
				benchmarkLocation("concrete-method", index*10+methodIndex),
			)
			facts.declaredMethods = append(facts.declaredMethods, method)
			facts.declarations = append(facts.declarations, declarationFact{
				id:       method.id,
				location: method.location,
			})
		}
	}
	return facts
}
