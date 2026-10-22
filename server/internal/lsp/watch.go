package lsp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

const (
	watchedFileCreated         = 1
	watchedFileChanged         = 2
	watchedFileDeleted         = 3
	registerCapabilityMethod   = "client/registerCapability"
	watchedFilesMethod         = "workspace/didChangeWatchedFiles"
	watchedFilesRegistrationID = "go-codelens-watched-files"
	codeLensRefreshMethod      = "workspace/codeLens/refresh"
	codeLensRefreshIDPrefix    = "go-codelens-refresh-"
)

// watchedFilesRegistrationState distinguishes a sent request from a capability
// that the client has acknowledged. The LSP client may reject a dynamic
// registration even after advertising support for the mechanism.
type watchedFilesRegistrationState uint8

const (
	watchedFilesUnregistered watchedFilesRegistrationState = iota
	watchedFilesRegistrationPending
	watchedFilesRegistered
)

func (s *serverState) watchedFilesRegistrationRequest() (registrationRequest, bool) {
	if !s.watchedFilesSupported || s.watchedFilesRegistration != watchedFilesUnregistered {
		return registrationRequest{}, false
	}
	s.watchedFilesRegistration = watchedFilesRegistrationPending

	return registrationRequest{
		JSONRPC: jsonRPCVersion,
		ID:      watchedFilesRegistrationID,
		Method:  registerCapabilityMethod,
		Params: registrationParams{Registrations: []registration{{
			ID:     watchedFilesRegistrationID,
			Method: watchedFilesMethod,
			RegisterOptions: didChangeWatchedFilesRegisterOptions{Watchers: []fileSystemWatcher{
				{GlobPattern: "**/*.go"},
				{GlobPattern: "**/go.mod"},
				{GlobPattern: "**/go.work"},
			}},
		}}},
	}, true
}

func (s *serverState) handleWatchedFilesRegistrationResponse(reply response) error {
	if !isWatchedFilesRegistrationResponse(reply.ID) {
		return nil
	}
	if s.watchedFilesRegistration != watchedFilesRegistrationPending {
		return nil
	}
	if reply.Error != nil {
		s.watchedFilesRegistration = watchedFilesUnregistered
		return fmt.Errorf(
			"register %s: client error %d: %s",
			watchedFilesMethod,
			reply.Error.Code,
			reply.Error.Message,
		)
	}

	s.watchedFilesRegistration = watchedFilesRegistered
	return nil
}

func isWatchedFilesRegistrationResponse(rawID json.RawMessage) bool {
	var id string
	if err := json.Unmarshal(rawID, &id); err != nil {
		return false
	}
	return id == watchedFilesRegistrationID
}

func (s *serverState) handleWatchedFiles(params json.RawMessage) {
	changes, err := watchedFileChanges(params)
	if err != nil {
		s.invalidateAnalysisScopes()
		s.invalidateAllSnapshots(analysis.NewChangeSet(analysis.Change{Kind: analysis.ChangeMetadata}))
		s.noteCodeLensRefresh()
		return
	}
	if len(changes) == 0 {
		return
	}
	if slices.ContainsFunc(
		changes,
		func(change analysis.Change) bool { return change.Kind == analysis.ChangeMetadata },
	) {
		s.invalidateAnalysisScopes()
	}

	for _, change := range changes {
		if !s.documents.IsOpen(change.Path) {
			s.suppressDocumentPositions(change.Path)
		}
	}
	s.invalidateSnapshotsForChanges(changes)
	// The edit did not pass through an editor buffer, so any lens payload the
	// client still holds may describe the previous revision.
	s.noteCodeLensRefresh()
}

// noteCodeLensRefresh marks the client's cached lens payloads as stale. The
// actual request is deferred so a batch of changes costs one refresh.
func (s *serverState) noteCodeLensRefresh() {
	s.refreshMu.Lock()
	s.refreshPending = true
	s.refreshMu.Unlock()
}

// flushCodeLensRefresh emits at most one refresh request for all invalidations
// observed since the previous flush. Clients that do not advertise refresh
// support would reject the server-initiated request, so it is skipped.
func (s *serverState) flushCodeLensRefresh(stream *responseStream) {
	if !s.codeLensRefreshSupported {
		return
	}
	s.refreshMu.Lock()
	pending := s.refreshPending
	if pending {
		s.refreshPending = false
		s.refreshSequence++
	}
	sequence := s.refreshSequence
	s.refreshMu.Unlock()
	if !pending {
		return
	}
	stream.write(codeLensRefreshMethod, codeLensRefreshRequest{
		JSONRPC: jsonRPCVersion,
		ID:      codeLensRefreshIDPrefix + strconv.FormatUint(sequence, 10),
		Method:  codeLensRefreshMethod,
	})
}

func watchedFileChanges(params json.RawMessage) ([]analysis.Change, error) {
	var input didChangeWatchedFilesParams
	if err := json.Unmarshal(params, &input); err != nil {
		return nil, fmt.Errorf("decode watched file changes: %w", err)
	}
	changes := make([]analysis.Change, 0, len(input.Changes))
	for _, event := range input.Changes {
		path, err := analysis.URIToPath(event.URI)
		if err != nil {
			return nil, fmt.Errorf("decode watched file URI: %w", err)
		}
		kind := analysis.ChangeContent
		switch event.Type {
		case watchedFileCreated:
			kind = analysis.ChangeCreated
		case watchedFileDeleted:
			kind = analysis.ChangeDeleted
		}
		if isModuleMetadataPath(path) {
			kind = analysis.ChangeMetadata
		}
		changes = append(changes, analysis.Change{Path: path, Kind: kind})
	}

	return changes, nil
}

func isModuleMetadataPath(path string) bool {
	base := filepath.Base(path)
	return base == "go.mod" || base == "go.work"
}
