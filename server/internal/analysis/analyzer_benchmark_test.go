package analysis

import (
	"context"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/testutil"
)

var benchmarkAnalysisSnapshot Snapshot
var benchmarkPackageFacts map[string]*packageFacts
var benchmarkRelationships *relationshipIndex

var (
	benchmarkInterfaceTargets map[Location][]Location
	benchmarkTypeInterfaces   map[Location][]namedRelationship
)

func BenchmarkAnalyzeModuleRepositoryFixture(b *testing.B) {
	moduleDir := repositoryFixtureModuleDir(b)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		snapshot, err := AnalyzeModule(context.Background(), moduleDir, nil)
		if err != nil {
			b.Fatalf("AnalyzeModule(%q) error = %v", moduleDir, err)
		}
		benchmarkAnalysisSnapshot = snapshot
	}
}

func BenchmarkAnalyzeLoadedPackagesRepositoryFixture(b *testing.B) {
	moduleDir := repositoryFixtureModuleDir(b)
	scope := ModuleAnalysisScope(moduleDir)
	loaded, err := loadScopePackages(context.Background(), scope, nil)
	if err != nil {
		b.Fatalf("loadScopePackages(%q) error = %v", moduleDir, err)
	}
	loaded.packages = packagesForAnalysis(loaded.packages)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		snapshot, analyzeErr := analyzeLoadedPackages(loaded.packages, loaded.sourceByPath)
		if analyzeErr != nil {
			b.Fatalf("analyzeLoadedPackages(%q) error = %v", moduleDir, analyzeErr)
		}
		benchmarkAnalysisSnapshot = snapshot
	}
}

func BenchmarkRetainedPackageFactsScale(b *testing.B) {
	fixture := testutil.WriteModule(b, testutil.ModuleSpec{
		ModulePath:          "example.com/retained-facts",
		PackageCount:        64,
		FilesPerPackage:     1,
		DeclarationsPerFile: 2,
		IncludeTests:        true,
	})
	loaded, err := loadScopePackages(context.Background(), ModuleAnalysisScope(fixture.Root), nil)
	if err != nil {
		b.Fatalf("loadScopePackages(%q) error = %v", fixture.Root, err)
	}
	func() {
		packagesToInspect := packagesForAnalysis(loaded.packages)
		sourceByPath := loaded.sourceByPath

		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			facts, extractErr := extractPackageFacts(
				packagesToInspect,
				newLocationResolver(sourceByPath),
			)
			if extractErr != nil {
				b.Fatalf("extractPackageFacts(%q) error = %v", fixture.Root, extractErr)
			}
			benchmarkPackageFacts = facts
		}
		b.StopTimer()
	}()

	loaded.packages = nil
	loaded.sourceByPath = nil
	runtime.GC()
	runtime.KeepAlive(benchmarkPackageFacts)
}

func repositoryFixtureModuleDir(b *testing.B) string {
	b.Helper()

	_, benchmarkFile, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("runtime.Caller() did not return the benchmark filename")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(benchmarkFile), "..", "..", ".."))
	return filepath.Join(repositoryRoot, "examples", "poc")
}

func BenchmarkCollectImplementationRelationshipsSparse(b *testing.B) {
	interfaces, concreteTypes := benchmarkTypeDeclarations(128, 2048)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		interfaceTargets := make(map[Location][]Location)
		typeInterfaces := make(map[Location][]namedRelationship)
		collectFactImplementationRelationships(
			interfaces,
			concreteTypes,
			interfaceTargets,
			typeInterfaces,
		)
		benchmarkInterfaceTargets = interfaceTargets
		benchmarkTypeInterfaces = typeInterfaces
	}
}

