// Command go-codelens-ls serves Go implementation and reference Code Lenses
// over the Language Server Protocol.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/lsp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "go-codelens-ls: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	trace, closeTrace, err := openTrace()
	if err != nil {
		return err
	}
	defer closeTrace()

	err = lsp.ServeWithTrace(context.Background(), os.Stdin, os.Stdout, trace)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	return nil
}

func openTrace() (io.Writer, func(), error) {
	path := os.Getenv("GO_CODELENS_TRACE")
	if path == "" {
		return nil, func() {}, nil
	}

	file, err := os.OpenFile(
		path,
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0o600,
	) //nolint:gosec // The user explicitly selects the trace destination.
	if err != nil {
		return nil, nil, fmt.Errorf("open trace file: %w", err)
	}

	return file, func() { _ = file.Close() }, nil //nolint:errcheck // There is no recovery path for a diagnostic trace close failure.
}
