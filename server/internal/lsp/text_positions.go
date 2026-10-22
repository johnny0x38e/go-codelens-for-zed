package lsp

import (
	"sort"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

func (b *textBuffer) recordPositionEdit(change textDocumentContentChangeEvent, start, end int) positionEdit {
	if change.Range != nil {
		return positionEdit{rangeBefore: *change.Range, end: replacementEnd(change.Range.Start, change.Text)}
	}
	text := change.Text
	if change.Range == nil {
		// Full-document synchronization may still contain a tiny edit. Preserve
		// the unchanged prefix/suffix without making an old-document copy.
		prefix := 0
		for prefix < b.logicalLength() && prefix < len(text) && b.logicalByte(prefix) == text[prefix] {
			prefix++
		}
		for prefix > 0 && prefix < len(text) && !utf8.RuneStart(text[prefix]) {
			prefix--
		}
		if prefix > 0 && prefix < b.logicalLength() && b.logicalByte(prefix-1) == '\r' &&
			b.logicalByte(prefix) == '\n' {
			prefix--
		}
		suffix := 0
		for suffix < b.logicalLength()-prefix && suffix < len(text)-prefix &&
			b.logicalByte(b.logicalLength()-1-suffix) == text[len(text)-1-suffix] {
			suffix++
		}
		for suffix > 0 && !utf8.RuneStart(text[len(text)-suffix]) {
			suffix--
		}
		oldEnd := b.logicalLength() - suffix
		if suffix > 0 && oldEnd > 0 && b.logicalByte(oldEnd-1) == '\r' && b.logicalByte(oldEnd) == '\n' {
			suffix--
		}
		start, end = prefix, b.logicalLength()-suffix
		text = text[prefix : len(text)-suffix]
	}
	value := analysis.Range{Start: b.positionAt(start), End: b.positionAt(end)}
	return positionEdit{rangeBefore: value, end: replacementEnd(value.Start, text)}
}

func (b *textBuffer) positionAt(offset int) analysis.Position {
	line := sort.Search(len(b.lineStarts), func(index int) bool { return b.lineStarts[index] > offset }) - 1
	position := analysis.Position{Line: line}
	for index := b.lineStarts[line]; index < offset; {
		value, size := b.decodeRune(index)
		position.Character += utf16.RuneLen(value)
		index += size
	}
	return position
}
