package analysis

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/packages"
)

// factExtraction is short-lived scratch state for one coherent type universe.
// It resolves declarations before uses, then discards all AST and types.Info
// indexes after compact packageFacts have been produced.
type factExtraction struct {
	locations         *locationResolver
	factsByPackage    map[string]*packageFacts
	objectIDs         map[types.Object]declarationReference
	declarationIDs    map[symbolID]struct{}
	methodsByIdentity map[string][]methodReferenceFact
}

// declarationReference points into a package-owned declaration slice. Storing
// an index instead of another copy keeps identity updates cheap during use
// extraction.
type declarationReference struct {
	facts *packageFacts
	index int
}

// methodReferenceFact contains just enough type information to match a method
// selection to interface and concrete declarations exactly.
type methodReferenceFact struct {
	id               symbolID
	function         *types.Func
	interfaceType    *types.Interface
	concreteReceiver types.Type
}

// packageFactCapacities is derived from syntax before extraction so the hot
// fact slices generally allocate once per package.
type packageFactCapacities struct {
	declarations    int
	namedTypes      int
	interfaces      int
	declaredMethods int
}

// extractPackageFacts converts one coherent go/packages load into compact
// semantic facts. All syntax, file-set, types.Info, source, and object-index
// references remain extraction-local and are released when this function returns.
func extractPackageFacts(packagesToInspect []*packages.Package, locations *locationResolver) (map[string]*packageFacts, error) {
	return extractPackageFactsWithRetained(packagesToInspect, locations, nil)
}

// extractPackageFactsWithRetained extracts transient packages while using
// stable declaration identities and method candidates from retained packages.
// The retained facts are read only and never become owned by the extraction.
func extractPackageFactsWithRetained(
	packagesToInspect []*packages.Package,
	locations *locationResolver,
	retained map[string]*packageFacts,
) (map[string]*packageFacts, error) {
	extraction := factExtraction{
		locations:      locations,
		factsByPackage: make(map[string]*packageFacts, len(packagesToInspect)),
	}
	declarationCapacity := 0
	declaredMethodCapacity := 0
	// Declarations must be indexed across every package variant before uses
	// are visited; otherwise cross-package uses would depend on traversal order.
	for _, loadedPackage := range packagesToInspect {
		facts := extraction.factsForPackage(loadedPackage)
		declarationCapacity += cap(facts.declarations)
		declaredMethodCapacity += cap(facts.declaredMethods)
	}
	extraction.objectIDs = make(map[types.Object]declarationReference, declarationCapacity)
	extraction.declarationIDs = make(map[symbolID]struct{}, declarationCapacity)
	extraction.methodsByIdentity = make(map[string][]methodReferenceFact, declaredMethodCapacity)
	extraction.seedRetainedFacts(retained)

	for _, loadedPackage := range packagesToInspect {
		if err := extraction.extractDeclarations(loadedPackage); err != nil {
			return nil, err
		}
	}
	for _, loadedPackage := range packagesToInspect {
		if err := extraction.extractUses(loadedPackage); err != nil {
			return nil, err
		}
	}
	return extraction.factsByPackage, nil
}

func (e *factExtraction) seedRetainedFacts(retained map[string]*packageFacts) {
	for _, facts := range retained {
		if facts == nil {
			continue
		}
		for _, declaration := range facts.declarations {
			e.declarationIDs[declaration.id] = struct{}{}
		}
		for _, interfaceDeclaration := range facts.interfaces {
			for _, method := range interfaceDeclaration.methods {
				e.methodsByIdentity[method.identity] = append(
					e.methodsByIdentity[method.identity],
					methodReferenceFact{
						id:            method.id,
						function:      method.function,
						interfaceType: interfaceDeclaration.interfaceType,
					},
				)
			}
		}
		for _, method := range facts.declaredMethods {
			e.methodsByIdentity[method.identity] = append(
				e.methodsByIdentity[method.identity],
				methodReferenceFact{
					id:               method.id,
					function:         method.function,
					concreteReceiver: method.receiver,
				},
			)
		}
	}
}

