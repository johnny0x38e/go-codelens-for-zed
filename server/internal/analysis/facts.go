package analysis

import (
	"cmp"
	"encoding/binary"
	"go/types"
	"slices"
	"strings"
)

// symbolKind distinguishes declarations that may share a package, owner, and
// source name while still representing different Go namespaces.
type symbolKind uint8

const (
	symbolInvalid symbolKind = iota
	symbolType
	symbolFunction
	symbolVariable
	symbolConstant
	symbolField
	symbolTypeParameter
	symbolMethod
)

// receiverKind describes the stable part of a selected method receiver. The
// zero value is deliberately invalid so incomplete descriptors are explicit.
type receiverKind uint8

const (
	receiverInvalid receiverKind = iota
	receiverValue
	receiverPointer
	receiverInterface
)

// symbolID is a compact, load-independent identity for a source declaration.
// Its canonical encoding contains package path, owner, name, kind, and ordinal.
// PackageID is intentionally absent: production declarations duplicated in an
// internal test variant must resolve to the same logical symbol.
type symbolID string

func newSymbolID(packagePath, owner, name string, kind symbolKind, ordinal uint32) symbolID {
	if packagePath == "" || name == "" || kind == symbolInvalid {
		return ""
	}

	var encoded strings.Builder
	encoded.Grow(len(packagePath) + len(owner) + len(name) + 8)
	encoded.WriteString(packagePath)
	encoded.WriteByte(0)
	encoded.WriteString(owner)
	encoded.WriteByte(0)
	encoded.WriteString(name)
	encoded.WriteByte(0)
	encoded.WriteByte(byte(kind))
	var ordinalBytes [4]byte
	binary.BigEndian.PutUint32(ordinalBytes[:], ordinal)
	encoded.Write(ordinalBytes[:])
	return symbolID(encoded.String())
}

func (id symbolID) valid() bool {
	return id != ""
}

func (id symbolID) name() string {
	encoded := string(id)
	if len(encoded) < 6 {
		return ""
	}
	identity := encoded[:len(encoded)-6]
	separator := strings.LastIndexByte(identity, 0)
	if separator < 0 {
		return ""
	}
	return identity[separator+1:]
}

type declarationFact struct {
	id       symbolID
	location Location
	showZero bool
}

// directUseFact is the compact common case for a declaration reference.
// Method-dispatch metadata is deliberately kept out of this hot slice.
type directUseFact struct {
	id       symbolID
	location Location
}

// methodUseFact describes a use whose selected receiver can contribute
// references to interface or concrete method declarations.
type methodUseFact struct {
	methodIdentity   string
	receiverID       symbolID
	receiverKind     receiverKind
	unstableReceiver bool
	matchedMethods   []symbolID
	location         Location
}

// methodFact retains only the Go type objects needed for exact relationship
// checks on a declared interface or concrete method.
type methodFact struct {
	id       symbolID
	identity string
	function *types.Func
	receiver types.Type
	location Location
}

// namedTypeFact describes a non-interface named type. methodIdentities is the
// compact union of the methods available through its pointer method set;
// pointer is precomputed once per extraction.
type namedTypeFact struct {
	id               symbolID
	name             string
	named            *types.Named
	pointer          *types.Pointer
	methodIdentities []string
	location         Location
}

// interfaceFact describes a named interface. methods contains only explicit
// source declarations; embedded methods remain available through interfaceType.
type interfaceFact struct {
	id            symbolID
	name          string
	interfaceType *types.Interface
	methods       []methodFact
	location      Location
}

// packageFacts owns semantic facts for one exact go/packages package variant.
// It must never retain syntax, file sets, types.Info, source buffers, or a
// packages.Package. packagePath is the logical Go package path shared by base
// and internal-test variants; packageID preserves their distinct ownership.
type packageFacts struct {
	packageID             string
	packagePath           string
	declarations          []declarationFact
	directUses            []directUseFact
	methodUses            []methodUseFact
	namedTypes            []namedTypeFact
	interfaces            []interfaceFact
	declaredMethods       []methodFact
	relationshipSensitive bool
}

func compareSymbolIDs(left, right symbolID) int {
	return cmp.Compare(left, right)
}

func uniqueSortedSymbolIDs(ids []symbolID) []symbolID {
	if len(ids) == 0 {
		return nil
	}
	slices.SortFunc(ids, compareSymbolIDs)
	return slices.Compact(ids)
}
