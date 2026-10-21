# Keep build, install, benchmark, and version logic in scripts/.
set minimum-version := "1.56.0"
set default-list
set shell := ["sh", "-eu", "-c"]

export GOWORK := "off"
# env() returns "" for a set-but-empty TMPDIR; fall through like ${TMPDIR:-/tmp}.
export TMPDIR := if env("TMPDIR", "") != "" { env("TMPDIR", "") } else if env("TEMP", "") != "" { env("TEMP", "") } else { "/tmp" }
export GOCACHE := env("GOCACHE", TMPDIR / "go-build-cache")

# Build native server.
[group("build")]
[unix]
build $target_os="" $target_arch="" $output_path="":
    sh ./scripts/build-server.sh "$target_os" "$target_arch" "$output_path"

# Build native server.
[group("build")]
[windows]
build $target_os="" $target_arch="" $output_path="":
    pwsh -NoLogo -NoProfile -File ./scripts/build-server.ps1 -TargetOS "$target_os" -TargetArch "$target_arch" -OutputPath "$output_path"

# Build Zed Wasm adapter.
[group("build")]
build-wasm:
    cargo build --locked --target wasm32-wasip2

# Build and install host server.
[group("build")]
[unix]
install:
    sh ./scripts/install-server.sh

# Build and install host server.
[group("build")]
[windows]
install:
    pwsh -NoLogo -NoProfile -File ./scripts/install-server.ps1

# Run all local verification checks.
[group("check")]
check: check-version _check-format test race vet lint build-wasm

# Format Go, Rust, and justfile.
[group("check")]
fmt:
    cd server && golangci-lint fmt --config ../.golangci.yaml ./...
    cargo fmt
    just --fmt

# Run Go and Rust tests.
[group("check")]
test:
    go -C server test -mod=readonly ./...
    cargo test --locked

# Go 1.27: one checker token avoids golang/go#81122 until CL 841585.
[doc("Run Go tests with the race detector.")]
[env("GOMAXPROCS", "1")]
[group("check")]
[working-directory("server")]
race:
    go test -mod=readonly -race ./...

# Vet Go server.
[group("check")]
[working-directory("server")]
vet:
    go vet -mod=readonly ./...

# Lint Go and Rust; check Go formatting.
[group("check")]
lint:
    cd server && golangci-lint run --config ../.golangci.yaml ./...
    cargo clippy --all-targets --locked -- -D warnings

_check-format:
    just --fmt --check
    cargo fmt -- --check
    cd server && golangci-lint fmt --diff --config ../.golangci.yaml ./...

# Run benchmark and profiling suite.
[group("perf")]
bench:
    sh ./scripts/benchmark-server.sh

# Synchronize release manifest versions.
[group("release")]
set-version $release_version:
    sh ./scripts/set-version.sh "$release_version"

# Check manifest versions against an optional tag.
[group("release")]
check-version $release_version="":
    sh ./scripts/set-version.sh --check "$release_version"