func (e *factExtraction) extractDeclarations(loadedPackage *packages.Package) error {
	facts := e.factsForPackage(loadedPackage)
	if loadedPackage.TypesInfo == nil || loadedPackage.Fset == nil {
		return nil
	}

	for _, file := range loadedPackage.Syntax {
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.GenDecl:
				if err := e.extractGeneralDeclaration(facts, loadedPackage, declaration); err != nil {
					return err
				}
			case *ast.FuncDecl:
				if err := e.extractFunctionDeclaration(facts, loadedPackage, declaration); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (e *factExtraction) extractGeneralDeclaration(facts *packageFacts, loadedPackage *packages.Package, declaration *ast.GenDecl) error {
	switch declaration.Tok {
	case token.TYPE:
		for _, specification := range declaration.Specs {
			typeSpecification, ok := specification.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if err := e.extractTypeDeclaration(facts, loadedPackage, typeSpecification); err != nil {
				return err
			}
		}
	case token.CONST, token.VAR:
		for _, specification := range declaration.Specs {
			valueSpecification, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if err := e.appendIdentifierDeclarations(
				facts,
				loadedPackage,
				valueSpecification.Names,
				"",
				false,
			); err != nil {
				return err
			}
		}
	}

	return nil
}

func (e *factExtraction) extractTypeDeclaration(facts *packageFacts, loadedPackage *packages.Package, specification *ast.TypeSpec) error {
	object, ok := loadedPackage.TypesInfo.Defs[specification.Name].(*types.TypeName)
	if !ok {
		return nil
	}
	location, err := e.locations.identLocation(loadedPackage.Fset, specification.Name)
	if err != nil {
		return err
	}
	_, isInterface := object.Type().Underlying().(*types.Interface)
	typeID := e.appendDeclaration(facts, object, "", location, isInterface)
	typeOwner := typeOwner(object.Name())
	if err := e.appendFieldListDeclarations(
		facts,
		loadedPackage,
		specification.TypeParams,
		typeOwner,
		false,
	); err != nil {
		return err
	}
	if structure, ok := specification.Type.(*ast.StructType); ok {
		if err := e.appendFieldListDeclarations(
			facts,
			loadedPackage,
			structure.Fields,
			typeOwner,
			false,
		); err != nil {
			return err
		}
	}
	if object.IsAlias() {
		return nil
	}

	named, ok := object.Type().(*types.Named)
	if !ok {
		return nil
	}
	if interfaceType, ok := named.Underlying().(*types.Interface); ok {
		interfaceType.Complete()
		interfaceDeclaration := interfaceFact{
			id:            typeID,
			name:          object.Name(),
			interfaceType: interfaceType,
			location:      location,
		}
		interfaceSyntax, ok := specification.Type.(*ast.InterfaceType)
		if ok {
			interfaceDeclaration.methods = make(
				[]methodFact,
				0,
				fieldListIdentifierCount(interfaceSyntax.Methods),
			)
			for _, field := range interfaceSyntax.Methods.List {
				for _, name := range field.Names {
					function, ok := loadedPackage.TypesInfo.Defs[name].(*types.Func)
					if !ok {
						continue
					}
					methodLocation, locationErr := e.locations.identLocation(loadedPackage.Fset, name)
					if locationErr != nil {
						return locationErr
					}
					methodID := e.appendDeclaration(facts, function, typeOwner, methodLocation, true)
					method := newMethodFact(methodID, function, methodLocation)
					interfaceDeclaration.methods = append(interfaceDeclaration.methods, method)
					e.methodsByIdentity[method.identity] = append(
						e.methodsByIdentity[method.identity],
						methodReferenceFact{
							id:            method.id,
							function:      function,
							interfaceType: interfaceType,
						},
					)
				}
			}
		}
		facts.interfaces = append(facts.interfaces, interfaceDeclaration)
		return nil
	}

	pointer := types.NewPointer(named)
	facts.namedTypes = append(facts.namedTypes, namedTypeFact{
		id:               typeID,
		name:             object.Name(),
		named:            named,
		pointer:          pointer,
		methodIdentities: effectiveMethodIdentities(named, pointer),
		location:         location,
	})
	return nil
}

func (e *factExtraction) extractFunctionDeclaration(facts *packageFacts, loadedPackage *packages.Package, declaration *ast.FuncDecl) error {
	function, ok := loadedPackage.TypesInfo.Defs[declaration.Name].(*types.Func)
	if !ok {
		return nil
	}
	location, err := e.locations.identLocation(loadedPackage.Fset, declaration.Name)
	if err != nil {
		return err
	}

	owner := ""
	typeParameterOwner := functionOwner(function.Name())
	if declaration.Recv != nil {
		receiverName := receiverName(function)
		owner = typeOwner(receiverName)
		typeParameterOwner = methodOwner(receiverName, function.Name())
	}
	functionID := e.appendDeclaration(facts, function, owner, location, true)
	if err := e.appendFieldListDeclarations(
		facts,
		loadedPackage,
		declaration.Type.TypeParams,
		typeParameterOwner,
		false,
	); err != nil {
		return err
	}
	if declaration.Recv == nil {
		return nil
	}

	method := newMethodFact(functionID, function, location)
	facts.declaredMethods = append(facts.declaredMethods, method)
	e.methodsByIdentity[method.identity] = append(
		e.methodsByIdentity[method.identity],
		methodReferenceFact{
			id:               method.id,
			function:         function,
			concreteReceiver: method.receiver,
		},
	)
	return nil
}

func (e *factExtraction) appendFieldListDeclarations(
	facts *packageFacts,
	loadedPackage *packages.Package,
	fields *ast.FieldList,
	owner string,
	showZero bool,
) error {
	if fields == nil {
		return nil
	}
	for _, field := range fields.List {
		if err := e.appendIdentifierDeclarations(
			facts,
			loadedPackage,
			field.Names,
			owner,
			showZero,
		); err != nil {
			return err
		}
	}
	return nil
}

func (e *factExtraction) appendIdentifierDeclarations(
	facts *packageFacts,
	loadedPackage *packages.Package,
	identifiers []*ast.Ident,
	owner string,
	showZero bool,
) error {
	for _, identifier := range identifiers {
		if identifier.Name == "_" {
			continue
		}
		object := loadedPackage.TypesInfo.Defs[identifier]
		if object == nil {
			continue
		}
		location, err := e.locations.identLocation(loadedPackage.Fset, identifier)
		if err != nil {
			return err
		}
		e.appendDeclaration(facts, object, owner, location, showZero)
	}
	return nil
}

func (e *factExtraction) appendDeclaration(
	facts *packageFacts,
	object types.Object,
	owner string,
	location Location,
	showZero bool,
) symbolID {
	id := symbolIDForObject(object, facts.packagePath, owner)
	if !id.valid() {
		return ""
	}
	index := len(facts.declarations)
	facts.declarations = append(facts.declarations, declarationFact{
		id:       id,
		location: location,
		showZero: showZero,
	})
	e.objectIDs[object] = declarationReference{facts: facts, index: index}
	e.declarationIDs[id] = struct{}{}
	return id
}

func (e *factExtraction) extractUses(loadedPackage *packages.Package) error {
	facts := e.factsForPackage(loadedPackage)
	if loadedPackage.TypesInfo == nil || loadedPackage.Fset == nil {
		return nil
	}

	var selectionsByIdentifier map[*ast.Ident]*types.Selection
	if len(loadedPackage.TypesInfo.Selections) > 0 {
		for selector, selection := range loadedPackage.TypesInfo.Selections {
			selectedObject := selection.Obj()
			method, isMethod := selectedObject.(*types.Func)
			field, isField := selectedObject.(*types.Var)
			if (!isMethod || len(e.methodsByIdentity[method.Id()]) == 0) &&
				(!isField || !field.IsField()) {
				continue
			}
			if selectionsByIdentifier == nil {
				selectionsByIdentifier = make(map[*ast.Ident]*types.Selection)
			}
			selectionsByIdentifier[selector.Sel] = selection
		}
	}

	for identifier, object := range loadedPackage.TypesInfo.Uses {
		selection := selectionsByIdentifier[identifier]
		directID, hasDirect := e.directDeclarationID(object)
		if !hasDirect {
			if selectedID, ok := selectedFieldDeclarationID(selection); ok {
				_, hasDirect = e.declarationIDs[selectedID]
				if hasDirect {
					directID = selectedID
				}
			}
		}
		usedMethod, isMethod := object.(*types.Func)
		var methodDeclarations []methodReferenceFact
		if isMethod && len(e.methodsByIdentity) > 0 {
			signature, hasSignature := usedMethod.Type().(*types.Signature)
			if !hasSignature || signature.Recv() == nil {
				isMethod = false
			} else {
				methodDeclarations = e.methodsByIdentity[usedMethod.Id()]
			}
		}
		if !hasDirect && len(methodDeclarations) == 0 {
			continue
		}

		location, err := e.locations.identLocation(loadedPackage.Fset, identifier)
		if err != nil {
			return err
		}
		if hasDirect {
			facts.directUses = append(facts.directUses, directUseFact{
				id:       directID,
				location: location,
			})
		}
		if isMethod {
			use := methodUseFact{
				methodIdentity: usedMethod.Id(),
				location:       location,
			}
			use.receiverID, use.receiverKind, use.unstableReceiver = selectedReceiverDescriptor(
				selection,
				false,
			)
			facts.relationshipSensitive = facts.relationshipSensitive || use.unstableReceiver
			for _, declaration := range methodDeclarations {
				if declaration.function == usedMethod ||
					!types.Identical(declaration.function.Type(), usedMethod.Type()) ||
					!methodReferenceFactMatchesSelection(declaration, selection) {
					continue
				}
				use.matchedMethods = append(use.matchedMethods, declaration.id)
			}
			use.matchedMethods = uniqueSortedSymbolIDs(use.matchedMethods)
			facts.methodUses = append(facts.methodUses, use)
		}
	}

	return nil
}

func (e *factExtraction) directDeclarationID(object types.Object) (symbolID, bool) {
	if reference, ok := e.objectIDs[object]; ok {
		if reference.index < len(reference.facts.declarations) {
			return reference.facts.declarations[reference.index].id, true
		}
	}
	id, ok := packageObjectID(object)
	if !ok {
		return "", false
	}
	_, requested := e.declarationIDs[id]
	return id, requested
}

func selectedFieldDeclarationID(selection *types.Selection) (symbolID, bool) {
	if selection == nil {
		return "", false
	}
	field, ok := selection.Obj().(*types.Var)
	if !ok || !field.IsField() {
		return "", false
	}

	current := types.Unalias(selection.Recv())
	indexes := selection.Index()
	for indexPosition, fieldIndex := range indexes {
		if pointer, ok := current.(*types.Pointer); ok {
			current = types.Unalias(pointer.Elem())
		}
		named, ok := current.(*types.Named)
		if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
			return "", false
		}
		structure, ok := named.Underlying().(*types.Struct)
		if !ok || fieldIndex < 0 || fieldIndex >= structure.NumFields() {
			return "", false
		}
		if indexPosition == len(indexes)-1 {
			return newSymbolID(
				named.Obj().Pkg().Path(),
				typeOwner(named.Obj().Name()),
				field.Name(),
				symbolField,
				0,
			), true
		}
		current = types.Unalias(structure.Field(fieldIndex).Type())
	}
	return "", false
}

func (e *factExtraction) factsForPackage(loadedPackage *packages.Package) *packageFacts {
	packageID := loadedPackage.ID
	if packageID == "" {
		packageID = loadedPackage.PkgPath
	}
	if facts := e.factsByPackage[packageID]; facts != nil {
		return facts
	}
	packagePath := loadedPackage.PkgPath
	if loadedPackage.Types != nil && loadedPackage.Types.Path() != "" {
		packagePath = loadedPackage.Types.Path()
	}
	capacities := countPackageFacts(loadedPackage)
	facts := &packageFacts{
		packageID:       packageID,
		packagePath:     packagePath,
		declarations:    make([]declarationFact, 0, capacities.declarations),
		namedTypes:      make([]namedTypeFact, 0, capacities.namedTypes),
		interfaces:      make([]interfaceFact, 0, capacities.interfaces),
		declaredMethods: make([]methodFact, 0, capacities.declaredMethods),
	}
	e.factsByPackage[packageID] = facts
	return facts
}

func countPackageFacts(loadedPackage *packages.Package) packageFactCapacities {
	var capacities packageFactCapacities
	if loadedPackage.TypesInfo == nil {
		return capacities
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
						capacities.declarations++
						capacities.declarations += fieldListIdentifierCount(typeSpecification.TypeParams)
						if structure, ok := typeSpecification.Type.(*ast.StructType); ok {
							capacities.declarations += fieldListIdentifierCount(structure.Fields)
						}
						if interfaceSyntax, ok := typeSpecification.Type.(*ast.InterfaceType); ok {
							capacities.declarations += fieldListIdentifierCount(interfaceSyntax.Methods)
						}

						object, ok := loadedPackage.TypesInfo.Defs[typeSpecification.Name].(*types.TypeName)
						if !ok || object.IsAlias() {
							continue
						}
						if _, isInterface := object.Type().Underlying().(*types.Interface); isInterface {
							capacities.interfaces++
						} else if _, named := object.Type().(*types.Named); named {
							capacities.namedTypes++
						}
					}
				case token.CONST, token.VAR:
					for _, specification := range declaration.Specs {
						if valueSpecification, ok := specification.(*ast.ValueSpec); ok {
							capacities.declarations += identifierCount(valueSpecification.Names)
						}
					}
				}
			case *ast.FuncDecl:
				capacities.declarations++
				capacities.declarations += fieldListIdentifierCount(declaration.Type.TypeParams)
				if declaration.Recv != nil {
					capacities.declaredMethods++
				}
			}
		}
	}
	return capacities
}

