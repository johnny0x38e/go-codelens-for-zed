package lsp

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/johnny/go-codelens-for-zed/server/internal/analysis"
)

const (
	// Gap slack is bounded so small edits rarely reallocate while a large open
	// file does not reserve an unbounded percentage of unused memory.
	minimumGapSlack        = 64
	maximumGapSlack        = 4 << 10
	inlineUndoBytes        = 64
	inlineUndoEntryCount   = 4
	lineStartCapacitySlack = 16
)

// textBuffer is a UTF-8 gap buffer with a logical-byte line index. Moving the
// gap makes clustered editor changes cheap; lineStarts avoids rescanning the
// entire document for every UTF-16 LSP position.
type textBuffer struct {
	data       []byte
	gapStart   int
	gapEnd     int
	lineStarts []int
}

// textBufferInsertion accepts either protocol text or undo bytes without
// converting between string and []byte on the hot path.
type textBufferInsertion struct {
	text  string
	bytes []byte
}

// textBufferUndo journals one replacement so a multi-change notification is
// atomic. Common short deletions remain inline and allocate no heap storage.
type textBufferUndo struct {
	start          int
	insertedLength int
	deletedLength  int
	deletedHeap    []byte
	inlineDeleted  [inlineUndoBytes]byte
}

func newTextBuffer(text string) *textBuffer {
	buffer := &textBuffer{}
	buffer.reset(text)
	return buffer
}

func (b *textBuffer) apply(changes []textDocumentContentChangeEvent) (bool, error) {
	if len(changes) == 0 {
		return false, nil
	}
	if len(changes) == 1 {
		return b.applyOne(changes[0])
	}

	// Zed normally sends only a handful of edits. Keep that journal on the
	// stack, but support arbitrarily large valid notifications when necessary.
	var inlineJournal [inlineUndoEntryCount]textBufferUndo
	journal := inlineJournal[:]
	if len(changes) > len(inlineJournal) {
		journal = make([]textBufferUndo, len(changes))
	}
	applied := 0
	for index, change := range changes {
		start, end, err := b.changeBounds(change)
		if err != nil {
			b.rollback(journal[:applied])
			return false, fmt.Errorf("apply content change %d: %w", index, err)
		}
		if b.rangeEqualsString(start, end, change.Text) {
			continue
		}

		undo := &journal[applied]
		undo.start = start
		undo.insertedLength = len(change.Text)
		undo.captureDeleted(b, start, end)
		b.replace(start, end, textBufferInsertion{text: change.Text})
		applied++
	}

	return applied > 0, nil
}

func (b *textBuffer) applyOne(change textDocumentContentChangeEvent) (bool, error) {
	if change.Range == nil {
		if !utf8.ValidString(change.Text) {
			return false, errors.New("replacement text is not valid UTF-8")
		}
		if b.equalsString(change.Text) {
			return false, nil
		}
		b.reset(change.Text)
		return true, nil
	}

	start, end, err := b.changeBounds(change)
	if err != nil {
		return false, err
	}
	if b.rangeEqualsString(start, end, change.Text) {
		return false, nil
	}

	b.replace(start, end, textBufferInsertion{text: change.Text})
	return true, nil
}

func (b *textBuffer) changeBounds(change textDocumentContentChangeEvent) (int, int, error) {
	if !utf8.ValidString(change.Text) {
		return 0, 0, errors.New("replacement text is not valid UTF-8")
	}
	if change.Range == nil {
		return 0, b.logicalLength(), nil
	}

	start, err := b.byteOffset(change.Range.Start)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range start: %w", err)
	}
	end, err := b.byteOffset(change.Range.End)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range end: %w", err)
	}
	if start > end {
		return 0, 0, fmt.Errorf("range start offset %d is after end offset %d", start, end)
	}

	return start, end, nil
}

