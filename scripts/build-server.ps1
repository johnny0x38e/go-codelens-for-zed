param(
    [string]$TargetOS,
    [string]$TargetArch,
    [string]$OutputPath
)

$ErrorActionPreference = "Stop"

$projectDir = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))
if ([string]::IsNullOrWhiteSpace($TargetOS)) {
    $TargetOS = (& go env GOOS).Trim()
}
if ([string]::IsNullOrWhiteSpace($TargetArch)) {
    $TargetArch = (& go env GOARCH).Trim()
}

$target = switch ("$TargetOS/$TargetArch") {
    "darwin/arm64" { "aarch64-apple-darwin" }
    "darwin/amd64" { "x86_64-apple-darwin" }
    "linux/arm64" { "aarch64-unknown-linux-gnu" }
    "linux/amd64" { "x86_64-unknown-linux-gnu" }
    "windows/arm64" { "aarch64-pc-windows-msvc" }
    "windows/amd64" { "x86_64-pc-windows-msvc" }
    default { throw "unsupported Go target: $TargetOS/$TargetArch" }
}
$suffix = if ($TargetOS -eq "windows") { ".exe" } else { "" }

if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $projectDir "bin/go-codelens-ls-$target$suffix"
} elseif (-not [System.IO.Path]::IsPathRooted($OutputPath)) {
    $OutputPath = Join-Path $projectDir $OutputPath
}
$OutputPath = [System.IO.Path]::GetFullPath($OutputPath)
[System.IO.Directory]::CreateDirectory([System.IO.Path]::GetDirectoryName($OutputPath)) | Out-Null

$previousCgoEnabled = $env:CGO_ENABLED
$previousGoOS = $env:GOOS
$previousGoArch = $env:GOARCH
try {
    $env:CGO_ENABLED = "0"
    $env:GOOS = $TargetOS
    $env:GOARCH = $TargetArch

    Push-Location (Join-Path $projectDir "server")
    try {
        & go build -buildvcs=false -mod=readonly -trimpath "-ldflags=-s -w" -o $OutputPath ./cmd/go-codelens-ls
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed with exit code $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }
} finally {
    $env:CGO_ENABLED = $previousCgoEnabled
    $env:GOOS = $previousGoOS
    $env:GOARCH = $previousGoArch
}

Write-Output $OutputPath
