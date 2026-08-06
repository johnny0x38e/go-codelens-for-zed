package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/johnny/go-implementation-lens-for-zed/server/internal/lsp"
)

func main() {
	if err := lsp.Serve(context.Background(), os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "go-implementation-lens-ls: %v\n", err)
		os.Exit(1)
	}
}
