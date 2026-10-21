package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

const (
	maxFreshnessChecks  = 64
	requestCanceledCode = -32800
)

func codeLensResponse(ctx context.Context, state *serverState, message request) response {
	result, err := state.semanticCodeLenses(ctx, message.Params)
	if err == nil {
		return response{JSONRPC: jsonRPCVersion, ID: message.ID, Result: result}
	}

	code := -32602
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = requestCanceledCode
	}
	return response{
		JSONRPC: jsonRPCVersion,
		ID:      message.ID,
		Error: &responseError{
			Code:    code,
			Message: err.Error(),
		},
	}
}

func (s *serverState) semanticCodeLenses(ctx context.Context, params json.RawMessage) ([]codeLens, error) {
	var input textDocumentParams
	if err := json.Unmarshal(params, &input); err != nil {
		return nil, fmt.Errorf("invalid textDocument/codeLens params: %w", err)
	}
	if input.TextDocument.URI == "" {
		return nil, errors.New("textDocument/codeLens params must include textDocument.uri")
	}

	document, err := s.documentPathFor(input.TextDocument.URI)
	if err != nil {
		return nil, fmt.Errorf("invalid textDocument/codeLens document URI: %w", err)
	}
	filename := document.filename
	scope, err := s.analysisScopeFor(filename)
	if errors.Is(err, analysis.ErrNoModule) {
		return []codeLens{}, nil
	}
	if err != nil {
		return []codeLens{}, nil //nolint:nilerr // Scope discovery failures suppress optional lenses without disrupting editing.
	}
	snapshot, err := s.snapshotForScope(ctx, scope)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return []codeLens{}, nil // Analysis failures suppress optional lenses without disrupting editing.
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	canonicalURI := document.uri
	semanticLenses := snapshot.Lenses(canonicalURI)
	// A cached snapshot can survive an external rewrite that never produced a
	// watched-file notification. Validate the exact files this answer depends on
	// before publishing coordinates that may belong to an older revision.
	if changes := staleLensChanges(snapshot, canonicalURI, semanticLenses); len(changes) > 0 {
		s.invalidateSnapshotsForChanges(changes)
		// The client also caches the previous Location payloads, and only a
		// refresh request makes Zed re-query and replace them.
		s.noteCodeLensRefresh()
		snapshot, err = s.snapshotForScope(ctx, scope)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return []codeLens{}, nil // Analysis failures suppress optional lenses without disrupting editing.
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		semanticLenses = snapshot.Lenses(canonicalURI)
	}

	result := make([]codeLens, 0, len(semanticLenses))
	for _, semanticLens := range semanticLenses {
		lensCommand := command{
			Title:   semanticLens.Title,
			Command: "editor.action.showReferences",
			Arguments: showReferencesArguments{
				canonicalURI,
				semanticLens.Range.Start,
				semanticLens.Targets,
			},
		}
		result = append(result, codeLens{
			Range:   semanticLens.Range,
			Command: lensCommand,
		})
	}

	return result, nil
}

// staleLensChanges returns content changes for the requested document and the
// distinct files carrying the returned navigation targets when their on-disk
// identity no longer matches the snapshot. The check is stat-only, bounded, and
// starts with the document whose lenses are being returned.
func staleLensChanges(
	snapshot analysis.Snapshot,
	documentURI string,
	lenses []analysis.Lens,
) []analysis.Change {
	// The fixed array keeps the common fresh path allocation-free; at most 64
	// URIs are compared and statted before the bound is reached.
	var checked [maxFreshnessChecks]string
	checked[0] = documentURI
	checkedCount := 1
	var changes []analysis.Change
	if change, stale := snapshot.StaleFile(documentURI); stale {
		changes = append(changes, change)
	}
	for _, lens := range lenses {
		for _, target := range lens.Targets {
			if checkedCount >= maxFreshnessChecks {
				return changes
			}
			uri := target.URI
			if slices.Contains(checked[:checkedCount], uri) {
				continue
			}
			checked[checkedCount] = uri
			checkedCount++
			if change, stale := snapshot.StaleFile(uri); stale {
				changes = append(changes, change)
			}
		}
	}
	return changes
}
