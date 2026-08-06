#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
output_dir="$project_dir/bin"
output_path="$output_dir/go-implementation-lens-ls-aarch64-apple-darwin"

mkdir -p "$output_dir"

cd "$project_dir/server"
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build \
    -trimpath \
    -o "$output_path" \
    ./cmd/go-implementation-lens-ls

echo "Built $output_path"
