//go:build !windows

package analysis

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

func fileURLPath(parsed *url.URL) (string, error) {
	if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		return "", fmt.Errorf("unsupported file URI host %q", parsed.Host)
	}

	return filepath.FromSlash(parsed.Path), nil
}

func filePathURL(path string) *url.URL {
	return &url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(path),
	}
}

func samePath(left, right string) bool {
	return left == right
}
