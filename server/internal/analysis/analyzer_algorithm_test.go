package analysis

import (
	"go/token"
	"go/types"
	"reflect"
	"strconv"
	"testing"
)

func TestImplementationRelationshipsMatchDirectSearch(t *testing.T) {
	for _, count := range []int{4, 40} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()
			testImplementationRelationshipsMatchDirectSearch(t, count)
		})
	}
}

func testImplementationRelationshipsMatchDirectSearch(t *testing.T, count int) {
	t.Helper()
	interfaces, concreteTypes := benchmarkTypeDeclarations(count, count)
	concreteTypes[0] = promotedPointerMethodCandidate(
		interfaces[0].interfaceType.Method(0),
		concreteTypes[0].location,
	)

	wantInterfaceTargets := make(map[Location][]Location)
	wantTypeInterfaces := make(map[Location][]namedRelationship)
	collectFactImplementationRelationshipsDirect(
		interfaces,
		concreteTypes,
		wantInterfaceTargets,
		wantTypeInterfaces,
	)

	gotInterfaceTargets := make(map[Location][]Location)
	gotTypeInterfaces := make(map[Location][]namedRelationship)
	collectPackageFactImplementationRelationships(
		map[string]*packageFacts{
			"contracts":       {interfaces: interfaces},
			"implementations": {namedTypes: concreteTypes},
		},
		len(interfaces),
		len(concreteTypes),
		gotInterfaceTargets,
		gotTypeInterfaces,
	)
	if !reflect.DeepEqual(gotInterfaceTargets, wantInterfaceTargets) {
		t.Fatalf(
			"indexed interface targets differ from direct search\ngot:  %#v\nwant: %#v",
			gotInterfaceTargets,
			wantInterfaceTargets,
		)
	}
	if !reflect.DeepEqual(gotTypeInterfaces, wantTypeInterfaces) {
		t.Fatalf(
			"indexed type interfaces differ from direct search\ngot:  %#v\nwant: %#v",
			gotTypeInterfaces,
			wantTypeInterfaces,
		)
	}
}

func promotedPointerMethodCandidate(interfaceMethod *types.Func, location Location) namedTypeFact {
	typePackage := interfaceMethod.Pkg()
	embeddedTypeName := types.NewTypeName(token.NoPos, typePackage, "EmbeddedImplementation", nil)
	embeddedType := types.NewNamed(embeddedTypeName, types.NewStruct(nil, nil), nil)
	receiver := types.NewVar(token.NoPos, typePackage, "", types.NewPointer(embeddedType))
	embeddedType.AddMethod(types.NewFunc(
		token.NoPos,
		typePackage,
		interfaceMethod.Name(),
		types.NewSignatureType(receiver, nil, nil, nil, nil, false),
	))

	embeddedField := types.NewField(
		token.NoPos,
		typePackage,
		embeddedTypeName.Name(),
		embeddedType,
		true,
	)
	outerTypeName := types.NewTypeName(token.NoPos, typePackage, "PromotedImplementation", nil)
	outerType := types.NewNamed(
		outerTypeName,
		types.NewStruct([]*types.Var{embeddedField}, nil),
		nil,
	)

	pointer := types.NewPointer(outerType)
	return namedTypeFact{
		name:             outerTypeName.Name(),
		named:            outerType,
		pointer:          pointer,
		methodIdentities: effectiveMethodIdentities(outerType, pointer),
		location:         location,
	}
}

// collectFactImplementationRelationshipsDirect is the independent Cartesian
// oracle for the production candidate index.
func collectFactImplementationRelationshipsDirect(
	interfaces []interfaceFact,
	concreteTypes []namedTypeFact,
	interfaceTargets map[Location][]Location,
	typeInterfaces map[Location][]namedRelationship,
) {
	for _, interfaceDeclaration := range interfaces {
		interfaceDeclaration.interfaceType.Complete()
		if interfaceDeclaration.interfaceType.NumMethods() == 0 {
			continue
		}
		for _, candidate := range concreteTypes {
			if !types.Implements(candidate.named, interfaceDeclaration.interfaceType) &&
				!types.Implements(candidate.pointer, interfaceDeclaration.interfaceType) {
				continue
			}
			interfaceTargets[interfaceDeclaration.location] = append(
				interfaceTargets[interfaceDeclaration.location],
				candidate.location,
			)
			typeInterfaces[candidate.location] = append(typeInterfaces[candidate.location], namedRelationship{
				name:     interfaceDeclaration.name,
				location: interfaceDeclaration.location,
			})
		}
	}
}
