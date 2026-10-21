#!/bin/sh

set -eu

usage() {
    printf '%s\n' \
        'Usage:' \
        '    ./scripts/set-version.sh <version>' \
        '    ./scripts/set-version.sh --check [<version>]' \
        '' \
        'The version must use stable semantic-version syntax, for example 0.3.3.' >&2
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)

mode=update
case ${1:-} in
    --check)
        mode=check
        shift
        ;;
    -h|--help)
        usage
        exit 0
        ;;
esac

if [ "$mode" = update ] && [ "$#" -ne 1 ]; then
    usage
    exit 2
fi
if [ "$mode" = check ] && [ "$#" -gt 1 ]; then
    usage
    exit 2
fi
requested_version=${1:-}

# metadata_value reads or rewrites exactly one version declaration. The parser
# is deliberately structural enough to avoid touching dependency versions.
metadata_value() {
    action=$1
    kind=$2
    path=$3
    awk -v action="$action" -v kind="$kind" -v path="$path" -v version="${version:-}" '
        {
            target = 0
            if (kind == "extension") {
                if ($0 ~ /^\[/) {
                    section = $0
                }
                target = (section == "" && $0 ~ /^version = "[^"]+"$/)
            } else if (kind == "cargo") {
                if ($0 ~ /^\[/) {
                    in_package = ($0 == "[package]")
                }
                target = (in_package && $0 ~ /^version = "[^"]+"$/)
            } else if (kind == "lock") {
                if ($0 == "[[package]]") {
                    in_target_package = 0
                } else if ($0 == "name = \"go-codelens-for-zed\"") {
                    in_target_package = 1
                }
                target = (in_target_package && $0 ~ /^version = "[^"]+"$/)
            }

            if (target) {
                found++
                if (action == "write") {
                    sub(/"[^"]+"/, "\"" version "\"")
                } else {
                    split($0, fields, "\"")
                    print fields[2]
                }
            }
            if (action == "write") {
                print
            }
        }
        END {
            if (found != 1) {
                print "expected exactly one version declaration in " path > "/dev/stderr"
                exit 1
            }
        }
    ' "$path"
}

if [ -n "$requested_version" ]; then
    version=${requested_version#v}
else
    version=$(metadata_value read extension "$project_dir/extension.toml")
fi
if ! printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
    echo "invalid stable semantic version: $requested_version" >&2
    exit 2
fi

verify_version() {
    extension=$(metadata_value read extension "$project_dir/extension.toml")
    cargo=$(metadata_value read cargo "$project_dir/Cargo.toml")
    lock=$(metadata_value read lock "$project_dir/Cargo.lock")

    if [ "$extension" != "$version" ] ||
        [ "$cargo" != "$version" ] ||
        [ "$lock" != "$version" ]; then
        printf '%s\n' \
            "version metadata does not match $version:" \
            "  extension.toml:                 $extension" \
            "  Cargo.toml:                     $cargo" \
            "  Cargo.lock:                     $lock" >&2
        return 1
    fi
}

if [ "$mode" = check ]; then
    verify_version
    echo "version metadata matches $version"
    exit 0
fi

for command_name in awk cargo mktemp; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
        echo "required command not found: $command_name" >&2
        exit 1
    fi
done

temporary_directory=$(mktemp -d "${TMPDIR:-/tmp}/go-codelens-version.XXXXXX")
mkdir -p "$temporary_directory/generated" "$temporary_directory/backup"

# All rewrites are prepared before the working tree is touched. Once applied,
# the EXIT trap restores every original file unless all validation completes.
applied=0
complete=0
cleanup() {
    status=$?
    trap - EXIT
    if [ "$applied" -eq 1 ] && [ "$complete" -eq 0 ]; then
        cp "$temporary_directory/backup/extension.toml" "$project_dir/extension.toml"
        cp "$temporary_directory/backup/Cargo.toml" "$project_dir/Cargo.toml"
        cp "$temporary_directory/backup/Cargo.lock" "$project_dir/Cargo.lock"
        echo "version update failed; restored the original metadata" >&2
    fi
    rm -rf "$temporary_directory"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

# Each rewriter requires exactly one structural match. This prevents a format
# change or duplicate declaration from silently producing a partial release.
metadata_value write extension "$project_dir/extension.toml" \
    >"$temporary_directory/generated/extension.toml"
metadata_value write cargo "$project_dir/Cargo.toml" \
    >"$temporary_directory/generated/Cargo.toml"
metadata_value write lock "$project_dir/Cargo.lock" \
    >"$temporary_directory/generated/Cargo.lock"

cp "$project_dir/extension.toml" "$temporary_directory/backup/extension.toml"
cp "$project_dir/Cargo.toml" "$temporary_directory/backup/Cargo.toml"
cp "$project_dir/Cargo.lock" "$temporary_directory/backup/Cargo.lock"

# Apply the prepared set only after every generated file passed its structural
# checks. Cargo then verifies that the edited manifest and lock remain coherent.
applied=1
cp "$temporary_directory/generated/extension.toml" "$project_dir/extension.toml"
cp "$temporary_directory/generated/Cargo.toml" "$project_dir/Cargo.toml"
cp "$temporary_directory/generated/Cargo.lock" "$project_dir/Cargo.lock"

(cd "$project_dir" && cargo metadata --locked --no-deps --format-version 1 >/dev/null)
verify_version

complete=1
echo "updated version metadata to $version"
