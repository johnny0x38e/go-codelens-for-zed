package analysis

import (
	"maps"
	"os"
)

// fileIdentity records the cheap filesystem identity of one source file at the
// moment its content entered an analysis. Size and modification time detect
// rewrites performed outside the editor without hashing whole files or
// retaining source bytes.
type fileIdentity struct {
	path       string
	size       int64
	modifiedAt int64
}

func statFileIdentity(path string) (fileIdentity, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return fileIdentity{}, false
	}
	return fileIdentity{
		path:       path,
		size:       info.Size(),
		modifiedAt: info.ModTime().UnixNano(),
	}, true
}

// withFileIdentities returns a snapshot view that can validate its navigation
// targets against the filesystem. The map is immutable and shared by every
// snapshot derived from the same analysis revision.
func (s Snapshot) withFileIdentities(files map[string]fileIdentity) Snapshot {
	s.files = files
	return s
}

// StaleFile reports whether the file behind uri still matches the identity
// recorded when this snapshot was produced. A changed or missing file yields
// the content change callers must feed back into analysis. ok is false when
// the snapshot recorded no identity for uri or the file is unchanged.
func (s Snapshot) StaleFile(uri string) (Change, bool) {
	identity, known := s.files[uri]
	if !known {
		return Change{}, false
	}
	current, available := statFileIdentity(identity.path)
	if available && current.size == identity.size && current.modifiedAt == identity.modifiedAt {
		return Change{}, false
	}
	return Change{Path: identity.path, Kind: ChangeContent}, true
}

// buildFileIdentities records current identities for the exact compiled files
// retained by one analysis. Files that cannot be statted are omitted, so a
// file legitimately absent from disk (for example an unsaved overlay) is never
// reported as stale by identity comparison alone.
func buildFileIdentities(paths []string) map[string]fileIdentity {
	identities := make(map[string]fileIdentity, len(paths))
	for _, path := range paths {
		identity, ok := statFileIdentity(path)
		if !ok {
			continue
		}
		uri, err := PathToURI(path)
		if err != nil {
			continue
		}
		identities[uri] = identity
	}
	return identities
}

// mergeFileIdentities returns an immutable copy-on-write identity map with the
// given per-URI updates applied.
func mergeFileIdentities(
	previous map[string]fileIdentity,
	updates map[string]fileIdentity,
) map[string]fileIdentity {
	if len(updates) == 0 {
		return previous
	}
	merged := make(map[string]fileIdentity, len(previous)+len(updates))
	maps.Copy(merged, previous)
	maps.Copy(merged, updates)
	return merged
}
