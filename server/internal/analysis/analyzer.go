package analysis

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

type namedTypeDeclaration struct {
	name          string
	namedType     *types.Named
	interfaceType *types.Interface
	location      Location
}

type interfaceMethodDeclaration struct {
	owner    *namedTypeDeclaration
	function *types.Func
	location Location
}

type concreteMethodDeclaration struct {
	function *types.Func
	location Location
}

type referenceDeclaration struct {
	object   types.Object
	location Location
	showZero bool
}

type methodReferenceDeclaration struct {
	function         *types.Func
	interfaceType    *types.Interface
	concreteReceiver types.Type
}

type namedRelationship struct {
	name     string
	location Location
}

type referenceLensData struct {
	showZero bool
	targets  []Location
}

// AnalyzeModule loads all packages in moduleDir and computes exact named type
// implementation relationships and declaration references, including tests.
// Callers must not mutate overlays until AnalyzeModule returns.
func AnalyzeModule(ctx context.Context, moduleDir string, overlays map[string][]byte) (Snapshot, error) {
	return AnalyzeScope(ctx, ModuleAnalysisScope(moduleDir), overlays)
}

// AnalyzeScope loads all packages and test variants in scope together and
// computes relationships across every included module. Callers must not mutate
// overlays until AnalyzeScope returns.
func AnalyzeScope(ctx context.Context, scope AnalysisScope, overlays map[string][]byte) (Snapshot, error) {
	absoluteOverlays, err := canonicalOverlays(overlays)
	if err != nil {
		return Snapshot{}, err
	}

	environment := append([]string(nil), os.Environ()...)
	if scope.WorkFile != "" {
		environment = append(environment, "GOWORK="+scope.WorkFile)
	} else {
		environment = append(environment, "GOWORK=off")
	}

	loaded, err := packages.Load(&packages.Config{
		Context: ctx,
		Dir:     scope.Root,
		Mode:    packages.LoadSyntax,
		Tests:   true,
		Overlay: absoluteOverlays,
		Env:     environment,
	}, scope.Patterns...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load Go analysis scope: %w", err)
	}
	loaded = packagesForAnalysis(loaded)

	locations := newLocationResolver(absoluteOverlays)
	declarations, interfaceMethods, concreteMethods, referenceDeclarations, err := collectDeclarations(loaded, locations)
	if err != nil {
		return Snapshot{}, err
	}
	if len(declarations) == 0 && len(referenceDeclarations) == 0 {
		return newSnapshot(make(map[string][]Lens)), nil
	}

	var (
		interfaces    []*namedTypeDeclaration
		concreteTypes []*namedTypeDeclaration
	)
	for _, declaration := range declarations {
		if declaration.interfaceType != nil {
			interfaces = append(interfaces, declaration)
		} else {
			concreteTypes = append(concreteTypes, declaration)
		}
	}

	interfaceTargets := make(map[Location][]Location)
	typeInterfaces := make(map[Location][]namedRelationship)
	for _, interfaceDeclaration := range interfaces {
		interfaceDeclaration.interfaceType.Complete()
		if interfaceDeclaration.interfaceType.NumMethods() == 0 {
			continue
		}

		for _, candidate := range concreteTypes {
			if !types.Implements(candidate.namedType, interfaceDeclaration.interfaceType) &&
				!types.Implements(types.NewPointer(candidate.namedType), interfaceDeclaration.interfaceType) {
				continue
			}

			interfaceTargets[interfaceDeclaration.location] = append(interfaceTargets[interfaceDeclaration.location], candidate.location)
			typeInterfaces[candidate.location] = append(typeInterfaces[candidate.location], namedRelationship{
				name:     interfaceDeclaration.name,
				location: interfaceDeclaration.location,
			})
		}
	}

	lensesByURI := make(map[string][]Lens)
	referenceTargets, err := collectReferenceTargets(
		loaded,
		referenceDeclarations,
		interfaceMethods,
		concreteMethods,
		locations,
	)
	if err != nil {
		return Snapshot{}, err
	}

	referenceLenses := make(map[Location]referenceLensData)
	for _, declaration := range referenceDeclarations {
		data := referenceLenses[declaration.location]
		data.showZero = data.showZero || declaration.showZero
		data.targets = append(data.targets, referenceTargets[declaration.object]...)
		referenceLenses[declaration.location] = data
	}

	for location, data := range referenceLenses {
		targets := uniqueSortedLocations(data.targets)
		if len(targets) == 0 && !data.showZero {
			continue
		}
		lensesByURI[location.URI] = append(lensesByURI[location.URI], Lens{
			Range:   location.Range,
			Title:   referenceCountTitle(len(targets)),
			Targets: targets,
			Kind:    LensKindReferences,
		})
	}

	for _, location := range uniqueDeclarationLocations(interfaces) {
		targets := uniqueSortedLocations(interfaceTargets[location])
		lensesByURI[location.URI] = append(lensesByURI[location.URI], Lens{
			Range:   location.Range,
			Title:   implementationCountTitle(len(targets)),
			Targets: targets,
			Kind:    LensKindImplementations,
		})
	}

	for _, location := range uniqueDeclarationLocations(concreteTypes) {
		relations := uniqueSortedRelationships(typeInterfaces[location])
		if len(relations) == 0 {
			continue
		}

		names := make([]string, 0, len(relations))
		targets := make([]Location, 0, len(relations))
		for _, relation := range relations {
			names = append(names, relation.name)
			targets = append(targets, relation.location)
		}

		lensesByURI[location.URI] = append(lensesByURI[location.URI], Lens{
			Range:   location.Range,
			Title:   "Implements: " + strings.Join(names, ", "),
			Targets: uniqueSortedLocations(targets),
			Kind:    LensKindImplementations,
		})
	}

	interfaceMethodTargets := make(map[Location][]Location)
	concreteMethodInterfaces := make(map[Location][]namedRelationship)
	concreteMethodsByID := make(map[string][]*concreteMethodDeclaration, len(concreteMethods))
	for _, concreteMethod := range concreteMethods {
		id := concreteMethod.function.Id()
		concreteMethodsByID[id] = append(concreteMethodsByID[id], concreteMethod)
	}

	for _, interfaceMethod := range interfaceMethods {
		for _, concreteMethod := range concreteMethodsByID[interfaceMethod.function.Id()] {
			if !types.Identical(interfaceMethod.function.Type(), concreteMethod.function.Type()) {
				continue
			}

			interfaceMethodTargets[interfaceMethod.location] = append(interfaceMethodTargets[interfaceMethod.location], concreteMethod.location)
			concreteMethodInterfaces[concreteMethod.location] = append(concreteMethodInterfaces[concreteMethod.location], namedRelationship{
				name:     qualifiedMethodName(interfaceMethod),
				location: interfaceMethod.location,
			})
		}
	}

	for _, location := range uniqueInterfaceMethodLocations(interfaceMethods) {
		targets := uniqueSortedLocations(interfaceMethodTargets[location])
		lensesByURI[location.URI] = append(lensesByURI[location.URI], Lens{
			Range:   location.Range,
			Title:   implementationCountTitle(len(targets)),
			Targets: targets,
			Kind:    LensKindImplementations,
		})
	}

	for _, location := range uniqueConcreteMethodLocations(concreteMethods) {
		relations := uniqueSortedRelationships(concreteMethodInterfaces[location])
		if len(relations) == 0 {
			continue
		}

		names := make([]string, 0, len(relations))
		targets := make([]Location, 0, len(relations))
		for _, relation := range relations {
			names = append(names, relation.name)
			targets = append(targets, relation.location)
		}

		lensesByURI[location.URI] = append(lensesByURI[location.URI], Lens{
			Range:   location.Range,
			Title:   "Implements: " + strings.Join(names, ", "),
			Targets: uniqueSortedLocations(targets),
			Kind:    LensKindImplementations,
		})
	}

	for uri := range lensesByURI {
		sort.Slice(lensesByURI[uri], func(i, j int) bool {
			left := lensesByURI[uri][i]
			right := lensesByURI[uri][j]
			if left.Range.Start != right.Range.Start {
				if left.Range.Start.Line != right.Range.Start.Line {
					return left.Range.Start.Line < right.Range.Start.Line
				}

				return left.Range.Start.Character < right.Range.Start.Character
			}
			if left.Kind != right.Kind {
				return left.Kind < right.Kind
			}

			return left.Title < right.Title
		})
	}

	return newSnapshot(lensesByURI), nil
}

