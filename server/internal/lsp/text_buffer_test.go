package lsp

import (
	"testing"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
)

func TestTextBufferAppliesASCIIInsertAndDelete(t *testing.T) {
	buffer := newTextBuffer("abc\ndef\n")
	changed, err := buffer.apply([]textDocumentContentChangeEvent{
		textBufferRangeChange(0, 1, 0, 2, ""),
		textBufferRangeChange(0, 1, 0, 1, "BC"),
	})
	if err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if !changed {
		t.Fatal("apply() reported no change")
	}
	assertTextBufferContent(t, buffer, "aBCc\ndef\n")
}

func TestTextBufferUsesUTF16Positions(t *testing.T) {
	buffer := newTextBuffer("a😀b\n你c\n")
	changed, err := buffer.apply([]textDocumentContentChangeEvent{
		textBufferRangeChange(0, 1, 0, 3, "🙂"),
		textBufferRangeChange(1, 1, 1, 2, "文"),
	})
	if err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if !changed {
		t.Fatal("apply() reported no change")
	}
	assertTextBufferContent(t, buffer, "a🙂b\n你文\n")
}

func TestTextBufferRejectsPositionInsideSurrogatePair(t *testing.T) {
	buffer := newTextBuffer("a😀b\n")
	original := string(buffer.bytes())

	_, err := buffer.apply([]textDocumentContentChangeEvent{
		textBufferRangeChange(0, 2, 0, 3, "x"),
	})
	if err == nil {
		t.Fatal("apply() error = nil, want an invalid UTF-16 position error")
	}
	assertTextBufferContent(t, buffer, original)
}

func TestTextBufferHandlesCRLFAndLineEnd(t *testing.T) {
	buffer := newTextBuffer("a\r\nb\r\n")
	changed, err := buffer.apply([]textDocumentContentChangeEvent{
		textBufferRangeChange(0, 1, 0, 1, "!"),
		textBufferRangeChange(1, 1, 1, 1, "?"),
	})
	if err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if !changed {
		t.Fatal("apply() reported no change")
	}
	assertTextBufferContent(t, buffer, "a!\r\nb?\r\n")

	original := string(buffer.bytes())
	_, err = buffer.apply([]textDocumentContentChangeEvent{
		textBufferRangeChange(0, 3, 0, 3, "x"),
	})
	if err == nil {
		t.Fatal("apply() error = nil, want a character-past-line-end error")
	}
	assertTextBufferContent(t, buffer, original)
}

func TestTextBufferUpdatesLineIndexAcrossMultilineReplacement(t *testing.T) {
	buffer := newTextBuffer("one\ntwo\nthree\n")
	changed, err := buffer.apply([]textDocumentContentChangeEvent{
		textBufferRangeChange(0, 1, 2, 2, "X\nY"),
		textBufferRangeChange(1, 1, 1, 1, "!"),
	})
	if err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if !changed {
		t.Fatal("apply() reported no change")
	}
	assertTextBufferContent(t, buffer, "oX\nY!ree\n")
}

func TestTextBufferAppliesFullReplacementBeforeFollowingChanges(t *testing.T) {
	buffer := newTextBuffer("old\n")
	changed, err := buffer.apply([]textDocumentContentChangeEvent{
		{Text: "new😀\n"},
		textBufferRangeChange(0, 3, 0, 5, "🙂"),
	})
	if err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if !changed {
		t.Fatal("apply() reported no change")
	}
	assertTextBufferContent(t, buffer, "new🙂\n")
}

func TestTextBufferMultiChangeFailureIsAtomic(t *testing.T) {
	buffer := newTextBuffer("abc\ndef\n")
	original := string(buffer.bytes())

	changed, err := buffer.apply([]textDocumentContentChangeEvent{
		textBufferRangeChange(0, 1, 1, 1, "X\nY\nZ"),
		textBufferRangeChange(9, 0, 9, 0, "invalid"),
	})
	if err == nil {
		t.Fatal("apply() error = nil, want an out-of-range line error")
	}
	if changed {
		t.Fatal("failed atomic apply() reported a change")
	}
	assertTextBufferContent(t, buffer, original)

	changed, err = buffer.apply([]textDocumentContentChangeEvent{
		textBufferRangeChange(1, 3, 1, 3, "!"),
	})
	if err != nil {
		t.Fatalf("apply() after rollback error = %v", err)
	}
	if !changed {
		t.Fatal("apply() after rollback reported no change")
	}
	assertTextBufferContent(t, buffer, "abc\ndef!\n")
}

func TestTextBufferRejectsInvalidRangesWithoutMutation(t *testing.T) {
	buffer := newTextBuffer("abc\n")
	original := string(buffer.bytes())

	invalidChanges := []textDocumentContentChangeEvent{
		textBufferRangeChange(-1, 0, 0, 0, "x"),
		textBufferRangeChange(0, -1, 0, 0, "x"),
		textBufferRangeChange(3, 0, 3, 0, "x"),
		textBufferRangeChange(0, 4, 0, 4, "x"),
		textBufferRangeChange(0, 2, 0, 1, "x"),
	}
	for index, change := range invalidChanges {
		if _, err := buffer.apply([]textDocumentContentChangeEvent{change}); err == nil {
			t.Errorf("invalid change %d returned no error", index)
		}
		assertTextBufferContent(t, buffer, original)
	}
}

func TestTextBufferNoOpDoesNotReportChange(t *testing.T) {
	buffer := newTextBuffer("abc\n")
	changed, err := buffer.apply([]textDocumentContentChangeEvent{
		textBufferRangeChange(0, 1, 0, 2, "b"),
	})
	if err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if changed {
		t.Fatal("identical replacement reported a change")
	}
	assertTextBufferContent(t, buffer, "abc\n")
}

func textBufferRangeChange(startLine, startCharacter, endLine, endCharacter int, text string) textDocumentContentChangeEvent {
	return textDocumentContentChangeEvent{
		Range: &analysis.Range{
			Start: analysis.Position{Line: startLine, Character: startCharacter},
			End:   analysis.Position{Line: endLine, Character: endCharacter},
		},
		Text: text,
	}
}

func assertTextBufferContent(t *testing.T, buffer *textBuffer, want string) {
	t.Helper()

	if got := string(buffer.bytes()); got != want {
		t.Fatalf("buffer content = %q, want %q", got, want)
	}
}
