# Quick Start Guide - LogCollector

A command-line tool to collect logs, system info, and application versions from Kubernetes clusters and network devices via SSH bastion host.

## Latest Release: v1.1.0

**Download cross-platform binaries from:** [GitHub Releases](https://github.com/nperiannan/EP1LogCollector/releases/tag/v1.1.0)

| Platform | Binary |
|----------|--------|
| Windows (x64) | `logcollector-windows-amd64.exe` |
| Linux (x64) | `logcollector-linux-amd64` |
| macOS (Intel) | `logcollector-darwin-amd64` |
| macOS (Apple Silicon) | `logcollector-darwin-arm64` |

**What's New in v1.1.0:**
- Renamed to **LogCollector** (was fetchlogs)
- Loki-style strict inclusion filter
- Build versioning with `-v` flag
- Automated build system with version injection

## Prerequisites

**1. Local Machine Requirements:**
- Go 1.16+ (if building from source)
- OpenSSH client with `scp` and `ssh-keygen` commands
  - **Windows**: Built-in (Windows 10+)
  - **Linux/Mac**: Pre-installed
  - Verify: Run `scp -V` and `ssh-keygen -V`

**2. Access Requirements:**
- SSH access to bastion host (password or key)
- AWS/target server accessible from bastion (requires SSH key)
- `kubectl` installed on target server
- Kubernetes cluster access from target server

## Quick Setup

### 1. Get the Tool

**Option A: Download Pre-built Binary (Recommended)**
- Download from [GitHub Releases](https://github.com/nperiannan/EP1LogCollector/releases/tag/v1.1.0)
- Extract/rename to `logcollector.exe` (Windows) or `logcollector` (Linux/Mac)
- Check version: `logcollector.exe -v` or `./logcollector -v`

**Option B: Build from Source**
```bash
go build -o logcollector.exe logcollector.go
# Or use automated build script
.\build.ps1
```

### 2. Configure `config.yaml`

**Minimum Required Configuration:**
```yaml
# Connection Details
username: your-username          # Your username for SSH connections
environment: dev                 # Environment name (dev/staging/prod)

# Bastion Server
bastion:
  host: bastion.example.com
  port: 22
  password: ""                   # Leave empty - tool will prompt and encrypt

# Target Server (AWS/Remote)
aws:
  host: "{environment}-server.example.com"  # Uses environment variable
  keyPath: ~/.ssh/id_rsa         # SSH key for target server

# Download Location
logs:
  outputDir: C:/Logs             # Windows: C:/Logs, Linux: /home/user/logs

# Performance Settings
options:
  downloadMethod: "scp"          # "scp" (fast) or "sftp" (parallel)
  maxSSHSessions: 1              # 1-2 recommended to avoid SSH session limits
  logLevel: INFO                 # DEBUG, INFO, WARN, ERROR
```

**First Run:**
- Leave `bastion.password` empty
- Tool will prompt for password
- Password gets encrypted and saved to config automatically

## How to Use

### Basic Commands

**Show Help:**
```bash
.\logcollector.exe -h
```
→ Display all available command-line flags and operation modes

**Check Version:**
```bash
.\logcollector.exe -v
```
→ Shows version, build number, and build date

**Collect Everything:**
```bash
.\logcollector.exe --all
```
→ Logs + System Info + App Versions + Temporal Workflows + Schedules + Network Devices

**Collect Only Logs:**
```bash
.\logcollector.exe --logs-only
```
→ Kubernetes pod logs only

**Collect System Info:**
```bash
.\logcollector.exe --sys-info
```
→ Cluster health, pod status, node info

**Collect App Versions:**
```bash
.\logcollector.exe --version
```
→ Application versions from all namespaces

**Collect Network Device Logs:**
```bash
.\logcollector.exe --device-logs
```
→ Network device diagnostics and log files (EXOS/VOSS switches)

**Use Config File Settings:**
```bash
.\logcollector.exe
```
→ Uses enabled/disabled flags from config.yaml

### Command-Line Overrides

**Change download method:**
```bash
.\logcollector.exe --native-scp    # Force native SCP (fast)
.\logcollector.exe --sftp          # Force SFTP (parallel)
```

**Change log level:**
```bash
.\logcollector.exe -log-level DEBUG
```

**Time-based log collection:**
```bash
.\logcollector.exe --logs-only -time-duration 30m   # Last 30 minutes
.\logcollector.exe --logs-only -time-duration 2h    # Last 2 hours
```

**Attach files to JIRA issue:**
```bash
.\logcollector.exe --all --jira XCP-17614           # Attach to JIRA issue
.\logcollector.exe --logs-only --jira XCP-12345     # Logs only, attach to JIRA
```

## Config.yaml Reference

### Essential Sections

| Section | Required Fields | Purpose |
|---------|----------------|---------|
| `username` | Username | SSH user for all connections |
| `environment` | Environment name | Used in templates `{environment}` |
| `bastion.host` | Hostname/IP | Bastion server address |
| `bastion.password` or `bastion.keyPath` | Auth method | How to connect to bastion |
| `aws.host` | Hostname/IP | Target server (AWS/remote) |
| `aws.keyPath` | SSH key path | Key for target server authentication |
| `logs.outputDir` | Directory path | Where to save downloaded files |
| `jira.email` (optional) | Email | Atlassian account email for JIRA integration |
| `jira.apiToken` (optional) | API token | Token from https://id.atlassian.com/manage-profile/security/api-tokens |

### Feature Toggles

**Log Collection** (`logCollection.enabled`):
- Collects logs from Kubernetes pods
- Creates timestamped tar.gz archives
- Auto-cleanup remote archives after download

**System Info** (`generalInfo.enabled`):
- Runs kubectl commands (get pods, describe nodes, etc.)
- Captures cluster health metrics
- Per-command output files

**App Versions** (`appVersionCollection.enabled`):
- Collects application versions from pods
- Supports multiple namespaces
- Timestamped output files

**Temporal Workflows** (`temporalWorkflowCollection.enabled`):
- Collects workflow details (input/output/history)
- Configurable workflow count and time duration
- Separate files per workflow

**Temporal Schedules** (`temporalScheduleCollection.enabled`):
- Lists and describes Temporal schedules
- Configurable schedule count
- Namespace-specific collection

**Log Analytics** (`logAnalytics.enabled`):
- Post-download log analysis
- Pattern matching, severity classification
- Generates comprehensive report

**Message Filter** (`messageFilter.enabled`):
- Filter logs by key-value pairs
- Filter by specific strings
- Creates filtered copies in `filtered_logs_<timestamp>/`

**Pod File Collection** (`podFileCollection.enabled`):
- Collect specific files from inside pods
- Supports wildcards (*.log, server*.log)
- Configurable namespace, pod prefix, and file paths
- Multiple collection configurations

**JIRA Integration** (`jira.attachmentEnabled`):
- Automatically attach downloaded files to JIRA issues
- Command-line flag: `--jira XCP-12345`
- Requires email and API token configuration
- Multi-file attachment support

### Templates

Use placeholders in config values:
- `{username}` → Your username
- `{environment}` → Environment name
- `{timestamp}` → Current timestamp (YYYYMMDD_HHMMSS)

**Examples:**
```yaml
aws:
  host: "{environment}-console.example.com"  # → dev-console.example.com

logs:
  pattern: /home/{username}/*.gz             # → /home/john/*.gz
  tempDir: "{environment}_logs"              # → dev_logs

appVersionCollection:
  outputFileName: "{environment}_versions_{timestamp}.txt"
  # → dev_versions_20260217_143025.txt
```

## Output Structure

After running `--all`, you'll get:
```
C:/Logs/
├── app_log_20260217_143025.tar.gz          # Kubernetes pod logs
├── logger_info_20260217_143025.txt         # Session log (all terminal output)
├── dev_app_versions_20260217_143025.txt    # Application versions
├── log_analytics_report_20260217_143025.txt  # Analytics report (if enabled)
├── filtered_logs_20260217_143025/          # Filtered logs (if enabled)
│   ├── pod1/
│   │   └── app.log
│   └── pod2/
│       └── service.log

Inside the archive (app_log_*.tar.gz):
├── General/                                # System info (inside archive)
│   ├── system_info.txt
│   ├── pods_all_namespaces.txt
│   └── nodes_detailed.txt
├── Temporal/                               # Temporal data (inside archive)
│   ├── workflow_list.txt
│   ├── workflow_<id>_details.txt
│   ├── schedule_list.txt
│   └── schedule_<id>_details.txt
└── PodFiles/                               # Pod-specific files (if enabled)
    ├── common/
    │   └── cs-configuration-xyz/
    │       ├── server.log
    │       ├── server_err.log
    │       └── old_logs.log.gz
    └── xiq/
        └── hmweb-abc/
            └── application.log
```

## Common Issues

**1. "ssh: rejected: administratively prohibited"**
→ Lower `maxSSHSessions` to 1 or 2 in config.yaml

**2. "Password authentication failed"**
→ Delete encrypted password from config.yaml, let tool re-prompt

**3. "scp: command not found"**
→ Install OpenSSH client or use `--sftp` flag

**4. Slow downloads**
→ Use `--native-scp` for 10x speed boost (requires scp command)

**5. "kubectl: command not found"**
→ Ensure kubectl is installed on target server, not local machine

## Advanced Configuration

### Log Analytics Patterns

Add custom error patterns:
```yaml
logAnalytics:
  enabled: true
  errorPatterns:
    - "ERROR"
    - "Exception"
    - "FATAL"
    - "panic:"
  excludeKeywords:
    - "TestError"        # Ignore test-related errors
    - "DeprecationWarning"
```

### Message Filtering

Filter specific owners or strings:
```yaml
messageFilter:
  enabled: true
  keyValueFilters:
    - key: "ownerID"
      value: "1096"      # Keep only ownerID=1096 lines
  specificStrings:
    - "Payment"          # Keep lines containing "Payment"
    - "Transaction"
```

### Pod File Collection

Collect specific files from inside pods:
```yaml
podFileCollection:
  enabled: true
  collections:
    - namespace: "common"
      podPrefix: "cs-configuration"
      logPath: "/var/log/configuration/"
      filePatterns:
        - "server.log"
        - "server_err.log"
        - "*.log.gz"
    - namespace: "xiq"
      podPrefix: "hmweb"
      logPath: "/opt/app/logs/"
      filePatterns:
        - "*.log"
        - "application*.log"
```

Files are saved to: `<archive>/PodFiles/<namespace>/<pod-name>/<files>`

### Temporal Workflow Collection

```yaml
temporalWorkflowCollection:
  enabled: true
  numberOfWorkflows: 10          # Collect last 10 workflows
  timeDuration: "24h"            # From last 24 hours
  namespace: "default"           # Temporal namespace
```

### Custom System Commands

Add your own kubectl/system commands:
```yaml
generalInfo:
  enabled: true
  commands:
    - name: "custom_check"
      command: "kubectl get configmaps -n production"
      description: "Production ConfigMaps"
```

## Performance Tips

1. **Use Native SCP**: Set `downloadMethod: "scp"` → 2-4 MB/s vs 400-600 KB/s with SFTP
2. **Keep Sessions Low**: Use `maxSSHSessions: 1-2` to avoid SSH limits
3. **Time-Based Logs**: Use `-time-duration 30m` instead of full logs for faster collection
4. **Disable Unused Features**: Set `enabled: false` for features you don't need
5. **Increase Log Level**: Use `logLevel: ERROR` in production for less output

## Security Notes

- Passwords are encrypted with AES-256-GCM before saving
- Temporary SSH keys are auto-deleted after use
- StrictHostKeyChecking is disabled (no fingerprint prompts)
- Avoid committing `config.yaml` with credentials to Git

## Need Help?

1. **Enable Debug Logging**: Use `-log-level DEBUG` to see detailed information
2. **Check Session Log**: Look at `logger_info_<timestamp>.txt` for full execution details
3. **Verify Connectivity**: Test SSH bastion and target server access manually first
4. **Review Config**: Ensure all paths and hostnames are correct with proper templates

---

**For full feature documentation, architecture details, and troubleshooting guide, see [README.md](README.md)**