func canonicalOverlays(overlays map[string][]byte) (map[string][]byte, error) {
	result := make(map[string][]byte, len(overlays))

	for path, content := range overlays {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("make overlay path absolute: %w", err)
		}
		result[filepath.Clean(absolute)] = content
	}

	return result, nil
}

func packagesForAnalysis(loaded []*packages.Package) []*packages.Package {
	result := loaded[:0]
	for _, loadedPackage := range loaded {
		// packages.Load synthesizes a package main ending in .test to run each
		// test binary. Its generated references are not part of the user's code.
		if loadedPackage.ForTest == "" && loadedPackage.Name == "main" &&
			strings.HasSuffix(loadedPackage.PkgPath, ".test") {
			continue
		}
		result = append(result, loadedPackage)
	}

	clear(loaded[len(result):])

	return result
}

func uniqueDeclarationLocations(declarations []*namedTypeDeclaration) []Location {
	locations := make([]Location, 0, len(declarations))
	for _, declaration := range declarations {
		locations = append(locations, declaration.location)
	}

	return uniqueSortedLocations(locations)
}

func uniqueInterfaceMethodLocations(declarations []*interfaceMethodDeclaration) []Location {
	locations := make([]Location, 0, len(declarations))
	for _, declaration := range declarations {
		locations = append(locations, declaration.location)
	}

	return uniqueSortedLocations(locations)
}

