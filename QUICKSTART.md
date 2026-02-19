# Quick Start Guide - LogCollector

A command-line tool to collect logs, system info, and application versions from Kubernetes clusters and network devices via SSH bastion host.

## Latest Release: v1.3.3

**Download cross-platform binaries from:** [GitHub Releases](https://github.com/nperiannan/EP1LogCollector/releases/latest)

| Platform | Binary |
|----------|--------|
| Windows (x64) | `logcollector-windows-amd64.exe` |
| Linux (x64) | `logcollector-linux-amd64` |
| macOS (Intel) | `logcollector-darwin-amd64` |
| macOS (Apple Silicon) | `logcollector-darwin-arm64` |

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
- Download from [GitHub Releases](https://github.com/nperiannan/EP1LogCollector/releases/latest)
- Extract/rename to `logcollector.exe` (Windows) or `logcollector` (Linux/Mac)
- Check version: `logcollector.exe -v` or `./logcollector -v`

**Option B: Build from Source**
```bash
# Build for current platform
go build -o logcollector.exe

# Or use automated build script for all platforms
.\build-all.ps1
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

**Collect Database Query Results:**
```bash
.\logcollector.exe --database
```
→ Execute PostgreSQL queries via SSH tunneling, collect results with cross-database parameter passing

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

## Quick Start: Device Logs

Collect diagnostics and logs from network devices (EXOS/VOSS switches).

**1. Configure devices in config.yaml:**
```yaml
deviceLogCollection:
  enabled: true
  devices:
    - name: switch-1
      host: 192.168.10.1
      username: admin
      password: "switch_password"
      type: exos
      commands:
        - "show version"
        - "show configuration"
        - "show log messages"
```

**2. Run collection:**
```bash
.\logcollector.exe --device-logs
```

**3. Output location:**
```
C:/Logs/20260219_143025/
├── logger_info.txt
└── DeviceLogs/
    └── switch-1_diagnostics_20260219_143025.txt
```

**Supported device types:**
- `exos` - Extreme Networks EXOS switches
- `voss` - Extreme Networks VOSS (Fabric) switches

## Quick Start: Database Queries

Execute PostgreSQL queries via SSH tunneling and collect results with cross-database parameter passing.

**1. Configure database queries in config.yaml:**
```yaml
databaseCollection:
  enabled: true                      # Enable database query collection
  
  # Global parameters used across all queries
  parameters:
    owner_id: "1096"                 # Initial parameter - used by first query
  
  # Database configurations
  databases:
    - name: platform_common_db       # Database name (used in output filename)
      
      queries:
        # Query 1: Get device IDs for owner
        - name: get_owner_devices
          sql: "SELECT id AS asset_device_id, name FROM devices WHERE owner_id = :owner_id;"
          # :owner_id comes from parameters above (value: "1096")
          # Returns: asset_device_id column values are extracted for next query
        
        # Query 2: Get details for each device from Query 1
        - name: device_details
          sql: "SELECT * FROM device_info WHERE asset_device_id = :asset_device_id;"
          # :asset_device_id comes from previous query's "asset_device_id" column
          # If Query 1 returns multiple rows, this query runs once per asset_device_id value
          # Output shows grouped results per asset_device_id value
```

**2. Run collection:**
```bash
.\logcollector.exe --database
```

**3. Output location:**
```
C:/Logs/20260219_143025/
├── logger_info.txt
└── Database/
    └── platform_common_db_queries_20260219_143025.txt
```

**Features:**
- **Cross-database parameters**: Query results from one query feed into the next as parameters
- **Grouped output**: Multi-value parameters show labeled sections per value with row counts
- **SSH tunneling**: Queries executed via bastion → AWS → RDS

**How parameter passing works:**
1. Query 1 uses `:owner_id` from `parameters` section (value: "1096")
2. Query 1 returns column `asset_device_id` with multiple values (e.g., "abc-123", "def-456")
3. Query 2 runs once per `asset_device_id` value extracted from Query 1
4. Output shows grouped results:
   ```
   --- asset_device_id = abc-123 ---
   (5 rows returned)
   
   --- asset_device_id = def-456 ---
   (3 rows returned)
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

**Device Log Collection** (`deviceLogCollection.enabled`):
- Collect diagnostics from network devices
- Supports EXOS and VOSS switches
- Configurable commands per device
- Direct SSH connection to devices

**Database Collection** (`databaseCollection.enabled`):
- Execute PostgreSQL queries via SSH tunneling
- Bash alias resolution from config
- Cross-database parameter passing
- Grouped output for multi-value parameters

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
C:/Logs/20260217_143025/
├── app_log_20260217_143025.tar.gz          # Kubernetes pod logs
├── logger_info.txt                         # Session log (all terminal output)
├── dev_app_versions_20260217_143025.txt    # Application versions
├── log_analytics_report_20260217_143025.txt  # Analytics report (if enabled)
├── filtered_logs_20260217_143025/          # Filtered logs (if enabled)
│   ├── pod1/
│   │   └── app.log
│   └── pod2/
│       └── service.log
├── Database/                               # Database query results (if enabled)
│   └── platform_common_db_queries_20260217_143025.txt
└── DeviceLogs/                             # Network device logs (if enabled)
    ├── switch-1_diagnostics_20260217_143025.txt
    └── switch-2_diagnostics_20260217_143025.txt

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
