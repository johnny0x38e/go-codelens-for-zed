package analysis

// Position is a zero-based UTF-16 LSP position.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range identifies a half-open range in a source document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location identifies a range in a source document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// LensKind determines stable ordering for lenses on the same declaration.
type LensKind uint8

const (
	// LensKindReferences identifies a declaration-reference relationship.
	LensKindReferences LensKind = iota
	// LensKindImplementations identifies an interface-implementation relationship.
	LensKindImplementations
)

// Lens is a semantic relationship rendered as an LSP CodeLens.
type Lens struct {
	Range   Range
	Title   string
	Targets []Location
	Kind    LensKind
}

// Snapshot is an immutable set of semantic lenses grouped by document URI.
type Snapshot struct {
	lensesByURI map[string][]Lens
}

func newSnapshot(lensesByURI map[string][]Lens) Snapshot {
	return Snapshot{lensesByURI: lensesByURI}
}

// Lenses returns an immutable view of the lenses for uri. The returned slice
// and its navigation targets are owned by the Snapshot and must not be modified.
func (s Snapshot) Lenses(uri string) []Lens {
	return s.lensesByURI[uri]
}
