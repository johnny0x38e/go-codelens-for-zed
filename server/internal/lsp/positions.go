package lsp

import (
	"cmp"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

const maxPositionSpans = 256

// positionEdit uses the buffer coordinates immediately before one edit. end is
// the position immediately after the replacement, in the resulting buffer.
type positionEdit struct {
	rangeBefore analysis.Range
	end         analysis.Position
}

// positionSpan pairs one unchanged region of the successful snapshot with its
// current editor coordinates. Inserted text has no snapshot coordinates.
type positionSpan struct {
	original analysis.Range
	current  analysis.Range
}

// positionMapping is bounded text-only state, not a history of keystrokes.
// Ordinary typing in an inserted region does not add spans. If fragmentation
// reaches the limit, suppress this file's old locations until a new analysis
// succeeds rather than retaining an unbounded edit log or guessing coordinates.
type positionMapping struct {
	spans   []positionSpan
	scratch []positionSpan
}

func newPositionMapping() *positionMapping {
	// All supported native targets have 64-bit ints. Leave ample arithmetic
	// headroom above any LSP document coordinate for suffix translations.
	end := analysis.Position{Line: int(^uint(0)>>1) / 4}
	span := analysis.Range{End: end}
	return &positionMapping{spans: []positionSpan{{original: span, current: span}}}
}

func (m *positionMapping) apply(edit positionEdit) {
	if len(m.spans) == 0 {
		return
	}
	next := m.scratch[:0]
	for _, span := range m.spans {
		switch {
		case comparePosition(span.current.End, edit.rangeBefore.Start) <= 0:
			next = appendPositionSpan(next, span)
		case comparePosition(span.current.Start, edit.rangeBefore.End) >= 0:
			span.current.Start = translatePosition(span.current.Start, edit.rangeBefore.End, edit.end)
			span.current.End = translatePosition(span.current.End, edit.rangeBefore.End, edit.end)
			next = appendPositionSpan(next, span)
		default:
			if comparePosition(span.current.Start, edit.rangeBefore.Start) < 0 {
				left := span
				left.original.End = translatePosition(edit.rangeBefore.Start, span.current.Start, span.original.Start)
				left.current.End = edit.rangeBefore.Start
				next = appendPositionSpan(next, left)
			}
			if comparePosition(span.current.End, edit.rangeBefore.End) > 0 {
				right := span
				right.original.Start = translatePosition(edit.rangeBefore.End, span.current.Start, span.original.Start)
				right.current.Start = edit.end
				right.current.End = translatePosition(span.current.End, edit.rangeBefore.End, edit.end)
				next = appendPositionSpan(next, right)
			}
		}
		if len(next) > maxPositionSpans {
			m.spans, m.scratch = nil, nil
			return
		}
	}
	m.spans, m.scratch = next, m.spans[:0]
}

func appendPositionSpan(spans []positionSpan, span positionSpan) []positionSpan {
	if len(spans) > 0 {
		last := &spans[len(spans)-1]
		if last.original.End == span.original.Start && last.current.End == span.current.Start {
			last.original.End, last.current.End = span.original.End, span.current.End
			return spans
		}
	}
	return append(spans, span)
}

func (m *positionMapping) mapRange(value analysis.Range) (analysis.Range, bool) {
	if m == nil {
		return value, true
	}
	index := sort.Search(len(m.spans), func(index int) bool {
		return comparePosition(m.spans[index].original.End, value.Start) > 0
	})
	if index == len(m.spans) {
		return analysis.Range{}, false
	}
	span := m.spans[index]
	if comparePosition(value.Start, span.original.Start) < 0 || comparePosition(value.End, span.original.End) > 0 {
		return analysis.Range{}, false
	}
	return analysis.Range{
		Start: translatePosition(value.Start, span.original.Start, span.current.Start),
		End:   translatePosition(value.End, span.original.Start, span.current.Start),
	}, true
}

func comparePosition(left, right analysis.Position) int {
	if line := cmp.Compare(left.Line, right.Line); line != 0 {
		return line
	}
	return cmp.Compare(left.Character, right.Character)
}

func translatePosition(value, before, after analysis.Position) analysis.Position {
	if value.Line == before.Line {
		value.Character += after.Character - before.Character
	}
	value.Line += after.Line - before.Line
	return value
}

func replacementEnd(start analysis.Position, text string) analysis.Position {
	if _, suffix, found := strings.CutLast(text, "\n"); found {
		return analysis.Position{Line: start.Line + strings.Count(text, "\n"), Character: utf16Length(suffix)}
	}
	start.Character += utf16Length(text)
	return start
}

func utf16Length(text string) int {
	length := 0
	for _, value := range text {
		length += utf16.RuneLen(value)
	}
	return length
}
