package lsp

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"strings"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

var errSavedSyntax = errors.New("saved Go source has syntax errors")

// validSavedGoSyntax is intentionally syntax-only. Ordinary type errors do not
// block CodeLens updates, and metadata is left to the Go module loader.
func validSavedGoSyntax(path string, source []byte) bool {
	if !strings.HasSuffix(path, ".go") {
		return true
	}
	_, err := parser.ParseFile(token.NewFileSet(), path, source, parser.SkipObjectResolution)
	return err == nil
}

// checkChangedGoSyntax examines exact changed paths, including closed files
// changed by a watcher. It never walks directories or invokes the Go toolchain.
func checkChangedGoSyntax(ctx context.Context, changes analysis.ChangeSet, documents *documentStore) error {
	for _, change := range changes.Changes() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if change.Kind == analysis.ChangeDeleted || !strings.HasSuffix(change.Path, ".go") {
			continue
		}
		if documents != nil && documents.SyntaxVerified(change.Path) {
			continue
		}
		source, err := os.ReadFile(
			change.Path,
		) //nolint:gosec // Exact workspace paths originate in document and file notifications.
		if errors.Is(err, os.ErrNotExist) {
			// A concurrent deletion changes build selection; let the loader resolve
			// it rather than treating a nonexistent file as a syntax error.
			continue
		}
		if err != nil {
			return err
		}
		if !validSavedGoSyntax(change.Path, source) {
			return errSavedSyntax
		}
	}
	return ctx.Err()
}
