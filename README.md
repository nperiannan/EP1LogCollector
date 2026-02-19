# LogCollector

Command-line tool for collecting logs and diagnostics from Kubernetes clusters and network devices via SSH bastion host.

## Download

**Latest Release: v1.3.3**

[GitHub Releases](https://github.com/nperiannan/EP1LogCollector/releases/tag/v1.3.3)

| Platform | Binary |
|----------|--------|
| Windows (x64) | `logcollector-windows-amd64.exe` |
| Linux (x64) | `logcollector-linux-amd64` |
| macOS (Intel) | `logcollector-darwin-amd64` |
| macOS (Apple Silicon) | `logcollector-darwin-arm64` |

**v1.3.3 Changes:**
- **Database query collection** - Execute PostgreSQL queries via SSH tunnel (Bastion → AWS → psql)
- **Config-based alias resolution** - Resolve psql aliases from config.yaml (no bash alias expansion needed)
- **Cross-database parameter sharing** - Extract values from query results and use in subsequent queries
- **Multi-value parameter support** - Comma-separated values execute queries N times automatically
- **Grouped query output** - Multi-value results clearly labeled by parameter value
- **Automatic dependency resolution** - Queries automatically ordered based on parameter availability
- **Security** - Only SELECT queries allowed; data-modifying statements rejected
- **Output directory fix** - Database files written to `C:\Logs\{timestamp}\Database\` per config

**v1.3.0 Changes:**
- **Multi-source credential retrieval** - Bastion passwords and JIRA tokens automatically stored in Windows Credential Manager
- **Security enhancement** - Credentials encrypted by Windows DPAPI, accessible only by your user account
- **First-run simplicity** - Enter credentials once, retrieved automatically thereafter
- **CI/CD support** - Environment variables (BASTION_PASSWORD, JIRA_API_TOKEN) for automation
- **Template support** - JIRA email field supports `{username}` and `{environment}` placeholders

**v1.2.2 Changes:**
- Automatic SFTP fallback when SCP fails (DNS resolution, network issues, etc.)

**v1.2.1 Changes:**
- Pod file collection: `matchPodName` option to filter logs by current pod name (excludes old pod logs)

**v1.2.0 Changes:**
- Loki-style replica log merging (combines replica pod logs into single files)
- Transaction ID correlation (groups related errors by correlation IDs)
- Timestamp sorting (chronological ordering of merged logs)
- Semantic error grouping (categorizes errors by type)
- Enhanced analytics report with transaction/request correlation section
- Security: encrypted passwords excluded from logger_info.txt

## What It Does

**Kubernetes:**
- Collect pod logs from multiple namespaces
- Time-based log collection (last X minutes/hours)
- Temporal workflow and schedule information
- Application version tracking
- System information (kubectl commands, pod status, nodes)
- Direct pod file collection (wildcard support, pod-name filtering)

**Network Devices:**
- EXOS/VOSS switch diagnostics via SSH
- CLI command execution with timeout handling
- Log file download via SFTP
- Ctrl+C recovery for stuck commands

**Download & Transfer:**
- Native SCP (2-4 MB/s, 10x faster than SFTP)
- Automatic bastion→AWS→local transfer
- Temporary SSH key generation and cleanup
- Parallel SFTP fallback if SCP unavailable

**Analysis & Filtering:**
- Pattern-based log analysis with error severity classification
- Cross-file correlation for common patterns
- Message filtering by key=value pairs or specific strings
- Filtered output in separate directory

**Integration:**
- JIRA attachment via REST API (`--jira XCP-12345`)
- Password encryption (AES-256-GCM)
- Template-based configuration (`{username}`, `{environment}`, `{timestamp}`)

## Prerequisites

- Go 1.16+ (building from source)
- SSH access to bastion host
- SSH key for target servers
- kubectl access on target server (for Kubernetes collection)
- OpenSSH client for SCP (Windows 10+, most Linux/Mac)

## Building

```powershell
.\build.ps1
```

Check version:
```bash
.\logcollector.exe -v
```

## Configuration

Configuration priority (highest to lowest):
1. Operation mode flags (`--all`, `--logs-only`, `--device-logs`, etc.)
2. Command-line flags (`-log-level`, `-time-duration`, etc.)
3. Config file (`config.yaml`)
4. Default values

### Basic config.yaml

```yaml
# Connection
username: your-username
environment: dev

bastion:
  host: bastion.example.com
  port: 22
  password: ""  # Leave empty - will prompt and encrypt

aws:
  host: "{environment}-console.qa.example.com"
  keyPath: ~/.ssh/id_ed25519

logs:
  outputDir: C:/Logs

options:
  downloadMethod: "scp"
  maxSSHSessions: 2
  logLevel: INFO

# Features (enable/disable as needed)
logCollection:
  enabled: true
  timeBasedCollection:
    enabled: true
    duration: "15m"
  
generalInfo:
  enabled: true

appVersionCollection:
  enabled: true

deviceLogCollection:
  enabled: false
```

See [full config example](config.yaml) for all options.

## Usage

```bash
# Show help
.\logcollector.exe -h

# Check version
.\logcollector.exe -v

# Collect everything
.\logcollector.exe --all

# Kubernetes logs only
.\logcollector.exe --logs-only

# Network device logs only
.\logcollector.exe --device-logs

# Database queries only
.\logcollector.exe --database

# System info only
.\logcollector.exe --sys-info

# Attach to JIRA
.\logcollector.exe --all --jira XCP-17614

# Last 30 minutes of logs
.\logcollector.exe --logs-only -time-duration 30m
```

## Operation Modes

| Mode | Description |
|------|-------------|
| `--all` | Logs + system info + app versions + devices + database (if enabled) |
| `--logs-only` | Kubernetes pod logs only |
| `--device-logs` | Network device diagnostics only |
| `--database` | Database query collection only |
| `--sys-info` | kubectl commands and cluster info |
| `--version` | Application version collection |
| (no flag) | Use config.yaml enabled/disabled settings |

## Features Detail

### Log Collection

Collects logs from Kubernetes pods via kubectl on remote server:
- Creates timestamped tar.gz archives on remote server
- Downloads using SCP or parallel SFTP
- Supports custom pod sources and namespaces
- Time-based collection (e.g., last 15 minutes)
- Auto-cleanup of remote archives after download

**Time-based collection:**
```yaml
logCollection:
  timeBasedCollection:
    enabled: true
    duration: "15m"  # 15m, 30m, 1h, 2h, etc.
```

### Temporal Workflow Collection

Collects Temporal workflow and schedule information:
- Workflow details (input, output, history)
- Schedule information
- Configurable time windows and limits

**Reference**: [Temporal Cheat Sheet](https://extremenetworks.atlassian.net/wiki/spaces/NCM/pages/371756102/Temporal+Cheat+Sheet)

```yaml
temporalWorkflowCollection:
  enabled: true
  numberOfWorkflows: 3
  namespace: "configuration"
```

### Network Device Collection

Direct SSH to EXOS/VOSS switches:
- Execute diagnostic CLI commands
- Download log files via SFTP
- Per-device and per-command timeouts
- Ctrl+C recovery for stuck commands
- Parallel device processing

```yaml
deviceLogCollection:
  enabled: true
  parallelDevices: 3
  globalTimeout: 600
  commandTimeout: 180
  devices:
    - name: "switch-01"
      host: "10.1.2.3"
      username: "admin"
      password: ""
      deviceType: "exos"
```

### Log Analytics

Pattern-based post-download analysis:
- Searches for configurable error patterns
- Cross-file correlation (e.g., "ERROR found in 5 files")
- Severity classification (CRITICAL, HIGH, MEDIUM, LOW)
- Context lines before/after matches
- Generates comprehensive report

**How it works:**
1. Extracts tar.gz archive
2. Scans all text files for patterns (case-insensitive)
3. Counts pattern occurrences per file
4. Correlates patterns appearing across multiple files
5. Assigns severity based on pattern type and frequency
6. Generates report with summary and details

**Limitations:**
- Pattern-based only (no transaction ID correlation)
- No timestamp-based correlation
- Does not understand causality or root causes
- Groups errors by keyword similarity, not semantic meaning

```yaml
logA nalytics:
  enabled: true
  errorPatterns:
    - "error"
    - "exception"
    - "failed"
    - "timeout"
  excludeKeywords:
    - "TestError"
  contextLines: 2
```

### Message Filter

Loki-style strict inclusion filtering:
- Filters logs by key=value pairs
- Keeps only lines matching specific strings
- Output in `filtered_logs_<timestamp>/` directory
- Preserves original archive directory structure

**Current behavior:**
- Each pod log file is filtered independently
- Does NOT combine replica logs (limitation)

```yaml
messageFilter:
  enabled: true
  keyValueFilters:
    - key: "ownerID"
      value: "1096"
  specificStrings:
    - "Payment"
```

### Pod File Collection

Collect files directly from inside pods:
- Wildcard support (`*.log`, `server*`, etc.)
- Multiple pods per namespace
- Preserves directory structure

```yaml
podFileCollection:
  enabled: true
  collections:
    - namespace: "common"
      podPrefix: "cs-configuration"
      logPath: "/var/log/configuration/"
      filePatterns:
        - "server.log"
        - "*.log.gz"
```

### JIRA Integration

Automatic file attachment to issues:
- REST API authentication (email + API token)
- Attaches all generated files
- Command-line flag: `--jira XCP-12345`

```yaml
jira:
  attachmentEnabled: true
  email: "user@example.com"
  apiToken: "your_api_token"
  baseURL: "https://example.atlassian.net"
```

## Output Structure

```
C:/Logs/
├── app_log_20260217_143025.tar.gz
│   ├── General/                    # System info
│   ├── Temporal/                   # Workflow data
│   ├── PodFiles/                   # Direct pod files
│   └── <namespace>/                # Pod logs
│       └── <pod-name>/
│           └── app.log
├── Database/                       # Database query results
│   └── platform_common_db_queries_20260217_143025.txt
├── Device_20260217_143025/
│   ├── switch-01_diagnostics.txt
│   └── switch-01_logs/
│       └── messages
├── logger_info_20260217_143025.txt           # Session log
├── dev_app_versions_20260217_143025.txt      # App versions
├── log_analytics_report_20260217_143025.txt  # Analysis
└── filtered_logs_20260217_143025/            # Filtered logs
    └── <namespace>/
        └── <pod-name>/
            └── app.log
```

## Troubleshooting

| Issue | Solution |
|-------|----------|
| "ssh: rejected: administratively prohibited" | Lower `maxSSHSessions` to 1-2 |
| "Password authentication failed" | Delete password from config, let tool re-prompt |
| "scp: command not found" | Install OpenSSH or use `--sftp` flag |
| Slow downloads | Use `--native-scp` (default) for 10x speed |
| "kubectl: command not found" | Ensure kubectl on target server, not local |
| Device commands stuck | Check `commandTimeout` in config |

**Debug logging:**
```bash
.\logcollector.exe -log-level DEBUG --all
```

## Configuration Reference

See [QUICKSTART.md](QUICKSTART.md) for detailed configuration examples and advanced usage.

## Credential Management

**Multi-Source Credential Retrieval** (v1.3.0+):

Credentials are retrieved from multiple sources in priority order:

### Bastion Password
**Priority Order:**
1. **Environment Variable**: `BASTION_PASSWORD`
2. **Windows Credential Manager** (Windows only, auto-saved on first use)
3. **Config file** (config.yaml, encrypted with AES-256-GCM)
4. **Interactive prompt** (saves to Credential Manager automatically)

**Storage location**: `LogCollector:Bastion:username@host`

### JIRA API Token
**Priority Order:**
1. **Environment Variable**: `JIRA_API_TOKEN`
2. **Windows Credential Manager** (Windows only, auto-saved on first use)
3. **Config file** (config.yaml, plaintext)
4. **Interactive prompt** (saves to Credential Manager automatically)

**Storage location**: `LogCollector:JIRA:email@domain.com`

### First-Run Experience
1. Run: `logcollector.exe --all`
2. Tool prompts for bastion password (one time)
3. Password automatically saved to Windows Credential Manager
4. Future runs retrieve automatically (no prompts)

### Viewing/Managing Credentials
**Windows:**
1. Press `Win+R`
2. Type: `control /name Microsoft.CredentialManager`
3. Click: **Windows Credentials**
4. Find entries starting with `LogCollector:`

**Environment Variables (CI/CD, automation):**
```powershell
# Windows PowerShell
$env:BASTION_PASSWORD = "your_password"
$env:JIRA_API_TOKEN = "your_token"
.\logcollector.exe --all --jira XCP-12345
```

```bash
# Linux/Mac
export BASTION_PASSWORD="your_password"
export JIRA_API_TOKEN="your_token"
./logcollector --all --jira XCP-12345
```

### Template Support
JIRA email field supports placeholders:
```yaml
jira:
  email: "{username}@extremenetworks.com"  # Replaced with actual username
```

### Security Benefits
- ✅ Credentials encrypted by Windows DPAPI
- ✅ Protected by your Windows login
- ✅ One-time setup
- ✅ No plaintext credentials in config files
- ✅ Backward compatible with existing configs
- ✅ IT-manageable via Group Policy

## Security

- Multi-source credential retrieval (Windows Credential Manager, env vars, config, prompts)
- Windows Credential Manager encryption via DPAPI (Windows only)
- Passwords encrypted with AES-256-GCM before saving to config file
- Temporary SSH keys auto-deleted after use
- StrictHostKeyChecking disabled (no fingerprint prompts)
- Do not commit config.yaml with credentials to Git

## License

Internal tool for Extreme Networks.
