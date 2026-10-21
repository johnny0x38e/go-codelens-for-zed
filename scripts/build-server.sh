#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
# extension.toml is the release-version source shared with the Zed adapter.
version=$(awk -F '"' '/^version = "[^"]+"$/ { print $2; found++ } END { if (found != 1) exit 1 }' "$project_dir/extension.toml")
if ! printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
    echo "invalid stable semantic version in extension.toml: $version" >&2
    exit 1
fi

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
lsp_package=$(go list -mod=readonly -f '{{.ImportPath}}' ./internal/lsp)
CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch go build \
    -buildvcs=false \
    -mod=readonly \
    -trimpath \
    -ldflags="-s -w -X=${lsp_package}.serverVersion=$version" \
    -o "$output_path" \
    ./cmd/go-codelens-ls

printf '%s\n' "$output_path"
