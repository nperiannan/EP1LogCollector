# GoFetch Logs - CLI Tool

A command-line utility for fetching log files from AWS instances via a bastion host with integrated Kubernetes log collection and comprehensive system information gathering.

## Recent Updates (v2.0)

### ⚡ High-Performance Native SCP Downloads (NEW)
- **10x Speed Improvement**: Native SCP achieves 2-4 MB/s vs 435-667 KB/s with SFTP
- **2-Step Architecture**: Optimized AWS→Bastion (internal SCP) + Bastion→Local (native SCP)
- **Passwordless Automation**: Automatic temporary SSH key generation and cleanup
- **Cross-Platform**: Works seamlessly on Windows and Linux
- **Smart Fallback**: Automatically falls back to parallel SFTP if SCP unavailable
- **30MB Files**: Downloads in 8-18 seconds (vs 50-75 seconds with SFTP)
- **🆕 Configurable Method**: Choose between SCP or SFTP via config file or command-line flags (`--native-scp` or `--sftp`)

### 🔐 Security Enhancements
- **Password Encryption**: AES-256-GCM encryption for secure password storage in config files
- **Masked Password Input**: Secure password entry with hidden characters during input
- **Auto-Encryption**: Passwords are automatically encrypted and saved when entered
- **Temporary Key Security**: Auto-generated ED25519 keys with automatic cleanup

### 🌐 Multi-Environment Support
- **Dynamic XIQ Namespace Detection**: Automatically detects and uses 'xiq' namespace when available
- **Intelligent Fallback**: Falls back to environment namespace when 'xiq' doesn't exist
- **Cross-Environment Compatibility**: Works seamlessly across dev, staging, and production

### 🎯 Simplified Operations
- **New Operation Modes**: `--all`, `--logs-only`, `--sys-info`, `--version` for easy usage
- **Mode-Based Control**: Simple, clear commands replacing complex flag combinations
- **Config Override**: Operation modes override config.yaml for quick operations

### ⏰ Version Tracking
- **Timestamped Outputs**: Application version files include automatic timestamps
- **Namespace Separators**: Visual separation between namespaces in version reports
- **Historical Tracking**: Unique filenames for tracking version changes over time

### 🔎 Post-Download Message Filter (NEW)
- **Key-Value Filtering**: Filter log lines by key-value pairs (e.g., keep only `ownerID=1096`)
- **Specific String Matching**: Keep only lines containing specified strings
- **Non-Destructive**: Original logs are never modified — filtered output goes to a separate `filtered_logs_<timestamp>/` directory
- **Directory Preservation**: Maintains the original archive directory structure in filtered output
- **Configurable**: Enable/disable via `messageFilter.enabled` in config.yaml

### 📂 Pod File Collection (NEW)
- **Direct Pod File Access**: Collect specific files from inside Kubernetes pods without using kubectl logs
- **Wildcard Support**: Use patterns like `*.log`, `server*`, `*error*` to match multiple files
- **Multiple Pods**: Automatically finds and collects from all pods matching a prefix
- **Flexible Configuration**: Add unlimited collection configurations for different namespaces and pods
- **Organized Output**: Files saved to `PodFiles/<namespace>/<pod-name>/` within the archive

### 🔗 JIRA Integration (NEW)
- **Automatic Attachment**: Attach downloaded files directly to JIRA issues via REST API
- **Command-Line Flag**: Use `--jira XCP-17614` to attach files to a specific issue
- **Secure Authentication**: API token-based authentication (email + API token)
- **Multi-File Support**: Automatically attaches all generated files (archives, reports, analytics)
- **Error Handling**: Graceful fallback if credentials missing or API call fails
- **Configurable**: Enable/disable via `jira.attachmentEnabled` in config.yaml

## Features

- **🔄 End-to-End Kubernetes Log Collection**: Remotely collect logs from Kubernetes pods, create timestamped archives, and download automatically
- **📊 General System Information Collection**: Automatically collect kubectl and system command outputs for comprehensive operational snapshots with per-command output files
- **🔍 Intelligent Log Analytics**: Post-download analysis of collected logs with configurable error patterns, cross-file correlation, severity classification, before/after context, and comprehensive `log_analytics_report.txt` generation
- **� Post-Download Message Filter**: Filter downloaded logs by key-value pairs and specific strings into a separate `filter/` directory without modifying originals
- **�📋 Application Version Collection**: Standalone feature to collect and format application version information from Kubernetes clusters
- **🗑️ Smart Archive Management**: Automatic cleanup of remote archives after successful download with configurable retention
- **📂 Pod File Collection**: Collect specific files directly from inside Kubernetes pods using wildcard patterns with organized output per namespace and pod
- **🔗 JIRA Integration**: Automatically attach downloaded files to JIRA issues via command-line flag using REST API
- **⚡ High-Performance Native SCP Downloads**: 2-step optimized architecture with native SCP achieving 2-4 MB/s (10x faster than SFTP)
- **🔧 Cross-Platform Compatibility**: Works seamlessly on Windows and Linux with automatic OS-specific optimizations
- **🔐 Password Encryption**: AES-256-GCM encryption for secure password storage in config files
- **🔒 Masked Password Input**: Secure password entry with hidden characters during input
- **🌐 Dynamic Namespace Detection**: Intelligent XIQ namespace detection for multi-environment deployments
- **📝 Session Logging**: Automatic session log file (`logger_info.txt`) that mirrors all terminal output for audit and troubleshooting
- **🎯 Simplified Operation Modes**: Easy-to-use `--all`, `--logs-only`, `--sys-info`, `--version` command modes
- Connect to AWS instances via a bastion host using SSH key authentication
- Find and list log files based on customizable patterns with template support
- **⚡ Ultra-fast downloads** with native SCP (2-4 MB/s) using 2-step optimized architecture
- Automatic temporary SSH key generation for passwordless SCP automation
- Smart fallback to parallel SFTP (8 connections) if SCP unavailable
- Real-time download progress indicator with speed reporting
- Interactive mode for selecting specific files to download
- **🎯 Template-based Configuration**: Support for dynamic placeholders like `{username}`, `{environment}`, and `{timestamp}`
- **📝 YAML Configuration**: Human-readable YAML config with comments and clear structure
- Configuration file support for storing common settings
- Detailed download summary with file sizes and success/failure counts
- Smart recovery: only retry failed chunks instead of redownloading entire files
- Advanced file verification and robust error handling
- Detailed debugging output to help diagnose issues

## Prerequisites

- Go 1.16 or later
- SSH access to a bastion host
- AWS instances accessible from the bastion host
- `kubectl` access on the AWS instance (for log collection)
- **Native SCP client** on local machine:
  - **Windows**: OpenSSH client (included in Windows 10+ by default)
  - **Linux**: OpenSSH client (typically pre-installed)
  - **Verification**: Run `scp -V` and `ssh-keygen -V` to confirm availability

## Building

```bash
go build -o gofetchlogs.exe fetchlogs.go
```

## Configuration

The tool uses YAML configuration files with a clear configuration hierarchy:

**Priority Order (highest to lowest):**
1. **Operation mode flags** (e.g., `--all`, `--logs-only`, `--version`, `--sys-info`)
2. **Command-line flags** (e.g., `-log-level DEBUG`, `-time-duration 15m`)
3. **Configuration file** (config.yaml)  
4. **Default values**

This means operation modes and command-line flags will always override config file settings, and config file settings override defaults.

### YAML Configuration (config.yaml)