func uniqueConcreteMethodLocations(declarations []*concreteMethodDeclaration) []Location {
	locations := make([]Location, 0, len(declarations))
	for _, declaration := range declarations {
		locations = append(locations, declaration.location)
	}

	return uniqueSortedLocations(locations)
}

func uniqueSortedRelationships(relationships []namedRelationship) []namedRelationship {
	sort.Slice(relationships, func(i, j int) bool {
		if relationships[i].name != relationships[j].name {
			return relationships[i].name < relationships[j].name
		}

		return locationLess(relationships[i].location, relationships[j].location)
	})

	result := relationships[:0]
	for _, relationship := range relationships {
		if len(result) > 0 && result[len(result)-1] == relationship {
			continue
		}
		result = append(result, relationship)
	}

	clear(relationships[len(result):])
	return result
}

func collectDeclarations(
	packagesToInspect []*packages.Package,
	locations *locationResolver,
) (
	[]*namedTypeDeclaration,
	[]*interfaceMethodDeclaration,
	[]*concreteMethodDeclaration,
	[]*referenceDeclaration,
	error,
) {
	var (
		namedTypes            []*namedTypeDeclaration
		interfaceMethods      []*interfaceMethodDeclaration
		concreteMethods       []*concreteMethodDeclaration
		referenceDeclarations []*referenceDeclaration
	)

	for _, loadedPackage := range packagesToInspect {
		if loadedPackage.TypesInfo == nil || loadedPackage.Fset == nil {
			continue
		}

		for _, file := range loadedPackage.Syntax {
			for _, declaration := range file.Decls {
				switch declaration := declaration.(type) {
				case *ast.GenDecl:
					switch declaration.Tok {
					case token.TYPE:
						for _, specification := range declaration.Specs {
							typeSpecification, ok := specification.(*ast.TypeSpec)
							if !ok {
								continue
							}
							object, ok := loadedPackage.TypesInfo.Defs[typeSpecification.Name].(*types.TypeName)
							if !ok {
								continue
							}
							location, err := locations.identLocation(loadedPackage.Fset, typeSpecification.Name)
							if err != nil {
								return nil, nil, nil, nil, err
							}
							_, isInterface := object.Type().Underlying().(*types.Interface)
							referenceDeclarations = append(referenceDeclarations, &referenceDeclaration{
								object:   object,
								location: location,
								showZero: isInterface,
							})
							referenceDeclarations, err = appendFieldListReferenceDeclarations(
								referenceDeclarations,
								loadedPackage,
								typeSpecification.TypeParams,
								locations,
								false,
							)
							if err != nil {
								return nil, nil, nil, nil, err
							}
							if structSyntax, ok := typeSpecification.Type.(*ast.StructType); ok {
								referenceDeclarations, err = appendFieldListReferenceDeclarations(
									referenceDeclarations,
									loadedPackage,
									structSyntax.Fields,
									locations,
									false,
								)
								if err != nil {
									return nil, nil, nil, nil, err
								}
							}
							if object.IsAlias() {
								continue
							}
							namedType, ok := object.Type().(*types.Named)
							if !ok {
								continue
							}
							var interfaceType *types.Interface
							if underlying, ok := namedType.Underlying().(*types.Interface); ok {
								interfaceType = underlying
							}
							namedDeclaration := &namedTypeDeclaration{
								name:          object.Name(),
								namedType:     namedType,
								interfaceType: interfaceType,
								location:      location,
							}
							namedTypes = append(namedTypes, namedDeclaration)

							interfaceSyntax, ok := typeSpecification.Type.(*ast.InterfaceType)
							if !ok {
								continue
							}
							for _, field := range interfaceSyntax.Methods.List {
								for _, name := range field.Names {
									function, ok := loadedPackage.TypesInfo.Defs[name].(*types.Func)
									if !ok {
										continue
									}
									methodLocation, err := locations.identLocation(loadedPackage.Fset, name)
									if err != nil {
										return nil, nil, nil, nil, err
									}
									interfaceMethods = append(interfaceMethods, &interfaceMethodDeclaration{
										owner:    namedDeclaration,
										function: function,
										location: methodLocation,
									})
									referenceDeclarations = append(referenceDeclarations, &referenceDeclaration{
										object:   function,
										location: methodLocation,
										showZero: true,
									})
								}
							}
						}
					case token.CONST, token.VAR:
						for _, specification := range declaration.Specs {
							valueSpecification, ok := specification.(*ast.ValueSpec)
							if !ok {
								continue
							}
							var err error
							referenceDeclarations, err = appendIdentReferenceDeclarations(
								referenceDeclarations,
								loadedPackage,
								valueSpecification.Names,
								locations,
								false,
							)
							if err != nil {
								return nil, nil, nil, nil, err
							}
						}
					}
				case *ast.FuncDecl:
					function, ok := loadedPackage.TypesInfo.Defs[declaration.Name].(*types.Func)
					if !ok {
						continue
					}
					location, err := locations.identLocation(loadedPackage.Fset, declaration.Name)
					if err != nil {
						return nil, nil, nil, nil, err
					}
					referenceDeclarations = append(referenceDeclarations, &referenceDeclaration{
						object:   function,
						location: location,
						showZero: true,
					})
					referenceDeclarations, err = appendFieldListReferenceDeclarations(
						referenceDeclarations,
						loadedPackage,
						declaration.Type.TypeParams,
						locations,
						false,
					)
					if err != nil {
						return nil, nil, nil, nil, err
					}
					if declaration.Recv == nil {
						continue
					}
					concreteMethods = append(concreteMethods, &concreteMethodDeclaration{
						function: function,
						location: location,
					})
				}
			}
		}
	}

	return namedTypes, interfaceMethods, concreteMethods, referenceDeclarations, nil
}