func BenchmarkRelationshipCandidates(b *testing.B) {
	factsByPackage, interfaces, concreteTypes := benchmarkRelationshipFacts(200, 2_000, 3)
	facts := newFactIndex(factsByPackage)

	directTargets := make(map[Location][]Location)
	directInterfaces := make(map[Location][]namedRelationship)
	collectFactImplementationRelationshipsDirect(
		interfaces,
		concreteTypes,
		directTargets,
		directInterfaces,
	)
	indexed := newRelationshipIndex(facts)
	if !reflect.DeepEqual(directTargets, indexed.interfaceTargets) ||
		!reflect.DeepEqual(directInterfaces, indexed.typeInterfaces) {
		b.Fatal("persistent candidate index changed exact implementation relationships")
	}
	if got, want := snapshotFromIndexes(facts, indexed), snapshotFromFacts(factsByPackage); !reflect.DeepEqual(got.lensMap(), want.lensMap()) {
		b.Fatal("persistent candidate index changed the final Snapshot")
	}

	b.Run("FullCartesian", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			interfaceTargets := make(map[Location][]Location)
			typeInterfaces := make(map[Location][]namedRelationship)
			collectFactImplementationRelationshipsDirect(
				interfaces,
				concreteTypes,
				interfaceTargets,
				typeInterfaces,
			)
			benchmarkInterfaceTargets = interfaceTargets
			benchmarkTypeInterfaces = typeInterfaces
		}
		b.ReportMetric(float64(len(interfaces)*len(concreteTypes)), "exact_checks/op")
	})

	b.Run("RarestMethodIndex", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkRelationships = newRelationshipIndex(facts)
		}
		b.ReportMetric(float64(relationshipCandidateChecks(facts)), "exact_checks/op")
	})
}

func benchmarkRelationshipFacts(
	interfaceCount, concreteTypeCount, methodsPerInterface int,
) (map[string]*packageFacts, []interfaceFact, []namedTypeFact) {
	interfaces, concreteTypes := benchmarkMultiMethodTypeDeclarations(
		interfaceCount,
		concreteTypeCount,
		methodsPerInterface,
	)
	interfaceFacts := &packageFacts{
		packageID:   "example.com/benchmark/contracts",
		packagePath: "example.com/benchmark/contracts",
		interfaces:  interfaces,
	}
	for index := range interfaceFacts.interfaces {
		declaration := &interfaceFacts.interfaces[index]
		declaration.id = newSymbolID(
			interfaceFacts.packagePath,
			"",
			declaration.name,
			symbolType,
			0,
		)
		interfaceFacts.declarations = append(interfaceFacts.declarations, declarationFact{
			id:       declaration.id,
			location: declaration.location,
		})
	}
	concreteFacts := &packageFacts{
		packageID:   "example.com/benchmark/implementations",
		packagePath: "example.com/benchmark/implementations",
		namedTypes:  concreteTypes,
	}
	for index := range concreteFacts.namedTypes {
		declaration := &concreteFacts.namedTypes[index]
		declaration.id = newSymbolID(
			concreteFacts.packagePath,
			"",
			declaration.name,
			symbolType,
			0,
		)
		concreteFacts.declarations = append(concreteFacts.declarations, declarationFact{
			id:       declaration.id,
			location: declaration.location,
		})
	}
	return map[string]*packageFacts{
		interfaceFacts.packageID: interfaceFacts,
		concreteFacts.packageID:  concreteFacts,
	}, interfaceFacts.interfaces, concreteFacts.namedTypes
}