```yaml
# Global Configuration
username: your-username          # Used for all connections
environment: dl1r1              # Environment identifier

# Bastion Server Configuration  
bastion:
  host: bastion-host.example.com
  port: 22
  password: ""                     # Leave empty - will prompt and encrypt on first run
  # Or use encrypted password: password: "ENC:A1B2C3D4E5F6..."
  keyPath: ~/.ssh/id_rsa          # Option 2: SSH key authentication (alternative to password)

# AWS Server Configuration
aws:
  host: "{environment}-console.qa.example.com"  # Template with environment
  keyPath: ~/.ssh/id_ed25519_key

# Log File Configuration
logs:
  pattern: /home/{username}/*.gz    # Template with username
  outputDir: C:/Logs

# General Options
options:
  autoRetry: true
  numChunks: 8                    # Used for SFTP fallback only (native SCP is default)
  logLevel: DEBUG                 # DEBUG, INFO, WARN, ERROR
  maxSSHSessions: 1               # Max concurrent SSH sessions per source (1-4, default: 1)
  downloadMethod: "scp"            # Download method: "scp" (native SCP, 10x faster) or "sftp" (parallel SFTP). Default: scp

# Log Collection Configuration
logCollection:
  enabled: false                    # Enable/disable log collection (use --all to override)
  logFileName: app_log
  tempDir: "{environment}_logs"
  useTimestamp: true
  deleteAfterCopy: true             # Automatically delete remote archive after download
  timestampFormat: "20060102_150405"
  customSources: []
  # Time-based log collection (collect logs from last X time)
  timeBasedCollection:
    enabled: false                  # Enable time-based collection (false = collect full logs)
    duration: ""                    # Duration like "15m", "30m", "1h", "2h" (leave empty for full logs)

# General System Information Collection
generalInfo:
  enabled: false                    # Enable/disable (use --all or --sys-info to override)
  outputDir: "General"              # Directory inside archive for system info
  commands:
    - name: "system_info"
      command: "uname -a"
      description: "System information"
    - name: "pods_all_namespaces"
      command: "kubectl get pods --all-namespaces"
      description: "Get pods in all namespaces"
    - name: "nodes_detailed"
      command: "kubectl get nodes -o wide"
      description: "Detailed node information"
    - name: "cpu_usage_ascending"
      command: "kubectl top pods --all-namespaces --sort-by=cpu"
      description: "Pod CPU usage in ascending order"
    - name: "memory_usage_ascending"
      command: "kubectl top pods --all-namespaces --sort-by=memory"
      description: "Pod memory usage in ascending order"
    - name: "describe_pods_current_namespace"
      command: "kubectl describe pods -n {environment}"
      description: "Describe pods in current environment namespace"
    - name: "describe_nodes"
      command: "kubectl describe nodes"
      description: "Describe all nodes in the cluster"
    - name: "cluster_info"
      command: "kubectl cluster-info"
      description: "Get cluster information"
    - name: "system_resources"
      command: "top -bn1 | head -20"
      description: "System resource snapshot"

# Application Version Collection Settings  
appVersionCollection:
  enabled: true                   # Enable/disable (use --version to collect only versions)
  outputFileName: "{environment}_app_versions_{timestamp}.txt"  # Timestamped output
  printToLog: true                # Print version information to console
  namespaces:
    - namespace: "common"
      description: "Common services namespace"
      podPrefixes: ["cas-api-service", "cls-api-service", "cns-api-service"]
    - namespace: "{environment}"  # Dynamic: uses 'xiq' if exists, else environment name
      description: "Environment-specific services (XIQ namespace)"
      podPrefixes: ["hmweb", "teconfig", "copilot-flink"]
    - namespace: "nvo"
      description: "NVO services namespace"
      podPrefixes: ["nvo-edge", "nvo-network"]

# JIRA Integration
jira:
  email: ""                          # Your Atlassian account email
  apiToken: ""                       # API token from https://id.atlassian.com/manage-profile/security/api-tokens
  attachmentEnabled: false           # Enable/disable automatic file attachment
  baseUrl: "https://extremenetworks.atlassian.net"  # JIRA instance URL
```

### Template Placeholders

The configuration supports dynamic templates:
- **`{username}`**: Replaced with the global username
- **`{environment}`**: Replaced with the environment identifier
- **`{timestamp}`**: Replaced with current timestamp (format: YYYYMMDD_HHMMSS)

**Examples:**
- `host: "{environment}-console.qa.example.com"` → `dl1r1-console.qa.example.com`
- `pattern: /home/{username}/*.gz` → `/home/john/*.gz`
- `tempDir: "{environment}_logs"` → `dl1r1_logs`
- `outputFileName: "{environment}_app_versions_{timestamp}.txt"` → `dl1r1_app_versions_20260216_143025.txt`

### Configuration Hierarchy

The tool follows a clear configuration hierarchy (highest to lowest priority):

1. **Operation mode flags** (highest priority: `--all`, `--logs-only`, `--sys-info`, `--version`)
2. **Command-line flags** (e.g., `-log-level`, `-time-duration`)
3. **Configuration file** (config.yaml)
4. **Default values** (lowest priority)

This means:
- If you specify `--all` mode, it will collect everything regardless of config.yaml settings
- If you specify `--version` mode, it will only collect versions regardless of config.yaml settings
- If you don't specify a mode, it will use the enabled/disabled settings from config.yaml
- Individual flags like `-log-level` always override config file values

**Key Operation Modes:**
- `--all` → Collects logs, versions, and system info (overrides all config settings)
- `--logs-only` → Collects only logs from Kubernetes pods (overrides config settings)
- `--sys-info` → Collects only system info (overrides config settings)
- `--version` → Collects only versions (overrides config settings)
- No mode → Uses `logCollection.enabled`, `appVersionCollection.enabled`, `generalInfo.enabled` from config.yaml

### SSH Performance Tuning

The tool includes configurable SSH session management to prevent "administratively prohibited" errors when collecting logs from many pods:

**maxSSHSessions Configuration:**
```yaml
options:
  maxSSHSessions: 1  # Recommended: 1-2 for most SSH servers
```

**Recommendations:**
- **Conservative (default)**: `maxSSHSessions: 1` - Most reliable, prevents session limit errors
- **Moderate**: `maxSSHSessions: 2` - Good balance of speed and reliability  
- **Aggressive**: `maxSSHSessions: 3-4` - Faster but may hit SSH session limits on some servers

**Why This Matters:**
- SSH servers typically limit concurrent sessions per connection (usually 10-20)
- With many pods, concurrent kubectl operations can exceed these limits
- The tool automatically manages session limits to prevent "ssh: rejected: administratively prohibited" errors
- Lower values = more reliable, higher values = potentially faster but riskier

## Usage

### Operation Modes

The tool supports multiple operation modes for different use cases:

**1. Default Mode (No Arguments)**
```bash
.\fetchlogs.exe
```
- Follows all settings in `config.yaml`
- Respects `logCollection.enabled`, `appVersionCollection.enabled`, `generalInfo.enabled`
- Best for regular use with pre-configured settings

**2. All Mode (`--all`)**
```bash
.\fetchlogs.exe --all
```
- Collects **everything**: logs, app versions, and system information
- Overrides all config file settings
- Perfect for comprehensive troubleshooting and diagnostics

**3. Logs Only Mode (`--logs-only`)**
```bash
.\fetchlogs.exe --logs-only
```
- Collects **only** logs from Kubernetes pods
- Skips app version collection and system info collection
- Ideal for focused log analysis and troubleshooting

**4. System Info Mode (`--sys-info`)**
```bash
.\fetchlogs.exe --sys-info
```
- Collects **only** general system information (kubectl commands, system stats)
- Skips log collection and version collection
- Ideal for quick cluster health checks

**5. Version Mode (`--version`)**
```bash
.\fetchlogs.exe --version
```
- Collects **only** application version information
- Skips log collection and system info
- Perfect for version audits and deployment verification

**6. List Mode (`--list`)**
```bash
.\fetchlogs.exe --list
```
- Lists available log files without downloading
- Traditional file listing mode

**7. Interactive Mode**
```bash
.\fetchlogs.exe -interactive
```
- Prompts for missing information
- Allows file selection

### Mode Priority

**Operation mode flags override config.yaml settings:**
- `--all` → Enables logs + versions + system info (regardless of config)
- `--logs-only` → Enables only log collection (regardless of config)
- `--sys-info` → Enables only system info (regardless of config)
- `--version` → Enables only versions (regardless of config)
- No mode flag → Uses config.yaml settings

### Basic Usage Examples

```bash
# Default: Use config.yaml settings
.\fetchlogs.exe

# Collect everything (comprehensive troubleshooting)
.\fetchlogs.exe --all

# Collect only logs from Kubernetes pods
.\fetchlogs.exe --logs-only

# Quick cluster health check
.\fetchlogs.exe --sys-info

# Version audit only
.\fetchlogs.exe --version

# Using a custom config file
.\fetchlogs.exe -config myconfig.yaml --all

# List files only
.\fetchlogs.exe --list

# Download Method Examples:

# Use native SCP (default - 10x faster)
.\fetchlogs.exe --all

# Force native SCP explicitly
.\fetchlogs.exe --all --native-scp

# Force parallel SFTP instead
.\fetchlogs.exe --all --sftp

# Use SFTP with more parallel connections
.\fetchlogs.exe --all --sftp -num-chunks 10

# Attach downloaded files to a JIRA issue
.\fetchlogs.exe --all --jira XCP-17614

# Combine with other options
.\fetchlogs.exe --logs-only --jira XCP-12345 --time-duration 1h
```

### Available Flags

**Operation Modes:**
- `--all`: Collect everything (logs, app versions, system info)
- `--logs-only`: Collect only logs from Kubernetes pods
- `--sys-info`: Collect only general system information
- `--version`: Collect only application version information
- `--list`: Only list log files without downloading

**Core Configuration:**
- `-config`: Path to configuration file (default: **config.yaml**)
- `-user`: SSH username for bastion host (uses global username from config if not specified)
- `-pass`: SSH password for bastion host (supports encrypted passwords)
- `-bastion`: Bastion host address (e.g., bastion.example.com)
- `-port`: SSH port for bastion host (default: 22)
- `-key`: Path to SSH key on bastion
- `-aws`: AWS host to connect to (supports templates like `{environment}-console.qa.example.com`)
- `-awsuser`: Username for AWS host (uses global username if not specified)

**File Management:**
- `-logs`: Log file pattern to search for (supports templates like `/home/{username}/*.gz`)
- `-outdir`: Local directory to save log files (default: current directory)
- `-interactive`: Run in interactive mode

**Download Options:**
- `-auto-retry`: Automatically retry failed download chunks (default: false)
- `-num-chunks`: Number of parallel connections for SFTP fallback (1-10, default: 8). Note: Native SCP is used by default and doesn't use chunks
- `-log-level`: Set log level (DEBUG, INFO, WARN, ERROR, default: INFO)