func appendFieldListReferenceDeclarations(
	declarations []*referenceDeclaration,
	loadedPackage *packages.Package,
	fields *ast.FieldList,
	locations *locationResolver,
	showZero bool,
) ([]*referenceDeclaration, error) {
	if fields == nil {
		return declarations, nil
	}

	for _, field := range fields.List {
		var err error
		declarations, err = appendIdentReferenceDeclarations(
			declarations,
			loadedPackage,
			field.Names,
			locations,
			showZero,
		)
		if err != nil {
			return nil, err
		}
	}

	return declarations, nil
}

func appendIdentReferenceDeclarations(
	declarations []*referenceDeclaration,
	loadedPackage *packages.Package,
	identifiers []*ast.Ident,
	locations *locationResolver,
	showZero bool,
) ([]*referenceDeclaration, error) {
	for _, identifier := range identifiers {
		if identifier.Name == "_" {
			continue
		}
		object := loadedPackage.TypesInfo.Defs[identifier]
		if object == nil {
			continue
		}
		location, err := locations.identLocation(loadedPackage.Fset, identifier)
		if err != nil {
			return nil, err
		}
		declarations = append(declarations, &referenceDeclaration{
			object:   object,
			location: location,
			showZero: showZero,
		})
	}

	return declarations, nil
}

