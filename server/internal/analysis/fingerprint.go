package analysis

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash"
	"slices"
	"strconv"
	"strings"
)

// sourceFingerprint separates declaration changes from package-graph changes.
// Both digests are derived from syntax only and never retain source bytes.
type sourceFingerprint struct {
	declaration [sha256.Size]byte
	structure   [sha256.Size]byte
}

type fingerprintWriter struct {
	// header encodes a domain tag and an unsigned length. Length-prefixing
	// prevents different token sequences from hashing to the same byte stream.
	digest hash.Hash
	header [9]byte
}

// fingerprintSyntax couples syntax with the FileSet that defines its token
// offsets. Mixing file sets would make sourceTokenRange invalid.
type fingerprintSyntax struct {
	fileSet *token.FileSet
	file    *ast.File
}

// declarationFingerprint returns a stable digest of declarations and imports
// while excluding function bodies and non-build comments.
func declarationFingerprint(source []byte) ([sha256.Size]byte, error) {
	fingerprint, err := sourceFingerprintForBytes(source)
	return fingerprint.declaration, err
}

func sourceFingerprintForBytes(source []byte) (sourceFingerprint, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(
		fileSet,
		"",
		source,
		parser.AllErrors|parser.ParseComments|parser.SkipObjectResolution,
	)
	if err != nil {
		return sourceFingerprint{}, err
	}
	return sourceFingerprintForFile(fileSet, file, source)
}

func sourceFingerprintForFile(
	fileSet *token.FileSet,
	file *ast.File,
	source []byte,
) (sourceFingerprint, error) {
	if file == nil || file.Name == nil {
		return sourceFingerprint{}, errors.New("parsed source has no package clause")
	}

	// Structure changes can alter package selection or the dependency graph and
	// therefore require packages.Load. Declaration changes can be handled by
	// rechecking the known reverse dependency closure. Function bodies are
	// excluded from both decisions and use the cheapest recheck path.
	structureHash := newFingerprintWriter()
	structureHash.write('p', []byte(file.Name.Name))
	for _, constraint := range buildConstraintComments(file) {
		structureHash.write('b', []byte(constraint))
	}
	importPaths := make([]string, 0, len(file.Imports))
	for _, imported := range file.Imports {
		if imported == nil || imported.Path == nil {
			continue
		}
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return sourceFingerprint{}, fmt.Errorf("decode import path %q: %w", imported.Path.Value, err)
		}
		importPaths = append(importPaths, path)
	}
	slices.Sort(importPaths)
	for _, importPath := range importPaths {
		structureHash.write('i', []byte(importPath))
	}

	declarationHash := newFingerprintWriter()
	declarationHash.write('p', []byte(file.Name.Name))
	for _, declaration := range file.Decls {
		start, end := declaration.Pos(), declaration.End()
		tag := byte('g')
		if declaration, ok := declaration.(*ast.FuncDecl); ok {
			tag = 'f'
			if declaration.Body != nil {
				end = declaration.Body.Lbrace + 1
			}
		}
		declarationSource, err := sourceTokenRange(fileSet, source, start, end)
		if err != nil {
			return sourceFingerprint{}, err
		}
		declarationHash.write(tag, declarationSource)
	}

	var result sourceFingerprint
	declarationHash.sum(&result.declaration)
	structureHash.sum(&result.structure)
	return result, nil
}

func buildConstraintComments(file *ast.File) []string {
	var constraints []string
	for _, group := range file.Comments {
		if group == nil || group.End() > file.Package {
			continue
		}
		for _, comment := range group.List {
			if comment == nil || !strings.HasPrefix(comment.Text, "//") {
				continue
			}
			text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			if strings.HasPrefix(text, "go:build") || strings.HasPrefix(text, "+build") {
				constraints = append(constraints, text)
			}
		}
	}
	return constraints
}

func sourceTokenRange(
	fileSet *token.FileSet,
	source []byte,
	start token.Pos,
	end token.Pos,
) ([]byte, error) {
	startFile := fileSet.File(start)
	endFile := fileSet.File(end)
	if startFile == nil || startFile != endFile {
		return nil, errors.New("source token range does not belong to one parsed file")
	}
	startOffset := int(start) - startFile.Base()
	endOffset := int(end) - startFile.Base()
	if startOffset < 0 || endOffset < startOffset || endOffset > len(source) {
		return nil, fmt.Errorf(
			"source token range [%d,%d) is outside content of length %d",
			startOffset,
			endOffset,
			len(source),
		)
	}
	return source[startOffset:endOffset], nil
}

func newFingerprintWriter() fingerprintWriter {
	return fingerprintWriter{digest: sha256.New()}
}

func (w *fingerprintWriter) write(tag byte, value []byte) {
	w.header[0] = tag
	binary.BigEndian.PutUint64(w.header[1:], uint64(len(value)))
	_, _ = w.digest.Write(w.header[:])
	_, _ = w.digest.Write(value)
}

func (w *fingerprintWriter) sum(destination *[sha256.Size]byte) {
	w.digest.Sum(destination[:0])
}
