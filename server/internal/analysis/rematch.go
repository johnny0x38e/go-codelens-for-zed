package analysis

import (
	"go/types"
	"maps"
	"slices"
	"strings"
)

type relationshipResolver struct {
	methodsByIdentity map[string][]methodReferenceFact
	receivers         map[symbolID]types.Type
}

func methodIdentitiesForPackages(
	factsByPackage map[string]*packageFacts,
	included map[string]struct{},
) map[string]struct{} {
	result := make(map[string]struct{})
	for packageID, facts := range factsByPackage {
		if included != nil {
			if _, ok := included[packageID]; !ok {
				continue
			}
		}
		if facts == nil {
			continue
		}
		for _, declaration := range facts.interfaces {
			for _, method := range declaration.methods {
				result[method.identity] = struct{}{}
			}
		}
		for _, method := range facts.declaredMethods {
			result[method.identity] = struct{}{}
		}
	}
	return result
}

func newRelationshipResolver(
	factsByPackage map[string]*packageFacts,
	states map[string]*packageState,
) *relationshipResolver {
	resolver := &relationshipResolver{
		methodsByIdentity: make(map[string][]methodReferenceFact),
		receivers:         make(map[symbolID]types.Type),
	}
	// Prefer production declarations before test variants when logical symbol
	// IDs coincide. This keeps receiver identity deterministic without merging
	// incompatible go/types universes.
	packageIDs := slices.AppendSeq(make([]string, 0, len(states)), maps.Keys(states))
	slices.SortFunc(packageIDs, func(left, right string) int {
		leftTest := states[left].forTest != ""
		rightTest := states[right].forTest != ""
		switch {
		case leftTest == rightTest:
			return strings.Compare(left, right)
		case leftTest:
			return 1
		default:
			return -1
		}
	})
	for _, packageID := range packageIDs {
		facts := factsByPackage[packageID]
		if facts == nil {
			continue
		}
		for _, named := range facts.namedTypes {
			if _, exists := resolver.receivers[named.id]; !exists {
				resolver.receivers[named.id] = named.named
			}
		}
		for _, declaration := range facts.interfaces {
			if _, exists := resolver.receivers[declaration.id]; !exists {
				resolver.receivers[declaration.id] = declaration.interfaceType
			}
			for _, method := range declaration.methods {
				resolver.methodsByIdentity[method.identity] = append(
					resolver.methodsByIdentity[method.identity],
					methodReferenceFact{
						id:            method.id,
						function:      method.function,
						interfaceType: declaration.interfaceType,
					},
				)
			}
		}
		for _, method := range facts.declaredMethods {
			resolver.methodsByIdentity[method.identity] = append(
				resolver.methodsByIdentity[method.identity],
				methodReferenceFact{
					id:               method.id,
					function:         method.function,
					concreteReceiver: method.receiver,
				},
			)
		}
	}
	return resolver
}

func rematchPackageMethodUses(
	facts *packageFacts,
	resolver *relationshipResolver,
	affected map[string]struct{},
) (*packageFacts, bool, error) {
	if facts == nil || len(facts.methodUses) == 0 {
		return facts, false, nil
	}
	var updated []methodUseFact
	for useIndex, use := range facts.methodUses {
		if affected != nil {
			if _, relevant := affected[use.methodIdentity]; !relevant {
				continue
			}
		}
		matched, err := resolver.matchMethodUse(use)
		if err != nil {
			return nil, false, err
		}
		if slices.Equal(matched, use.matchedMethods) {
			continue
		}
		if updated == nil {
			updated = slices.Clone(facts.methodUses)
		}
		updated[useIndex].matchedMethods = matched
	}
	if updated == nil {
		return facts, false, nil
	}
	replacement := *facts
	replacement.methodUses = updated
	return &replacement, true, nil
}

func (r *relationshipResolver) matchMethodUse(use methodUseFact) ([]symbolID, error) {
	candidates := r.methodsByIdentity[use.methodIdentity]
	if len(candidates) == 0 {
		return nil, nil
	}
	if use.unstableReceiver || !use.receiverID.valid() {
		return nil, incrementalFallbackf(
			fallbackSemanticUncertainty,
			"method use %q has no stable receiver",
			use.methodIdentity,
		)
	}
	selectedReceiver := r.receivers[use.receiverID]
	if selectedReceiver == nil {
		return nil, incrementalFallbackf(
			fallbackSemanticUncertainty,
			"method use %q receiver is unavailable",
			use.methodIdentity,
		)
	}
	if use.receiverKind == receiverPointer {
		if _, pointer := selectedReceiver.(*types.Pointer); !pointer {
			selectedReceiver = types.NewPointer(selectedReceiver)
		}
	}
	usedMethod := methodByIdentity(selectedReceiver, use.methodIdentity)
	if usedMethod == nil {
		return nil, incrementalFallbackf(
			fallbackSemanticUncertainty,
			"method %q is absent from its retained receiver",
			use.methodIdentity,
		)
	}
	usedID, _ := methodSymbolID(usedMethod)
	matched := make([]symbolID, 0, len(candidates))
	for _, declaration := range candidates {
		if declaration.id == usedID || declaration.function == nil ||
			!types.Identical(declaration.function.Type(), usedMethod.Type()) ||
			!methodReferenceFactMatchesReceiver(declaration, selectedReceiver) {
			continue
		}
		matched = append(matched, declaration.id)
	}
	return uniqueSortedSymbolIDs(matched), nil
}

func methodByIdentity(receiver types.Type, identity string) *types.Func {
	methodSet := types.NewMethodSet(receiver)
	for selection := range methodSet.Methods() {
		function, ok := selection.Obj().(*types.Func)
		if ok && function.Id() == identity {
			return function
		}
	}
	return nil
}