**Download Method Flags:**
- `--native-scp`: Force use of native SCP for downloads (10x faster, default behavior)
- `--sftp`: Force use of parallel SFTP for downloads instead of native SCP
- Note: If neither flag is specified, the method from `config.yaml` is used (defaults to SCP)

**Log Collection Options:**
- `-log-name`: Name for the log collection (without extension)
- `-user-id`: User ID for log collection (uses global username if not specified)
- `-time-duration`: Collect logs from last X time (e.g., `15m`, `30m`, `1h`, `2h`). Use `0` or `disabled` to force full logs. Leave empty to use config setting

## 🔐 Authentication Options

The tool supports multiple authentication methods for connecting to the bastion server:

### Password Authentication
```bash
# Using command line
.\gofetchlogs.exe -user myuser -pass mypassword -bastion bastion.example.com

# Using config file
bastion:
  host: bastion-host.example.com
  password: "your-password"
  keyPath: ""                    # Leave empty when using password
```

### SSH Key Authentication
```bash
# Using command line
.\gofetchlogs.exe -user myuser -bastion-key ~/.ssh/id_rsa -bastion bastion.example.com

# Using config file
bastion:
  host: bastion-host.example.com
  password: ""                   # Leave empty when using key
  keyPath: ~/.ssh/id_rsa        # Path to private key
```

### Key Features
- **Flexible Authentication**: Choose between password or SSH key authentication for bastion
- **Key Path Expansion**: Supports `~/` home directory expansion for key paths
- **Fallback Support**: If key authentication fails, will try password authentication if both are provided
- **Priority Order**: SSH key authentication is attempted first, then password authentication
- **Local Key Files**: SSH keys are read from your local machine (not from bastion)
- **🔐 Password Encryption**: Automatic AES-256-GCM encryption for passwords stored in config files
- **🔒 Secure Input**: Masked password entry (hidden characters) when prompted for password

**Note**: For AWS server connections, SSH key authentication is always used (keys are read from the bastion server).

## 🔐 Password Encryption & Security

The tool includes robust password security features to protect sensitive credentials in configuration files.

### Automatic Password Encryption

When you enter a password for the first time or when authentication fails, the tool will:
1. **Prompt** for password with masked input (no characters shown on screen)
2. **Encrypt** the password using AES-256-GCM encryption
3. **Display** the encrypted password in the terminal for manual backup
4. **Save** the encrypted password to config.yaml automatically

**Example:**
```bash
$ .\fetchlogs.exe --version
Enter bastion password: ********

🔐 Encrypted Password (copy this if needed):
ENC:A1B2C3D4E5F6G7H8I9J0K1L2M3N4O5P6Q7R8S9T0U1V2W3X4Y5Z6

✅ Password encrypted and saved to config file
```

### Password Format in Config

**Before encryption (plaintext):**
```yaml
bastion:
  password: "my-secret-password"
```

**After encryption:**
```yaml
bastion:
  password: "ENC:A1B2C3D4E5F6G7H8I9J0K1L2M3N4O5P6Q7R8S9T0U1V2W3X4Y5Z6"
```

### Encryption Features

- **AES-256-GCM**: Industry-standard encryption with authenticated encryption
- **Random Nonces**: Each encryption uses a unique random nonce for maximum security
- **Base64 Encoding**: Encrypted passwords are Base64-encoded for safe YAML storage
- **ENC: Prefix**: Clear indication that password is encrypted
- **Auto-Detection**: Tool automatically detects and decrypts encrypted passwords
- **Portable**: Works across different machines with the same encryption key

### Masked Password Input

When prompted for a password, the tool uses secure terminal input:
- **No Echo**: Characters are not displayed on the screen
- **No Stars**: Complete masking (not even `*` shown) for maximum security
- **Clipboard Safety**: Password not visible in terminal history

### Security Workflow

**First-Time Setup:**
```bash
1. Leave password empty in config.yaml: password: ""
2. Run the tool: .\fetchlogs.exe --version
3. Enter password when prompted (masked input)
4. Password is encrypted and saved automatically
5. Copy the displayed encrypted password for backup
```

**Authentication Failure Recovery:**
```bash
1. If authentication fails with current password
2. Tool will prompt for password again (masked input)
3. New password is encrypted and saved
4. Connection is retried with new password
```

### Manual Encryption

If you prefer to manually update the config file:
1. Run the tool and enter your password when prompted
2. Copy the displayed encrypted password from terminal
3. Manually paste it into config.yaml with the "ENC:" prefix

### Best Practices

- ✅ **Keep encrypted passwords**: More secure than plaintext
- ✅ **Backup encrypted passwords**: Copy displayed encrypted password to a secure location
- ✅ **Use file permissions**: Set config.yaml to read-only for your user (chmod 600)
- ❌ **Don't share encryption key**: The hardcoded encryption key should remain in the source code
- ❌ **Don't commit plaintext passwords**: Always use encrypted format in version control

## 🎯 Template System

The configuration system supports dynamic templates to make configs reusable across different environments and users.

### Supported Placeholders

- **`{username}`**: Replaced with the global username from config
- **`{environment}`**: Replaced with the environment identifier from config
- **`{timestamp}`**: Replaced with current timestamp (format: YYYYMMDD_HHMMSS)

### Template Examples

**AWS Host Configuration:**
```yaml
aws:
  host: "{environment}-console.qa.xcloudiq.com"
```
- With `environment: dl1r1` → `dl1r1-console.qa.xcloudiq.com`
- With `environment: dl2r2` → `dl2r2-console.qa.xcloudiq.com`

**Log Pattern Configuration:**
```yaml
logs:
  pattern: /home/{username}/*.gz
```
- With `username: john` → `/home/john/*.gz`
- With `username: alice` → `/home/alice/*.gz`

**App Version File Configuration:**
```yaml
appVersionCollection:
  outputFileName: "{environment}_app_versions_{timestamp}.txt"
```
- With `environment: dl1r1` on 2026-02-16 at 14:30:25 → `dl1r1_app_versions_20260216_143025.txt`

### Benefits

- **🔄 Environment Portability**: Same config works across dev, staging, production
- **👥 User Flexibility**: Easy to switch between different users
- **⏰ Timestamp Tracking**: Automatic timestamp tracking for version snapshots
- **🛠️ Maintenance**: Single config file for multiple environments
- **📝 Clear Intent**: Templates show structure and variability clearly

## 🚀 Enhanced Workflow: End-to-End Log Collection

The tool now supports a complete end-to-end workflow that replaces the manual shell script execution:

### Traditional Workflow (OLD)
1. SSH to AWS console manually
2. Switch to root user (`sudo su -`)
3. Run `fetch_log.sh` script manually
4. Wait for script to collect logs and create `.tar.gz`
5. Run this tool to download the `.tar.gz` file

### New Automated Workflow (NEW)
```bash
# Single command to collect and download logs
.\fetchlogs.exe --all -log-name "my_log_collection" -user-id "nperiannan"
```

This command will:
1. 🔗 Connect to AWS console via SSH
2. **Collect general system information** (kubectl, system commands) into individual files
3. **Collect logs from Kubernetes pods**:
   - **NVO namespace**: `nvo-edge`, `nvo-network`, `nvo-system` pods
   - **DL1R1 namespace**: `hac` and `teconfig` pods
4. 🗜️ Create compressed archive (`my_log_collection.tar.gz`) including General/ directory
5. 📂 Move archive to `/home/nperiannan/`
6. 🧹 Clean up temporary files on remote server
7. ⬇️ Download the archive using parallel download
8. 🗑️ Delete the remote archive after successful download (if enabled)
9. ✅ Complete end-to-end automation!

### Kubernetes Pod Sources

The tool collects logs from these default sources (matching the original shell script):

| Namespace       | Pod Prefix    | Log Path                  | Output File           |
|-----------------|---------------|---------------------------|-----------------------|
| `nvo`           | `nvo-edge`    | `/data/log/nvo-edge/`     | `{pod}-server.log`    |
| `nvo`           | `nvo-network` | `/data/log/nvo-network/`  | `{pod}-server.log`    |
| `nvo`           | `nvo-system`  | `/data/log/nvo-system/`   | `{pod}-server.log`    |
| `{environment}` | `hacr`        | `/opt/hacr/logs/`         | `hac.log`             |
| `{environment}` | `teconfig`    | `/opt/tomcat/logs/`       | `teconfig.log`        |
| `common`        | `hacr`        | `/opt/hacr/logs/`         | `hac.log`             |

**Note**: `{environment}` and `{pod}` are template variables:
- `{environment}` is replaced with your environment identifier (e.g., `dl1r1`, `dl2r2`)  
- `{pod}` is replaced with the actual pod name found in the cluster

**Example**: With `environment: dl1r1`, the tool collects from `dl1r1` namespace and creates files like `nvo-edge-58967bc8cb-28qdf-server.log`.

### 🌐 Dynamic XIQ Namespace Detection

The tool intelligently detects the appropriate namespace for XIQ applications in different environments:

**Behavior:**
- **Checks** if a dedicated `xiq` namespace exists in the cluster
- **Uses** `xiq` namespace if it exists (e.g., some production environments)
- **Falls back** to environment namespace (e.g., `dl2r1`) if `xiq` doesn't exist
- **Logs** the detection result for transparency

