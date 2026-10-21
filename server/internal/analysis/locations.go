package analysis

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// locationResolver owns source bytes and canonical URIs for one analysis.
// Keeping this cache analysis-scoped avoids repeated disk reads without
// retaining source content in the long-lived Snapshot.
type locationResolver struct {
	sourceByPath map[string][]byte
	files        map[string]locationFile
	readFile     func(string) ([]byte, error)
}

type locationFile struct {
	content []byte
	uri     string
}

func newLocationResolver(sourceByPath map[string][]byte) *locationResolver {
	return &locationResolver{
		sourceByPath: sourceByPath,
		files:        make(map[string]locationFile),
		readFile:     os.ReadFile,
	}
}

func (r *locationResolver) identLocation(fileSet *token.FileSet, ident *ast.Ident) (Location, error) {
	start := fileSet.PositionFor(ident.Pos(), false)
	end := fileSet.PositionFor(ident.End(), false)
	file, ok := r.files[start.Filename]
	if !ok {
		filename, err := filepath.Abs(start.Filename)
		if err != nil {
			return Location{}, fmt.Errorf("make declaration filename absolute: %w", err)
		}
		filename = filepath.Clean(filename)

		content, available := r.sourceByPath[filename]
		if !available {
			content, err = r.readFile(filename)
		}
		if err != nil {
			return Location{}, fmt.Errorf("read declaration source %q: %w", filename, err)
		}
		uri, uriErr := PathToURI(filename)
		if uriErr != nil {
			return Location{}, uriErr
		}
		file = locationFile{content: content, uri: uri}
		r.files[start.Filename] = file
	}

	startPosition, err := positionForTokenPosition(file.content, start)
	if err != nil {
		return Location{}, err
	}
	endPosition, err := positionForTokenPosition(file.content, end)
	if err != nil {
		return Location{}, err
	}

	return Location{
		URI: file.uri,
		Range: Range{
			Start: startPosition,
			End:   endPosition,
		},
	}, nil
}

func positionForTokenPosition(content []byte, position token.Position) (Position, error) {
	if position.Offset < 0 || position.Offset > len(content) {
		return Position{}, fmt.Errorf("source offset %d is outside content of length %d", position.Offset, len(content))
	}
	if position.Line < 1 || position.Column < 1 {
		return Position{}, fmt.Errorf("invalid token position at line %d column %d", position.Line, position.Column)
	}
	lineStart := position.Offset - position.Column + 1
	if lineStart < 0 {
		return Position{}, fmt.Errorf("token column %d exceeds source offset %d", position.Column, position.Offset)
	}

	return Position{
		Line:      position.Line - 1,
		Character: utf16Length(content[lineStart:position.Offset]),
	}, nil
}

func utf16Length(content []byte) int {
	asciiLength := 0
	for asciiLength < len(content) && content[asciiLength] < utf8.RuneSelf {
		asciiLength++
	}

	length := asciiLength
	content = content[asciiLength:]
	for len(content) > 0 {
		r, size := utf8.DecodeRune(content)
		content = content[size:]
		if r > 0xffff {
			length += 2
		} else {
			length++
		}
	}

	return length
}
