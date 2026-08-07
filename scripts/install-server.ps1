param(
    [string]$InstallDir = $env:GO_CODELENS_BIN_DIR
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    $InstallDir = Join-Path $HOME ".local/bin"
}
$InstallDir = [System.IO.Path]::GetFullPath($InstallDir)
$sourcePath = & (Join-Path $PSScriptRoot "build-server.ps1")
$installPath = Join-Path $InstallDir "go-codelens-ls.exe"

[System.IO.Directory]::CreateDirectory($InstallDir) | Out-Null
Copy-Item -LiteralPath $sourcePath -Destination $installPath -Force

Write-Output "Installed $installPath"
