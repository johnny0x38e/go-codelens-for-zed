//go:build windows

package lsp

import "strings"

func pathsEqual(left, right string) bool {
	return strings.EqualFold(left, right)
}

func pathPrefix(path, prefix string) bool {
	return len(path) >= len(prefix) && strings.EqualFold(path[:len(prefix)], prefix)
}
