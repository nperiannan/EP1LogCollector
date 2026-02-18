# build-all.ps1 - Build logcollector for all platforms
# Usage: .\build-all.ps1 [-Version "1.3.0"]

param(
    [string]$Version = "1.3.0"
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

# Create release directory
$releaseDir = "release"
if (!(Test-Path $releaseDir)) {
    New-Item -ItemType Directory -Path $releaseDir | Out-Null
}

Write-Host "`nBuilding LogCollector v$Version (build #$buildNum) for all platforms..." -ForegroundColor Cyan
Write-Host ("=" * 70) -ForegroundColor Cyan

# Platform configurations: GOOS, GOARCH, OutputName
$platforms = @(
    @{OS="windows"; Arch="amd64"; Output="logcollector-windows-amd64.exe"; Name="Windows (x64)"},
    @{OS="linux"; Arch="amd64"; Output="logcollector-linux-amd64"; Name="Linux (x64)"},
    @{OS="darwin"; Arch="amd64"; Output="logcollector-darwin-amd64"; Name="macOS (Intel)"},
    @{OS="darwin"; Arch="arm64"; Output="logcollector-darwin-arm64"; Name="macOS (Apple Silicon)"}
)

$successCount = 0
$failedBuilds = @()

foreach ($platform in $platforms) {
    $outputPath = Join-Path $releaseDir $platform.Output
    
    Write-Host "`nBuilding for $($platform.Name)..." -ForegroundColor Yellow
    Write-Host "  Platform: $($platform.OS)/$($platform.Arch)" -ForegroundColor Gray
    Write-Host "  Output: $outputPath" -ForegroundColor Gray
    
    $env:GOOS = $platform.OS
    $env:GOARCH = $platform.Arch
    $env:CGO_ENABLED = "0"
    
    try {
        $output = & go build -ldflags $ldflags -o $outputPath . 2>&1
        
        if ($LASTEXITCODE -eq 0) {
            $fileSize = (Get-Item $outputPath).Length / 1MB
            Write-Host "  ✓ Success! Size: $($fileSize.ToString('N2')) MB" -ForegroundColor Green
            $successCount++
        } else {
            Write-Host "  ✗ Build FAILED" -ForegroundColor Red
            Write-Host "  Error: $output" -ForegroundColor Red
            $failedBuilds += $platform.Name
        }
    } catch {
        Write-Host "  ✗ Build FAILED with exception" -ForegroundColor Red
        Write-Host "  Error: $_" -ForegroundColor Red
        $failedBuilds += $platform.Name
    }
}

# Clean up environment variables
Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue

Write-Host "`n" + ("=" * 70) -ForegroundColor Cyan
Write-Host "Build Summary:" -ForegroundColor Cyan
Write-Host "  Version: v$Version (build #$buildNum)" -ForegroundColor White
Write-Host "  Build Date: $buildDate" -ForegroundColor White
Write-Host "  Success: $successCount / $($platforms.Count)" -ForegroundColor $(if ($successCount -eq $platforms.Count) {"Green"} else {"Yellow"})

if ($failedBuilds.Count -gt 0) {
    Write-Host "  Failed: $($failedBuilds -join ', ')" -ForegroundColor Red
    Write-Host "`nBuild completed with errors." -ForegroundColor Red
    exit 1
} else {
    Write-Host "`nAll builds completed successfully!" -ForegroundColor Green
    Write-Host "Binaries are in the '$releaseDir' directory." -ForegroundColor Green
}
