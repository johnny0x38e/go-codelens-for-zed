//go:build windows

package lsp

import "testing"

func TestWindowsPathComparison(t *testing.T) {
	t.Parallel()

	if !pathsEqual(`C:\Workspace\Module`, `c:\workspace\module`) {
		t.Fatal("pathsEqual() treated drive paths as case-sensitive")
	}
	if !pathWithinRoot(`c:\workspace\module\source.go`, `C:\Workspace\Module`) {
		t.Fatal("pathWithinRoot() treated nested drive paths as case-sensitive")
	}
	if pathWithinRoot(`C:\workspace\module-old\source.go`, `C:\workspace\module`) {
		t.Fatal("pathWithinRoot() accepted a sibling with the same prefix")
	}
}
