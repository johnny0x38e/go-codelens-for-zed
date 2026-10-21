package lsp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestServeRefreshesLensTargetsAfterSilentExternalEdit covers the failure mode
// where a file is rewritten by a tool that never sends a watched-file
// notification. The cached snapshot must notice the on-disk change and return
// coordinates for the current revision.
func TestServeRefreshesLensTargetsAfterSilentExternalEdit(t *testing.T) {
	moduleDir, declarationURI, referenceURI, referenceFile := writeFreshnessFixture(t)

	session := startFreshnessTestSession(t)
	session.initialize(t, moduleDir, true)
	assertFreshnessLensTarget(t, session.requestLenses(t, declarationURI, 2), referenceURI, 2)

	writeFreshnessExternalEdit(t, referenceFile)

	lenses := session.requestLenses(t, declarationURI, 3)
	assertFreshnessLensTarget(t, lenses, referenceURI, 6)
	refresh := readTestFrame(t, session.reader)
	if got := refresh["method"]; got != codeLensRefreshMethod {
		t.Fatalf("request after silent external edit = %#v, want %s", refresh, codeLensRefreshMethod)
	}

	session.finish(t)
}

func TestServeRequestsCodeLensRefreshForWatchedFileChanges(t *testing.T) {
	moduleDir, declarationURI, referenceURI, referenceFile := writeFreshnessFixture(t)

	session := startFreshnessTestSession(t)
	session.initialize(t, moduleDir, true)
	assertFreshnessLensTarget(t, session.requestLenses(t, declarationURI, 2), referenceURI, 2)

	writeFreshnessExternalEdit(t, referenceFile)
	writeTestFrame(t, session.input, map[string]any{
		"jsonrpc": "2.0",
		"method":  watchedFilesMethod,
		"params":  watchedFilesParams(t, referenceFile),
	})
	refresh := readTestFrame(t, session.reader)
	if got := refresh["method"]; got != codeLensRefreshMethod {
		t.Fatalf("request after watched-file change = %#v, want %s", refresh, codeLensRefreshMethod)
	}

	assertFreshnessLensTarget(t, session.requestLenses(t, declarationURI, 3), referenceURI, 6)
	session.finish(t)
}

func TestServeDoesNotRequestCodeLensRefreshForEditorChanges(t *testing.T) {
	moduleDir, declarationURI, referenceURI, referenceFile := writeFreshnessFixture(t)

	session := startFreshnessTestSession(t)
	session.initialize(t, moduleDir, true)

	original, err := os.ReadFile(referenceFile)
	if err != nil {
		t.Fatal(err)
	}
	updated := "// editor change 1\n// editor change 2\n// editor change 3\n// editor change 4\n" + string(original)
	writeSemanticTestDidOpen(t, session.input, referenceURI, original)
	writeTestFrame(t, session.input, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didChange",
		"params": map[string]any{
			"textDocument":   map[string]any{"uri": referenceURI, "version": 2},
			"contentChanges": []map[string]any{{"text": updated}},
		},
	})

	assertFreshnessLensTarget(t, session.requestLenses(t, declarationURI, 2), referenceURI, 6)
	session.finish(t)
	if frame, err := readFrame(session.reader); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected frame after editor change = %#v, error = %v", frame, err)
	}
}

func TestServeKeepsFreshnessValidationWithoutRefreshSupport(t *testing.T) {
	moduleDir, declarationURI, referenceURI, referenceFile := writeFreshnessFixture(t)

	session := startFreshnessTestSession(t)
	session.initialize(t, moduleDir, false)
	assertFreshnessLensTarget(t, session.requestLenses(t, declarationURI, 2), referenceURI, 2)

	writeFreshnessExternalEdit(t, referenceFile)
	assertFreshnessLensTarget(t, session.requestLenses(t, declarationURI, 3), referenceURI, 6)
	session.finish(t)
	if frame, err := readFrame(session.reader); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected refresh without client support = %#v, error = %v", frame, err)
	}
}

type freshnessTestSession struct {
	input  *io.PipeWriter
	reader *bufio.Reader
	done   chan error
}

func startFreshnessTestSession(t *testing.T) *freshnessTestSession {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	done := make(chan error, 1)
	state := newServerState()
	go func() {
		err := serveWithState(context.Background(), inputReader, outputWriter, nil, state)
		_ = outputWriter.Close()
		done <- err
	}()
	t.Cleanup(func() {
		_ = inputWriter.Close()
		_ = outputReader.Close()
		_ = outputWriter.Close()
	})
	return &freshnessTestSession{
		input:  inputWriter,
		reader: bufio.NewReader(outputReader),
		done:   done,
	}
}

func (s *freshnessTestSession) initialize(t *testing.T, moduleDir string, refreshSupport bool) {
	t.Helper()
	writeTestFrame(t, s.input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"rootUri": semanticTestFileURI(t, moduleDir),
			"capabilities": map[string]any{
				"workspace": map[string]any{
					"codeLens": map[string]any{"refreshSupport": refreshSupport},
				},
			},
		},
	})
	_ = readTestFrame(t, s.reader)
}

func (s *freshnessTestSession) requestLenses(t *testing.T, documentURI string, id int) []any {
	t.Helper()
	writeSemanticTestCodeLensRequest(t, s.input, id, documentURI)
	response := readTestFrame(t, s.reader)
	if got := response["id"]; got != float64(id) {
		t.Fatalf("response id = %#v, want %d", got, id)
	}
	return semanticTestResponseLenses(t, response)
}

func (s *freshnessTestSession) finish(t *testing.T) {
	t.Helper()
	writeTestFrame(t, s.input, map[string]any{"jsonrpc": "2.0", "method": "exit"})
	if err := s.input.Close(); err != nil {
		t.Fatalf("close LSP input: %v", err)
	}
	if err := <-s.done; err != nil {
		t.Fatalf("serveWithState() error = %v", err)
	}
}

func writeFreshnessFixture(t *testing.T) (moduleDir, declarationURI, referenceURI, referenceFile string) {
	t.Helper()
	moduleDir = t.TempDir()
	writeSemanticTestFile(
		t,
		filepath.Join(moduleDir, "go.mod"),
		[]byte("module example.com/freshness\n\ngo 1.26\n"),
	)
	declarationFile := filepath.Join(moduleDir, "lib.go")
	referenceFile = filepath.Join(moduleDir, "use.go")
	writeSemanticTestFile(t, declarationFile, []byte("package sample\n\nfunc Target() {}\n"))
	writeSemanticTestFile(t, referenceFile, []byte("package sample\n\nfunc Use() { Target() }\n"))
	return moduleDir, semanticTestFileURI(t, declarationFile), semanticTestFileURI(t, referenceFile), referenceFile
}

func writeFreshnessExternalEdit(t *testing.T, referenceFile string) {
	t.Helper()
	original, err := os.ReadFile(referenceFile) // #nosec G304 -- test-owned temporary file.
	if err != nil {
		t.Fatal(err)
	}
	writeSemanticTestFile(
		t,
		referenceFile,
		[]byte("// external 1\n// external 2\n// external 3\n// external 4\n"+string(original)),
	)
}

func assertFreshnessLensTarget(t *testing.T, lenses []any, uri string, line float64) {
	t.Helper()
	lens := findSemanticTestLens(t, lenses, "1 reference", 2)
	assertSemanticTestTargets(t, lens, uri, line, 13)
}
