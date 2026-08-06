package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/johnny/go-implementation-lens-for-zed/server/internal/lsp"
)

func main() {
	trace, closeTrace, err := openTrace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go-implementation-lens-ls: %v\n", err)
		os.Exit(1)
	}
	defer closeTrace()

	if err := lsp.ServeWithTrace(context.Background(), os.Stdin, os.Stdout, trace); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "go-implementation-lens-ls: %v\n", err)
		os.Exit(1)
	}
}

func openTrace() (io.Writer, func(), error) {
	path := os.Getenv("GO_IMPLEMENTATION_LENS_TRACE")
	if path == "" {
		return io.Discard, func() {}, nil
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open trace file: %w", err)
	}
	return file, func() { _ = file.Close() }, nil
}