**Configuration:**
```yaml
logCollection:
  customSources:
    - namespace: "{environment}"     # Dynamic: uses 'xiq' if exists, else environment name
      podPrefix: "hmweb"
      logPath: "/opt/hmweb/logs"
      outputName: "hmweb.log"
```

**Example Output:**
```
🔍 Checking if 'xiq' namespace exists...
📋 Available namespaces: [common, nvo, xiq, kube-system, ...]
✅ Found dedicated 'xiq' namespace - will use it for XIQ applications
```

Or:
```
🔍 Checking if 'xiq' namespace exists...
📋 Available namespaces: [common, nvo, dl2r1, kube-system, ...]
📌 No dedicated 'xiq' namespace found - will use environment namespace 'dl2r1' for XIQ applications
```

**Benefits:**
- **Multi-Environment Support**: Works across dev, staging, and production environments automatically
- **No Manual Configuration**: Automatically adapts to cluster namespace structure
- **Transparent Operation**: Clear logging of which namespace is being used
- **Backward Compatible**: Falls back to environment namespace for older deployments

### 🚀 Parallel Log Collection Performance

The tool uses advanced parallel processing for optimal log collection performance:

**⚡ Parallel Source Collection:**
- Each log source (namespace/pod combination) is collected concurrently
- Independent SSH sessions for each collection operation
- Progress tracking: Shows "1/6, 2/6, 3/6..." as sources complete
- Significant time savings when collecting from multiple pod sources

**🔧 Optimized Session Management:**
- **Collection Phase**: Parallel independent SSH sessions (one per log source)
- **Archive Phase**: Single optimized SSH session for file operations (archive, move, cleanup)
- **Robust Error Handling**: Fresh SSH sessions prevent "Stdout already set" errors
- **Resource Efficient**: Proper session cleanup and connection management

**📊 Performance Benefits:**
- **6 log sources**: ~85% faster collection (parallel vs sequential)
- **Large deployments**: Scales efficiently with number of pod sources
- **Network optimization**: Reduced total connection time
- **Error isolation**: Failed collections don't impact other sources

The parallel architecture ensures maximum performance while maintaining reliability and proper resource management.

### Example Commands

```bash
# Collect everything (logs, versions, system info) - comprehensive troubleshooting
.\fetchlogs.exe --all

# Collect logs and system info using config.yaml defaults
.\fetchlogs.exe

# Collect only application versions for version audit
.\fetchlogs.exe --version

# Collect only system information for cluster health check
.\fetchlogs.exe --sys-info

# Collect everything with custom log name
.\fetchlogs.exe --all -log-name "production_issue_2026"

# Collect everything with debug output
.\fetchlogs.exe --all -log-level DEBUG

# Traditional mode: just download existing files
.\fetchlogs.exe -logs "/home/nperiannan/*.tar.gz"

# ⏰ NEW: Time-based log collection (collect logs from last 15 minutes)
.\fetchlogs.exe --all -time-duration "15m"

# ⏰ Collect logs from last 30 minutes with custom name
.\fetchlogs.exe --all -time-duration "30m" -log-name "recent_logs"

# ⏰ Collect logs from last 1 hour
.\fetchlogs.exe --all -time-duration "1h"

# ⏰ Collect logs from last 2 hours with debug output
.\fetchlogs.exe --all -time-duration "2h" -log-level DEBUG

# 📄 Force full log collection (override config setting even if time-based is enabled in config)
.\fetchlogs.exe --all -time-duration "0"

# 📄 Alternative way to force full logs
.\fetchlogs.exe --all -time-duration "disabled"
```

### ⏰ **Time-Based Log Collection (NEW)**

The tool now supports collecting logs from a specific time period instead of downloading entire log files. This is perfect for troubleshooting recent issues without transferring large historical log files.

**Key Features:**
- **Faster Collection**: Only collects recent logs using `kubectl logs --since-time`
- **Smaller Archives**: Significantly reduced file sizes for recent troubleshooting
- **Configurable Duration**: Support for minutes and hours (e.g., `15m`, `30m`, `1h`, `2h`)
- **Automatic Naming**: Files are automatically named with the time duration for clarity
- **Backward Compatible**: Leave empty to collect full log files as before

**Usage Examples:**
```bash
# Collect logs from last 15 minutes (common for quick troubleshooting)
.\fetchlogs.exe --all -time-duration "15m"

# Collect logs from last 1 hour (good for recent issue analysis)
.\fetchlogs.exe --all -time-duration "1h"

# Collect logs from last 30 minutes with custom naming
.\fetchlogs.exe --all -time-duration "30m" -log-name "issue_analysis"
```

**Configuration Options:**
```yaml
logCollection:
  timeBasedCollection:
    enabled: true                   # Enable time-based collection by default
    duration: "30m"                 # Default to last 30 minutes
```

**Time Duration Format:**
- `15m` = 15 minutes
- `30m` = 30 minutes  
- `1h` = 1 hour
- `2h` = 2 hours
- `1h30m` = 1 hour 30 minutes

**Special Values:**
- `0` or `disabled` = Force full log collection (overrides config setting)
- Empty/not specified = Use config file setting

**Generated Files:**
- Time-based: `nvo-edge-abc123_2025-07-01T10-30-00Z.log`
- Full logs: `nvo-edge-abc123-server.log` (traditional)

**When to Use:**
- **Time-based**: Recent issues, performance troubleshooting, quick analysis
- **Full logs**: Historical analysis, comprehensive debugging, archival purposes

#### ⏰ **Timestamp-Based Naming**
```bash
# Enable automatic timestamp naming
.\fetchlogs.exe --all -log-name "production_logs"
# Creates: production_logs_20250624_021345.tar.gz
```

#### 🗑️ **Auto-Cleanup After Download**
```bash
# Automatically delete source archive after successful download (recommended)
.\fetchlogs.exe --all
# Downloads the file AND deletes it from AWS server (controlled by deleteAfterCopy config)
```

The `deleteAfterCopy` setting in config.yaml controls this behavior:
- `true`: Archive is deleted from remote server after successful download (recommended for production)
- `false`: Archive remains on remote server for manual cleanup (useful for debugging)

#### 🔧 **Configuration Options**
The `logCollection` section in `config.yaml` supports:
- `logFileName`: Base name for the log collection archive
- `tempDir`: Temporary directory on AWS for log collection (supports templates, automatically created)
- `useTimestamp`: Auto-append timestamp to filename (default: true)
- `deleteAfterCopy`: Delete source file after successful download (default: true)
- `timestampFormat`: Custom timestamp format (default: "20060102_150405")

```yaml
logCollection:
  logFileName: "app_log"
  tempDir: "{environment}_logs"
  useTimestamp: true
  deleteAfterCopy: true
  timestampFormat: "20060102_150405"
```

## 🔍 **Intelligent Log Analytics**

After downloading log archives, the tool automatically extracts and analyzes all log files for error patterns, correlates issues across files, and generates a comprehensive **`log_analytics_report.txt`** in the output directory. This helps quickly identify systemic problems vs. isolated errors.

### Key Features

- **🎯 Pattern-Based Detection**: Configurable search patterns for common error conditions (error, panic, failure, etc.)
- **📊 Cross-File Correlation**: Identifies patterns that appear across multiple log files, indicating systemic issues
- **🔗 Before/After Context**: Shows configurable lines before and after each match for full context
- **📈 Severity Classification**: Automatically classifies findings as CRITICAL, HIGH, MEDIUM, or LOW
- **📋 Executive Summary**: Quick overview with severity counts and top issues
- **📁 Per-File Breakdown**: Detailed match listing per file with line numbers and context

### Configuration

```yaml
logCollection:
  logAnalysis:
    enabled: true                   # Enable automatic log analysis
    outputFile: "log_analytics_report.txt"  # Report file name in output directory
    errorPatterns:                  # Patterns to search for (case-insensitive)
      - "error"
      - "panic" 
      - "failure"
      - "failed"
      - "exception"
      - "fatal"
      - "critical"
      - "timeout"
      - "connection refused"
      - "unable to"
      - "cannot"
      - "permission denied"
    maxMatches: 20                  # Maximum matches per log file
    contextLines: 2                 # Lines before/after each match for context
```

### Generated Report Structure

The report (`log_analytics_report.txt`) is saved in your output directory and contains 5 sections:

**Section 1 - Executive Summary:**
```
================================================================================
  LOG ANALYTICS REPORT
================================================================================
  Generated:     2025-07-10 12:00:00
  Archive:       app_log_20250710_120000.tar.gz
  Patterns:      error, panic, failure, failed, exception, fatal, ...
  Context Lines: 2 before / 2 after
  Files Analyzed: 8 with matches out of total scanned
  Total Matches: 47

  CRITICAL:  2 occurrences
  HIGH:      12 occurrences
  MEDIUM:    28 occurrences
  LOW:       5 occurrences

  Top Issues:
    1. [HIGH] 'error' - 25 occurrences [CROSS-FILE: 6 files]
    2. [CRITICAL] 'panic' - 2 occurrences [CROSS-FILE: 2 files]
    3. [MEDIUM] 'timeout' - 8 occurrences
```

