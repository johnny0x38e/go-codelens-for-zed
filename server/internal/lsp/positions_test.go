package lsp

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

func positionRange(line, start, end int) analysis.Range {
	return analysis.Range{
		Start: analysis.Position{Line: line, Character: start},
		End:   analysis.Position{Line: line, Character: end},
	}
}

func TestPositionMappingPreservesOnlyUnchangedRanges(t *testing.T) {
	for _, test := range []struct {
		name string
		edit textDocumentContentChangeEvent
		want analysis.Range
		ok   bool
	}{
		{"line insertion", textBufferRangeChange(0, 0, 0, 0, "// 😀\n"), positionRange(3, 5, 11), true},
		{"UTF16 insertion", textBufferRangeChange(2, 0, 2, 0, "😀"), positionRange(2, 7, 13), true},
		{"at declaration start", textBufferRangeChange(2, 5, 2, 5, "prefix"), positionRange(2, 11, 17), true},
		{"at declaration end", textBufferRangeChange(2, 11, 2, 11, "suffix"), positionRange(2, 5, 11), true},
		{"overwritten", textBufferRangeChange(2, 5, 2, 11, "Other"), analysis.Range{}, false},
		{"split identifier", textBufferRangeChange(2, 8, 2, 8, "X"), analysis.Range{}, false},
		{"delete preceding line", textBufferRangeChange(1, 0, 2, 0, ""), positionRange(1, 5, 11), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mapping := newPositionMapping()
			mapping.apply(
				positionEdit{rangeBefore: *test.edit.Range, end: replacementEnd(test.edit.Range.Start, test.edit.Text)},
			)
			got, ok := mapping.mapRange(positionRange(2, 5, 11))
			if ok != test.ok || got != test.want {
				t.Fatalf("mapRange() = (%#v, %t), want (%#v, %t)", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestPositionMappingCoalescesTypingAndBoundsFragmentation(t *testing.T) {
	mapping := newPositionMapping()
	for index := range 10_000 {
		start := analysis.Position{Character: index}
		mapping.apply(
			positionEdit{rangeBefore: analysis.Range{Start: start, End: start}, end: replacementEnd(start, "x")},
		)
	}
	if len(mapping.spans) != 1 {
		t.Fatalf("typing retained %d spans, want 1 unchanged suffix", len(mapping.spans))
	}
	if got, ok := mapping.mapRange(positionRange(0, 5, 11)); !ok || got != positionRange(0, 10_005, 10_011) {
		t.Fatalf("range after typing = (%#v, %t)", got, ok)
	}
	for index := range maxPositionSpans + 1 {
		start := analysis.Position{Line: index + 1}
		mapping.apply(
			positionEdit{rangeBefore: analysis.Range{Start: start, End: start}, end: replacementEnd(start, "x")},
		)
	}
	if len(mapping.spans) != 0 || len(mapping.scratch) != 0 {
		t.Fatal("fragmented mapping retained unbounded state")
	}
	if _, ok := mapping.mapRange(positionRange(0, 5, 11)); ok {
		t.Fatal("exhausted mapping guessed an old location")
	}
}

func TestRecordedFullTextChangesPreserveUTF16AndCRLF(t *testing.T) {
	original := "package sample\r\n// 😀\r\nfunc Target() {}\r\n"
	buffer := newTextBuffer(original)
	mapping := newPositionMapping()
	for _, text := range []string{
		strings.Replace(original, "😀", "😁", 1),
		strings.Replace(original, "func Target", "// inserted 😀\r\nfunc Target", 1),
	} {
		var inline [inlineUndoEntryCount]positionEdit
		changed, edits, err := buffer.applyRecorded([]textDocumentContentChangeEvent{{Text: text}}, inline[:0])
		if err != nil || !changed {
			t.Fatalf("applyRecorded() = (%t, %v)", changed, err)
		}
		for _, edit := range edits {
			mapping.apply(edit)
		}
	}
	got, ok := mapping.mapRange(positionRange(2, 5, 11))
	if !ok || got != positionRange(3, 5, 11) {
		t.Fatalf("full-text mapping = (%#v, %t), want shifted Target", got, ok)
	}
}

func TestPositionMappingMatchesTrackedCharacters(t *testing.T) {
	var source strings.Builder
	for index := range 12 {
		fmt.Fprintf(&source, "func Target%02d() {}\n", index)
	}
	baseline := source.String()
	current := []rune(baseline)
	identities := make([]int, len(current))
	for index := range identities {
		identities[index] = index
	}
	buffer := newTextBuffer(baseline)
	mapping := newPositionMapping()
	random := rand.New(rand.NewPCG(17, 29))
	insertions := []string{"", "x", "😀", "\n", "abc", "\n😀\n"}
	for range 120 {
		start := random.IntN(len(current) + 1)
		end := min(start+random.IntN(5), len(current))
		text := insertions[random.IntN(len(insertions))]
		startPosition := replacementEnd(analysis.Position{}, string(current[:start]))
		endPosition := replacementEnd(analysis.Position{}, string(current[:end]))
		change := textDocumentContentChangeEvent{
			Range: &analysis.Range{Start: startPosition, End: endPosition},
			Text:  text,
		}
		var inline [inlineUndoEntryCount]positionEdit
		changed, edits, err := buffer.applyRecorded([]textDocumentContentChangeEvent{change}, inline[:0])
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			inserted := []rune(text)
			updated := append([]rune(nil), current[:start]...)
			updated = append(updated, inserted...)
			current = append(updated, current[end:]...)
			updatedIDs := append([]int(nil), identities[:start]...)
			for range inserted {
				updatedIDs = append(updatedIDs, -1)
			}
			identities = append(updatedIDs, identities[end:]...)
			for _, edit := range edits {
				mapping.apply(edit)
			}
		}
		for index := range 12 {
			name := fmt.Sprintf("Target%02d", index)
			originalStart := strings.Index(baseline, name)
			originalRange := positionRange(index, 5, 5+len(name))
			found := -1
			for candidate := range len(identities) - len(name) + 1 {
				intact := true
				for offset := range len(name) {
					intact = intact && identities[candidate+offset] == originalStart+offset
				}
				if intact {
					found = candidate
					break
				}
			}
			got, ok := mapping.mapRange(originalRange)
			if ok != (found >= 0) {
				t.Fatalf("mapping %s disagrees with tracked characters: range=%#v ok=%t found=%d", name, got, ok, found)
			}
			if found >= 0 {
				want := analysis.Range{
					Start: replacementEnd(analysis.Position{}, string(current[:found])),
					End:   replacementEnd(analysis.Position{}, string(current[:found+len(name)])),
				}
				if got != want {
					t.Fatalf("mapping %s = %#v, want %#v", name, got, want)
				}
			}
		}
	}
}

func TestRetainedPositionDocumentsRemainBounded(t *testing.T) {
	var cached cachedSnapshot
	for index := range maxMappedDocuments + 1 {
		cached.positionMappingFor(fmt.Sprintf("file:///file%d.go", index))
	}
	if !cached.positionsLost || len(cached.positions) != 0 {
		t.Fatal("excess old revisions retained unbounded per-file mappings")
	}
}

func TestRecordedChangesRemainAtomic(t *testing.T) {
	buffer := newTextBuffer("package sample\nfunc Target() {}\n")
	before := string(buffer.bytes())
	var inline [inlineUndoEntryCount]positionEdit
	changed, edits, err := buffer.applyRecorded([]textDocumentContentChangeEvent{
		textBufferRangeChange(1, 0, 1, 0, "// inserted\n"),
		textBufferRangeChange(999, 0, 999, 0, "invalid"),
	}, inline[:0])
	if err == nil || changed || len(edits) != 0 || string(buffer.bytes()) != before {
		t.Fatalf("failed notification changed buffer or positions: changed=%t edits=%v error=%v", changed, edits, err)
	}
}
