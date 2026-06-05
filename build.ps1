# build.ps1 — Build logcollector with auto-incrementing build number
# Usage: .\build.ps1 [-Version "1.0.0"] [-Output "logcollector.exe"]
#
# Each run increments the build number in build_number.txt and injects
# version, build number, and build date via Go's -ldflags.

param(
    [string]$Version = "2.1.6",
    [string]$Output = "logcollector.exe"
)

$ErrorActionPreference = "Stop"

$buildNumberFile = Join-Path $PSScriptRoot "build_number.txt"

# Read and increment build number
if (Test-Path $buildNumberFile) {
    $buildNum = [int](Get-Content $buildNumberFile -Raw).Trim()
} else {
    $buildNum = 0
}
$buildNum++
Set-Content -Path $buildNumberFile -Value $buildNum -NoNewline

# Build date
$buildDate = Get-Date -Format "yyyy-MM-dd HH:mm:ss"

# Build with ldflags
$ldflags = "-X 'main.appVersion=$Version' -X 'main.buildNumber=$buildNum' -X 'main.buildDate=$buildDate'"

Write-Host "Building logcollector v$Version (build #$buildNum) ..." -ForegroundColor Cyan
go build -ldflags $ldflags -o $Output .

if ($LASTEXITCODE -eq 0) {
    Write-Host "Build successful: $Output (v$Version build #$buildNum)" -ForegroundColor Green
} else {
    Write-Host "Build FAILED" -ForegroundColor Red
    exit 1
}
