//go:build !windows

package lsp

import "strings"

func pathsEqual(left, right string) bool {
	return left == right
}

func pathPrefix(path, prefix string) bool {
	return strings.HasPrefix(path, prefix)
}
