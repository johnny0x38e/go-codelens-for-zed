package analysis

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
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

// AnalyzeModule loads all packages in moduleDir and computes exact named type
// and interface implementation relationships.
func AnalyzeModule(ctx context.Context, moduleDir string, overlays map[string][]byte) (Snapshot, error) {
	absoluteOverlays, err := canonicalOverlays(overlays)
	if err != nil {
		return Snapshot{}, err
	}
	loaded, err := packages.Load(&packages.Config{
		Context: ctx,
		Dir:     moduleDir,
		Mode:    packages.LoadSyntax,
		Tests:   false,
		Overlay: absoluteOverlays,
	}, "./...")
	if err != nil {
		return Snapshot{}, fmt.Errorf("load Go module: %w", err)
	}

	declarations, interfaceMethods, concreteMethods, err := collectDeclarations(loaded, absoluteOverlays)
	if err != nil {
		return Snapshot{}, err
	}
	if len(declarations) == 0 {
		return newSnapshot(make(map[string][]Lens)), nil
	}

	var interfaces []*namedTypeDeclaration
	var concreteTypes []*namedTypeDeclaration
	for _, declaration := range declarations {
		if declaration.interfaceType != nil {
			interfaces = append(interfaces, declaration)
		} else {
			concreteTypes = append(concreteTypes, declaration)
		}
	}

	interfaceTargets := make(map[*namedTypeDeclaration][]Location)
	typeInterfaces := make(map[*namedTypeDeclaration][]*namedTypeDeclaration)
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
			interfaceTargets[interfaceDeclaration] = append(interfaceTargets[interfaceDeclaration], candidate.location)
			typeInterfaces[candidate] = append(typeInterfaces[candidate], interfaceDeclaration)
		}
	}

	lensesByURI := make(map[string][]Lens)
	for _, declaration := range interfaces {
		targets := uniqueSortedLocations(interfaceTargets[declaration])
		lensesByURI[declaration.location.URI] = append(lensesByURI[declaration.location.URI], Lens{
			Range:   declaration.location.Range,
			Title:   implementationCountTitle(len(targets)),
			Targets: targets,
		})
	}
	for _, declaration := range concreteTypes {
		relations := typeInterfaces[declaration]
		if len(relations) == 0 {
			continue
		}
		sort.Slice(relations, func(i, j int) bool {
			if relations[i].name != relations[j].name {
				return relations[i].name < relations[j].name
			}
			return locationLess(relations[i].location, relations[j].location)
		})
		names := make([]string, 0, len(relations))
		targets := make([]Location, 0, len(relations))
		for _, relation := range relations {
			names = append(names, relation.name)
			targets = append(targets, relation.location)
		}
		lensesByURI[declaration.location.URI] = append(lensesByURI[declaration.location.URI], Lens{
			Range:   declaration.location.Range,
			Title:   "Implements: " + strings.Join(names, ", "),
			Targets: uniqueSortedLocations(targets),
		})
	}

	interfaceMethodTargets := make(map[*interfaceMethodDeclaration][]Location)
	concreteMethodInterfaces := make(map[*concreteMethodDeclaration][]*interfaceMethodDeclaration)
	for _, interfaceMethod := range interfaceMethods {
		for _, concreteMethod := range concreteMethods {
			if interfaceMethod.function.Id() != concreteMethod.function.Id() ||
				!types.Identical(interfaceMethod.function.Type(), concreteMethod.function.Type()) {
				continue
			}
			interfaceMethodTargets[interfaceMethod] = append(interfaceMethodTargets[interfaceMethod], concreteMethod.location)
			concreteMethodInterfaces[concreteMethod] = append(concreteMethodInterfaces[concreteMethod], interfaceMethod)
		}
	}
	for _, interfaceMethod := range interfaceMethods {
		targets := uniqueSortedLocations(interfaceMethodTargets[interfaceMethod])
		lensesByURI[interfaceMethod.location.URI] = append(lensesByURI[interfaceMethod.location.URI], Lens{
			Range:   interfaceMethod.location.Range,
			Title:   implementationCountTitle(len(targets)),
			Targets: targets,
		})
	}
	for _, concreteMethod := range concreteMethods {
		relations := concreteMethodInterfaces[concreteMethod]
		if len(relations) == 0 {
			continue
		}
		sort.Slice(relations, func(i, j int) bool {
			leftName := qualifiedMethodName(relations[i])
			rightName := qualifiedMethodName(relations[j])
			if leftName != rightName {
				return leftName < rightName
			}
			return locationLess(relations[i].location, relations[j].location)
		})
		names := make([]string, 0, len(relations))
		targets := make([]Location, 0, len(relations))
		for _, relation := range relations {
			names = append(names, qualifiedMethodName(relation))
			targets = append(targets, relation.location)
		}
		lensesByURI[concreteMethod.location.URI] = append(lensesByURI[concreteMethod.location.URI], Lens{
			Range:   concreteMethod.location.Range,
			Title:   "Implements: " + strings.Join(names, ", "),
			Targets: uniqueSortedLocations(targets),
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
		result[filepath.Clean(absolute)] = append([]byte(nil), content...)
	}
	return result, nil
}

func collectDeclarations(
	packagesToInspect []*packages.Package,
	overlays map[string][]byte,
) (
	[]*namedTypeDeclaration,
	[]*interfaceMethodDeclaration,
	[]*concreteMethodDeclaration,
	error,
) {
	var namedTypes []*namedTypeDeclaration
	var interfaceMethods []*interfaceMethodDeclaration
	var concreteMethods []*concreteMethodDeclaration
	for _, loadedPackage := range packagesToInspect {
		if loadedPackage.TypesInfo == nil || loadedPackage.Fset == nil {
			continue
		}
		for _, file := range loadedPackage.Syntax {
			for _, declaration := range file.Decls {
				switch declaration := declaration.(type) {
				case *ast.GenDecl:
					if declaration.Tok != token.TYPE {
						continue
					}
					for _, specification := range declaration.Specs {
						typeSpecification, ok := specification.(*ast.TypeSpec)
						if !ok {
							continue
						}
						object, ok := loadedPackage.TypesInfo.Defs[typeSpecification.Name].(*types.TypeName)
						if !ok || object.IsAlias() {
							continue
						}
						namedType, ok := object.Type().(*types.Named)
						if !ok {
							continue
						}
						location, err := identLocation(loadedPackage.Fset, typeSpecification.Name, overlays)
						if err != nil {
							return nil, nil, nil, err
						}
						interfaceType, _ := namedType.Underlying().(*types.Interface)
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
								methodLocation, err := identLocation(loadedPackage.Fset, name, overlays)
								if err != nil {
									return nil, nil, nil, err
								}
								interfaceMethods = append(interfaceMethods, &interfaceMethodDeclaration{
									owner:    namedDeclaration,
									function: function,
									location: methodLocation,
								})
							}
						}
					}
				case *ast.FuncDecl:
					if declaration.Recv == nil {
						continue
					}
					function, ok := loadedPackage.TypesInfo.Defs[declaration.Name].(*types.Func)
					if !ok {
						continue
					}
					location, err := identLocation(loadedPackage.Fset, declaration.Name, overlays)
					if err != nil {
						return nil, nil, nil, err
					}
					concreteMethods = append(concreteMethods, &concreteMethodDeclaration{
						function: function,
						location: location,
					})
				}
			}
		}
	}
	return namedTypes, interfaceMethods, concreteMethods, nil
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

func uniqueSortedLocations(locations []Location) []Location {
	sort.Slice(locations, func(i, j int) bool {
		return locationLess(locations[i], locations[j])
	})
	result := make([]Location, 0, len(locations))
	for _, location := range locations {
		if len(result) > 0 && result[len(result)-1] == location {
			continue
		}
		result = append(result, location)
	}
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