func (b *textBuffer) byteOffset(position analysis.Position) (int, error) {
	if position.Line < 0 {
		return 0, fmt.Errorf("line %d is negative", position.Line)
	}
	if position.Character < 0 {
		return 0, fmt.Errorf("character %d is negative", position.Character)
	}
	if position.Line >= len(b.lineStarts) {
		return 0, fmt.Errorf("line %d is outside %d lines", position.Line, len(b.lineStarts))
	}

	lineStart := b.lineStarts[position.Line]
	lineEnd := b.logicalLength()
	if position.Line+1 < len(b.lineStarts) {
		lineEnd = b.lineStarts[position.Line+1] - 1
		if lineEnd > lineStart && b.logicalByte(lineEnd-1) == '\r' {
			lineEnd--
		}
	}

	// LSP characters count UTF-16 code units, not bytes or Unicode code points.
	// Rejecting a position inside a surrogate pair prevents splitting UTF-8.
	offset := lineStart
	utf16Character := 0
	for offset < lineEnd {
		if utf16Character == position.Character {
			return offset, nil
		}

		runeValue, size := b.decodeRune(offset)
		if runeValue == utf8.RuneError && size == 1 {
			return 0, fmt.Errorf("document contains invalid UTF-8 at byte offset %d", offset)
		}
		runeLength := utf16.RuneLen(runeValue)
		if utf16Character+runeLength > position.Character {
			return 0, fmt.Errorf("character %d is inside a UTF-16 surrogate pair", position.Character)
		}
		utf16Character += runeLength
		offset += size
	}
	if utf16Character == position.Character {
		return lineEnd, nil
	}

	return 0, fmt.Errorf(
		"character %d is past line %d length %d",
		position.Character,
		position.Line,
		utf16Character,
	)
}

func (b *textBuffer) replace(start, end int, insertion textBufferInsertion) {
	// Grow the existing gap across the deletion, then consume it with the
	// insertion. Only gap movement copies unchanged document bytes.
	deleted := end - start
	b.moveGap(start)
	b.gapEnd += deleted
	b.ensureGap(insertion.length())
	insertion.copyTo(b.data[b.gapStart:])
	b.gapStart += insertion.length()
	b.updateLineStarts(start, deleted, insertion)
}

func (b *textBuffer) rollback(journal []textBufferUndo) {
	// Reverse order is required because each stored offset belongs to the buffer
	// state immediately before its corresponding forward change.
	for index := range slices.Backward(journal) {
		undo := &journal[index]
		deleted := undo.deletedHeap
		if deleted == nil {
			deleted = undo.inlineDeleted[:undo.deletedLength]
		}
		b.replace(
			undo.start,
			undo.start+undo.insertedLength,
			textBufferInsertion{bytes: deleted},
		)
	}
}

func (b *textBuffer) moveGap(offset int) {
	if offset < b.gapStart {
		moved := b.gapStart - offset
		copy(b.data[b.gapEnd-moved:b.gapEnd], b.data[offset:b.gapStart])
		b.gapStart = offset
		b.gapEnd -= moved
		return
	}
	if offset > b.gapStart {
		moved := offset - b.gapStart
		copy(b.data[b.gapStart:b.gapStart+moved], b.data[b.gapEnd:b.gapEnd+moved])
		b.gapStart += moved
		b.gapEnd += moved
	}
}

func (b *textBuffer) ensureGap(required int) {
	if b.gapLength() >= required {
		return
	}

	logicalLength := b.logicalLength()
	newGapLength := required + gapSlack(logicalLength)
	newData := make([]byte, logicalLength+newGapLength)
	copy(newData, b.data[:b.gapStart])
	suffixLength := len(b.data) - b.gapEnd
	newGapEnd := len(newData) - suffixLength
	copy(newData[newGapEnd:], b.data[b.gapEnd:])
	b.data = newData
	b.gapEnd = newGapEnd
}

func (b *textBuffer) updateLineStarts(start, deleted int, insertion textBufferInsertion) {
	// Binary-search the affected line-index interval, replace just that interval,
	// then shift the untouched suffix by the net byte delta.
	end := start + deleted
	firstRemoved := sort.Search(len(b.lineStarts), func(index int) bool {
		return b.lineStarts[index] > start
	})
	afterRemoved := sort.Search(len(b.lineStarts), func(index int) bool {
		return b.lineStarts[index] > end
	})

	insertedLines := 0
	for index := range insertion.length() {
		if insertion.byteAt(index) == '\n' {
			insertedLines++
		}
	}
	removedLines := afterRemoved - firstRemoved
	oldLength := len(b.lineStarts)
	newLength := oldLength - removedLines + insertedLines
	newTailStart := firstRemoved + insertedLines
	if newLength > cap(b.lineStarts) {
		capacity := newLength + lineStartCapacitySlack
		lineStarts := make([]int, newLength, capacity)
		copy(lineStarts, b.lineStarts[:firstRemoved])
		copy(lineStarts[newTailStart:], b.lineStarts[afterRemoved:])
		b.lineStarts = lineStarts
	} else {
		b.lineStarts = b.lineStarts[:newLength]
		copy(b.lineStarts[newTailStart:], b.lineStarts[afterRemoved:oldLength])
	}

	lineIndex := firstRemoved
	for index := range insertion.length() {
		if insertion.byteAt(index) != '\n' {
			continue
		}
		b.lineStarts[lineIndex] = start + index + 1
		lineIndex++
	}
	delta := insertion.length() - deleted
	for index := newTailStart; index < len(b.lineStarts); index++ {
		b.lineStarts[index] += delta
	}
}