func fieldListIdentifierCount(fields *ast.FieldList) int {
	if fields == nil {
		return 0
	}
	count := 0
	for _, field := range fields.List {
		count += identifierCount(field.Names)
	}
	return count
}

func identifierCount(identifiers []*ast.Ident) int {
	count := 0
	for _, identifier := range identifiers {
		if identifier.Name != "_" {
			count++
		}
	}
	return count
}

func symbolIDForObject(object types.Object, packagePath, owner string) symbolID {
	if object == nil || object.Name() == "_" {
		return ""
	}
	kind := symbolKindForObject(object, owner)
	if kind == symbolInvalid {
		return ""
	}
	if object.Pkg() != nil {
		packagePath = object.Pkg().Path()
	}
	return newSymbolID(packagePath, owner, object.Name(), kind, 0)
}

func symbolKindForObject(object types.Object, owner string) symbolKind {
	switch object := object.(type) {
	case *types.TypeName:
		if _, ok := object.Type().(*types.TypeParam); ok {
			return symbolTypeParameter
		}
		return symbolType
	case *types.Func:
		if owner != "" {
			return symbolMethod
		}
		if signature, ok := object.Type().(*types.Signature); ok && signature.Recv() != nil {
			return symbolMethod
		}
		return symbolFunction
	case *types.Var:
		if object.IsField() {
			return symbolField
		}
		return symbolVariable
	case *types.Const:
		return symbolConstant
	default:
		return symbolInvalid
	}
}