**Section 2 - Correlated Issues (Cross-File Analysis):**
```
  CORRELATED ISSUE #1
  Pattern:    'error'
  Severity:   HIGH
  Occurrences: 25 total across 6 files
  Affected Files:
    - app_log/nvo-edge-pod1.log
    - app_log/cs-configuration-pod2.log
    - app_log/cas-api-service-pod1.log
  Assessment: Pattern 'error' found 25 times across 6 files. This cross-file
              occurrence suggests a systemic issue that may have cascading effects.
```

**Section 3 - Per-File Analysis (With Before/After Context):**
```
  FILE 1: app_log/nvo-edge-pod1.log
  Total Matches: 8
  Pattern Breakdown:
    - 'error': 5
    - 'timeout': 3

    Match 1/8 [Pattern: 'error'] at Line 1547:

      --- BEFORE ---
      1545 | 2025-07-09 10:23:14 INFO [NetworkManager] Attempting connection retry
      1546 | 2025-07-09 10:23:14 DEBUG [NetworkManager] Retry attempt 3 of 5
      >>> MATCHED LINE <<<
      1547 | 2025-07-09 10:23:15 ERROR [NetworkManager] Connection timeout occurred
      --- AFTER ---
      1548 | 2025-07-09 10:23:16 WARN [NetworkManager] Falling back to backup server
      1549 | 2025-07-09 10:23:17 INFO [NetworkManager] Backup connection established
```

**Section 4 - Pattern Frequency Table:**
```
  PATTERN                       COUNT      FILES
  -------------------------  ---------- ----------
  error                             25          6
  timeout                            8          3
  failed                             7          2
  panic                              2          2
```

**Section 5 - Recommendations:**
```
  Cross-file correlated issues detected. These errors appear in multiple log files
  simultaneously, suggesting a systemic problem (e.g., infrastructure, connectivity,
  or deployment issue) rather than an isolated bug.
```

### How It Works

1. **⬇️ Download**: Archive is downloaded from the server to local output directory
2. **📦 Extract**: Archive is extracted to a temporary directory for analysis
3. **🔍 Scan**: Each log/text file is scanned line-by-line for configured error patterns
4. **📝 Context Capture**: For each match, N lines before and after are captured (configurable `contextLines`)
5. **🔗 Correlation**: Patterns found across multiple files are flagged as cross-file correlated issues
6. **📊 Classification**: Each pattern is assigned a severity level based on type and frequency
7. **📋 Report**: Comprehensive report generated in the output directory
8. **🧹 Cleanup**: Temporary extraction directory is automatically removed

### Severity Classification

| Severity | Patterns | Criteria |
|----------|----------|----------|
| **CRITICAL** | `panic`, `fatal`, `critical` | Always critical — indicates crashes |
| **HIGH** | `exception`, `permission denied` | Always high; also `error`/`failure` when across 3+ files with 10+ occurrences |
| **MEDIUM** | `error`, `failure`, `failed`, `timeout`, `connection refused`, `unable to`, `cannot` | Standard error patterns |
| **LOW** | Other patterns | Low frequency or impact |

### Usage Examples

```bash
# Log collection with automatic analysis (runs after download if enabled in config)
.\fetchlogs.exe --all

# The report is generated in the output directory (e.g., C:\Logs\)
# Look for: log_analytics_report.txt
```

### When Analysis Helps

- **Production Incidents**: Quick assessment of error conditions across services
- **Health Checks**: Regular monitoring for warning signs and error patterns  
- **Deployment Validation**: Verify no new errors introduced after deployments
- **Performance Issues**: Identify timeout and connection problems
- **Capacity Planning**: Spot resource-related error patterns
- **Root Cause Analysis**: Cross-file correlation helps trace cascading failures

**Note**: The analysis runs locally after the archive is downloaded. The report is saved alongside the downloaded archive in your configured output directory.

## � **Post-Download Message Filter**

After downloading and analyzing log archives, the tool can optionally **filter** all log files by key-value pairs and/or specific strings, writing the filtered output into a `filter/` subdirectory. Original downloaded logs are never modified.

### Key Features

- **🔑 Key-Value Filtering**: Define key-value pairs (e.g., `ownerID=1096`). Lines containing the key are kept only if the value matches. Lines that don't contain the key at all pass through unaffected.
- **📝 Specific String Matching**: Define exact strings that must appear in a line for it to be kept. If configured, only lines containing at least one of the specified strings are included.
- **📁 Directory Preservation**: Filtered output mirrors the original archive directory structure inside `filter/<archiveName>/`.
- **🛡️ Non-Destructive**: Original logs remain untouched. Filtered copies are written to a separate directory.
- **📊 Summary Statistics**: Reports files filtered, lines kept, and lines removed.

### How It Works

1. **📦 Extract**: The downloaded `.tar.gz` archive is extracted to a temporary directory
2. **🔍 Scan**: Each text/log file is read line by line
3. **🔑 Key-Value Check**: For each key-value filter, if the line contains the key, it's kept only if the value matches. Lines without the key pass through.
4. **📝 String Check**: If specific strings are configured, the line must contain at least one to be kept.
5. **💾 Write**: Filtered lines are written to `filter/<archiveName>/` preserving the directory structure
6. **🧹 Cleanup**: Temporary extraction directory is removed automatically

### Filter Logic

| Scenario | Key-Value Filter Result | Specific String Filter Result |
|----------|------------------------|------------------------------|
| Line does NOT contain the key | ✅ Passes (unaffected) | N/A |
| Line contains key with **matching** value | ✅ Passes | N/A |
| Line contains key with **different** value | ❌ Filtered out | N/A |
| Line contains at least one specific string | N/A | ✅ Passes |
| Line does NOT contain any specific string | N/A | ❌ Filtered out |
| Both filters active | Must pass BOTH key-value AND specific string checks | |

### Configuration

```yaml
logCollection:
  messageFilter:
    enabled: true                   # Enable/disable message filtering
    keyValueFilters:                # Keep lines where key is absent OR key has matching value
      - key: "ownerID"
        value: "1096"              # Only keep lines with ownerID=1096 (or lines without ownerID)
      - key: "serial"
        value: ""                  # Only keep lines with empty serial (or lines without serial)
    specificStrings: []             # Only keep lines containing these strings (empty = no filter)
```

### Configuration Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `enabled` | bool | `false` | Enable/disable post-download message filtering |
| `keyValueFilters` | list | `[]` | List of key-value pairs to filter by |
| `keyValueFilters[].key` | string | — | Key to look for in log lines (e.g., `ownerID`, `serial`) |
| `keyValueFilters[].value` | string | — | Value to match. Lines with the key but a different value are removed |
| `specificStrings` | list | `[]` | Only keep lines containing at least one of these strings (empty = no filter) |

### Output Structure

```
C:\Logs\
├── app_log_20260217_120000.tar.gz          # Original archive (untouched)
├── log_analytics_report_20260217.txt       # Analytics report
├── logger_info.txt                         # Session log
└── filter/                                 # Filtered output directory
    └── app_log_20260217_120000/             # Named after the archive
        ├── app_log/
        │   ├── nvo-edge-pod1.log            # Filtered version
        │   ├── cs-configuration-pod2.log    # Filtered version
        │   └── cas-api-service-pod1.log     # Filtered version
        └── General/
            └── nodes_detailed.txt          # Filtered version
```

### Usage Examples

```yaml
# Example 1: Keep only lines for a specific ownerID
messageFilter:
  enabled: true
  keyValueFilters:
    - key: "ownerID"
      value: "1096"
  specificStrings: []

# Example 2: Keep only lines containing specific error messages
messageFilter:
  enabled: true
  keyValueFilters: []
  specificStrings:
    - "connection refused"
    - "timeout expired"

# Example 3: Combine both — ownerID filtering + specific string matching
messageFilter:
  enabled: true
  keyValueFilters:
    - key: "ownerID"
      value: "1096"
    - key: "serial"
      value: "SN12345"
  specificStrings:
    - "ERROR"
    - "WARN"
```

### When Message Filter Helps

- **Multi-Tenant Logs**: Filter logs by tenant/owner ID when multiple tenants share the same log files
- **Device-Specific Issues**: Isolate log entries for a specific device serial number
- **Focused Debugging**: Narrow down large log files to only error/warning messages for a specific entity
- **Noise Reduction**: Remove irrelevant log entries before manual review

**Note**: The message filter runs after log analytics. Both the original logs and the analytics report remain unchanged. Filtered files are created separately in the `filter/` directory.

## �📊 General System Information Collection

The tool automatically collects comprehensive system and cluster information alongside log files, providing a complete operational snapshot. Each command output is saved as a separate file within a `General/` directory inside the archive.

### Key Features

- **🔄 Automated Execution**: Runs kubectl and system commands as root user on remote server
- **📁 Organized Output**: Each command output saved to separate files in `General/` directory within the archive
- **📝 Detailed Headers**: Each file includes command, description, timestamp, and execution status
- **🛠️ Template Support**: Commands support `{environment}` and `{username}` placeholders
- **⚡ Integrated Collection**: System info collected **before** archive creation, ensuring inclusion in the downloaded archive
- **🔧 Configurable**: Fully customizable command list via config.yaml
- **⏰ Proper Timing**: General info collection happens during the log collection phase, not after download

### Archive Structure