func (b *textBuffer) bytes() []byte {
	// Analysis overlays must be immutable after the document lock is released;
	// materialize one contiguous copy only when a dirty overlay is requested.
	content := make([]byte, b.logicalLength())
	copy(content, b.data[:b.gapStart])
	copy(content[b.gapStart:], b.data[b.gapEnd:])
	return content
}

func (b *textBuffer) reset(text string) {
	slack := gapSlack(len(text))
	required := len(text) + slack
	if cap(b.data) < required {
		b.data = make([]byte, required)
	} else {
		b.data = b.data[:required]
	}
	copy(b.data, text)
	b.gapStart = len(text)
	b.gapEnd = len(b.data)

	lineCount := strings.Count(text, "\n") + 1
	if cap(b.lineStarts) < lineCount {
		b.lineStarts = make([]int, 1, lineCount+lineStartCapacitySlack)
	} else {
		b.lineStarts = b.lineStarts[:1]
	}
	b.lineStarts[0] = 0
	for offset := 0; offset < len(text); {
		newline := strings.IndexByte(text[offset:], '\n')
		if newline < 0 {
			break
		}
		offset += newline + 1
		b.lineStarts = append(b.lineStarts, offset)
	}
}

func (b *textBuffer) logicalLength() int {
	return len(b.data) - b.gapLength()
}

func (b *textBuffer) gapLength() int {
	return b.gapEnd - b.gapStart
}

func (b *textBuffer) logicalByte(offset int) byte {
	if offset < b.gapStart {
		return b.data[offset]
	}
	return b.data[offset+b.gapLength()]
}

func (b *textBuffer) decodeRune(offset int) (rune, int) {
	if offset < b.gapStart {
		return utf8.DecodeRune(b.data[offset:b.gapStart])
	}
	physicalOffset := offset + b.gapLength()
	return utf8.DecodeRune(b.data[physicalOffset:])
}

func (b *textBuffer) rangeEqualsString(start, end int, text string) bool {
	if end-start != len(text) {
		return false
	}
	for index := range len(text) {
		if b.logicalByte(start+index) != text[index] {
			return false
		}
	}
	return true
}

func (b *textBuffer) equalsString(text string) bool {
	if b.logicalLength() != len(text) {
		return false
	}
	return bytes.Equal(b.data[:b.gapStart], []byte(text[:b.gapStart])) &&
		bytes.Equal(b.data[b.gapEnd:], []byte(text[b.gapStart:]))
}

func (b *textBuffer) equalsBytes(content []byte) bool {
	if b.logicalLength() != len(content) {
		return false
	}
	return bytes.Equal(b.data[:b.gapStart], content[:b.gapStart]) &&
		bytes.Equal(b.data[b.gapEnd:], content[b.gapStart:])
}

func (u *textBufferUndo) captureDeleted(buffer *textBuffer, start, end int) {
	length := end - start
	u.deletedLength = length
	var destination []byte
	// Most interactive deletions fit in inlineDeleted, keeping multi-edit
	// notifications allocation-free apart from an uncommon large journal.
	if length <= len(u.inlineDeleted) {
		destination = u.inlineDeleted[:length]
	} else {
		u.deletedHeap = make([]byte, length)
		destination = u.deletedHeap
	}
	for index := range length {
		destination[index] = buffer.logicalByte(start + index)
	}
}

func (i textBufferInsertion) length() int {
	if i.bytes != nil {
		return len(i.bytes)
	}
	return len(i.text)
}

func (i textBufferInsertion) byteAt(index int) byte {
	if i.bytes != nil {
		return i.bytes[index]
	}
	return i.text[index]
}

func (i textBufferInsertion) copyTo(destination []byte) {
	if i.bytes != nil {
		copy(destination, i.bytes)
		return
	}
	copy(destination, i.text)
}

func gapSlack(logicalLength int) int {
	slack := logicalLength / 16
	if slack < minimumGapSlack {
		return minimumGapSlack
	}
	if slack > maximumGapSlack {
		return maximumGapSlack
	}
	return slack
}
