#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
target_os=${1:-$(go env GOOS)}
target_arch=${2:-$(go env GOARCH)}

case "$target_os/$target_arch" in
    darwin/arm64)
        target=aarch64-apple-darwin
        suffix=
        ;;
    darwin/amd64)
        target=x86_64-apple-darwin
        suffix=
        ;;
    linux/arm64)
        target=aarch64-unknown-linux-gnu
        suffix=
        ;;
    linux/amd64)
        target=x86_64-unknown-linux-gnu
        suffix=
        ;;
    windows/arm64)
        target=aarch64-pc-windows-msvc
        suffix=.exe
        ;;
    windows/amd64)
        target=x86_64-pc-windows-msvc
        suffix=.exe
        ;;
    *)
        echo "unsupported Go target: $target_os/$target_arch" >&2
        exit 1
        ;;
esac

default_output="$project_dir/bin/go-codelens-ls-$target$suffix"
output_path=${3:-$default_output}
case "$output_path" in
    /*) ;;
    *) output_path="$project_dir/$output_path" ;;
esac
mkdir -p "$(dirname -- "$output_path")"

cd "$project_dir/server"
CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch go build \
    -buildvcs=false \
    -mod=readonly \
    -trimpath \
    -ldflags="-s -w" \
    -o "$output_path" \
    ./cmd/go-codelens-ls

printf '%s\n' "$output_path"
