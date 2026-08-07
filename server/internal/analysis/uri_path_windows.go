//go:build windows

package analysis

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func fileURLPath(parsed *url.URL) (string, error) {
	path := filepath.FromSlash(parsed.Path)
	if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		return `\\` + parsed.Host + path, nil
	}
	if len(path) >= 3 && os.IsPathSeparator(path[0]) && path[2] == ':' {
		path = path[1:]
	}

	return path, nil
}

func filePathURL(path string) *url.URL {
	slashPath := filepath.ToSlash(path)
	if strings.HasPrefix(slashPath, "//") {
		host, remainder, found := strings.Cut(strings.TrimPrefix(slashPath, "//"), "/")
		if found {
			return &url.URL{
				Scheme: "file",
				Host:   host,
				Path:   "/" + remainder,
			}
		}
	}

	return &url.URL{
		Scheme: "file",
		Path:   "/" + slashPath,
	}
}

func samePath(left, right string) bool {
	return strings.EqualFold(left, right)
}
