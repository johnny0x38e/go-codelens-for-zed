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

// lensKey identifies one independently replaceable lens family for a logical
// declaration. It lets incremental snapshots update exact URI slices without
// comparing rendered titles or navigation targets.
type lensKey struct {
	declaration symbolID
	kind        LensKind
}

// snapshotData is immutable after construction. keysByURI is present on
// incremental snapshots and kept parallel to lensesByURI; one-shot snapshots
// omit it to avoid retaining data they will never update.
type snapshotData struct {
	lensesByURI map[string][]Lens
	keysByURI   map[string][]lensKey
}

var emptySnapshotData = &snapshotData{
	lensesByURI: map[string][]Lens{},
}

// Snapshot is an immutable set of semantic lenses grouped by document URI.
// files retains the cheap on-disk identity recorded for each analysed file so
// callers can detect external rewrites that never produced an LSP notification.
type Snapshot struct {
	data  *snapshotData
	files map[string]fileIdentity
}

func newSnapshot(lensesByURI map[string][]Lens) Snapshot {
	if len(lensesByURI) == 0 {
		return Snapshot{data: emptySnapshotData}
	}
	return Snapshot{data: &snapshotData{lensesByURI: lensesByURI}}
}

func newKeyedSnapshot(lensesByURI map[string][]Lens, keysByURI map[string][]lensKey) Snapshot {
	return Snapshot{data: &snapshotData{
		lensesByURI: lensesByURI,
		keysByURI:   keysByURI,
	}}
}

// Lenses returns an immutable view of the lenses for uri. The returned slice
// and its navigation targets are owned by the Snapshot and must not be modified.
func (s Snapshot) Lenses(uri string) []Lens {
	if s.data == nil {
		return nil
	}
	return s.data.lensesByURI[uri]
}

func (s Snapshot) lensMap() map[string][]Lens {
	if s.data == nil {
		return nil
	}
	return s.data.lensesByURI
}