The downloaded archive includes both logs and system information:
```
my_log_collection_20250624_143021.tar.gz
├── General/                                  # System information directory
│   ├── system_info.txt                       # uname -a output
│   ├── pods_all_namespaces.txt               # All pods across namespaces
│   ├── nodes_detailed.txt                    # Node information
│   ├── cpu_usage_ascending.txt               # CPU usage by pod
│   ├── memory_usage_ascending.txt            # Memory usage by pod
│   ├── describe_pods_current_namespace.txt   # Pod details for environment
│   ├── describe_nodes.txt                    # Node details and conditions
│   ├── cluster_info.txt                      # Cluster endpoint information
│   └── system_resources.txt                  # System resource snapshot
└── [log files from pods]                     # Pod log files
```

### Default Commands Collected

| Command | Output File | Description |
|---------|-------------|-------------|
| `uname -a` | `system_info.txt` | System information |
| `kubectl get pods --all-namespaces` | `pods_all_namespaces.txt` | All pods across namespaces |
| `kubectl get nodes -o wide` | `nodes_detailed.txt` | Detailed node information |
| `kubectl top pods --all-namespaces --sort-by=cpu` | `cpu_usage_ascending.txt` | CPU usage by pod |
| `kubectl top pods --all-namespaces --sort-by=memory` | `memory_usage_ascending.txt` | Memory usage by pod |
| `kubectl describe pods -n {environment}` | `describe_pods_current_namespace.txt` | Pod details for environment |
| `kubectl describe nodes` | `describe_nodes.txt` | Node details and conditions |
| `kubectl cluster-info` | `cluster_info.txt` | Cluster endpoint information |
| `top -bn1 \| head -20` | `system_resources.txt` | System resource snapshot |

### Configuration

```yaml
generalInfo:
  enabled: true                    # Enable/disable general info collection
  outputDir: "General"            # Directory name within the archive
  commands:
    - name: "pods_all_namespaces"           # Output filename (without .txt)
      command: "kubectl get pods --all-namespaces"
      description: "Get pods in all namespaces"
    - name: "cpu_usage_ascending"
      command: "kubectl top pods --all-namespaces --sort-by=cpu"
      description: "Pod CPU usage in ascending order"
    # Add custom commands here
```

### Template Usage in Commands

Commands support the same template placeholders as other configuration sections:

```yaml
commands:
  - name: "environment_pods"
    command: "kubectl get pods -n {environment}"  # Becomes: kubectl get pods -n ws4r1
    description: "Pods in current environment"
  - name: "user_processes"
    command: "ps aux | grep {username}"           # Becomes: ps aux | grep nperiannan
    description: "User-specific processes"
```

### Output Format

Each generated file includes:
```
# Command: kubectl get pods --all-namespaces
# Description: Get pods in all namespaces
# Executed at: 2025-06-24 07:48:21
# Template replaced command: kubectl get pods --all-namespaces

# Command executed successfully

NAMESPACE     NAME                    READY   STATUS    RESTARTS   AGE
nvo           nvo-edge-abc123         1/1     Running   0          5d
ws4r1         hacr-def456             1/1     Running   0          3d
...
```

### Usage Control

```bash
# Collect logs and system info (default behavior with --all)
.\fetchlogs.exe --all

# Collect only system info (no logs or versions)
.\fetchlogs.exe --sys-info

# Disable general info collection via config
# Set generalInfo.enabled: false in config.yaml

# View detailed execution with debug logging
.\fetchlogs.exe --all -log-level DEBUG
```

The general system information is automatically included in the same archive as the log files, providing a comprehensive operational snapshot for troubleshooting and analysis. The collection happens **before** archive creation, ensuring all information is properly included in the downloaded file.

## ⏳ Temporal Workflow Collection (NEW)

The tool can automatically collect comprehensive debugging information from **Temporal workflows** by connecting to the Temporal admin pod in the Kubernetes cluster. For each workflow, it collects input, output, activity details, and failure information — saving everything into separate, well-organized files.

### Key Features

- **🔍 Automatic Pod Discovery**: Finds the running `temporal-admintools` pod in the `common` namespace
- **📋 Workflow Listing**: Lists recent workflows with status, ID, type, and start time
- **📥 Input/Output Collection**: Decodes base64-encoded workflow payloads and pretty-prints JSON
- **⚙️ Activity Details**: Collects input, output, and failure data for every activity in each workflow
- **🎯 Prefix Filtering**: Optionally filter workflows by ID prefix (e.g., `deploy-profile-`)
- **📁 Per-Workflow Files**: Each workflow's complete data saved to a separate file in the `Temporal/` directory
- **🗜️ Archive Integration**: Temporal data included in the same archive as logs and system info

### Archive Structure

```
my_log_collection_20250710_120000.tar.gz
├── Temporal/                                          # Temporal workflow data
│   ├── workflow_list.txt                              # Tabular workflow listing (human-readable)
│   ├── workflow_list.json                             # JSON workflow listing (machine-parseable)
│   ├── deploy-profile-Test_profile-20260105-141248.txt  # Per-workflow details
│   └── deploy-profile-Prod_profile-20260105-150000.txt
├── General/                                           # System information
└── [log files from pods]                              # Pod log files
```

### Per-Workflow File Contents

Each workflow file contains all collected data in a structured format:

```
# Temporal Workflow Details
# Workflow ID: deploy-profile-Test_profile-20260105-141248
# Namespace: configuration
# Collected: 2025-07-10 12:00:00

================================================================================
  WORKFLOW INPUT
================================================================================

{
  "batchId": "batch-abc123",
  "profileName": "Test_profile",
  ...
}

================================================================================
  WORKFLOW OUTPUT
================================================================================

{
  "batchStatus": { ... },
  ...
}

================================================================================
  ACTIVITIES
================================================================================

5  EVENT_TYPE_ACTIVITY_TASK_SCHEDULED  GetConfigurationFeatures
8  EVENT_TYPE_ACTIVITY_TASK_SCHEDULED  PrepareDeviceRecipe
11 EVENT_TYPE_ACTIVITY_TASK_SCHEDULED  ProvisionConfigurationFeatures

--------------------------------------------------------------------------------
  ACTIVITY INPUT: GetConfigurationFeatures
--------------------------------------------------------------------------------

{ ... }

--------------------------------------------------------------------------------
  ACTIVITY OUTPUT: GetConfigurationFeatures
--------------------------------------------------------------------------------

{ ... }

--------------------------------------------------------------------------------
  ACTIVITY FAILURE: GetConfigurationFeatures
--------------------------------------------------------------------------------

No failure data found for activity GetConfigurationFeatures
```

### Configuration

```yaml
logCollection:
  # Temporal workflow information collection
  temporalWorkflowCollection:
    enabled: true                    # Enable/disable temporal workflow data collection
    workflowIdPrefix: ""            # Filter workflows by ID prefix (empty = all workflows)
    numberOfWorkflows: 3            # Number of recent workflows to collect (1-20, default 3)
    namespace: "configuration"      # Temporal namespace to query
```

### Configuration Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `enabled` | bool | `false` | Enable/disable Temporal workflow collection |
| `workflowIdPrefix` | string | `""` | Filter workflows by ID prefix (empty = all) |
| `numberOfWorkflows` | int | `3` | Number of recent workflows to collect (1-20) |
| `namespace` | string | `configuration` | Temporal namespace to query |

### How It Works

1. **Pod Discovery**: Finds the running `temporal-admintools-*` pod in the `common` namespace
2. **Workflow Listing**: Runs `temporal workflow list --namespace configuration` inside the admin pod (saved as tabular `workflow_list.txt` and JSON `workflow_list.json`)
3. **Filtering**: Applies prefix filter and limits to the configured number of workflows
4. **Per-Workflow Collection**: For each workflow ID, collects:
   - **Workflow Input**: Decoded JSON payload from the start event
   - **Workflow Output**: Decoded JSON result from the completion event
   - **Activities List**: All activity events with IDs, types, and names
   - **Per-Activity Details**: For each unique activity:
     - Activity input (decoded JSON)
     - Activity output (decoded JSON)
     - Activity failure details (if any)
5. **File Output**: Saves a comprehensive file per workflow in the `Temporal/` directory
6. **Archive**: All temporal data is included in the final `.tar.gz` archive

### Usage Examples

```bash
# Collect logs with temporal workflow data
.\fetchlogs.exe --all
# (temporal collection runs automatically if enabled in config.yaml)

# Collect only from specific workflow prefix
# Set workflowIdPrefix: "deploy-profile-" in config.yaml

# Collect more workflows (up to 20)
# Set numberOfWorkflows: 10 in config.yaml
```

### Temporal Cheat Sheet

The config.yaml also includes a `Temporal_Cheat_Sheet` section with bash aliases for manual Temporal debugging. These are the same commands the automated collection uses internally:

```bash
# Manual usage: kubectl exec into the admin pod first
kubectl exec -it <temporal-admin-pod> -n common -- bash

# Then use the aliases:
tc-list                                    # List all workflows
tc-input <workflow-id>                     # Show workflow input
tc-output <workflow-id>                    # Show workflow output
tc-activities <workflow-id>                # List activities with event IDs
tc-activity-input-by-name <wf-id> <name>  # Activity input by name
tc-activity-output <wf-id> <name>         # Activity output by name
tc-activity-failure <wf-id> <name>        # Activity failure details
```