func collectReferenceTargets(
	packagesToInspect []*packages.Package,
	declarations []*referenceDeclaration,
	interfaceMethods []*interfaceMethodDeclaration,
	concreteMethods []*concreteMethodDeclaration,
	locations *locationResolver,
) (map[types.Object][]Location, error) {
	requested := make(map[types.Object]struct{}, len(declarations))
	for _, declaration := range declarations {
		requested[declaration.object] = struct{}{}
	}

	methodDeclarationsByID := collectMethodReferenceDeclarations(
		requested,
		interfaceMethods,
		concreteMethods,
	)

	targets := make(map[types.Object][]Location, len(declarations))

	for _, loadedPackage := range packagesToInspect {
		if loadedPackage.TypesInfo == nil || loadedPackage.Fset == nil {
			continue
		}

		var selectionsByIdentifier map[*ast.Ident]*types.Selection
		if len(methodDeclarationsByID) > 0 && len(loadedPackage.TypesInfo.Selections) > 0 {
			selectionsByIdentifier = make(map[*ast.Ident]*types.Selection, len(loadedPackage.TypesInfo.Selections))
			for selector, selection := range loadedPackage.TypesInfo.Selections {
				selectionsByIdentifier[selector.Sel] = selection
			}
		}
		for identifier, object := range loadedPackage.TypesInfo.Uses {
			_, directlyRequested := requested[object]
			usedMethod, isMethod := object.(*types.Func)
			var methodDeclarations []methodReferenceDeclaration

			if isMethod && len(methodDeclarationsByID) > 0 {
				signature, hasSignature := usedMethod.Type().(*types.Signature)
				if !hasSignature || signature.Recv() == nil {
					isMethod = false
				} else {
					methodDeclarations = methodDeclarationsByID[usedMethod.Id()]
				}
			}
			if !directlyRequested && len(methodDeclarations) == 0 {
				continue
			}
			location, err := locations.identLocation(loadedPackage.Fset, identifier)
			if err != nil {
				return nil, err
			}
			if directlyRequested {
				targets[object] = append(targets[object], location)
			}
			if !isMethod {
				continue
			}
			selection := selectionsByIdentifier[identifier]
			for _, declaration := range methodDeclarations {
				if declaration.function == usedMethod ||
					!types.Identical(declaration.function.Type(), usedMethod.Type()) ||
					!methodReferenceMatchesSelection(declaration, selection) {
					continue
				}
				targets[declaration.function] = append(targets[declaration.function], location)
			}
		}
	}

	return targets, nil
}

func collectMethodReferenceDeclarations(
	requested map[types.Object]struct{},
	interfaceMethods []*interfaceMethodDeclaration,
	concreteMethods []*concreteMethodDeclaration,
) map[string][]methodReferenceDeclaration {
	result := make(map[string][]methodReferenceDeclaration)
	for _, method := range interfaceMethods {
		if _, ok := requested[method.function]; !ok {
			continue
		}

		method.owner.interfaceType.Complete()
		result[method.function.Id()] = append(result[method.function.Id()], methodReferenceDeclaration{
			function:      method.function,
			interfaceType: method.owner.interfaceType,
		})
	}

	for _, method := range concreteMethods {
		if _, ok := requested[method.function]; !ok {
			continue
		}
		signature, ok := method.function.Type().(*types.Signature)
		if !ok || signature.Recv() == nil {
			continue
		}
		result[method.function.Id()] = append(result[method.function.Id()], methodReferenceDeclaration{
			function:         method.function,
			concreteReceiver: signature.Recv().Type(),
		})
	}

	return result
}

func methodReferenceMatchesSelection(declaration methodReferenceDeclaration, selection *types.Selection) bool {
	if selection == nil {
		return false
	}

	selectedReceiver := selection.Recv()
	selectedInterface, receiverIsInterface := selectedReceiver.Underlying().(*types.Interface)
	if receiverIsInterface {
		selectedInterface.Complete()
		if declaration.interfaceType != nil {
			return true
		}
		return typeOrPointerImplements(declaration.concreteReceiver, selectedInterface)
	}
	if declaration.interfaceType == nil {
		return false
	}

	return typeOrPointerImplements(selectedReceiver, declaration.interfaceType)
}

func typeOrPointerImplements(candidate types.Type, interfaceType *types.Interface) bool {
	if candidate == nil || interfaceType == nil {
		return false
	}
	if types.Implements(candidate, interfaceType) {
		return true
	}
	if _, isPointer := candidate.Underlying().(*types.Pointer); isPointer {
		return false
	}

	return types.Implements(types.NewPointer(candidate), interfaceType)
}

func qualifiedMethodName(method *interfaceMethodDeclaration) string {
	return method.owner.name + "." + method.function.Name()
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
	sort.Slice(locations, func(i, j int) bool {
		return locationLess(locations[i], locations[j])
	})

	result := locations[:0]
	for _, location := range locations {
		if len(result) > 0 && result[len(result)-1] == location {
			continue
		}
		result = append(result, location)
	}

	clear(locations[len(result):])
	return result
}

func locationLess(left, right Location) bool {
	if left.URI != right.URI {
		return left.URI < right.URI
	}
	if left.Range.Start != right.Range.Start {
		if left.Range.Start.Line != right.Range.Start.Line {
			return left.Range.Start.Line < right.Range.Start.Line
		}
		return left.Range.Start.Character < right.Range.Start.Character
	}
	if left.Range.End.Line != right.Range.End.Line {
		return left.Range.End.Line < right.Range.End.Line
	}

	return left.Range.End.Character < right.Range.End.Character
}
