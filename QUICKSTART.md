# Quick Start Guide - LogCollector

A command-line tool to collect logs, system info, and application versions from Kubernetes clusters and network devices via SSH bastion host.

## Latest Release: v1.3.0

**Download cross-platform binaries from:** [GitHub Releases](https://github.com/nperiannan/EP1LogCollector/releases/tag/v1.3.0)

| Platform | Binary |
|----------|--------|
| Windows (x64) | `logcollector-windows-amd64.exe` |
| Linux (x64) | `logcollector-linux-amd64` |
| macOS (Intel) | `logcollector-darwin-amd64` |
| macOS (Apple Silicon) | `logcollector-darwin-arm64` |

**What's New in v1.3.0:**
- **Multi-source credential retrieval** - Bastion passwords and JIRA tokens stored securely in Windows Credential Manager
- **One-time password entry** - Enter once, retrieved automatically on subsequent runs
- **CI/CD support** - Environment variables (BASTION_PASSWORD, JIRA_API_TOKEN) for automation
- **Template support** - JIRA email supports `{username}` and `{environment}` placeholders 
- **Enhanced security** - Windows DPAPI encryption, accessible only by your user account

**What's New in v1.2.2:**
- **Automatic SFTP fallback** - When SCP fails (DNS resolution, network issues), automatically falls back to parallel SFTP for reliable downloads

**v1.2.1 Features:**
- **Pod file filtering** - New `matchPodName` option collects only current pod logs, excluding old pod logs in persistent directories

**v1.2.0 Features:**
- **Loki-style replica log merging** - Combines replica pod logs into single files
- **Transaction ID correlation** - Groups related errors by correlation IDs across files
- **Timestamp sorting** - Chronological ordering of merged logs
- **Semantic error grouping** - Categorizes errors by type (Database, Permission, Network, Resource)
- **Enhanced analytics** - Transaction/request correlation section in reports
- **Security improvement** - Encrypted passwords excluded from logger_info.txt

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
- Download from [GitHub Releases](https://github.com/nperiannan/EP1LogCollector/releases/tag/v1.2.1)
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
- Password gets encrypted and saved to Windows Credential Manager automatically (Windows)
- Subsequent runs retrieve password automatically (no prompts)

## Credential Management (v1.3.0+)

### How It Works

Credentials are retrieved from **4 sources** in priority order:

#### Bastion Password
1. 🅱 **Environment Variable**: `BASTION_PASSWORD` (CI/CD, automation)
2. 🔐 **Windows Credential Manager** (secure, encrypted by Windows)
3. 🗄 **Config file** (config.yaml, AES-256-GCM encrypted)
4. ⌨️ **Interactive prompt** (first-time, auto-saves to Credential Manager)

**Stored as**: `LogCollector:Bastion:nperiannan@usnc-awsgtwy-02.extremenetworks.com`

#### JIRA API Token
1. 🅱 **Environment Variable**: `JIRA_API_TOKEN` (CI/CD, automation)
2. 🔐 **Windows Credential Manager** (secure, encrypted by Windows)
3. 🗄 **Config file** (config.yaml, plaintext)
4. ⌨️ **Interactive prompt** (first-time, auto-saves to Credential Manager)

**Stored as**: `LogCollector:JIRA:nperiannan@extremenetworks.com`

### First-Run Experience

**Step 1**: Configure config.yaml (leave passwords empty)
```yaml
username: nperiannan
bastion:
  host: usnc-awsgtwy-02.extremenetworks.com
  password: ""  # Leave empty

jira:
  email: "{username}@extremenetworks.com"  # Use template
  apiToken: ""  # Leave empty
```

**Step 2**: Run the tool
```bash
.\logcollector.exe --all
```

**Step 3**: Enter credentials when prompted (ONE TIME)
```
Enter bastion password: ****
```

**Step 4**: Credentials saved automatically
✅ Windows Credential Manager stores password (encrypted)
✅ Future runs retrieve automatically
✅ No more prompts!

### Viewing Credentials (Windows)

**Method 1: Control Panel**
1. Press `Win+R`
2. Type: `control /name Microsoft.CredentialManager`
3. Click: **Windows Credentials**
4. Look for entries: `LogCollector:Bastion:*` and `LogCollector:JIRA:*`

**Method 2: PowerShell**
```powershell
cmdkey /list | Select-String "LogCollector"
```

### Removing/Editing Credentials

**Remove specific credential:**
```powershell
cmdkey /delete:"LogCollector:Bastion:nperiannan@usnc-awsgtwy-02.extremenetworks.com"
cmdkey /delete:"LogCollector:JIRA:nperiannan@extremenetworks.com"
```

**Or use Control Panel:**
1. Open Credential Manager (see above)
2. Find `LogCollector:*` entry
3. Click **Remove**
4. Next run will prompt for credentials again

### Using Environment Variables (CI/CD)

**Windows PowerShell:**
```powershell
# Set credentials for session
$env:BASTION_PASSWORD = "your_password"
$env:JIRA_API_TOKEN = "atatt3xFfGF0abc123..."

# Run tool (no prompts)
.\logcollector.exe --all --jira XCP-12345
```

**Linux/macOS Bash:**
```bash
# Set credentials for session
export BASTION_PASSWORD="your_password"
export JIRA_API_TOKEN="atatt3xFfGF0abc123..."

# Run tool (no prompts)
./logcollector --all --jira XCP-12345
```

**Permanent (use carefully!):**
```powershell
# Windows (System Environment Variables)
[Environment]::SetEnvironmentVariable("BASTION_PASSWORD", "your_password", "User")

# Linux/Mac (~/.bashrc or ~/.zshrc)
echo 'export BASTION_PASSWORD="your_password"' >> ~/.bashrc
```

### JIRA Email Templates

Use placeholders in JIRA email configuration:

```yaml
jira:
  email: "{username}@extremenetworks.com"  # → nperiannan@extremenetworks.com
  # OR
  email: "support-{environment}@company.com"  # → support-dev@company.com
```

**Supported placeholders:**
- `{username}` → Your username from config
- `{environment}` → Environment name from config

### Security Benefits

✅ **Windows DPAPI encryption** - Credentials encrypted by Windows Data Protection API  
✅ **User-scoped** - Only accessible by your Windows user account  
✅ **Login-protected** - Cannot be accessed when computer is locked  
✅ **One-time setup** - Enter credentials once, used automatically thereafter  
✅ **Backward compatible** - Old config files still work  
✅ **Priority system** - Use env vars for CI/CD, Credential Manager for desktop  
✅ **IT-manageable** - Can be deployed via Group Policy  

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

- **Multi-source credentials**: Environment variables → Windows Credential Manager → Config file → Interactive prompts
- **Windows Credential Manager**: Credentials encrypted via DPAPI (Windows only)
- **Config file passwords**: Encrypted with AES-256-GCM (legacy support)
- **Temporary SSH keys**: Auto-deleted after use
- **StrictHostKeyChecking**: Disabled (no fingerprint prompts)
- **Best practice**: Use Windows Credential Manager or environment variables, avoid storing in config files

## Need Help?

1. **Enable Debug Logging**: Use `-log-level DEBUG` to see detailed information
2. **Check Session Log**: Look at `logger_info_<timestamp>.txt` for full execution details
3. **Verify Connectivity**: Test SSH bastion and target server access manually first
4. **Review Config**: Ensure all paths and hostnames are correct with proper templates

---

**For full feature documentation, architecture details, and troubleshooting guide, see [README.md](README.md)**
