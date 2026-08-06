package analysis

import "testing"

func TestPositionForOffsetUsesUTF16CodeUnits(t *testing.T) {
	content := []byte("😀界x")

	afterEmoji, err := positionForOffset(content, len([]byte("😀")))
	if err != nil {
		t.Fatalf("positionForOffset() error = %v", err)
	}
	if afterEmoji != (Position{Line: 0, Character: 2}) {
		t.Fatalf("position after emoji = %#v, want character 2", afterEmoji)
	}

	afterCJK, err := positionForOffset(content, len([]byte("😀界")))
	if err != nil {
		t.Fatalf("positionForOffset() error = %v", err)
	}
	if afterCJK != (Position{Line: 0, Character: 3}) {
		t.Fatalf("position after CJK = %#v, want character 3", afterCJK)
	}
}