func benchmarkMultiMethodTypeDeclarations(
	interfaceCount, concreteTypeCount, methodsPerInterface int,
) ([]interfaceFact, []namedTypeFact) {
	typePackage := types.NewPackage("example.com/benchmark", "benchmark")
	interfaces := make([]interfaceFact, 0, interfaceCount)
	for interfaceIndex := range interfaceCount {
		methods := make([]*types.Func, 0, methodsPerInterface)
		for methodIndex := range methodsPerInterface {
			methods = append(methods, types.NewFunc(
				token.NoPos,
				typePackage,
				benchmarkMethodName(interfaceIndex, methodIndex),
				types.NewSignatureType(nil, nil, nil, nil, nil, false),
			))
		}
		interfaces = append(interfaces, interfaceFact{
			name:          "Interface" + strconv.Itoa(interfaceIndex),
			interfaceType: types.NewInterfaceType(methods, nil).Complete(),
			location:      benchmarkLocation("interface", interfaceIndex),
		})
	}

	concreteTypes := make([]namedTypeFact, 0, concreteTypeCount)
	for concreteIndex := range concreteTypeCount {
		typeName := types.NewTypeName(
			token.NoPos,
			typePackage,
			"Concrete"+strconv.Itoa(concreteIndex),
			nil,
		)
		namedType := types.NewNamed(typeName, types.NewStruct(nil, nil), nil)
		receiver := types.NewVar(token.NoPos, typePackage, "", namedType)
		interfaceIndex := concreteIndex % interfaceCount
		for methodIndex := range methodsPerInterface {
			namedType.AddMethod(types.NewFunc(
				token.NoPos,
				typePackage,
				benchmarkMethodName(interfaceIndex, methodIndex),
				types.NewSignatureType(receiver, nil, nil, nil, nil, false),
			))
		}
		pointer := types.NewPointer(namedType)
		concreteTypes = append(concreteTypes, namedTypeFact{
			name:             typeName.Name(),
			named:            namedType,
			pointer:          pointer,
			methodIdentities: effectiveMethodIdentities(namedType, pointer),
			location:         benchmarkLocation("concrete", concreteIndex),
		})
	}
	return interfaces, concreteTypes
}

func benchmarkMethodName(interfaceIndex, methodIndex int) string {
	return "Method" + strconv.Itoa(interfaceIndex) + "_" + strconv.Itoa(methodIndex)
}

func relationshipCandidateChecks(facts *factIndex) int {
	checks := 0
	for _, declaration := range facts.interfaces {
		interfaceType := declaration.interfaceType.Complete()
		minimum := 0
		for method := range interfaceType.Methods() {
			count := len(facts.namedTypesByMethod[method.Id()])
			if count == 0 {
				minimum = 0
				break
			}
			if minimum == 0 || count < minimum {
				minimum = count
			}
		}
		checks += minimum
	}
	return checks
}

func benchmarkTypeDeclarations(
	interfaceCount int,
	concreteTypeCount int,
) ([]interfaceFact, []namedTypeFact) {
	typePackage := types.NewPackage("example.com/benchmark", "benchmark")
	interfaces := make([]interfaceFact, 0, interfaceCount)
	for index := range interfaceCount {
		methodName := "Method" + strconv.Itoa(index)
		method := types.NewFunc(token.NoPos, typePackage, methodName, types.NewSignatureType(
			nil,
			nil,
			nil,
			nil,
			nil,
			false,
		))
		interfaceType := types.NewInterfaceType([]*types.Func{method}, nil).Complete()
		interfaces = append(interfaces, interfaceFact{
			name:          "Interface" + strconv.Itoa(index),
			interfaceType: interfaceType,
			location:      benchmarkLocation("interface", index),
		})
	}

	concreteTypes := make([]namedTypeFact, 0, concreteTypeCount)
	for index := range concreteTypeCount {
		typeName := types.NewTypeName(
			token.NoPos,
			typePackage,
			"Concrete"+strconv.Itoa(index),
			nil,
		)
		namedType := types.NewNamed(typeName, types.NewStruct(nil, nil), nil)
		receiver := types.NewVar(token.NoPos, typePackage, "", namedType)
		methodName := "Method" + strconv.Itoa(index%interfaceCount)
		namedType.AddMethod(types.NewFunc(token.NoPos, typePackage, methodName, types.NewSignatureType(
			receiver,
			nil,
			nil,
			nil,
			nil,
			false,
		)))
		pointer := types.NewPointer(namedType)
		concreteTypes = append(concreteTypes, namedTypeFact{
			name:             typeName.Name(),
			named:            namedType,
			pointer:          pointer,
			methodIdentities: effectiveMethodIdentities(namedType, pointer),
			location:         benchmarkLocation("concrete", index),
		})
	}

	return interfaces, concreteTypes
}

func benchmarkLocation(kind string, index int) Location {
	return Location{
		URI: "file:///benchmark/" + kind + ".go",
		Range: Range{
			Start: Position{Line: index},
			End:   Position{Line: index, Character: 1},
		},
	}
}
