#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
install_dir=${GO_CODELENS_BIN_DIR:-"$HOME/.local/bin"}
install_path="$install_dir/go-codelens-ls"

source_path=$("$script_dir/build-server.sh")
mkdir -p "$install_dir"
install -m 0755 "$source_path" "$install_path"

echo "Installed $install_path"
