# run-gui.ps1 — Launch LogCollector in GUI (web) mode with an optional custom port
#
# Usage:
#   .\run-gui.ps1                       # GUI on default port 9090
#   .\run-gui.ps1 -Port 8080           # GUI on a custom port
#   .\run-gui.ps1 -Port 8080 -Build    # Rebuild the binary first, then launch
#   .\run-gui.ps1 -Config my.yaml      # Use a different config file
#
# The GUI binary serves on http://127.0.0.1:<port> and opens your browser
# automatically. Press Ctrl+C in this window to stop the server.

[CmdletBinding()]
param(
    [ValidateRange(1024, 65535)]
    [int]$Port = 9090,

    [string]$Config = "config.yaml",

    [switch]$Build
)

$ErrorActionPreference = "Stop"

# Resolve paths relative to this script so it works from any working directory
$root    = $PSScriptRoot
$exeName = "logcollector.exe"
$exePath = Join-Path $root $exeName
$buildScript = Join-Path $root "build.ps1"

# Build the binary if requested or if it doesn't exist yet
if ($Build -or -not (Test-Path $exePath)) {
    if (-not (Test-Path $buildScript)) {
        Write-Host "ERROR: build.ps1 not found and $exeName is missing. Cannot build." -ForegroundColor Red
        exit 1
    }
    Write-Host "Building $exeName ..." -ForegroundColor Cyan
    & $buildScript -Output $exeName
    if ($LASTEXITCODE -ne 0) {
        Write-Host "ERROR: Build failed." -ForegroundColor Red
        exit 1
    }
}

# Resolve the config path (relative to script root if not absolute)
if (-not [System.IO.Path]::IsPathRooted($Config)) {
    $configPath = Join-Path $root $Config
} else {
    $configPath = $Config
}
if (-not (Test-Path $configPath)) {
    Write-Host "WARNING: Config file not found: $configPath (GUI will start with an empty config)" -ForegroundColor Yellow
}

# Warn early if the chosen port is already in use
$inUse = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($inUse) {
    Write-Host "ERROR: Port $Port is already in use. Choose another with -Port <number>." -ForegroundColor Red
    exit 1
}

Write-Host "Starting LogCollector GUI on http://127.0.0.1:$Port" -ForegroundColor Green
Write-Host "Config: $configPath" -ForegroundColor DarkGray
Write-Host "Press Ctrl+C to stop." -ForegroundColor DarkGray

# Launch the GUI server (blocks until Ctrl+C; the binary opens the browser itself)
& $exePath --gui --gui-port $Port --config $configPath
exit $LASTEXITCODE
