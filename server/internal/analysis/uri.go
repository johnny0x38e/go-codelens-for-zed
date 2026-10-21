package analysis

import (
	"fmt"
	"net/url"
	"path/filepath"
)

// URIToPath converts an absolute file URI to a cleaned absolute path.
func URIToPath(rawURI string) (string, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return "", fmt.Errorf("parse document URI: %w", err)
	}
	if parsed.Scheme != "file" {
		return "", fmt.Errorf("unsupported URI scheme %q", parsed.Scheme)
	}
	if parsed.Opaque != "" {
		return "", fmt.Errorf("unsupported opaque file URI %q", rawURI)
	}
	path, err := fileURLPath(parsed)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
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
	return filePathURL(filepath.Clean(absolute)).String(), nil
}
