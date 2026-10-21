package analysis

import (
	"path/filepath"
	"testing"
)

func TestPathToURIRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "source with spaces", "示例.go")
	uri, err := PathToURI(path)
	if err != nil {
		t.Fatalf("PathToURI() error = %v", err)
	}
	got, err := URIToPath(uri)
	if err != nil {
		t.Fatalf("URIToPath() error = %v", err)
	}
	if !samePath(got, path) {
		t.Fatalf("URI round trip = %q, want %q", got, path)
	}
}

func TestURIToPathRejectsNonFileAndOpaqueURIs(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{"https://example.com/source.go", "file:opaque"} {
		if _, err := URIToPath(uri); err == nil {
			t.Fatalf("URIToPath(%q) unexpectedly succeeded", uri)
		}
	}
}