## 📋 Application Version Collection

The tool provides a standalone feature to collect and display application version information from Kubernetes clusters in a formatted output, perfect for documenting deployment states and troubleshooting version-related issues.

### Key Features

- **🚀 Standalone Operation**: Runs independently using `--version` mode
- **📊 Formatted Output**: Produces clean, tabular version reports with timestamps
- **🎯 Environment-Aware**: Automatically extracts base environment (e.g., "dl1" from "dl1r1")
- **🌐 Dynamic Namespace Detection**: Automatically detects XIQ namespace (uses 'xiq' if exists, else environment name)
- **⏰ Timestamped Files**: Includes `{timestamp}` in filename for version tracking history
- **🔧 Configurable Namespaces**: Supports multiple namespaces and pod prefixes
- **📝 Local Output**: Saves version report to local directory
- **⚙️ Template Support**: Namespace names support `{environment}` and `{timestamp}` placeholders

### Output Format

The tool generates a clean, formatted report showing component versions with namespace separation:

```
### dl1 Cloud testbed ###
### dl1 RDC components versions ###
Date and Time:
Tue Feb 16 14:30:25 UTC 2026

Namespace                      Pod                                          Version
------------------------------------------------------------------------------------

=== common namespace ===
common                         cas-api-service-6d86b6495b-fhncd             25.4.0-73
common                         cls-api-service-67bc7c94db-kt5xj             25.4.0-73
common                         cns-api-service-859b64b87-2t2nl              25.4.0-73

=== xiq namespace ===
xiq                            hmweb-64d648fc69-6t2j2                       25.4.0-109
xiq                            teconfig-8bb6bcd9-6k49x                      25.4.0-109

=== nvo namespace ===
nvo                            nvo-edge-58967bc8cb-28qdf                    24.5.0-39
nvo                            nvo-network-7cb6dcf8f-26dch                  24.5.0-39
```

**Note**: Namespace separators (===) are added between different namespaces for better readability.

### Configuration

```yaml
appVersionCollection:
  enabled: true                    # Enable/disable app version collection
  outputFileName: "{environment}_app_versions_{timestamp}.txt"  # Timestamped output file
  printToLog: true                 # Print version information to console
  namespaces:                      # Namespaces and pod prefixes to check for versions
    - namespace: "common"
      description: "Common services namespace"
      podPrefixes: ["cas-api-service", "cls-api-service", "cns-api-service", "cs-configstate"]
    - namespace: "{environment}"   # Dynamic: uses 'xiq' if exists, else environment name
      description: "Environment-specific services (XIQ namespace)"
      podPrefixes: ["hmweb", "teconfig", "copilot-flink"]
    - namespace: "nvo"
      description: "NVO services namespace"
      podPrefixes: ["nvo-edge", "nvo-network"]
```

### Usage Examples

```bash
# Collect app versions using new mode syntax
.\fetchlogs.exe --version

# Collect app versions with debug output
.\fetchlogs.exe --version -log-level DEBUG

# Enable via config file (run without mode flags)
# Set appVersionCollection.enabled: true in config.yaml
.\fetchlogs.exe

# Collect everything including versions
.\fetchlogs.exe --all
```

### Output File Naming

With the `{timestamp}` placeholder in the configuration:
- **Config**: `outputFileName: "{environment}_app_versions_{timestamp}.txt"`
- **Output**: `dl1r1_app_versions_20260216_143025.txt`

This creates unique version snapshots for tracking version changes over time.

### How It Works

1. **🔗 Connects** to AWS server via bastion (same as log collection)
2. **🔍 Detects** XIQ namespace dynamically (checks if 'xiq' exists)
3. **🔍 Queries** each configured namespace for pods matching the specified prefixes
4. **📦 Extracts** version information from pod labels or container images
5. **📊 Formats** the output with namespace separators and clean tabular format
6. **💾 Saves** the report with timestamp to local directory
7. **✅ Exits** cleanly (no file downloads or other operations)

### Dynamic Namespace Detection

For namespaces configured with `{environment}`:
- **Checks** if dedicated `xiq` namespace exists
- **Uses** `xiq` if found (typical in production environments)
- **Falls back** to environment name (e.g., `dl2r1`) if `xiq` doesn't exist
- **Logs** the detection result: "✅ Found dedicated 'xiq' namespace" or "📌 No dedicated 'xiq' namespace found"

### Notes

- **Standalone Operation**: Does not interfere with log collection
- **First Pod Selection**: When multiple pods exist with the same prefix, only the first is used
- **Version Sources**: Attempts to get version from pod labels first, then from container image tags
- **Environment Extraction**: Automatically extracts base environment name (removes last 2 characters from environment ID)
- **Template Support**: Namespace names support `{environment}` and output filename supports `{timestamp}`

This feature is perfect for creating deployment documentation, version audits, and troubleshooting version compatibility issues across different environments.

## Notes

- **Operation modes** (`--all`, `--logs-only`, `--sys-info`, `--version`) override config.yaml settings
- **No mode specified**: Uses enabled/disabled settings from config.yaml for each feature
- **Password encryption**: Leave password empty in config, will prompt and encrypt on first run
- **Masked input**: Password entry is secure with hidden characters (no echo to screen)
- **Dynamic namespace**: XIQ applications use 'xiq' namespace if it exists, else environment namespace
- In interactive mode, you can select specific log files to download
- Operation mode flags take precedence over configuration file values
- Create a `config.yaml` file to store frequently used settings
- For large files, parallel downloading is used, splitting the file into chunks (default: 3, max: 10)
- You can configure the number of chunks in `config.yaml` with:
  ```yaml
  options:
    numChunks: 4
  ```
- Or use the `-num-chunks` flag to override the config file setting (max value: 10)
- If any chunk fails during download, you'll be prompted to retry just the failed chunks
- Configure automatic retry behavior in `config.yaml`:
  ```yaml
  options:
    autoRetry: true  # Automatically retry failed chunks without prompting
    logLevel: INFO   # Control output verbosity (DEBUG, INFO, WARN, ERROR)
  ```
