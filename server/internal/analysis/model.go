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

// Lens is a semantic relationship rendered as an LSP CodeLens.
type Lens struct {
	Range   Range
	Title   string
	Targets []Location
}

// Snapshot is an immutable set of semantic lenses grouped by document URI.
type Snapshot struct {
	lensesByURI map[string][]Lens
}

func newSnapshot(lensesByURI map[string][]Lens) Snapshot {
	return Snapshot{lensesByURI: lensesByURI}
}

// Lenses returns defensive copies of the lenses and their navigation targets.
func (s Snapshot) Lenses(uri string) []Lens {
	lenses := s.lensesByURI[uri]
	result := make([]Lens, len(lenses))
	for i, lens := range lenses {
		result[i] = lens
		result[i].Targets = make([]Location, len(lens.Targets))
		copy(result[i].Targets, lens.Targets)
	}
	return result
}
