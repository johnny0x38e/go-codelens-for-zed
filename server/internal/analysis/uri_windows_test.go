//go:build windows

package analysis

import "testing"

func TestWindowsFileURIs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		uri  string
		path string
	}{
		{name: "drive", uri: "file:///C:/workspace/source.go", path: `C:\workspace\source.go`},
		{name: "UNC", uri: "file://server/share/source.go", path: `\\server\share\source.go`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path, err := URIToPath(test.uri)
			if err != nil {
				t.Fatalf("URIToPath() error = %v", err)
			}
			if !samePath(path, test.path) {
				t.Fatalf("URIToPath(%q) = %q, want %q", test.uri, path, test.path)
			}

			uri, err := PathToURI(test.path)
			if err != nil {
				t.Fatalf("PathToURI() error = %v", err)
			}
			if uri != test.uri {
				t.Fatalf("PathToURI(%q) = %q, want %q", test.path, uri, test.uri)
			}
		})
	}
}