- **Cleanup Behavior**: After successful download, the tool automatically:
  - Deletes the source archive from AWS server (if `deleteAfterCopy: true` in config)
  - Safely removes the temporary directory on remote server (only if it's empty to prevent accidental data loss)
  - Provides detailed logging of cleanup operations in DEBUG mode

## ⚡ High-Performance Native SCP Download Architecture

The tool uses an optimized **2-step SCP-based architecture** for maximum download performance, achieving **10x faster speeds** compared to traditional SFTP approaches.

### 🏗️ 2-Step Download Architecture

**Step 1: AWS → Bastion (Internal Network)**
- Uses native `scp` command on AWS instance
- Leverages high-speed internal network connection
- Near-instantaneous transfer for typical log files (30-40MB)
- No external network bottleneck

**Step 2: Bastion → Local Machine (Native SCP)**
- Uses **native SCP client** on your local machine
- Achieves **2-4 MB/s** download speeds (vs 435-667 KB/s with SFTP)
- Automatic temporary SSH key generation for passwordless operation
- OpenSSH performance optimizations (TCP window sizing, compression)

### 🔑 Passwordless SCP Automation

**Automatic Key Management:**
1. Generates temporary ED25519 SSH key pair (`ssh-keygen`)
2. Adds public key to bastion's `~/.ssh/authorized_keys`
3. Executes native `scp` command with temporary private key
4. Automatically removes key from bastion after download
5. Cleans up temporary key files from local machine

**Benefits:**
- **Zero user interaction**: Fully automated without password prompts
- **Security**: Temporary keys deleted immediately after use
- **Compatibility**: Standard ED25519 keys work everywhere
- **Reliability**: No manual key management required

### 🚀 Performance Comparison

| Method | Speed | Time (30MB) | Notes |
|--------|-------|-------------|-------|
| **Native SCP** | **2-4 MB/s** | **8-18 sec** | ⚡ Current (10x faster) |
| SFTP (8 parallel) | 435-667 KB/s | 50-75 sec | Fallback option |
| SFTP (4 parallel) | 258-318 KB/s | 95-120 sec | Previous version |
| SFTP (single) | 137-160 KB/s | 3-5 min | Original baseline |

**Real-World Performance:**
- **30-40MB log files**: 8-18 seconds (vs 50-75 seconds with SFTP)
- **Sustained throughput**: 2-4 MB/s consistently
- **Network efficiency**: Optimal TCP window sizing and compression
- **Minimal overhead**: Native OpenSSH implementation

### 🔄 Intelligent Fallback System

**Automatic SFTP Fallback:**
If native SCP fails for any reason, the tool automatically falls back to parallel SFTP:
- **8 independent SSH connections** for parallel chunks
- **Separate chunk files** for zero I/O contention
- **667 KB/s speeds** with 8 parallel connections
- **Reliable fallback** ensures downloads always succeed

**Fallback Triggers:**
- SCP client not available (`scp` command missing)
- SSH key generation fails (`ssh-keygen` unavailable)
- Temporary key setup fails on bastion
- SCP command execution errors

### 🌍 Cross-Platform Support

**Windows:**
- Uses OpenSSH client (included in Windows 10+)
- `UserKnownHostsFile=NUL` for host key bypass
- Native `scp.exe` and `ssh-keygen.exe` executables

**Linux:**
- Uses standard OpenSSH client (pre-installed)
- `UserKnownHostsFile=/dev/null` for host key bypass
- Native `scp` and `ssh-keygen` utilities

**Automatic Detection:**
- Runtime OS detection (`runtime.GOOS`)
- Platform-specific path handling
- No configuration changes needed

### ⚙️ Configuration

```yaml
options:
  downloadMethod: "scp"  # "scp" (native SCP, default) or "sftp" (parallel SFTP)
  numChunks: 8           # Used only for SFTP fallback (1-10)
```

**Download Method Options:**
- **`"scp"` (default)**: Uses native OpenSSH SCP client for maximum performance
  - **Speed**: 2-4 MB/s (10x faster than SFTP)
  - **Overhead**: Minimal - single optimized stream
  - **Requirements**: `scp` command available on local machine
  
- **`"sftp"`**: Uses parallel SFTP connections for compatibility
  - **Speed**: 435-667 KB/s with 8 parallel connections
  - **Overhead**: Multiple SSH sessions, chunk management
  - **Use case**: When SCP client unavailable or troubleshooting

**Configuration Priority:**
1. **Command-line flags** (highest): `--native-scp` or `--sftp`
2. **Config file**: `options.downloadMethod`
3. **Default** (lowest): `"scp"`

**Note:** The `numChunks` setting is now used only for SFTP fallback scenarios. Native SCP transfers the entire file in a single optimized stream.

### 🎯 Choosing Between SCP and SFTP

**When to Use Native SCP (Default):**
- ✅ **Best for**: Maximum download speed (2-4 MB/s)
- ✅ **Use when**: OpenSSH client available (Windows 10+, Linux, macOS)
- ✅ **Benefits**: 10x faster, simpler, less overhead
- ✅ **Recommended**: For all regular usage

**When to Use SFTP:**
- 🔧 **Use when**: SCP client not available on your system
- 🔧 **Use when**: Troubleshooting SCP issues
- 🔧 **Use when**: Corporate restrictions prevent SCP usage
- ⚙️ **Performance**: Still good (435-667 KB/s with 8 connections)

**Command-Line Examples:**

```bash
# Default behavior (uses SCP from config.yaml or defaults to SCP)
.\fetchlogs.exe --all

# Explicitly force SCP (override config)
.\fetchlogs.exe --all --native-scp

# Force SFTP with default connections (8)
.\fetchlogs.exe --all --sftp

# Force SFTP with 10 parallel connections for maximum SFTP speed
.\fetchlogs.exe --all --sftp -num-chunks 10

# Debug to see which method is being used
.\fetchlogs.exe --all -log-level DEBUG
```

**Configuration File Examples:**

```yaml
# Default: use native SCP (recommended)
options:
  downloadMethod: "scp"
  numChunks: 8  # Only used if falling back to SFTP

# Alternative: use SFTP by default
options:
  downloadMethod: "sftp"
  numChunks: 10  # Use 10 parallel connections for SFTP
```

### 📊 Technical Details

**SCP Command Parameters:**
```bash
scp -i <temp_key> \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -P <port> \
    user@bastion:path \
    local_path
```

**Key Features:**
- **Temporary key authentication**: No password prompts
- **Host key verification bypass**: Automated operation
- **Custom port support**: Flexible bastion configuration
- **Native OpenSSH**: Maximum performance and compatibility

### 🔧 Usage Examples

```bash
# Standard download (uses native SCP automatically)
.\fetchlogs.exe --all

# Debug mode to see SCP command details
.\fetchlogs.exe --all -log-level DEBUG

# Force SFTP fallback by using older version
# (Native SCP is automatic in current version)
```

**What You'll See:**
```
⚡ Step 2: Bastion -> Local (via native SCP)
Downloading via native SCP (expect 2-4 MB/s)...
Download Progress: ████████████████████ 100% | 35.2 MB/35.2 MB | 3.8 MB/s
✓ File downloaded and verified successfully
```

## Log Levels

The tool supports four log levels to control output verbosity:

- **DEBUG**: Shows all messages including detailed chunk-by-chunk download progress, file verification details, and diagnostic information
- **INFO**: Shows connection status, download progress, and general operational messages (default)
- **WARN**: Shows warnings and important operational messages, minimal connection output
- **ERROR**: Shows only errors and critical messages, very minimal output

You can set the log level in your config.yaml file:
```yaml
options:
  logLevel: DEBUG
```

Or override it with the command-line flag:
```bash
.\gofetchlogs.exe -log-level DEBUG
```

## Error Recovery

When downloading large files in parallel, network interruptions can cause individual chunks to fail. The tool handles this by:

1. Tracking which specific chunks failed during download
2. Offering to retry only the failed chunks (instead of redownloading the entire file)
3. Merging the retried chunks with the successful ones to create a complete file
4. Performing thorough verification of the final file:
   - Checking file size matches the expected size
   - Verifying content at multiple positions (start, middle, end)
   - Ensuring the file isn't empty or filled with zeros
   - Sampling file content throughout to ensure valid data

The tool includes detailed debugging output to help diagnose issues with the download process, including:
- Chunk-by-chunk download progress and independent connection creation
- File size analysis for hybrid approach selection (single vs parallel)
- Connection load balancing and bastion resource usage
- Size and content information for each parallel chunk
- Verification of data at multiple points in the file
- Warnings if file content appears suspicious (e.g., mostly zeros)
- Detailed error messages for failed chunks and connection issues

**🔧 Advanced SSH Session Management:**
- **Independent sessions**: Each operation uses fresh SSH connections to prevent "Stdout already set" errors
- **Automatic session cleanup**: Proper resource management and connection pooling
- **Error isolation**: Failed operations don't impact other concurrent processes
- **Robust log collection**: Parallel collection with isolated SSH sessions for maximum reliability

This ensures that even with unreliable connections, you can successfully download complete and valid files while minimizing load on the bastion server.

## 🔧 Troubleshooting

### Common Issues and Solutions

**1. General info files missing from downloaded archive:**
- **Cause**: General info collection happening after archive creation (fixed in current version)
- **Solution**: Use latest version where general info is collected before archive creation

**2. "EOF errors" during log collection:**
- **Cause**: Too many concurrent SSH sessions
- **Solution**: Reduce `numChunks` to 3-5 in config, use `logLevel: DEBUG`

**3. "user does not exist" errors:**
- **Cause**: Incorrect sudo command syntax (fixed in current version)
- **Solution**: Use the latest version with `sudo bash -c` commands

**4. No namespace logs collected:**
- **Cause**: kubectl commands failing due to permissions or connectivity
- **Solution**: 
  - Verify kubectl access: `kubectl get pods --all-namespaces`
  - Check namespace exists: `kubectl get namespaces`
  - Use `logLevel: DEBUG` to see detailed error messages

**5. General info commands timing out:**
- **Cause**: Heavy commands like `kubectl top` or `describe` operations
- **Solution**: Use simpler commands or increase timeout values

**6. Archive creation fails:**
- **Cause**: Insufficient disk space or permissions
- **Solution**: 
  - Check disk space: `df -h`
  - Verify temp directory permissions
  - Set `deleteAfterCopy: false` for debugging

**7. Remote archive not deleted after download:**
- **Cause**: `deleteAfterCopy` set to false or download failed
- **Solution**: 
  - Set `deleteAfterCopy: true` in config.yaml
  - Check download completed successfully (no errors in log)
  - Use `logLevel: DEBUG` to see cleanup operations

### Session Log File (`logger_info.txt`)

Every run of the tool creates a **`logger_info.txt`** file in the current working directory that mirrors all terminal output. This is useful for:

- **Audit trail**: Review exactly what happened during a collection run
- **Troubleshooting**: Share the log file with others to diagnose issues
- **Automation**: Parse the log file programmatically for CI/CD pipelines

The file is created fresh each run with a session header:

```
================================================================================
  EP1 Log Collector - Session Log
  Started: 2025-07-10 12:00:00
  Log Level: INFO
================================================================================

[INFO] Starting log collection...
[INFO] Connected to bastion host...
...
```

### Best Practices

- **Start with DEBUG logging**: Set `logLevel: DEBUG` for initial testing and troubleshooting
- **Use recommended settings**: `numChunks: 5`, `deleteAfterCopy: true` for production use
- **Test with simple commands**: Start with basic system commands before complex kubectl operations
- **Monitor resource usage**: Use `top` or `htop` on target system during collection
- **Verify archive contents**: Extract downloaded archive locally to verify General/ directory is included
- **Enable auto-cleanup**: Set `deleteAfterCopy: true` to prevent accumulation of old archives on server

### Performance Tuning

- **For stable connections**: Increase `numChunks` to 7-10
- **For unstable connections**: Decrease `numChunks` to 3-5
- **For large files**: Enable `autoRetry: true`
- **For quick testing**: Disable features in config.yaml: set `enabled: false` for each feature
- **For production use**: Enable `deleteAfterCopy: true` to manage disk space
- **For debugging**: Set `deleteAfterCopy: false` and `logLevel: DEBUG`
