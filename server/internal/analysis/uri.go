package analysis

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ErrNoModule reports that no go.mod exists between a document and its
// workspace boundary.
var ErrNoModule = errors.New("document is not inside a Go module")

// URIToPath converts an absolute file URI to a cleaned absolute path.
func URIToPath(rawURI string) (string, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return "", fmt.Errorf("parse document URI: %w", err)
	}
	if parsed.Scheme != "file" {
		return "", fmt.Errorf("unsupported URI scheme %q", parsed.Scheme)
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return "", fmt.Errorf("unsupported file URI host %q", parsed.Host)
	}
	path, err := filepath.Abs(filepath.FromSlash(parsed.Path))
	if err != nil {
		return "", fmt.Errorf("make file URI path absolute: %w", err)
	}
	return filepath.Clean(path), nil
}

// PathToURI converts a local path to its canonical file URI representation.
func PathToURI(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Clean(absolute))}).String(), nil
}

// FindModuleRoot finds the closest go.mod above filename without walking above
// workspaceRoot. An empty workspace root allows walking to the filesystem root.
func FindModuleRoot(filename, workspaceRoot string) (string, error) {
	filename, err := filepath.Abs(filename)
	if err != nil {
		return "", fmt.Errorf("make filename absolute: %w", err)
	}
	dir := filepath.Dir(filename)

	boundary := ""
	if workspaceRoot != "" {
		boundary, err = filepath.Abs(workspaceRoot)
		if err != nil {
			return "", fmt.Errorf("make workspace root absolute: %w", err)
		}
		boundary = filepath.Clean(boundary)
		if !pathWithin(dir, boundary) {
			return "", ErrNoModule
		}
	}

	for {
		if info, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil && !info.IsDir() {
			return dir, nil
		}
		if boundary != "" && dir == boundary {
			return "", ErrNoModule
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNoModule
		}
		dir = parent
	}
}

func pathWithin(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func identLocation(fileSet *token.FileSet, ident *ast.Ident, overlays map[string][]byte) (Location, error) {
	start := fileSet.PositionFor(ident.Pos(), false)
	end := fileSet.PositionFor(ident.End(), false)
	filename, err := filepath.Abs(start.Filename)
	if err != nil {
		return Location{}, fmt.Errorf("make declaration filename absolute: %w", err)
	}
	filename = filepath.Clean(filename)

	content, ok := overlays[filename]
	if !ok {
		content, err = os.ReadFile(filename)
		if err != nil {
			return Location{}, fmt.Errorf("read declaration source %q: %w", filename, err)
		}
	}
	startPosition, err := positionForOffset(content, start.Offset)
	if err != nil {
		return Location{}, err
	}
	endPosition, err := positionForOffset(content, end.Offset)
	if err != nil {
		return Location{}, err
	}
	uri, err := PathToURI(filename)
	if err != nil {
		return Location{}, err
	}
	return Location{
		URI: uri,
		Range: Range{
			Start: startPosition,
			End:   endPosition,
		},
	}, nil
}

func positionForOffset(content []byte, offset int) (Position, error) {
	if offset < 0 || offset > len(content) {
		return Position{}, fmt.Errorf("source offset %d is outside content of length %d", offset, len(content))
	}
	prefix := content[:offset]
	line := bytes.Count(prefix, []byte{'\n'})
	lineStart := bytes.LastIndexByte(prefix, '\n') + 1
	return Position{
		Line:      line,
		Character: utf16Length(prefix[lineStart:]),
	}, nil
}

func utf16Length(content []byte) int {
	length := 0
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