func packageObjectID(object types.Object) (symbolID, bool) {
	if object == nil || object.Pkg() == nil {
		return "", false
	}
	if function, ok := object.(*types.Func); ok {
		if id, method := methodSymbolID(function); method {
			return id, true
		}
	}
	if object.Parent() != object.Pkg().Scope() {
		return "", false
	}
	id := symbolIDForObject(object, object.Pkg().Path(), "")
	return id, id.valid()
}

func methodSymbolID(function *types.Func) (symbolID, bool) {
	if function == nil || function.Pkg() == nil {
		return "", false
	}
	receiver := receiverName(function)
	if receiver == "" {
		return "", false
	}
	return newSymbolID(
		function.Pkg().Path(),
		typeOwner(receiver),
		function.Name(),
		symbolMethod,
		0,
	), true
}

func newMethodFact(id symbolID, function *types.Func, location Location) methodFact {
	method := methodFact{
		id:       id,
		identity: function.Id(),
		function: function,
		location: location,
	}
	if signature, ok := function.Type().(*types.Signature); ok && signature.Recv() != nil {
		method.receiver = signature.Recv().Type()
	}
	return method
}

func effectiveMethodIdentities(named *types.Named, pointer *types.Pointer) []string {
	var identities []string
	if namedTypeHasEmbeddedFields(named) {
		// Embedded fields can promote methods that are absent from
		// named.Methods. Building the full pointer method set is necessary for
		// correctness in that less-common case.
		methodSet := types.NewMethodSet(pointer)
		identities = make([]string, 0, methodSet.Len())
		for selection := range methodSet.Methods() {
			if function, ok := selection.Obj().(*types.Func); ok {
				identities = append(identities, function.Id())
			}
		}
	} else {
		// Without embedding, declared methods are the effective candidates and
		// avoid the considerably more expensive method-set construction.
		identities = make([]string, 0, named.NumMethods())
		for function := range named.Methods() {
			identities = append(identities, function.Id())
		}
	}
	slices.Sort(identities)
	return slices.Compact(identities)
}

