# Build Instructions for LogCollector

## Quick Build

### Option 1: Use the build script (RECOMMENDED)
```powershell
.\build-all.ps1
```
This will build all 4 platforms (Windows, Linux, macOS Intel, macOS ARM) and place binaries in the `release/` directory.

---

### Option 2: Build for Windows only
```powershell
# Correct way - builds all .go files in the package
go build -o logcollector.exe

# Alternative - explicitly list all files
go build -o logcollector.exe logcollector.go credentials_windows.go
```

---

## ❌ Common Mistakes

### DON'T do this:
```powershell
# This will FAIL with "undefined: getJIRAApiToken, getBastionPassword"
go build -o logcollector.exe logcollector.go
```

**Why?** When you specify a single `.go` file, Go only compiles that file and doesn't include the platform-specific credential files (`credentials_windows.go` or `credentials_other.go`).

---

## Platform-Specific Files

The project uses build tags for platform-specific code:

- **`credentials_windows.go`** - Windows Credential Manager implementation
  - Build tag: `//go:build windows`
  - Functions: `getJIRAApiToken()`, `getBastionPassword()`
  
- **`credentials_other.go`** - Linux/macOS fallback implementation  
  - Build tag: `//go:build !windows`
  - Functions: Same interface, uses env vars/config

When you run `go build` (without specifying files), Go automatically:
1. Detects your platform
2. Selects the correct credential file based on build tags
3. Compiles all `.go` files in the package

---

## Manual Cross-Platform Build

```powershell
# Windows (x64)
$env:GOOS="windows"; $env:GOARCH="amd64"; go build -o logcollector-windows.exe

# Linux (x64)
$env:GOOS="linux"; $env:GOARCH="amd64"; go build -o logcollector-linux

# macOS (Intel)
$env:GOOS="darwin"; $env:GOARCH="amd64"; go build -o logcollector-macos-intel

# macOS (Apple Silicon)
$env:GOOS="darwin"; $env:GOARCH="arm64"; go build -o logcollector-macos-arm
```

---

## Troubleshooting

### Error: "undefined: getJIRAApiToken"
**Solution:** Don't specify individual files. Use `go build` or the build script.

### Error: "undefined: getBastionPassword"  
**Solution:** Same as above - use `go build` without specifying files.

### Files are still in PWD after build
**Solution:** This is normal. The executable is in PWD. Run `.\logcollector.exe` to test it.

---

## Recommended Workflow

1. Make code changes to `logcollector.go` or credential files
2. Run `go build` to test locally
3. Run `.\build-all.ps1` to build all platforms for release
4. Test the binary: `.\logcollector.exe --all`
5. Commit and push changes
6. Tag and release

---

## Build Script Details

The `build-all.ps1` script:
- Increments build number automatically
- Builds for 4 platforms (Windows, Linux, macOS Intel, macOS ARM)
- Adds version info and build date
- Places binaries in `release/` directory
- Reports build summary with file sizes

**Usage:**
```powershell
.\build-all.ps1          # Uses default version from script
.\build-all.ps1 -Version "1.4.0"  # Override version
```
