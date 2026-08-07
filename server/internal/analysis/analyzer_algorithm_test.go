package analysis

import (
	"go/token"
	"go/types"
	"reflect"
	"testing"
)

func TestIndexedImplementationRelationshipsMatchDirectSearch(t *testing.T) {
	interfaces, concreteTypes := benchmarkTypeDeclarations(40, 40)
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
	collectFactImplementationRelationships(
		interfaces,
		concreteTypes,
		gotInterfaceTargets,
		gotTypeInterfaces,
	)
	if !reflect.DeepEqual(gotInterfaceTargets, wantInterfaceTargets) {
		t.Fatalf("indexed interface targets differ from direct search\ngot:  %#v\nwant: %#v", gotInterfaceTargets, wantInterfaceTargets)
	}
	if !reflect.DeepEqual(gotTypeInterfaces, wantTypeInterfaces) {
		t.Fatalf("indexed type interfaces differ from direct search\ngot:  %#v\nwant: %#v", gotTypeInterfaces, wantTypeInterfaces)
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