func namedTypeHasEmbeddedFields(named *types.Named) bool {
	structure, ok := named.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for field := range structure.Fields() {
		if field.Embedded() {
			return true
		}
	}
	return false
}

func receiverName(function *types.Func) string {
	if function == nil {
		return ""
	}
	signature, ok := function.Type().(*types.Signature)
	if !ok || signature.Recv() == nil {
		return ""
	}
	named := receiverNamedType(signature.Recv().Type())
	if named == nil || named.Obj() == nil {
		return ""
	}
	return named.Obj().Name()
}

func receiverNamedType(receiver types.Type) *types.Named {
	receiver = types.Unalias(receiver)
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = types.Unalias(pointer.Elem())
	}
	named, ok := receiver.(*types.Named)
	if !ok {
		return nil
	}
	return named
}

func selectedReceiverDescriptor(selection *types.Selection, alreadySensitive bool) (symbolID, receiverKind, bool) {
	if selection == nil {
		return "", receiverInvalid, true
	}
	receiver := types.Unalias(selection.Recv())
	kind := receiverValue
	if pointer, ok := receiver.(*types.Pointer); ok {
		kind = receiverPointer
		receiver = types.Unalias(pointer.Elem())
	}
	named, ok := receiver.(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		if _, isInterface := receiver.Underlying().(*types.Interface); isInterface {
			kind = receiverInterface
		}
		return "", kind, true
	}
	if typeArguments := named.TypeArgs(); typeArguments != nil && typeArguments.Len() > 0 {
		alreadySensitive = true
	}
	if _, isInterface := named.Underlying().(*types.Interface); isInterface {
		kind = receiverInterface
	}
	return newSymbolID(
		named.Obj().Pkg().Path(),
		"",
		named.Obj().Name(),
		symbolType,
		0,
	), kind, alreadySensitive
}

func methodReferenceFactMatchesSelection(declaration methodReferenceFact, selection *types.Selection) bool {
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

func typeOwner(name string) string {
	return "type:" + name
}

func functionOwner(name string) string {
	return "func:" + name
}

func methodOwner(receiver, name string) string {
	return "method:" + receiver + "." + name
}
