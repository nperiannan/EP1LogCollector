package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// Build-time version information injected via -ldflags
var (
	appVersion  = "2.8.1"   // Semantic version (set via -ldflags)
	buildNumber = "dev"     // Auto-incrementing build number (set via -ldflags)
	buildDate   = "unknown" // Build timestamp (set via -ldflags)
)

// LogLevel represents different logging levels
type LogLevel int

const (
	DEBUG LogLevel = iota
	INFO
	WARN
	ERROR
)

// String returns the string representation of the log level
func (l LogLevel) String() string {
	switch l {
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// ParseLogLevel converts a string to LogLevel
func ParseLogLevel(level string) LogLevel {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return DEBUG
	case "INFO":
		return INFO
	case "WARN", "WARNING":
		return WARN
	case "ERROR", "ERR":
		return ERROR
	default:
		return INFO // Default to INFO level
	}
}

// Logger provides structured logging with timestamps
type Logger struct {
	minLevel LogLevel
	logFile  *os.File
}

// NewLogger creates a new logger instance with the specified minimum log level.
// If outputDir is empty, creates logger_info.txt in PWD.
// If outputDir is provided, creates logger_info.txt in that directory.
func NewLogger(minLevel LogLevel, outputDir string) *Logger {
	l := &Logger{minLevel: minLevel}

	// Determine log file path
	var logFilePath string
	if outputDir != "" {
		logFilePath = filepath.Join(outputDir, "logger_info.txt")
	} else {
		logFilePath = "logger_info.txt"
	}

	// Create log file for capturing all output
	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Printf("Warning: Could not create %s: %v\n", logFilePath, err)
	} else {
		l.logFile = f
		// Write header
		header := fmt.Sprintf("# LogCollector Session Log\n# Started: %s\n# Log Level: %s\n%s\n\n",
			time.Now().Format("2006-01-02 15:04:05"), minLevel.String(), strings.Repeat("-", 60))
		f.WriteString(header)
	}
	return l
}

// Close closes the log file
func (l *Logger) Close() {
	if l.logFile != nil {
		l.logFile.Close()
	}
}

// MoveLogFileTo moves the logger_info.txt into the specified directory.
// The existing log file is closed, its content is copied to the new location,
// the old file is deleted, and subsequent log output goes to the new file.
func (l *Logger) MoveLogFileTo(outputDir string) {
	if l.logFile == nil {
		return
	}
	l.logFile.Sync()

	oldPath := l.logFile.Name()
	newPath := filepath.Join(outputDir, "logger_info.txt")

	// Don't move if already in the target directory
	absOld, _ := filepath.Abs(oldPath)
	absNew, _ := filepath.Abs(newPath)
	if absOld == absNew {
		return
	}

	// Close the file first so we can read it (file is opened write-only)
	l.logFile.Close()

	// Read existing content from closed file
	content, err := os.ReadFile(oldPath)
	if err != nil {
		fmt.Printf("Warning: Could not read logger_info.txt for move: %v\n", err)
		return
	}

	// Create new file in target directory (append mode for subsequent writes)
	newFile, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("Warning: Could not create logger_info.txt in %s: %v\n", outputDir, err)
		return
	}

	// Write existing content to new file
	if _, err := newFile.Write(content); err != nil {
		fmt.Printf("Warning: Failed to write to new log file: %v\n", err)
		newFile.Close()
		return
	}

	// Remove old file
	if err := os.Remove(oldPath); err != nil {
		fmt.Printf("Warning: Could not remove original logger_info.txt: %v\n", err)
		// Continue anyway - the new file exists
	}

	// Switch to new file
	l.logFile = newFile
}

// CopyLogFileTo copies the current log file to the specified output directory.
// If archiveTimestamp is non-empty, the file is saved as logger_info_{timestamp}.txt.
// After copying, the original file in PWD is deleted to prevent clutter.
func (l *Logger) CopyLogFileTo(outputDir string, archiveTimestamp string) {
	if l.logFile == nil {
		return
	}
	l.logFile.Sync()

	srcPath := l.logFile.Name()
	dstFileName := "logger_info.txt"
	if archiveTimestamp != "" {
		dstFileName = fmt.Sprintf("logger_info_%s.txt", archiveTimestamp)
	}
	dstPath := filepath.Join(outputDir, dstFileName)

	absSrc, _ := filepath.Abs(srcPath)
	absDst, _ := filepath.Abs(dstPath)
	if absSrc == absDst {
		return
	}

	src, err := os.Open(srcPath)
	if err != nil {
		fmt.Printf("Warning: Could not read log file for copy: %v\n", err)
		return
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		fmt.Printf("Warning: Could not create log file in output dir: %v\n", err)
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		fmt.Printf("Warning: Failed to copy log file: %v\n", err)
		return
	}

	// Delete the original file from PWD after successful copy
	if err := os.Remove(srcPath); err != nil {
		fmt.Printf("Warning: Could not remove original logger_info.txt: %v\n", err)
	}
}

// Log prints a message with the specified log level
func (l *Logger) Log(level LogLevel, format string, args ...interface{}) {
	// Nil-safe: if logger is not yet initialized, fall back to stdout
	if l == nil {
		timestamp := time.Now().Format("2006-01-02 15:04:05")
		message := fmt.Sprintf(format, args...)
		fmt.Printf("[%s] [%s] %s\n", timestamp, level.String(), message)
		return
	}

	// Only print if the message level is >= minimum level
	if level < l.minLevel {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	message := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] [%s] %s\n", timestamp, level.String(), message)

	// Terminal output: truncate to 125 visible characters + " ..." (129 total).
	// ERROR-level lines are shown in full — truncating diagnostic detail (e.g. a
	// JIRA API error body) makes failures impossible to understand at a glance
	// and forces digging through the log file for the real reason.
	const maxTerminalWidth = 125
	lineNoNewline := strings.TrimRight(line, "\n")
	if level != ERROR && len(lineNoNewline) > maxTerminalWidth {
		fmt.Println(lineNoNewline[:maxTerminalWidth] + " ...")
	} else {
		fmt.Print(line)
	}

	// Log file: always write full untruncated line
	if l.logFile != nil {
		l.logFile.WriteString(line)
	}
}

// Debug prints debug messages (only when debug mode is enabled)
func (l *Logger) Debug(format string, args ...interface{}) {
	l.Log(DEBUG, format, args...)
}

// Info prints info messages
func (l *Logger) Info(format string, args ...interface{}) {
	l.Log(INFO, format, args...)
}

// Warn prints warning messages
func (l *Logger) Warn(format string, args ...interface{}) {
	l.Log(WARN, format, args...)
}

// Error prints error messages
func (l *Logger) Error(format string, args ...interface{}) {
	l.Log(ERROR, format, args...)
}

// Global logger instance
var logger *Logger

// Encryption key for password encryption (32 bytes for AES-256)
// Change this key if you want different encryption
const encryptionKey = "g0F3tchL0gs!Secr3tK3y@2026EncV1!" // 32 bytes exactly

// encryptPassword encrypts a password using AES-GCM
func encryptPassword(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	key := []byte(encryptionKey)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	encoded := base64.StdEncoding.EncodeToString(ciphertext)
	return "ENC:" + encoded, nil
}

// decryptPassword decrypts an encrypted password
func decryptPassword(encrypted string) (string, error) {
	if encrypted == "" {
		return "", nil
	}

	// If not encrypted, return as is
	if !strings.HasPrefix(encrypted, "ENC:") {
		return encrypted, nil
	}

	// Remove ENC: prefix
	encrypted = strings.TrimPrefix(encrypted, "ENC:")

	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("failed to decode encrypted password: %v", err)
	}

	key := []byte(encryptionKey)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt password: %v", err)
	}

	return string(plaintext), nil
}

// promptPassword prompts the user to enter a password with masked input
func promptPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println() // Print newline after masked input
	if err != nil {
		return "", fmt.Errorf("failed to read password: %v", err)
	}
	return strings.TrimSpace(string(passwordBytes)), nil
}

// saveConfigWithEncryptedPassword saves the config file with encrypted password
func saveConfigWithEncryptedPassword(configPath string, config *Config, newPassword string) error {
	// Encrypt the password
	encryptedPass, err := encryptPassword(newPassword)
	if err != nil {
		return fmt.Errorf("failed to encrypt password: %v", err)
	}

	// Read the original config file to preserve formatting and comments
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config file: %v", err)
	}

	// Convert to string and replace the password line
	content := string(data)
	lines := strings.Split(content, "\n")

	// Find and replace the password line
	passwordReplaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Match lines like: password: "..." or password: '...' or password: ...
		if strings.HasPrefix(trimmed, "password:") {
			// Use consistent 2-space indentation for password field under bastion
			lines[i] = fmt.Sprintf("  password: \"%s\"", encryptedPass)
			passwordReplaced = true
			break
		}
	}

	if !passwordReplaced {
		return fmt.Errorf("could not find password field in config file")
	}

	// Join lines back together
	newContent := strings.Join(lines, "\n")

	// Write back to file
	if err := ioutil.WriteFile(configPath, []byte(newContent), 0600); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	logger.Info("Password encrypted and saved to config file")
	return nil
}

// ConnectionParams holds parameters needed to create independent SSH connections
type ConnectionParams struct {
	BastionClient     *ssh.Client
	BastionUsername   string
	BastionPassword   string
	BastionHost       string
	BastionPort       int
	AWSHost           string
	KeyPath           string
	PreferredUsername string
}

// SSHConnectionPool manages a pool of SSH connections to avoid session limits
type SSHConnectionPool struct {
	connections chan *ssh.Client
	params      ConnectionParams
	poolSize    int
	mu          sync.Mutex
	closed      bool
}

// NewSSHConnectionPool creates a new connection pool
func NewSSHConnectionPool(params ConnectionParams, poolSize int) (*SSHConnectionPool, error) {
	pool := &SSHConnectionPool{
		connections: make(chan *ssh.Client, poolSize),
		params:      params,
		poolSize:    poolSize,
	}

	// Pre-populate the pool with connections
	for i := 0; i < poolSize; i++ {
		conn, err := sshConnectAWSViaBastionWithLogging(params.BastionClient, params.AWSHost, params.KeyPath, params.PreferredUsername, false)
		if err != nil {
			// Close any connections we already created
			pool.Close()
			return nil, fmt.Errorf("failed to create connection %d: %v", i, err)
		}
		pool.connections <- conn
	}

	return pool, nil
}

// Get retrieves a connection from the pool
func (p *SSHConnectionPool) Get() (*ssh.Client, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("connection pool is closed")
	}
	p.mu.Unlock()

	select {
	case conn := <-p.connections:
		// Test if connection is still alive
		session, err := conn.NewSession()
		if err != nil {
			// Connection is dead, create a new one
			logger.Debug("Recreating dead SSH connection in pool")
			newConn, newErr := sshConnectAWSViaBastionWithLogging(p.params.BastionClient, p.params.AWSHost, p.params.KeyPath, p.params.PreferredUsername, false)
			if newErr != nil {
				return nil, fmt.Errorf("failed to recreate connection: %v", newErr)
			}
			return newConn, nil
		}
		session.Close()
		return conn, nil
	default:
		// Pool is empty, create a temporary connection
		logger.Debug("Creating temporary SSH connection (pool exhausted)")
		return sshConnectAWSViaBastionWithLogging(p.params.BastionClient, p.params.AWSHost, p.params.KeyPath, p.params.PreferredUsername, false)
	}
}

// Put returns a connection to the pool
func (p *SSHConnectionPool) Put(conn *ssh.Client) {
	if conn == nil {
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		conn.Close()
		return
	}
	p.mu.Unlock()

	select {
	case p.connections <- conn:
		// Successfully returned to pool
	default:
		// Pool is full, close the connection
		conn.Close()
	}
}

// Close closes all connections in the pool
func (p *SSHConnectionPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	close(p.connections)
	for conn := range p.connections {
		if conn != nil {
			conn.Close()
		}
	}
}

// LogCollectionConfig holds configuration for Kubernetes log collection
type LogCollectionConfig struct {
	LogFileName string   `yaml:"logFileName"`
	UserID      string   `yaml:"userID"`
	Namespaces  []string `yaml:"namespaces"`
}

// PodLogSource defines a source for collecting logs from Kubernetes pods
type PodLogSource struct {
	Namespace  string `yaml:"namespace"`
	PodPrefix  string `yaml:"podPrefix"`
	LogPath    string `yaml:"logPath"`
	OutputName string `yaml:"outputName"`
}

// PodFileCollection defines configuration for collecting specific files from inside pods
type PodFileCollection struct {
	Namespace    string   `yaml:"namespace"`    // Kubernetes namespace
	PodPrefix    string   `yaml:"podPrefix"`    // Pod name prefix to match
	LogPath      string   `yaml:"logPath"`      // Path inside the pod (e.g., /var/log/configuration/)
	FilePatterns []string `yaml:"filePatterns"` // File patterns to collect (e.g., *.log, server.log)
	MatchPodName bool     `yaml:"matchPodName"` // Only collect files where filename starts with pod name
}

// ZtfOnboardWorkflowConfig configures collection of ZTF (Zero-Touch onboarding) Temporal workflows,
// whose workflow IDs follow the shape: ztf-onboard-<serial>-<ownerID>-<hash>, e.g.
// "ztf-onboard-JA142040G-00471-1029-2817cb" (serial "JA142040G-00471", ownerID "1029").
// Note the serial number itself may contain dashes, so it cannot be split out positionally —
// matching is done via a "-<serial>-" substring search instead (see filterWorkflowIDsBySerialNumbers).
type ZtfOnboardWorkflowConfig struct {
	Enabled           bool   `yaml:"enabled"`           // Enable ZTF onboarding workflow collection
	WorkflowIdPrefix  string `yaml:"workflowIdPrefix"`  // Workflow ID prefix (default: "ztf-onboard-")
	SerialNumbers     string `yaml:"serialNumbers"`     // Comma-separated device serial numbers to filter to; empty = use numberOfWorkflows fallback
	NumberOfWorkflows int    `yaml:"numberOfWorkflows"` // Used only when serialNumbers is empty — collects the most recent N matching workflows (1-20)
	Namespace         string `yaml:"namespace"`         // Temporal namespace (default: configuration)
	KubeNamespace     string `yaml:"kubeNamespace"`     // Kubernetes namespace hosting the temporal-admintools pod (default: common)
	FilterByOwnerID   bool   `yaml:"filterByOwnerID"`   // Filter workflows by resolved ownerID via temporal --query 'OwnerId="..."'
	OwnerID           string `yaml:"-"`                 // Resolved ownerID injected at runtime (not from yaml)
}

// Default log collection sources matching the shell script
var defaultLogSources = []PodLogSource{
	// NVO namespace logs
	{Namespace: "nvo", PodPrefix: "nvo-edge", LogPath: "/data/log/nvo-edge", OutputName: "server.log"},
	{Namespace: "nvo", PodPrefix: "nvo-network", LogPath: "/data/log/nvo-network", OutputName: "server.log"},
	{Namespace: "nvo", PodPrefix: "nvo-system", LogPath: "/data/log/nvo-system", OutputName: "server.log"},
	// Environment namespace logs
	{Namespace: "{environment}", PodPrefix: "hacr", LogPath: "/opt/hacr/logs", OutputName: "hac.log"},
	{Namespace: "{environment}", PodPrefix: "teconfig", LogPath: "/opt/tomcat/logs", OutputName: "hm.log"},
	// Common namespace logs
	{Namespace: "common", PodPrefix: "hacr", LogPath: "/opt/hacr/logs", OutputName: "hac.log"},
}

// Default pod file collections (controlled by defaultEP1Logs setting)
var defaultPodFileCollections = []PodFileCollection{
	// Common namespace - CS Configuration service
	{
		Namespace:    "common",
		PodPrefix:    "cs-configuration",
		LogPath:      "/var/log/configuration/",
		FilePatterns: []string{"*.log"},
		MatchPodName: true,
	},
	// NVO namespace - Orchestration Wired service
	{
		Namespace:    "nvo",
		PodPrefix:    "nvo-orchestration-wired",
		LogPath:      "/data/log/nvo-orchestration-wired/",
		FilePatterns: []string{"*.log"},
		MatchPodName: true,
	},
	// NVO namespace - Orchestration Wireless service
	{
		Namespace:    "nvo",
		PodPrefix:    "nvo-orchestration-wireless",
		LogPath:      "/data/log/nvo-orchestration-wireless/",
		FilePatterns: []string{"*.log"},
		MatchPodName: true,
	},
}

// Default EXOS device diagnostic commands
func getDefaultExosCommands() []DeviceCommand {
	return []DeviceCommand{
		{Name: "version", Command: "show version", Description: "Software version and hardware info"},
		{Name: "switch", Command: "show switch", Description: "Switch status and uptime"},
		{Name: "inlets", Command: "show inlets", Description: "Inlets status"},
		{Name: "iqagent", Command: "show iqagent", Description: "IQ Agent status"},
		{Name: "telemetry", Command: "show telemetry", Description: "Telemetry configuration"},
		{Name: "vlan", Command: "show vlan", Description: "VLAN configuration"},
		{Name: "running_config", Command: "show configuration", Description: "Running configuration"},
		{Name: "openapi_logs", Command: "show openapi-server logs", Description: "OpenAPI server logs"},
	}
}

// Default VOSS device diagnostic commands
func getDefaultVossCommands() []DeviceCommand {
	return []DeviceCommand{
		{Name: "software", Command: "show software", Description: "Software version information"},
		{Name: "sys_info", Command: "show sys-info", Description: "System information and uptime"},
		{Name: "vlan", Command: "show vlan basic", Description: "VLAN configuration"},
		{Name: "iqagent", Command: "show application iqagent", Description: "IQ Agent status"},
		{Name: "inlets", Command: "show application inlets", Description: "Inlets status"},
		{Name: "running_config", Command: "show running-config", Description: "Running configuration"},
	}
}

// SystemInfoCommand represents a command to collect general system information
type SystemInfoCommand struct {
	Name        string `yaml:"name"`
	Command     string `yaml:"command"`
	Description string `yaml:"description"`
}

// AppVersionNamespace represents a namespace configuration for app version collection
type AppVersionNamespace struct {
	Namespace   string   `yaml:"namespace"`
	Description string   `yaml:"description"`
	PodPrefixes []string `yaml:"podPrefixes"`
}

// JiraConfig represents JIRA integration configuration
type JiraConfig struct {
	Email             string `yaml:"email"`
	ApiToken          string `yaml:"apiToken"`
	AttachmentEnabled bool   `yaml:"attachmentEnabled"`
	BaseURL           string `yaml:"baseUrl"`
}

// DeviceCommand represents a CLI command to execute on a network device
type DeviceCommand struct {
	Name        string `yaml:"name"`
	Command     string `yaml:"command"`
	Description string `yaml:"description"`
}

// DeviceCLISettings contains CLI-related settings for device communication
type DeviceCLISettings struct {
	CommandTimeout int `yaml:"commandTimeout"` // Timeout per command in seconds
	CommandDelay   int `yaml:"commandDelay"`   // Delay between commands in seconds
}

// ExosDefaultConfig contains default settings for EXOS devices
type ExosDefaultConfig struct {
	// All EXOS defaults are now hardcoded in the functions
	// No config needed - kept for backward compatibility
}

// VossDefaultConfig contains default settings for VOSS devices
type VossDefaultConfig struct {
	// All VOSS defaults are now hardcoded in the functions
	// No config needed - kept for backward compatibility
}

// DeviceDiagnosticConfig controls diagnostic command collection for a device
type DeviceDiagnosticConfig struct {
	Enabled            bool            `yaml:"enabled"`
	UseDefaults        bool            `yaml:"useDefaults"`
	AdditionalCommands []DeviceCommand `yaml:"additionalCommands"`
}

// DeviceLogConfig controls log file collection for a device
type DeviceLogConfig struct {
	Enabled              bool     `yaml:"enabled"`
	CompressionEnabled   bool     `yaml:"compressionEnabled"`
	CompressionCommand   string   `yaml:"compressionCommand"`
	CompressedFilePath   string   `yaml:"compressedFilePath"`
	FallbackFiles        []string `yaml:"fallbackFiles"`
	RemoveCompressedFile bool     `yaml:"removeCompressedFile"`
	RemoveOldLogs        bool     `yaml:"removeOldLogs"`
	OldLogsPattern       string   `yaml:"oldLogsPattern"`
}

// NetworkDevice represents a network switch or device for log collection
type NetworkDevice struct {
	Name        string                 `yaml:"name"`
	Type        string                 `yaml:"type"` // "exos" or "voss"
	Enabled     bool                   `yaml:"enabled"`
	IPAddress   string                 `yaml:"ipAddress"`
	Port        int                    `yaml:"port"`
	Username    string                 `yaml:"username"`
	Password    string                 `yaml:"password"`
	Diagnostics DeviceDiagnosticConfig `yaml:"diagnostics"`
	Logs        DeviceLogConfig        `yaml:"logs"`
}

// DetectedDevice represents a device discovered from the database (hm_device table)
type DetectedDevice struct {
	SerialNumber       string // serial_number column
	ConfiguredHostName string // configured_host_name column
	DeviceFamily       string // device_family column (e.g., EXOS, VOSS)
	SoftwareVersion    string // software_version column
	AgentVersion       string // agent_version column
	IPAddress          string // ip_address column
	IsConnected        string // is_connected column
	InletsCapable      string // inlets_capable column
	SimType            string // sim_type column
}

// DeviceLogCollection contains configuration for network device log collection
type DeviceLogCollection struct {
	Enabled            bool              `yaml:"enabled"`
	OutputDir          string            `yaml:"outputDir"`
	ParallelDownloads  bool              `yaml:"parallelDownloads"`
	GlobalTimeout      int               `yaml:"globalTimeout"`
	CLISettings        DeviceCLISettings `yaml:"cliSettings"`
	ExosDefaults       ExosDefaultConfig `yaml:"exosDefaults"`
	VossDefaults       VossDefaultConfig `yaml:"vossDefaults"`
	DefaultNosLogFiles struct {
		Enabled bool `yaml:"enabled"` // When true, use hardcoded NOS log file paths (EXOS/VOSS) instead of config.yaml
	} `yaml:"defaultNosLogFiles"`
	Devices []NetworkDevice `yaml:"devices"`
}

// DatabaseQuery represents a single SQL query to execute
type DatabaseQuery struct {
	Name       string   `yaml:"name"`       // Query name
	SQL        string   `yaml:"sql"`        // SQL query with parameter placeholders like {param_name}
	Parameters []string `yaml:"parameters"` // Column names to extract FROM results and add TO global parameters
}

// DatabaseConfig represents configuration for a single database
type DatabaseConfig struct {
	Name    string          `yaml:"name"`    // Database name
	Alias   string          `yaml:"alias"`   // psql alias to use
	Enabled bool            `yaml:"enabled"` // Enable/disable this database
	Queries []DatabaseQuery `yaml:"queries"` // List of queries to execute
}

// QueryResultGroup represents results from a single parameter-value execution
type QueryResultGroup struct {
	ParamName  string     // Parameter name that was iterated (empty if single execution)
	ParamValue string     // Parameter value used for this execution
	Rows       [][]string // Result rows (first row is header)
}

// DatabaseCollection contains configuration for database query collection
type DatabaseCollection struct {
	Enabled      bool              `yaml:"enabled"`      // Master enable/disable
	OutputDir    string            `yaml:"outputDir"`    // Output directory for query results
	Parameters   map[string]string `yaml:"parameters"`   // Global parameters (serial_number, owner_id)
	QueryTimeout int               `yaml:"queryTimeout"` // Query timeout in seconds
	Aliases      map[string]string `yaml:"aliases"`      // Database aliases map
	Databases    []DatabaseConfig  `yaml:"databases"`    // List of databases with queries
}

// Configuration structure for the application
type Config struct {
	Username         string `yaml:"username"`     // Global username for all connections
	Environment      string `yaml:"environment"`  // Environment identifier (e.g., dl1r1, g2r1)
	EnvLoginID       string `yaml:"env_login_id"` // Login email for automatic owner ID resolution from accountdb
	OwnerID          string `yaml:"ownerID"`      // Tenant/Owner ID — used by dynamic device detection, database queries, and message filters
	archiveTimestamp string // Runtime-only: timestamp extracted from archive name (not persisted in YAML)
	Bastion          struct {
		Host     string `yaml:"host"`
		Port     int    `yaml:"port"`
		Password string `yaml:"password"`
	} `yaml:"bastion"`
	AWS struct {
		Host    string `yaml:"host"`
		KeyPath string `yaml:"keyPath"`
	} `yaml:"aws"`
	Logs struct {
		Pattern   string `yaml:"pattern"`
		OutputDir string `yaml:"outputDir"`
	} `yaml:"logs"`
	Options struct {
		AutoRetry      bool   `yaml:"autoRetry"`
		NumChunks      int    `yaml:"numChunks"`
		LogLevel       string `yaml:"logLevel"`       // Changed from debugMode to logLevel
		MaxSSHSessions int    `yaml:"maxSSHSessions"` // Maximum concurrent SSH sessions per source
		DownloadMethod string `yaml:"downloadMethod"` // Download method: "scp" or "sftp"
	} `yaml:"options"`
	// New section for log collection
	LogCollection struct {
		Enabled                bool `yaml:"enabled"`
		DefaultEP1Logs         bool `yaml:"defaultEP1Logs"` // Enable/disable default EP1 log collection (kubectl logs + temporal) (default: true)
		DynamicDeviceDetection struct {
			Enabled    bool `yaml:"enabled"`    // When true, detect devices from configdb_1 during AWS phase
			MaxDevices int  `yaml:"maxDevices"` // Maximum number of devices to use from DB (default: 3)
		} `yaml:"dynamicDeviceDetection"`
		LogFileName         string         `yaml:"logFileName"`
		CustomSources       []PodLogSource `yaml:"customSources"`
		TempDir             string         `yaml:"tempDir"`
		UseTimestamp        bool           `yaml:"useTimestamp"`
		DeleteAfterCopy     bool           `yaml:"deleteAfterCopy"`
		AutoDeleteTempDir   bool           `yaml:"autoDeleteTempDir"`
		TimestampFormat     string         `yaml:"timestampFormat"`
		TimeBasedCollection struct {
			Enabled  bool   `yaml:"enabled"`  // Enable time-based collection
			Duration string `yaml:"duration"` // Duration like "15m", "1h", "30m"
		} `yaml:"timeBasedCollection"`
		TemporalWorkflowCollection struct {
			Enabled              bool                `yaml:"enabled"`              // Enable temporal workflow data collection
			WorkflowIdPrefix     string              `yaml:"workflowIdPrefix"`     // Filter by workflow ID prefix
			NumberOfWorkflows    int                 `yaml:"numberOfWorkflows"`    // Number of workflows to collect (1-20)
			Namespace            string              `yaml:"namespace"`            // Temporal namespace (default: configuration)
			KubeNamespace        string              `yaml:"kubeNamespace"`        // Kubernetes namespace hosting the temporal-admintools pod (default: common)
			FilterByOwnerID      bool                `yaml:"filterByOwnerID"`      // Filter workflows by resolved ownerID via temporal --query 'OwnerId="..."'
			WorkflowIdKeyword    string              `yaml:"workflowIdKeyword"`    // Only keep workflow IDs containing this substring (e.g. "batch"); empty = no filtering
			WorkflowActivitySets map[string][]string `yaml:"workflowActivitySets"` // Per-workflow-type activity lists keyed by workflow ID prefix (e.g. "deploy-site")
			OwnerID              string              `yaml:"-"`                    // Resolved ownerID injected at runtime (not from yaml)
		} `yaml:"temporalWorkflowCollection"`
		TemporalScheduleCollection struct {
			Enabled           bool   `yaml:"enabled"`           // Enable temporal schedule data collection
			NumberOfSchedules int    `yaml:"numberOfSchedules"` // Number of schedules to collect (1-20)
			Namespace         string `yaml:"namespace"`         // Temporal namespace (default: configuration)
		} `yaml:"temporalScheduleCollection"`
		ZtfOnboardWorkflowCollection ZtfOnboardWorkflowConfig `yaml:"ztfOnboardWorkflowCollection"` // ZTF onboarding workflow collection (ztf-onboard-<serial>-<ownerID>-<hash>)
		LogAnalysis                  struct {
			Enabled         bool     `yaml:"enabled"`         // Enable automatic log analysis
			OutputFile      string   `yaml:"outputFile"`      // Output file name for analysis report
			ErrorPatterns   []string `yaml:"errorPatterns"`   // Patterns to search for (case-insensitive)
			ExcludeKeywords []string `yaml:"excludeKeywords"` // Keywords to exclude from matches (case-insensitive)
			MaxMatches      int      `yaml:"maxMatches"`      // Max matches per log file
			ContextLines    int      `yaml:"contextLines"`    // Lines before/after each match
			CorrelationKeys []struct {
				Pattern string `yaml:"pattern"` // Regex pattern to match correlation IDs
				Type    string `yaml:"type"`    // Type name (transaction, request, correlation, trace)
			} `yaml:"correlationKeys"` // Extract correlation IDs for cross-file grouping
			TimestampPatterns []string `yaml:"timestampPatterns"` // Timestamp formats for chronological correlation
			ErrorGroups       []struct {
				Name     string   `yaml:"name"`     // Group name
				Patterns []string `yaml:"patterns"` // Patterns belonging to this group
				Severity string   `yaml:"severity"` // Severity for this group
			} `yaml:"errorGroups"` // Semantic error grouping
			TemporalAnalysis TemporalAnalysisConfig `yaml:"temporalAnalysis"` // Temporal activity output status validation
		} `yaml:"logAnalysis"`
		MessageFilter struct {
			Enabled              bool `yaml:"enabled"`              // Enable/disable post-download message filtering
			FilterDuringDownload bool `yaml:"filterDuringDownload"` // Apply simple grep filters during kubectl logs download (faster, less bandwidth)
			KeyValueFilters      []struct {
				Key   string `yaml:"key"`   // Key to look for in log lines (e.g., ownerID, serial)
				Value string `yaml:"value"` // Value to match (lines with key but different value are excluded)
			} `yaml:"keyValueFilters"` // Keep lines where key is absent OR key has specified value
			SpecificStrings  []string `yaml:"specificStrings"`  // Only keep lines containing these exact strings
			CombineReplicas  bool     `yaml:"combineReplicas"`  // Merge replica pod logs into single file per service
			ReplicaPattern   string   `yaml:"replicaPattern"`   // Regex pattern to detect replica suffix
			SortByTimestamp  bool     `yaml:"sortByTimestamp"`  // Sort combined logs chronologically
			TimestampPattern string   `yaml:"timestampPattern"` // Regex to extract timestamp from log lines
		} `yaml:"messageFilter"`
		PodFileCollection struct {
			Enabled     bool                `yaml:"enabled"`     // Enable/disable pod file collection
			Collections []PodFileCollection `yaml:"collections"` // List of pod file collection configurations
		} `yaml:"podFileCollection"`
	} `yaml:"logCollection"`
	// System information collection
	SystemInfo struct {
		Enabled        bool                `yaml:"enabled"`
		OutputDir      string              `yaml:"outputDir"`
		CommandTimeout int                 `yaml:"commandTimeout"` // Timeout per command in seconds (60-300)
		Commands       []SystemInfoCommand `yaml:"commands"`
	} `yaml:"systemInfo"`
	// Application version collection
	AppVersionCollection struct {
		Enabled        bool                  `yaml:"enabled"`
		OutputFileName string                `yaml:"outputFileName"`
		PrintToLog     bool                  `yaml:"printToLog"`
		Namespaces     []AppVersionNamespace `yaml:"namespaces"`
	} `yaml:"appVersionCollection"`
	// JIRA integration for attaching files
	Jira JiraConfig `yaml:"jira"`
	// Network device log collection
	DeviceLogCollection DeviceLogCollection `yaml:"deviceLogCollection"`
	// Database query collection
	DatabaseCollection DatabaseCollection `yaml:"databaseCollection"`
}

// LoadConfig loads the configuration from a YAML file
func LoadConfig(configPath string) (*Config, error) {
	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return &Config{}, nil // Return empty config if file doesn't exist
	}

	// Read the config file
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	// Normalize line endings (strip \r from Windows CRLF) and remove BOM for cross-platform robustness
	text := string(data)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.TrimPrefix(text, "\xEF\xBB\xBF") // UTF-8 BOM
	data = []byte(text)

	// Parse the YAML config
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("error parsing YAML config file: %v", err)
	}

	// Set default for MaxSSHSessions if not configured
	if config.Options.MaxSSHSessions <= 0 {
		config.Options.MaxSSHSessions = 1 // Conservative default
	}

	// Apply {username} and {environment} template replacement to env_login_id
	if config.EnvLoginID != "" {
		config.EnvLoginID = strings.ReplaceAll(config.EnvLoginID, "{username}", config.Username)
		config.EnvLoginID = strings.ReplaceAll(config.EnvLoginID, "{environment}", config.Environment)
	}

	// Propagate top-level ownerID into databaseCollection.parameters so SQL {owner_id} substitution works
	if config.OwnerID != "" {
		if config.DatabaseCollection.Parameters == nil {
			config.DatabaseCollection.Parameters = make(map[string]string)
		}
		// Only seed if not already set in parameters (allow explicit override)
		if config.DatabaseCollection.Parameters["owner_id"] == "" {
			config.DatabaseCollection.Parameters["owner_id"] = config.OwnerID
		}
		// Auto-fill messageFilter ownerID value if empty
		for i := range config.LogCollection.MessageFilter.KeyValueFilters {
			kv := &config.LogCollection.MessageFilter.KeyValueFilters[i]
			if strings.EqualFold(kv.Key, "ownerID") && kv.Value == "" {
				kv.Value = config.OwnerID
			}
		}
	}

	return &config, nil
}

// printCollectionSummary displays what will be collected before starting connections
func printCollectionSummary(mode string, collectLogs, collectInfo, collectAppVersions, collectDeviceLogs, collectDatabase, listOnly bool, config *Config, logger *Logger) bool {
	logger.Info("")
	logger.Info("========================================")
	logger.Info("PRE-FLIGHT COLLECTION SUMMARY")
	logger.Info("========================================")
	logger.Info("")
	logger.Info("Operation Mode: %s", mode)
	if config.Environment != "" {
		logger.Info("Environment: %s", strings.ToUpper(config.Environment))
	}
	logger.Info("")

	if listOnly {
		logger.Info("Mode: LIST ONLY (no files will be downloaded)")
		logger.Info("")
		logger.Info("========================================")
		return true // List mode always has work to do
	}

	collectionCount := 0
	hasActualWork := false // Track if there's actual work to do

	// Kubernetes Logs
	if collectLogs {
		collectionCount++
		logger.Info("[%d] Kubernetes Log Collection: ENABLED", collectionCount)

		// Check if there's actually anything to collect
		hasKubectlLogs := (config.LogCollection.DefaultEP1Logs || len(config.LogCollection.CustomSources) > 0)
		if hasKubectlLogs {
			hasActualWork = true
		}
		if config.LogCollection.TimeBasedCollection.Enabled && config.LogCollection.TimeBasedCollection.Duration != "" {
			logger.Info("    - Time-based collection: Last %s", config.LogCollection.TimeBasedCollection.Duration)
		} else {
			logger.Info("    - Time-based collection: Full logs")
		}
		if config.LogCollection.MessageFilter.Enabled {
			filterMode := "post-download"
			if config.LogCollection.MessageFilter.FilterDuringDownload {
				filterMode = "during-download"
			}
			logger.Info("    - Message filtering: ENABLED (%s)", filterMode)
			if config.LogCollection.MessageFilter.CombineReplicas {
				logger.Info("    - Replica log merging: ENABLED")
			}
		}
		// Temporal collections are controlled by defaultEP1Logs setting
		if config.LogCollection.DefaultEP1Logs && config.LogCollection.TemporalWorkflowCollection.Enabled {
			logger.Info("    - Temporal workflows: %d workflows (prefix: %s)",
				config.LogCollection.TemporalWorkflowCollection.NumberOfWorkflows,
				config.LogCollection.TemporalWorkflowCollection.WorkflowIdPrefix)
		}
		if config.LogCollection.DefaultEP1Logs && config.LogCollection.TemporalScheduleCollection.Enabled {
			logger.Info("    - Temporal schedules: %d schedules",
				config.LogCollection.TemporalScheduleCollection.NumberOfSchedules)
		}
		if config.LogCollection.DefaultEP1Logs && config.LogCollection.ZtfOnboardWorkflowCollection.Enabled {
			if config.LogCollection.ZtfOnboardWorkflowCollection.SerialNumbers != "" {
				logger.Info("    - ZTF onboarding workflows: serial number(s) %s",
					config.LogCollection.ZtfOnboardWorkflowCollection.SerialNumbers)
			} else {
				logger.Info("    - ZTF onboarding workflows: most recent %d workflow(s)",
					config.LogCollection.ZtfOnboardWorkflowCollection.NumberOfWorkflows)
			}
		}
		// Pod file collection - show defaults when defaultEP1Logs=true
		if config.LogCollection.PodFileCollection.Enabled {
			totalCollections := 0
			if config.LogCollection.DefaultEP1Logs {
				totalCollections += len(defaultPodFileCollections)
			}
			totalCollections += len(config.LogCollection.PodFileCollection.Collections)

			if totalCollections > 0 {
				logger.Info("    - Pod file collection: %d collection(s)", totalCollections)
				if config.LogCollection.DefaultEP1Logs && len(defaultPodFileCollections) > 0 {
					logger.Info("      * Built-in: cs-configuration, nvo-orchestration-wired, nvo-orchestration-wireless")
				}
				for _, podCol := range config.LogCollection.PodFileCollection.Collections {
					logger.Info("      * Custom: %s/%s (%d pattern(s))", podCol.Namespace, podCol.PodPrefix, len(podCol.FilePatterns))
				}
			}
		}
		if config.LogCollection.LogAnalysis.Enabled {
			logger.Info("    - Log analysis: ENABLED (%d error patterns)", len(config.LogCollection.LogAnalysis.ErrorPatterns))
		}
		logger.Info("")
	}

	// General System Information
	if collectInfo {
		collectionCount++
		logger.Info("[%d] System Information Collection: ENABLED", collectionCount)
		if len(config.SystemInfo.Commands) > 0 {
			hasActualWork = true
			logger.Info("    - Commands configured: %d", len(config.SystemInfo.Commands))
			for _, cmd := range config.SystemInfo.Commands {
				logger.Info("      * %s", cmd.Name)
			}
		} else {
			logger.Info("    - No commands configured in config.yaml")
		}
		logger.Info("")
	}

	// Application Version Collection
	if collectAppVersions {
		collectionCount++
		logger.Info("[%d] Application Version Collection: ENABLED", collectionCount)
		if len(config.AppVersionCollection.Namespaces) > 0 {
			hasActualWork = true
			logger.Info("    - Namespaces to check: %d", len(config.AppVersionCollection.Namespaces))
			for _, ns := range config.AppVersionCollection.Namespaces {
				logger.Info("      * %s: %d pod prefix(es)", ns.Namespace, len(ns.PodPrefixes))
			}
		} else {
			logger.Info("    - No namespaces configured in config.yaml")
		}
		logger.Info("")
	}

	// Network Device Log Collection
	if collectDeviceLogs {
		collectionCount++
		if config.DeviceLogCollection.Enabled {
			enabledDevices := 0
			for _, dev := range config.DeviceLogCollection.Devices {
				if dev.Enabled {
					enabledDevices++
					hasActualWork = true
				}
			}
			logger.Info("[%d] Network Device Log Collection: ENABLED", collectionCount)
			logger.Info("    - Enabled devices: %d of %d", enabledDevices, len(config.DeviceLogCollection.Devices))
			if config.DeviceLogCollection.ParallelDownloads {
				logger.Info("    - Parallel downloads: ENABLED")
			}
			for _, dev := range config.DeviceLogCollection.Devices {
				if dev.Enabled {
					logger.Info("      * %s (%s) - %s:%d", dev.Name, dev.Type, dev.IPAddress, dev.Port)
				}
			}
		} else {
			logger.Info("[%d] Network Device Log Collection: DISABLED in config", collectionCount)
		}
		logger.Info("")
	}

	// Database Query Collection
	if collectDatabase {
		collectionCount++
		if config.DatabaseCollection.Enabled {
			enabledDBs := 0
			for _, db := range config.DatabaseCollection.Databases {
				if db.Enabled {
					enabledDBs++
					hasActualWork = true
				}
			}
			logger.Info("[%d] Database Query Collection: ENABLED", collectionCount)
			logger.Info("    - Enabled databases: %d of %d", enabledDBs, len(config.DatabaseCollection.Databases))

			// Show global parameters
			if len(config.DatabaseCollection.Parameters) > 0 {
				logger.Info("    - Global parameters:")
				for key, value := range config.DatabaseCollection.Parameters {
					if value != "" {
						logger.Info("      * %s = %s", key, value)
					}
				}
			}

			// Show enabled databases with query counts
			for _, db := range config.DatabaseCollection.Databases {
				if db.Enabled {
					logger.Info("      * %s (%s): %d queries", db.Name, db.Alias, len(db.Queries))
				}
			}
		} else {
			logger.Info("[%d] Database Query Collection: DISABLED in config", collectionCount)
		}
		logger.Info("")
	}

	if collectionCount == 0 {
		logger.Warn("No collections enabled!")
		logger.Info("")
	} else {
		logger.Info("Total collections to perform: %d", collectionCount)
		logger.Info("")
	}

	if !hasActualWork {
		logger.Info("========================================")
		logger.Warn("WARNING: No actual work to do!")
		logger.Info("")
		logger.Info("Collections are enabled but have no configured items:")
		if collectLogs && !config.LogCollection.DefaultEP1Logs && len(config.LogCollection.CustomSources) == 0 {
			logger.Info("  - Kubernetes logs: defaultEP1Logs=false and no custom sources")
		}
		if collectInfo && len(config.SystemInfo.Commands) == 0 {
			logger.Info("  - System info: no commands configured")
		}
		if collectAppVersions && len(config.AppVersionCollection.Namespaces) == 0 {
			logger.Info("  - App versions: no namespaces configured")
		}
		if collectDeviceLogs && (!config.DeviceLogCollection.Enabled || !hasEnabledDevices(config.DeviceLogCollection.Devices)) {
			logger.Info("  - Device logs: no enabled devices")
		}
		if collectDatabase && (!config.DatabaseCollection.Enabled || !hasEnabledDatabases(config.DatabaseCollection.Databases)) {
			logger.Info("  - Database: no enabled databases")
		}
		logger.Info("")
		logger.Info("Please enable at least one feature with actual configuration.")
		logger.Info("========================================")
		logger.Info("")
		return false
	}

	logger.Info("========================================")
	logger.Info("Starting connection process...")
	logger.Info("========================================")
	logger.Info("")
	return true
}

// Helper function to check if there are any enabled devices
func hasEnabledDevices(devices []NetworkDevice) bool {
	for _, dev := range devices {
		if dev.Enabled {
			return true
		}
	}
	return false
}

// Helper function to check if there are any enabled databases
func hasEnabledDatabases(databases []DatabaseConfig) bool {
	for _, db := range databases {
		if db.Enabled {
			return true
		}
	}
	return false
}

func sshConnectBastion(username, password, host string, port int) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second, // Increased timeout for initial connection
		// SSH performance optimizations
		Config: ssh.Config{
			Ciphers: []string{
				"aes128-ctr", "aes192-ctr", "aes256-ctr", // Fast CTR mode ciphers
				"aes128-gcm@openssh.com", "aes256-gcm@openssh.com", // GCM modes for performance
			},
			MACs: []string{
				"hmac-sha2-256-etm@openssh.com", // Fast MAC algorithms
				"hmac-sha2-256",
				"hmac-sha1",
			},
		},
	} // Format address properly for both IPv4 and IPv6
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	// Establish TCP connection first for optimization
	tcpConn, err := net.DialTimeout("tcp", addr, config.Timeout)
	if err != nil {
		return nil, err
	}

	// Optimize TCP connection
	if tcp, ok := tcpConn.(*net.TCPConn); ok {
		tcp.SetKeepAlive(true)
		tcp.SetKeepAlivePeriod(30 * time.Second)
		tcp.SetNoDelay(true) // Disable Nagle's algorithm for lower latency
	}

	// Create SSH connection over optimized TCP connection
	clientConn, chans, reqs, err := ssh.NewClientConn(tcpConn, addr, config)
	if err != nil {
		tcpConn.Close()
		return nil, err
	}

	client := ssh.NewClient(clientConn, chans, reqs)
	return client, nil
}

// Connect to AWS server from bastion using .ssh key
func sshConnectAWSViaBastion(bastionClient *ssh.Client, awsHost, keyPath string, preferredUsername string) (*ssh.Client, error) {
	return sshConnectAWSViaBastionWithLogging(bastionClient, awsHost, keyPath, preferredUsername, true)
}

// Connect to AWS server from bastion using .ssh key with optional verbose logging
func sshConnectAWSViaBastionWithLogging(bastionClient *ssh.Client, awsHost, keyPath string, preferredUsername string, verboseLogging bool) (*ssh.Client, error) {
	// Read private key from bastion
	sess, err := bastionClient.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session on bastion: %v", err)
	}
	defer sess.Close()

	if verboseLogging {
		logger.Info("Reading key from %s...", keyPath)
	} else {
		logger.Debug("Reading key from %s...", keyPath)
	}
	keyBytes, err := sess.Output("cat " + keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file from bastion at %s: %v", keyPath, err)
	}

	// Try to parse the key; if passphrase-protected, prompt the user
	key, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		// Detect passphrase-protected keys (covers both x/crypto error strings)
		errMsg := err.Error()
		if strings.Contains(errMsg, "passphrase") || strings.Contains(errMsg, "cannot decode encrypted private keys") {
			if verboseLogging {
				logger.Info("SSH key %s is passphrase-protected, prompting for passphrase...", keyPath)
			}
			passphrase, promptErr := promptPassword(fmt.Sprintf("Enter passphrase for key %s: ", keyPath))
			if promptErr != nil {
				return nil, fmt.Errorf("failed to read passphrase: %v", promptErr)
			}
			key, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(passphrase))
			if err != nil {
				return nil, fmt.Errorf("failed to parse private key with passphrase: %v", err)
			}
		} else {
			// Print the first few characters of the key for debugging
			keyPreview := string(keyBytes)
			if len(keyPreview) > 100 {
				keyPreview = keyPreview[:100] + "..."
			}
			return nil, fmt.Errorf("failed to parse private key: %v\nKey begins with: %s", err, keyPreview)
		}
	}

	// Setup users to try
	users := []string{"ec2-user", "ubuntu", "centos", "admin", "root"}

	// Add the current bastion username as a possibility
	currentUser, err := sess.Output("whoami")
	if err == nil {
		users = append([]string{strings.TrimSpace(string(currentUser))}, users...)
	}

	// If preferred username is provided, try it first
	if preferredUsername != "" {
		users = append([]string{preferredUsername}, users...)
	}

	var lastError error
	for _, user := range users {
		if verboseLogging {
			logger.Debug("Trying to connect as user '%s'...", user)
		} else {
			logger.Debug("Trying to connect as user '%s'...", user)
		}

		config := &ssh.ClientConfig{
			User:            user,
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         8 * time.Second, // Optimized timeout for faster connection failures
			// SSH performance optimizations
			Config: ssh.Config{
				Ciphers: []string{
					"aes128-ctr", "aes192-ctr", "aes256-ctr", // Fast CTR mode ciphers
					"aes128-gcm@openssh.com", "aes256-gcm@openssh.com", // GCM modes for performance
				},
				MACs: []string{
					"hmac-sha2-256-etm@openssh.com", // Fast MAC algorithms
					"hmac-sha2-256",
					"hmac-sha1",
				},
			},
		}

		// Create a tunnel from bastion to AWS host
		conn, err := bastionClient.Dial("tcp", awsHost+":22")
		if err != nil {
			return nil, fmt.Errorf("failed to establish connection to %s:22: %v", awsHost, err)
		}

		// Optimize the tunneled connection if it's TCP
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.SetKeepAlive(true)
			tcpConn.SetKeepAlivePeriod(30 * time.Second)
			tcpConn.SetNoDelay(true) // Disable Nagle's algorithm for lower latency
		}

		clientConn, chans, reqs, err := ssh.NewClientConn(conn, awsHost+":22", config)
		if err != nil {
			lastError = err
			logger.Debug("Authentication failed for user '%s': %v", user, err)
			conn.Close() // Close the connection before trying the next user
			continue
		}

		if verboseLogging {
			logger.Debug("Successfully connected as '%s'", user)
		} else {
			logger.Debug("Successfully connected as '%s'", user)
		}

		client := ssh.NewClient(clientConn, chans, reqs)
		return client, nil
	}

	return nil, fmt.Errorf("failed to authenticate with any user: %v", lastError)
}

// collectKubernetesLogs executes the log collection process remotely on AWS
func collectKubernetesLogs(awsClient *ssh.Client, logFileName, userID, tempDir string, customSources []PodLogSource, useTimestamp bool, timestampFormat, environment, username string, collectInfo bool, systemInfoConfig struct {
	Enabled        bool                `yaml:"enabled"`
	OutputDir      string              `yaml:"outputDir"`
	CommandTimeout int                 `yaml:"commandTimeout"`
	Commands       []SystemInfoCommand `yaml:"commands"`
}, timeBasedEnabled bool, timeDurationStr string, maxSSHSessions int, autoDeleteTempDir bool, defaultEP1Logs bool, messageFilterConfig struct {
	Enabled              bool `yaml:"enabled"`
	FilterDuringDownload bool `yaml:"filterDuringDownload"`
	KeyValueFilters      []struct {
		Key   string `yaml:"key"`
		Value string `yaml:"value"`
	} `yaml:"keyValueFilters"`
	SpecificStrings []string `yaml:"specificStrings"`
}, temporalConfig struct {
	Enabled              bool                `yaml:"enabled"`
	WorkflowIdPrefix     string              `yaml:"workflowIdPrefix"`
	NumberOfWorkflows    int                 `yaml:"numberOfWorkflows"`
	Namespace            string              `yaml:"namespace"`
	KubeNamespace        string              `yaml:"kubeNamespace"`
	FilterByOwnerID      bool                `yaml:"filterByOwnerID"`
	WorkflowIdKeyword    string              `yaml:"workflowIdKeyword"`
	WorkflowActivitySets map[string][]string `yaml:"workflowActivitySets"`
	OwnerID              string              `yaml:"-"`
}, temporalScheduleConfig struct {
	Enabled           bool   `yaml:"enabled"`
	NumberOfSchedules int    `yaml:"numberOfSchedules"`
	Namespace         string `yaml:"namespace"`
}, ztfOnboardConfig ZtfOnboardWorkflowConfig, podFileCollectionConfig struct {
	Enabled     bool                `yaml:"enabled"`
	Collections []PodFileCollection `yaml:"collections"`
}) (string, error) {
	// Start timing the log collection process
	logCollectionStartTime := time.Now()
	logger.Info("Starting Kubernetes log collection...")
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  LOG COLLECTION - Kubernetes pod logs")
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Debug("Received tempDir parameter: '%s'", tempDir)

	// Parse time-based collection parameters
	var sinceTime string
	if timeBasedEnabled && timeDurationStr != "" {
		duration, err := time.ParseDuration(timeDurationStr)
		if err != nil {
			return "", fmt.Errorf("invalid time duration '%s': %v", timeDurationStr, err)
		}

		// Get current time from AWS server (not local machine)
		logger.Info("Getting current time from AWS server for accurate time-based collection...")
		awsCurrentTime, err := getAWSServerTime(awsClient)
		if err != nil {
			return "", fmt.Errorf("failed to get AWS server time: %v", err)
		}

		// Calculate the time to collect logs since (using AWS server time)
		since := awsCurrentTime.Add(-duration)
		sinceTime = since.Format("2006-01-02T15:04:05Z")
		logger.Info("AWS server time: %s", awsCurrentTime.Format("2006-01-02T15:04:05Z"))
		logger.Info("Time-based collection: collecting logs since %s (last %s)", sinceTime, timeDurationStr)

		// Append time duration to filename for clarity
		if useTimestamp {
			finalLogFileName := fmt.Sprintf("%s_%s", logFileName, strings.ReplaceAll(timeDurationStr, "h", "hr"))
			logFileName = finalLogFileName
		} else {
			logFileName = fmt.Sprintf("%s_%s", logFileName, strings.ReplaceAll(timeDurationStr, "h", "hr"))
		}
	} else {
		logger.Info("Full log collection enabled - collecting complete logs from all pods")
	}

	// Quick fix: correct tempDir pattern to match config expectation
	if strings.HasSuffix(tempDir, "_temp_logs") {
		tempDir = strings.Replace(tempDir, "_temp_logs", "_templogs", 1)
		logger.Debug("Corrected tempDir to: '%s'", tempDir)
	}

	// Generate timestamped filename if requested
	finalLogFileName := logFileName
	if useTimestamp {
		if timestampFormat == "" {
			timestampFormat = "20060102_150405" // Default format: YYYYMMDD_HHMMSS
		}
		timestamp := time.Now().Format(timestampFormat)
		finalLogFileName = fmt.Sprintf("%s_%s", logFileName, timestamp)
		logger.Info("Generated timestamped filename: %s", finalLogFileName)
	}

	// Use default sources based on defaultEP1Logs setting
	var sources []PodLogSource
	if len(customSources) > 0 {
		// Custom sources always take precedence
		sources = customSources
	} else if defaultEP1Logs {
		// Use built-in default sources only when defaultEP1Logs is enabled
		sources = defaultLogSources
		logger.Debug("Using default EP1 log sources (defaultEP1Logs=true)")
	} else {
		// No sources when defaultEP1Logs is disabled and no custom sources
		sources = []PodLogSource{}
		logger.Info("Default EP1 log collection is disabled (defaultEP1Logs=false), skipping built-in kubectl logs collection")
	}

	// Detect XIQ namespace: check if 'xiq' namespace exists, otherwise use environment name
	logger.Debug("Checking for 'xiq' namespace...")
	xiqNamespace := environment
	nsSession, nsErr := awsClient.NewSession()
	if nsErr == nil {
		nsCmd := "kubectl get namespaces --no-headers | awk '{print \\$1}'"
		nsOutput, nsExecErr := nsSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", nsCmd))
		nsSession.Close()

		if nsExecErr == nil {
			availableNamespaces := strings.Split(strings.TrimSpace(string(nsOutput)), "\n")
			for _, ns := range availableNamespaces {
				if strings.TrimSpace(ns) == "xiq" {
					xiqNamespace = "xiq"
					logger.Debug("Found dedicated 'xiq' namespace - will use it for log collection")
					break
				}
			}
			if xiqNamespace != "xiq" {
				logger.Debug("No dedicated 'xiq' namespace found - will use environment namespace '%s'", environment)
			}
		} else {
			logger.Debug("Failed to check namespaces: %v", nsExecErr)
		}
	} else {
		logger.Debug("Failed to create session for namespace check: %v", nsErr)
	}

	// Apply template replacement to sources
	// For Namespace: use xiqNamespace for {environment} so that 'xiq' is used if it exists
	for i := range sources {
		sources[i].Namespace = strings.ReplaceAll(sources[i].Namespace, "{environment}", xiqNamespace)
		sources[i].Namespace = strings.ReplaceAll(sources[i].Namespace, "{username}", username)
		sources[i].PodPrefix = strings.ReplaceAll(sources[i].PodPrefix, "{environment}", environment)
		sources[i].PodPrefix = strings.ReplaceAll(sources[i].PodPrefix, "{username}", username)
		sources[i].LogPath = strings.ReplaceAll(sources[i].LogPath, "{environment}", environment)
		sources[i].LogPath = strings.ReplaceAll(sources[i].LogPath, "{username}", username)
		sources[i].OutputName = strings.ReplaceAll(sources[i].OutputName, "{environment}", environment)
		sources[i].OutputName = strings.ReplaceAll(sources[i].OutputName, "{username}", username)
	}

	// Create the log collection directory
	// Use tempDir from config, fallback to default if empty
	if tempDir == "" {
		tempDir = fmt.Sprintf("%s_%s_templogs", environment, username) // Default fallback using environment and username
	}
	logDir := fmt.Sprintf("%s/%s", tempDir, finalLogFileName)
	logger.Info("Creating log directory: %s", logDir)

	// Check if tempDir already exists on the remote server
	checkSession, err := awsClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %v", err)
	}
	output, checkErr := checkSession.CombinedOutput(fmt.Sprintf("sudo test -d %s && echo EXISTS || echo NOTEXISTS", tempDir))
	checkSession.Close()

	if checkErr == nil && strings.TrimSpace(string(output)) == "EXISTS" {
		logger.Info("Temp directory already exists: %s", tempDir)
		if autoDeleteTempDir {
			// Auto-delete the existing directory
			logger.Info("Auto-deleting existing temp directory: %s", tempDir)
			delSession, err := awsClient.NewSession()
			if err != nil {
				return "", fmt.Errorf("failed to create session for cleanup: %v", err)
			}
			if err := executeCommandAsRoot(delSession, fmt.Sprintf("rm -rf %s", tempDir)); err != nil {
				delSession.Close()
				return "", fmt.Errorf("failed to delete existing temp directory %s: %v", tempDir, err)
			}
			delSession.Close()
			logger.Info("Existing temp directory deleted successfully")
		} else {
			// Prompt user
			fmt.Printf("\nTemp directory '%s' already exists on the remote server.\n", tempDir)
			fmt.Print("Do you want to delete it and create a fresh one? (yes/no): ")
			reader := bufio.NewReader(os.Stdin)
			answer, _ := reader.ReadString('\n')
			answer = strings.TrimSpace(strings.ToLower(answer))
			if answer == "yes" || answer == "y" {
				logger.Info("User chose to delete existing temp directory")
				delSession, err := awsClient.NewSession()
				if err != nil {
					return "", fmt.Errorf("failed to create session for cleanup: %v", err)
				}
				if err := executeCommandAsRoot(delSession, fmt.Sprintf("rm -rf %s", tempDir)); err != nil {
					delSession.Close()
					return "", fmt.Errorf("failed to delete existing temp directory %s: %v", tempDir, err)
				}
				delSession.Close()
				logger.Info("Existing temp directory deleted successfully")
			} else {
				// Rename with incrementing suffix
				newTempDir := tempDir
				for suffix := 1; ; suffix++ {
					candidate := fmt.Sprintf("%s_%d", tempDir, suffix)
					chkSess, err := awsClient.NewSession()
					if err != nil {
						return "", fmt.Errorf("failed to create session: %v", err)
					}
					out, _ := chkSess.CombinedOutput(fmt.Sprintf("sudo test -d %s && echo EXISTS || echo NOTEXISTS", candidate))
					chkSess.Close()
					if strings.TrimSpace(string(out)) == "NOTEXISTS" {
						newTempDir = candidate
						break
					}
				}
				logger.Info("Using alternate temp directory: %s", newTempDir)
				tempDir = newTempDir
				logDir = fmt.Sprintf("%s/%s", tempDir, finalLogFileName)
			}
		}
	}

	session, err := awsClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %v", err)
	}
	defer session.Close()

	// First ensure the tempDir exists
	logger.Debug("Ensuring temp directory exists: %s", tempDir)
	if err := executeCommandAsRoot(session, fmt.Sprintf("mkdir -p %s", tempDir)); err != nil {
		return "", fmt.Errorf("failed to create temp directory %s: %v", tempDir, err)
	}

	// Create a new session for the full log directory structure
	session1b, err := awsClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session for log directory: %v", err)
	}
	defer session1b.Close()

	// Create the full log directory structure as root
	if err := executeCommandAsRoot(session1b, fmt.Sprintf("mkdir -p %s", logDir)); err != nil {
		return "", fmt.Errorf("failed to create log directory %s: %v", logDir, err)
	}

	// Collect logs from each source in parallel
	totalSources := len(sources)
	logger.Info("Starting parallel log collection from %d sources...", totalSources)

	// Create channels for results and wait group for synchronization
	type logResult struct {
		source PodLogSource
		index  int
		err    error
	}

	resultChan := make(chan logResult, totalSources)
	var wg sync.WaitGroup

	// Start parallel log collection
	for i, source := range sources {
		wg.Add(1)
		go func(src PodLogSource, idx int) {
			defer wg.Done()

			logger.Info("Collecting logs from %s namespace, pod prefix: %s (%d/%d)",
				src.Namespace, src.PodPrefix, idx+1, totalSources)

			// Create independent SSH session for this collection
			err := collectLogsFromSourceParallel(awsClient, src, logDir, timeBasedEnabled, sinceTime, maxSSHSessions, defaultEP1Logs, messageFilterConfig)
			resultChan <- logResult{source: src, index: idx, err: err}
		}(source, i)
	}

	// Wait for all collections to complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results
	successCount := 0
	for result := range resultChan {
		if result.err != nil {
			logger.Warn("Failed to collect logs from %s/%s: %v",
				result.source.Namespace, result.source.PodPrefix, result.err)
		} else {
			successCount++
		}
	}

	logger.Info("Log collection completed: %d/%d sources successful", successCount, totalSources)

	// Debug: Show what files were actually collected
	logger.Debug("Checking collected files in log directory...")
	debugSession, err := awsClient.NewSession()
	if err == nil {
		defer debugSession.Close()
		listCmd := fmt.Sprintf("find %s -type f -name '*.log' 2>/dev/null | head -20", logDir)
		if debugOutput, debugErr := debugSession.CombinedOutput(fmt.Sprintf("sudo su - -c '%s'", listCmd)); debugErr == nil && len(debugOutput) > 0 {
			logger.Debug("Found log files: %s", strings.TrimSpace(string(debugOutput)))
		} else {
			logger.Debug("No log files found in collected directory")
			// Also check directory structure
			dirCmd := fmt.Sprintf("find %s -type d 2>/dev/null", logDir)
			if dirOutput, dirErr := debugSession.CombinedOutput(fmt.Sprintf("sudo su - -c '%s'", dirCmd)); dirErr == nil && len(dirOutput) > 0 {
				logger.Debug("Directory structure: %s", strings.TrimSpace(string(dirOutput)))
			}
		}
	}

	// Collect system information before archiving (if enabled)
	if collectInfo && systemInfoConfig.Enabled {
		logger.Info("Starting system information collection...")
		err = collectSystemInfo(awsClient, systemInfoConfig, environment, username, tempDir, finalLogFileName)
		if err != nil {
			logger.Warn("General info collection failed: %v", err)
			// Don't return here - continue with archive creation
		}
	}

	// Collect Temporal workflow information before archiving (if enabled and defaultEP1Logs is true)
	if defaultEP1Logs && temporalConfig.Enabled {
		logger.Info("Starting Temporal workflow information collection...")
		err = collectTemporalWorkflowInfo(awsClient, temporalConfig, false, environment, username, tempDir, finalLogFileName)
		if err != nil {
			logger.Warn("Temporal workflow collection failed: %v", err)
			// Don't return here - continue with archive creation
		}
	} else if !defaultEP1Logs && temporalConfig.Enabled {
		logger.Info("Temporal workflow collection skipped (defaultEP1Logs=false)")
	}

	// Collect ZTF onboarding workflow information before archiving (if enabled and defaultEP1Logs is true)
	if defaultEP1Logs && ztfOnboardConfig.Enabled {
		err = collectZtfOnboardWorkflows(awsClient, ztfOnboardConfig, temporalConfig.WorkflowActivitySets, false, tempDir, finalLogFileName)
		if err != nil {
			logger.Warn("ZTF onboarding workflow collection failed: %v", err)
			// Don't return here - continue with archive creation
		}
	} else if !defaultEP1Logs && ztfOnboardConfig.Enabled {
		logger.Info("ZTF onboarding workflow collection skipped (defaultEP1Logs=false)")
	}

	// Collect Temporal schedule information before archiving (if enabled and defaultEP1Logs is true)
	if defaultEP1Logs && temporalScheduleConfig.Enabled {
		logger.Info("Starting Temporal schedule information collection...")
		err = collectTemporalScheduleInfo(awsClient, temporalScheduleConfig, environment, username, tempDir, finalLogFileName)
		if err != nil {
			logger.Warn("Temporal schedule collection failed: %v", err)
			// Don't return here - continue with archive creation
		}
	} else if !defaultEP1Logs && temporalScheduleConfig.Enabled {
		logger.Info("Temporal schedule collection skipped (defaultEP1Logs=false)")
	}

	// Collect pod files before archiving (if enabled)
	if podFileCollectionConfig.Enabled {
		// Build collection list: start with defaults when defaultEP1Logs is enabled
		var collectionsToProcess []PodFileCollection

		if defaultEP1Logs {
			// Add default collections (cs-configuration, nvo-orchestration-wired, nvo-orchestration-wireless)
			collectionsToProcess = append(collectionsToProcess, defaultPodFileCollections...)
			logger.Debug("Added %d default pod file collections (defaultEP1Logs=true)", len(defaultPodFileCollections))
		}

		// Add any additional collections from config
		if len(podFileCollectionConfig.Collections) > 0 {
			collectionsToProcess = append(collectionsToProcess, podFileCollectionConfig.Collections...)
			logger.Debug("Added %d custom pod file collections from config", len(podFileCollectionConfig.Collections))
		}

		if len(collectionsToProcess) > 0 {
			logger.Info("Starting pod file collection (total: %d collections)...", len(collectionsToProcess))
			err = collectPodFiles(awsClient, collectionsToProcess, tempDir, finalLogFileName)
			if err != nil {
				logger.Warn("Pod file collection failed: %v", err)
				// Don't return here - continue with archive creation
			}
		} else {
			logger.Debug("Pod file collection enabled but no collections configured")
		}
	}

	// Post-collection file operations - each operation uses a fresh session
	logger.Info("Creating archive: %s.tar.gz", finalLogFileName)

	// Create archive using a fresh session
	archiveSession, err := awsClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session for archive operations: %v", err)
	}
	defer archiveSession.Close()

	archiveCmd := fmt.Sprintf("cd %s && tar -czf %s.tar.gz %s", tempDir, finalLogFileName, finalLogFileName)
	if err := executeCommandAsRoot(archiveSession, archiveCmd); err != nil {
		return "", fmt.Errorf("failed to create archive: %v", err)
	}

	// Get file size for informational purposes using a fresh session
	var fileSize string
	sizeSession, err := awsClient.NewSession()
	if err == nil {
		defer sizeSession.Close()
		sizeCmd := fmt.Sprintf("ls -lh %s/%s.tar.gz | awk '{print $5}'", tempDir, finalLogFileName)
		if output, err := sizeSession.Output(sizeCmd); err == nil {
			fileSize = strings.TrimSpace(string(output))
		}
	}

	// Get archive size in bytes for validation
	var fileSizeBytes int64
	byteSizeSession, byteSizeErr := awsClient.NewSession()
	if byteSizeErr == nil {
		defer byteSizeSession.Close()
		sizeBytesCmd := fmt.Sprintf("stat -c %%s %s/%s.tar.gz 2>/dev/null || stat -f %%z %s/%s.tar.gz 2>/dev/null", tempDir, finalLogFileName, tempDir, finalLogFileName)
		if bytesOutput, bytesExecErr := byteSizeSession.Output(sizeBytesCmd); bytesExecErr == nil {
			if parsedSize, parseErr := strconv.ParseInt(strings.TrimSpace(string(bytesOutput)), 10, 64); parseErr == nil {
				fileSizeBytes = parsedSize
			}
		}
	}

	// Validate archive size - if too small, likely no data was collected
	if fileSizeBytes > 0 && fileSizeBytes < 1024 { // Less than 1KB (just tar overhead)
		logger.Warn("Archive size is very small (%s / %d bytes) - no data was collected", fileSize, fileSizeBytes)
		logger.Info("")
		logger.Info("Cleaning up empty archive and temporary directory...")

		// Delete the empty archive
		delArchiveSession, _ := awsClient.NewSession()
		if delArchiveSession != nil {
			defer delArchiveSession.Close()
			delArchiveCmd := fmt.Sprintf("rm -f %s/%s.tar.gz", tempDir, finalLogFileName)
			executeCommandAsRoot(delArchiveSession, delArchiveCmd)
		}

		// Delete temp directory
		delTempSession, _ := awsClient.NewSession()
		if delTempSession != nil {
			defer delTempSession.Close()
			delTempCmd := fmt.Sprintf("rm -rf %s/%s", tempDir, finalLogFileName)
			executeCommandAsRoot(delTempSession, delTempCmd)
		}

		logger.Info("")
		logger.Info("========================================")
		logger.Error("No data collected - possible reasons:")
		logger.Info("  1. No matching pods found for the specified namespaces/prefixes")
		logger.Info("  2. Pods exist but have no log files")
		logger.Info("  3. Time-based collection window has no logs")
		logger.Info("  4. Message filtering excluded all log entries")
		logger.Info("")
		logger.Info("Please check your configuration and try again.")
		logger.Info("========================================")
		return "", fmt.Errorf("no data collected - archive size too small (%d bytes)", fileSizeBytes)
	}

	if fileSize != "" {
		logger.Info("Archive created successfully: %s", fileSize)
	} else {
		logger.Info("Archive created successfully")
	}

	logger.Info("Moving archive to /home/%s/", userID)

	logger.Info("Moving archive to /home/%s/", userID)
	// Move archive to user directory using a fresh session
	moveSession, err := awsClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session for move operation: %v", err)
	}
	defer moveSession.Close()

	moveCmd := fmt.Sprintf("mv %s/%s.tar.gz /home/%s/", tempDir, finalLogFileName, userID)
	if err := executeCommandAsRoot(moveSession, moveCmd); err != nil {
		return "", fmt.Errorf("failed to move archive: %v", err)
	}

	logger.Info("Setting permissions for archive file...")
	// Set proper permissions for the archive file using a fresh session
	chmodSession, err := awsClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session for chmod operation: %v", err)
	}
	defer chmodSession.Close()

	chmodCmd := fmt.Sprintf("chmod 644 /home/%s/%s.tar.gz", userID, finalLogFileName)
	if err := executeCommandAsRoot(chmodSession, chmodCmd); err != nil {
		logger.Warn("Failed to set permissions for archive: %v (this may cause download issues)", err)
	} else {
		logger.Info("Archive permissions set successfully")
	}

	// Cleanup temporary directory using a fresh session
	logger.Info("Cleaning up temporary files...")
	cleanupSession, err := awsClient.NewSession()
	if err != nil {
		logger.Warn("Failed to create session for cleanup: %v", err)
	} else {
		defer cleanupSession.Close()

		// Remove the log collection subdirectory
		cleanupCmd := fmt.Sprintf("rm -rf %s/%s", tempDir, finalLogFileName)
		if err := executeCommandAsRoot(cleanupSession, cleanupCmd); err != nil {
			logger.Warn("Failed to cleanup temporary directory: %v", err)
		}
	}

	// Check if tempDir is empty and delete it using a fresh session
	logger.Info("Checking if tempDir %s is empty and can be deleted...", tempDir)
	emptyCheckSession, err := awsClient.NewSession()
	if err != nil {
		logger.Debug("Failed to create session for tempDir check: %v", err)
	} else {
		defer emptyCheckSession.Close()

		// Check if directory is empty using a more reliable method
		// Count non-hidden files and directories (excluding . and ..)
		checkCmd := fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 | wc -l", tempDir)
		output, err := emptyCheckSession.CombinedOutput(fmt.Sprintf("sudo su - -c '%s'", checkCmd))
		if err != nil {
			logger.Debug("Failed to check tempDir contents: %v", err)
		} else {
			itemCount := strings.TrimSpace(string(output))
			logger.Debug("Items in tempDir %s: %s", tempDir, itemCount)

			if itemCount == "0" {
				// Directory is empty, safe to delete - use another fresh session
				deleteSession, err := awsClient.NewSession()
				if err != nil {
					logger.Warn("Failed to create session for tempDir deletion: %v", err)
				} else {
					defer deleteSession.Close()
					logger.Info("tempDir %s is empty, deleting it...", tempDir)
					deleteTempDirCmd := fmt.Sprintf("rmdir %s", tempDir) // Use rmdir for safety
					if err := executeCommandAsRoot(deleteSession, deleteTempDirCmd); err != nil {
						logger.Warn("Failed to delete tempDir %s: %v", tempDir, err)
					} else {
						logger.Info("Successfully deleted empty tempDir: %s", tempDir)
					}
				}
			} else {
				logger.Info("tempDir %s is not empty (%s items), skipping deletion for safety", tempDir, itemCount)
			}
		}
	}

	// Calculate and display log collection time
	logCollectionDuration := time.Since(logCollectionStartTime)
	logger.Info("Log collection completed successfully! (took %s)", logCollectionDuration.Round(time.Millisecond))
	if fileSize != "" {
		logger.Info("Archive created: /home/%s/%s.tar.gz (Size: %s)", userID, finalLogFileName, fileSize)
	} else {
		logger.Info("Archive created: /home/%s/%s.tar.gz", userID, finalLogFileName)
	}

	// Store the final filename for later use (deletion after download)
	return finalLogFileName, nil
}

// collectLogsFromSourceParallel collects logs from a specific pod source using an independent SSH session
func collectLogsFromSourceParallel(awsClient *ssh.Client, source PodLogSource, logDir string, timeBasedEnabled bool, sinceTime string, maxSSHSessions int, defaultEP1Logs bool, messageFilterConfig struct {
	Enabled              bool `yaml:"enabled"`
	FilterDuringDownload bool `yaml:"filterDuringDownload"`
	KeyValueFilters      []struct {
		Key   string `yaml:"key"`
		Value string `yaml:"value"`
	} `yaml:"keyValueFilters"`
	SpecificStrings []string `yaml:"specificStrings"`
}) error {
	// kubectl command to get pod names - try multiple approaches for robustness
	// NOTE: Each command needs its own SSH session to avoid "Stdout already set" errors

	// First, check environment and user context
	logger.Debug("Checking environment and user context...")

	// Create separate session for whoami
	whoSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create whoami session: %v", err)
	}
	whoAmI, _ := whoSession.CombinedOutput("whoami")
	whoSession.Close()
	logger.Debug("Current user: %s", strings.TrimSpace(string(whoAmI)))

	// Create separate session for environment check
	envSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create env session: %v", err)
	}
	envCheck, _ := envSession.CombinedOutput("echo $HOME && ls -la ~/.kube/ 2>/dev/null || echo 'No .kube directory'")
	envSession.Close()
	logger.Debug("Environment check: %s", string(envCheck))

	// Create separate session for kubectl version
	versionSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create version session: %v", err)
	}
	kubectlVersion, _ := versionSession.CombinedOutput("kubectl version --client --short 2>/dev/null || echo 'kubectl not found or not accessible'")
	versionSession.Close()
	logger.Debug("Kubectl version: %s", string(kubectlVersion))

	// First, try to get all pods in the namespace to see what's available (as regular user)
	listAllCmd := fmt.Sprintf("kubectl get pods -n %s --no-headers", source.Namespace)
	logger.Debug("First, listing all pods in namespace %s (as regular user): %s", source.Namespace, listAllCmd)

	// Create separate session for listing all pods
	listSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create list session: %v", err)
	}
	allPodsOutput, err := listSession.CombinedOutput(listAllCmd)
	listSession.Close()
	logger.Debug("All pods output (regular user): '%s'", string(allPodsOutput))

	if err != nil {
		logger.Debug("Error listing all pods as regular user: %v", err)

		// Try as root if regular user fails
		logger.Debug("Trying as root user...")

		// Create separate session for root context check
		rootCtxSession, err := awsClient.NewSession()
		if err != nil {
			return fmt.Errorf("failed to create root context session: %v", err)
		}
		rootWhoAmI, _ := rootCtxSession.CombinedOutput("sudo su - -c 'whoami && echo $HOME && ls -la ~/.kube/ 2>/dev/null || echo \"No .kube directory\"'")
		rootCtxSession.Close()
		logger.Debug("Root user context: %s", string(rootWhoAmI))

		// Create separate session for root list pods
		rootListSession, err := awsClient.NewSession()
		if err != nil {
			return fmt.Errorf("failed to create root list session: %v", err)
		}
		allPodsOutputRoot, errRoot := rootListSession.CombinedOutput(fmt.Sprintf("sudo su - -c '%s'", listAllCmd))
		rootListSession.Close()
		logger.Debug("All pods output (root): '%s'", string(allPodsOutputRoot))
		if errRoot != nil {
			logger.Debug("Error listing all pods as root: %v", errRoot)
		}
	}

	// Now try the original command with better error handling (try regular user first)
	getPodCmd := fmt.Sprintf("kubectl get pods -n %s --no-headers -o custom-columns=\":metadata.name\"", source.Namespace)
	logger.Debug("Getting pod names (as regular user): %s", getPodCmd)

	// Create separate session for getting pod names
	getPodSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create getPod session: %v", err)
	}
	output, err := getPodSession.CombinedOutput(getPodCmd)
	getPodSession.Close()
	logger.Debug("Pod names output (regular user): '%s'", string(output))

	if err != nil {
		logger.Debug("kubectl get pods failed as regular user: %v", err)

		// Try as root
		logger.Debug("Trying kubectl as root user...")
		rootCmd := fmt.Sprintf("sudo su - -c '%s'", getPodCmd)
		logger.Debug("Root command: %s", rootCmd)

		// Create separate session for root kubectl
		rootSession, rootErr := awsClient.NewSession()
		if rootErr != nil {
			return fmt.Errorf("failed to create root session: %v", rootErr)
		}
		output, err = rootSession.CombinedOutput(rootCmd)
		rootSession.Close()
		logger.Debug("Pod names output (root): '%s'", string(output))

		if err != nil {
			logger.Debug("kubectl get pods failed as root too: %v", err)
			// Try alternative approach without custom columns
			altCmd := fmt.Sprintf("kubectl get pods -n %s --no-headers", source.Namespace)
			logger.Debug("Trying alternative command (regular user): %s", altCmd)

			// Create separate session for alternative command
			altSession, altSessionErr := awsClient.NewSession()
			if altSessionErr != nil {
				return fmt.Errorf("failed to create alt session: %v", altSessionErr)
			}
			altOutput, altErr := altSession.CombinedOutput(altCmd)
			altSession.Close()
			logger.Debug("Alternative output (regular user): '%s'", string(altOutput))

			if altErr != nil {
				logger.Debug("Alternative command failed as regular user: %v", altErr)
				// Try alternative as root
				altRootCmd := fmt.Sprintf("sudo su - -c '%s'", altCmd)
				logger.Debug("Trying alternative command as root: %s", altRootCmd)

				// Create separate session for alternative root command
				altRootSession, altRootSessionErr := awsClient.NewSession()
				if altRootSessionErr != nil {
					return fmt.Errorf("failed to create alt root session: %v", altRootSessionErr)
				}
				altOutput, altErr = altRootSession.CombinedOutput(altRootCmd)
				altRootSession.Close()
				logger.Debug("Alternative output (root): '%s'", string(altOutput))

				if altErr != nil {
					return fmt.Errorf("failed to get pods for %s/%s: kubectl regular error: %v, output: %s, kubectl root error: %v, alt regular error: %v, alt root error: %v, alt root output: %s",
						source.Namespace, source.PodPrefix, err, string(output), err, altErr, altErr, string(altOutput))
				}
			}

			// Parse alternative output (first column is pod name)
			lines := strings.Split(strings.TrimSpace(string(altOutput)), "\n")
			var podNames []string
			for _, line := range lines {
				if line != "" {
					fields := strings.Fields(line)
					if len(fields) > 0 {
						podName := fields[0]
						if strings.HasPrefix(podName, source.PodPrefix) {
							podNames = append(podNames, podName)
						}
					}
				}
			}
			logger.Debug("Filtered pod names from alternative output: %v", podNames)

			if len(podNames) == 0 {
				logger.Warn("No pods found for prefix %s in namespace %s", source.PodPrefix, source.Namespace)
				return nil
			}

			// Continue with the filtered pod names
			output = []byte(strings.Join(podNames, "\n"))
		} else {
			// Root command succeeded, filter the output for pods matching the prefix
			allPodNames := strings.Split(strings.TrimSpace(string(output)), "\n")
			var filteredPods []string
			for _, podName := range allPodNames {
				if podName != "" && strings.HasPrefix(strings.TrimSpace(podName), source.PodPrefix) {
					filteredPods = append(filteredPods, strings.TrimSpace(podName))
				}
			}
			logger.Debug("All pods (root): %v, Filtered pods for prefix %s: %v", allPodNames, source.PodPrefix, filteredPods)
			output = []byte(strings.Join(filteredPods, "\n"))
		}
	} else {
		// Regular user command succeeded, filter the output for pods matching the prefix
		allPodNames := strings.Split(strings.TrimSpace(string(output)), "\n")
		var filteredPods []string
		for _, podName := range allPodNames {
			if podName != "" && strings.HasPrefix(strings.TrimSpace(podName), source.PodPrefix) {
				filteredPods = append(filteredPods, strings.TrimSpace(podName))
			}
		}
		logger.Debug("All pods (regular): %v, Filtered pods for prefix %s: %v", allPodNames, source.PodPrefix, filteredPods)
		output = []byte(strings.Join(filteredPods, "\n"))
	}

	podNames := strings.Split(strings.TrimSpace(string(output)), "\n")
	logger.Debug("Raw pod output: '%s'", string(output))
	logger.Debug("Parsed pod names: %v", podNames)

	if len(podNames) == 0 || (len(podNames) == 1 && podNames[0] == "") {
		logger.Warn("No pods found for prefix %s in namespace %s", source.PodPrefix, source.Namespace)
		return nil
	}

	logger.Debug("Found %d pods with prefix %s", len(podNames), source.PodPrefix)

	// Handle time-based vs file-based collection
	if timeBasedEnabled && sinceTime != "" {
		logger.Info("Using time-based collection for %s namespace (since %s)", source.Namespace, sinceTime)
		return collectTimeBasedLogs(awsClient, source, logDir, podNames, sinceTime, maxSSHSessions, defaultEP1Logs, messageFilterConfig)
	} else {
		logger.Debug("Using file-based collection for %s namespace", source.Namespace)
		return collectFileBased(awsClient, source, logDir, podNames, maxSSHSessions)
	}
}

// buildGrepFilter constructs a grep command chain from the message filter configuration
func buildGrepFilter(config struct {
	Enabled              bool `yaml:"enabled"`
	FilterDuringDownload bool `yaml:"filterDuringDownload"`
	KeyValueFilters      []struct {
		Key   string `yaml:"key"`
		Value string `yaml:"value"`
	} `yaml:"keyValueFilters"`
	SpecificStrings []string `yaml:"specificStrings"`
}) string {
	if !config.FilterDuringDownload {
		return ""
	}

	var grepParts []string

	// Build grep patterns for key-value filters
	// For JSON logs, we look for "key":"value" or key.*value patterns
	for _, filter := range config.KeyValueFilters {
		if filter.Key == "" {
			continue
		}
		if filter.Value != "" {
			// Escape special characters in key and value for grep
			key := strings.ReplaceAll(filter.Key, `"`, `\"`)
			value := strings.ReplaceAll(filter.Value, `"`, `\"`)
			// Match "key":"value" pattern (JSON) or key.*value (flexible)
			grepParts = append(grepParts, fmt.Sprintf(`grep -E '"%s".*"%s"|%s.*%s'`, key, value, key, value))
		} else {
			// Just match lines containing the key
			key := strings.ReplaceAll(filter.Key, `"`, `\"`)
			grepParts = append(grepParts, fmt.Sprintf(`grep '%s'`, key))
		}
	}

	// Build grep patterns for specific strings
	if len(config.SpecificStrings) > 0 {
		var patterns []string
		for _, str := range config.SpecificStrings {
			if str != "" {
				// Escape special regex characters for literal matching
				escaped := strings.ReplaceAll(str, `\`, `\\`)
				escaped = strings.ReplaceAll(escaped, `'`, `'\''`)
				patterns = append(patterns, escaped)
			}
		}
		if len(patterns) > 0 {
			// Combine all patterns with OR (|) for efficiency
			grepParts = append(grepParts, fmt.Sprintf(`grep -E '%s'`, strings.Join(patterns, "|")))
		}
	}

	// Chain all grep commands with pipes
	if len(grepParts) > 0 {
		return " | " + strings.Join(grepParts, " | ")
	}

	return ""
}

// collectTimeBasedLogs collects logs using kubectl logs with --since parameter
func collectTimeBasedLogs(awsClient *ssh.Client, source PodLogSource, logDir string, podNames []string, sinceTime string, maxSSHSessions int, defaultEP1Logs bool, messageFilterConfig struct {
	Enabled              bool `yaml:"enabled"`
	FilterDuringDownload bool `yaml:"filterDuringDownload"`
	KeyValueFilters      []struct {
		Key   string `yaml:"key"`
		Value string `yaml:"value"`
	} `yaml:"keyValueFilters"`
	SpecificStrings []string `yaml:"specificStrings"`
}) error {
	// Create directories for logs
	namespaceDir := fmt.Sprintf("%s/%s", logDir, source.Namespace)

	session2, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for directories: %v", err)
	}
	defer session2.Close()

	var batchCommands []string
	for _, podName := range podNames {
		if podName == "" {
			continue
		}
		podName = strings.TrimSpace(podName)
		podDir := fmt.Sprintf("%s/%s", namespaceDir, podName)
		batchCommands = append(batchCommands, fmt.Sprintf("mkdir -p %s", podDir))
	}

	if len(batchCommands) > 0 {
		createDirsCmd := strings.Join(batchCommands, " && ")
		if err := executeCommandAsRoot(session2, createDirsCmd); err != nil {
			logger.Warn("Failed to create some directories: %v", err)
		}
	}

	// Check if pod logs download is disabled
	if !defaultEP1Logs {
		logger.Info("Default EP1 log collection is disabled (defaultEP1Logs=false), skipping built-in kubectl logs collection")
		return nil
	}

	// Collect logs from each pod using kubectl logs --since-time (CONFIGURABLE SSH management)
	var podWg sync.WaitGroup

	// Use configurable concurrency limit with safe defaults
	if maxSSHSessions <= 0 || maxSSHSessions > 4 {
		maxSSHSessions = 1 // Default to 1 if not configured or invalid
	}
	maxConcurrentPods := maxSSHSessions // Use configured value
	semaphore := make(chan struct{}, maxConcurrentPods)

	for _, podName := range podNames {
		if podName == "" {
			continue
		}
		podName = strings.TrimSpace(podName)

		podWg.Add(1)
		go func(pod string) {
			defer podWg.Done()

			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Small delay to reduce SSH server pressure significantly
			time.Sleep(time.Duration(200) * time.Millisecond)

			logger.Debug("Collecting time-based logs from pod: %s", pod)

			podDir := fmt.Sprintf("%s/%s", namespaceDir, pod)
			var outputFile string

			// Determine output filename based on pod type
			switch {
			case strings.HasPrefix(source.PodPrefix, "hacr") || source.PodPrefix == "hacr":
				outputFile = fmt.Sprintf("%s/hac_%s.log", podDir, strings.ReplaceAll(sinceTime, ":", "-"))
			case strings.HasPrefix(source.PodPrefix, "teconfig"):
				outputFile = fmt.Sprintf("%s/hm_%s.log", podDir, strings.ReplaceAll(sinceTime, ":", "-"))
			case strings.HasPrefix(source.PodPrefix, "nvo-"):
				outputFile = fmt.Sprintf("%s/%s_%s.log", podDir, pod, strings.ReplaceAll(sinceTime, ":", "-"))
			default:
				outputFile = fmt.Sprintf("%s/%s_%s.log", podDir, pod, strings.ReplaceAll(sinceTime, ":", "-"))
			}

			// Create new session for kubectl logs command
			session3, err := awsClient.NewSession()
			if err != nil {
				logger.Warn("Failed to create session for kubectl logs for pod %s: %v", pod, err)
				return
			}
			defer session3.Close()

			// Use kubectl logs with --since parameter
			logsCmd := fmt.Sprintf("timeout 180 kubectl logs -n %s %s --since-time=%s", source.Namespace, pod, sinceTime)

			// Apply grep filtering during download if enabled
			grepFilter := buildGrepFilter(messageFilterConfig)
			if grepFilter != "" {
				logsCmd += grepFilter
				logger.Debug("Applying during-download filter: kubectl logs with grep filters")
			}

			logger.Debug("Executing kubectl logs: %s", logsCmd)

			// Execute and redirect output to file
			fullCmd := fmt.Sprintf("%s > %s 2>&1", logsCmd, outputFile)
			if err := executeCommandAsRoot(session3, fullCmd); err != nil {
				logger.Warn("Failed to collect logs from pod %s: %v", pod, err)
			} else {
				logger.Debug("Successfully collected time-based logs from %s", pod)
			}
		}(podName)
	}

	// Wait for all pod log collections to complete
	podWg.Wait()

	return nil
}

// collectFileBased handles traditional file-based log collection with parallel SSH connections
func collectFileBased(awsClient *ssh.Client, source PodLogSource, logDir string, podNames []string, maxSSHSessions int) error {
	// Create directories for logs
	namespaceDir := fmt.Sprintf("%s/%s", logDir, source.Namespace)

	session2, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for directories: %v", err)
	}
	defer session2.Close()

	var batchCommands []string
	for _, podName := range podNames {
		if podName == "" {
			continue
		}
		podName = strings.TrimSpace(podName)
		podDir := fmt.Sprintf("%s/%s", namespaceDir, podName)
		batchCommands = append(batchCommands, fmt.Sprintf("mkdir -p %s", podDir))
	}

	if len(batchCommands) > 0 {
		createDirsCmd := strings.Join(batchCommands, " && ")
		if err := executeCommandAsRoot(session2, createDirsCmd); err != nil {
			logger.Warn("Failed to create some directories: %v", err)
		}
	}

	// Copy logs from each pod using OPTIMIZED SSH connections with configurable session limit management
	var podWg sync.WaitGroup

	// Use configurable concurrency limit with safe defaults
	if maxSSHSessions <= 0 || maxSSHSessions > 4 {
		maxSSHSessions = 1 // Default to 1 if not configured or invalid
	}
	maxConcurrentPods := maxSSHSessions // Use configured value
	semaphore := make(chan struct{}, maxConcurrentPods)

	for _, podName := range podNames {
		if podName == "" {
			continue
		}
		podName = strings.TrimSpace(podName)

		podWg.Add(1)
		go func(pod string) {
			defer podWg.Done()

			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Small delay to reduce SSH server pressure significantly
			time.Sleep(time.Duration(200) * time.Millisecond)

			logger.Debug("Collecting file-based logs from pod: %s", pod)

			// Determine the source and destination paths
			var srcPath, dstPath string
			podDir := fmt.Sprintf("%s/%s", namespaceDir, pod)

			switch {
			case strings.HasPrefix(source.PodPrefix, "hacr") || source.PodPrefix == "hacr":
				srcPath = fmt.Sprintf("%s:/opt/hacr/logs/hac.log", pod)
				dstPath = fmt.Sprintf("%s/hac.log", podDir)
			case strings.HasPrefix(source.PodPrefix, "teconfig") || source.PodPrefix == "teconfig":
				srcPath = fmt.Sprintf("%s:/opt/tomcat/logs/hm.log", pod)
				dstPath = fmt.Sprintf("%s/hm.log", podDir)
			case strings.HasPrefix(source.PodPrefix, "nvo-"):
				srcPath = fmt.Sprintf("%s:%s/%s-server.log", pod, source.LogPath, pod)
				dstPath = fmt.Sprintf("%s/%s-server.log", podDir, pod)
			default:
				srcPath = fmt.Sprintf("%s:%s/%s", pod, source.LogPath, source.OutputName)
				dstPath = fmt.Sprintf("%s/%s", podDir, source.OutputName)
			}

			// Create independent SSH session for this pod's operations (consolidate to reduce session count)
			session3, err := awsClient.NewSession()
			if err != nil {
				logger.Warn("Failed to create session for kubectl cp for pod %s: %v", pod, err)
				return
			}
			defer session3.Close()

			// kubectl cp command with timeout and retries for better performance
			cpCmd := fmt.Sprintf("timeout 180 kubectl -n %s cp %s %s --retries=2", source.Namespace, srcPath, dstPath)
			logger.Debug("Executing kubectl cp: %s", cpCmd)

			// Execute kubectl cp
			output, err := session3.CombinedOutput(fmt.Sprintf("sudo su - -c '%s'", cpCmd))
			if err != nil {
				logger.Warn("Failed to copy logs from pod %s: %v", pod, err)
				if len(output) > 0 {
					logger.Debug("kubectl cp output: %s", string(output))
				}
			} else {
				// Verify the file was actually copied using the same session
				verifyCmd := fmt.Sprintf("ls -la %s 2>/dev/null || echo 'File not found'", dstPath)
				if verifyOutput, verifyErr := session3.CombinedOutput(fmt.Sprintf("sudo su - -c '%s'", verifyCmd)); verifyErr == nil {
					if strings.Contains(string(verifyOutput), "File not found") {
						logger.Warn("Log file was not copied successfully for pod %s", pod)
					} else {
						logger.Debug("Successfully copied logs from pod: %s", pod)
						logger.Debug("File verification: %s", strings.TrimSpace(string(verifyOutput)))
					}
				}
				if len(output) > 0 {
					logger.Debug("kubectl cp output: %s", string(output))
				}
			}
		}(podName)
	}

	// Wait for all pod log collections to complete
	podWg.Wait()

	return nil
}

// getAWSServerTime gets the current time from the AWS server for accurate time-based log collection
func getAWSServerTime(awsClient *ssh.Client) (time.Time, error) {
	// Create new session to get server time
	session, err := awsClient.NewSession()
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to create session for time query: %v", err)
	}
	defer session.Close()

	// Get current time in UTC from AWS server using date command
	timeCmd := "date -u '+%Y-%m-%dT%H:%M:%SZ'"
	logger.Debug("Getting AWS server time: %s", timeCmd)

	output, err := session.CombinedOutput(timeCmd)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get server time: %v", err)
	}

	// Parse the time string from server
	timeStr := strings.TrimSpace(string(output))
	logger.Debug("AWS server returned time: %s", timeStr)

	serverTime, err := time.Parse("2006-01-02T15:04:05Z", timeStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse server time '%s': %v", timeStr, err)
	}

	logger.Debug("Parsed AWS server time: %s", serverTime.Format("2006-01-02T15:04:05Z"))
	return serverTime, nil
}

// startHeartbeat prints a periodic spinner + elapsed time to the console so a long silent
// remote operation (e.g. archiving gigabytes of logs) doesn't look like the tool has hung.
// Call the returned stop func once the operation finishes; it clears the spinner line.
func startHeartbeat(message string) (stop func()) {
	done := make(chan struct{})
	go func() {
		frames := []string{"|", "/", "-", "\\"}
		ticker := time.NewTicker(700 * time.Millisecond)
		defer ticker.Stop()
		start := time.Now()
		i := 0
		for {
			select {
			case <-done:
				fmt.Print("\r" + strings.Repeat(" ", len(message)+24) + "\r")
				return
			case <-ticker.C:
				elapsed := time.Since(start).Round(time.Second)
				fmt.Printf("\r  %s %s (%s elapsed)", frames[i%len(frames)], message, elapsed)
				i++
			}
		}
	}()
	return func() { close(done) }
}

// runWithHeartbeat runs fn and, only if it hasn't returned within delay, starts a spinner so
// fast operations stay silent (no flicker) while slow ones visibly show progress instead of
// leaving the terminal looking stuck. The spinner animation itself is console-only (it would be
// unreadable noise in a text log file), but crossing the delay threshold IS recorded via
// logger.Debug (start + total elapsed) so logger_info.txt still shows what ran long and for how
// long - it just won't contain every intermediate frame.
func runWithHeartbeat(message string, delay time.Duration, fn func() error) error {
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- fn() }()

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case err := <-done:
		return err
	case <-timer.C:
	}

	if logger != nil {
		logger.Debug("Still running after %s: %s", delay.Round(time.Second), message)
	}

	stop := startHeartbeat(message)
	err := <-done
	stop()

	if logger != nil {
		logger.Debug("Finished after %s: %s", time.Since(start).Round(time.Second), message)
	}
	return err
}

// heartbeatLabel derives a short, single-line description of a remote command for the spinner
// message, so the user sees roughly what's running instead of a generic "please wait".
func heartbeatLabel(command string) string {
	label := strings.Join(strings.Fields(command), " ")
	const maxLen = 70
	if len(label) > maxLen {
		label = label[:maxLen] + "..."
	}
	return label
}

// executeCommandAsRoot executes a command as root user using sudo su -
func executeCommandAsRoot(session *ssh.Session, command string) error {
	// Wrap the command to run as root
	rootCommand := fmt.Sprintf("sudo su - -c '%s'", command)
	logger.Debug("Executing as root: %s", rootCommand)

	// Debounced spinner: silent for fast commands, shows progress once a command runs past
	// 3s so the terminal never looks stuck during e.g. a large remote tar/archive step.
	var output []byte
	err := runWithHeartbeat(heartbeatLabel(command), 3*time.Second, func() error {
		var cmdErr error
		output, cmdErr = session.CombinedOutput(rootCommand)
		return cmdErr
	})
	if err != nil {
		logger.Debug("Root command failed: %s\nOutput: %s", rootCommand, string(output))
		return fmt.Errorf("root command failed: %v\nOutput: %s", err, string(output))
	}

	if len(output) > 0 {
		logger.Debug("Root command output: %s", string(output))
	}

	return nil
}

// deleteArchiveFromAWS deletes the archive file from AWS after successful download
func deleteArchiveFromAWS(awsClient *ssh.Client, archivePath, environment string) error {
	logger.Info("Deleting source archive from %s: %s", environment, archivePath)

	session, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for deletion: %v", err)
	}
	defer session.Close()

	deleteCmd := fmt.Sprintf("rm -f %s", archivePath)
	if err := executeCommandAsRoot(session, deleteCmd); err != nil {
		return fmt.Errorf("failed to delete archive: %v", err)
	}

	logger.Info("Successfully deleted source archive from %s", environment)
	return nil
}

// archiveAndDownloadRemoteDir compresses a remote directory (tempDir/dirName) into a
// tar.gz archive, moves it to remoteUser's home directory (root-owned tempDirs aren't
// otherwise readable by the SSH login user), downloads it to localOutputDir, and cleans
// up the remote temp files. Returns the local path to the downloaded archive. Mirrors the
// archive/move/chmod/download/cleanup pattern used by the main log collection pipeline.
func archiveAndDownloadRemoteDir(awsClient *ssh.Client, tempDir, dirName, remoteUser, localOutputDir string, autoRetry bool, numChunks int, downloadMethod string, connParams *ConnectionParams, logger *Logger) (string, error) {
	archiveSession, err := awsClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session for archive operation: %v", err)
	}
	defer archiveSession.Close()

	archiveCmd := fmt.Sprintf("cd %s && tar -czf %s.tar.gz %s", tempDir, dirName, dirName)
	if err := executeCommandAsRoot(archiveSession, archiveCmd); err != nil {
		return "", fmt.Errorf("failed to create archive: %v", err)
	}

	moveSession, err := awsClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session for move operation: %v", err)
	}
	defer moveSession.Close()
	moveCmd := fmt.Sprintf("mv %s/%s.tar.gz /home/%s/", tempDir, dirName, remoteUser)
	if err := executeCommandAsRoot(moveSession, moveCmd); err != nil {
		return "", fmt.Errorf("failed to move archive: %v", err)
	}

	if chmodSession, chmodErr := awsClient.NewSession(); chmodErr == nil {
		defer chmodSession.Close()
		chmodCmd := fmt.Sprintf("chmod 644 /home/%s/%s.tar.gz", remoteUser, dirName)
		if err := executeCommandAsRoot(chmodSession, chmodCmd); err != nil {
			logger.Warn("Failed to set permissions for archive: %v (this may cause download issues)", err)
		}
	}

	remotePath := fmt.Sprintf("/home/%s/%s.tar.gz", remoteUser, dirName)
	if err := os.MkdirAll(localOutputDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create local output directory: %v", err)
	}
	localPath := filepath.Join(localOutputDir, dirName+".tar.gz")

	logger.Info("Downloading archive to: %s", localPath)
	if err := downloadFileFromAWS(awsClient, remotePath, localPath, autoRetry, numChunks, connParams, downloadMethod); err != nil {
		return "", fmt.Errorf("failed to download archive: %v", err)
	}

	// Best-effort remote cleanup (archive + temp dir)
	if cleanupSession, cleanupErr := awsClient.NewSession(); cleanupErr == nil {
		defer cleanupSession.Close()
		cleanupCmd := fmt.Sprintf("rm -f %s && rm -rf %s/%s", remotePath, tempDir, dirName)
		if err := executeCommandAsRoot(cleanupSession, cleanupCmd); err != nil {
			logger.Debug("Remote cleanup warning: %v", err)
		}
	}

	return localPath, nil
}

// Download file from AWS to bastion using SFTP with parallel download
func downloadFileFromAWS(awsClient *ssh.Client, remotePath, localPath string, autoRetry bool, numChunks int, connParams *ConnectionParams, downloadMethod string) error {
	_ = autoRetry // Not used - SCP-based download doesn't have chunk retries

	// Use more chunks for better parallel performance from bastion to local
	if numChunks <= 0 {
		numChunks = 8 // Default to 8 parallel connections for good performance
	}
	if numChunks > 10 {
		numChunks = 10 // Cap at 10 to avoid overwhelming SSH server
	}

	// Record start time
	startTime := time.Now()

	// Step 1: Use SCP on the bastion to copy file from AWS to bastion (internal network = fast)
	// Step 2: SFTP download from bastion to local (single hop, pipelined reads)

	bastionClient := connParams.BastionClient
	awsHost := connParams.AWSHost
	keyPath := connParams.KeyPath
	awsUsername := connParams.PreferredUsername

	// Determine the remote filename
	remoteFileName := filepath.Base(remotePath)
	bastionTempPath := fmt.Sprintf("/tmp/%s", remoteFileName)

	// --- STEP 1: SCP from AWS to Bastion (internal network) ---
	logger.Info("Step 1: Copying file from AWS to bastion via SCP (internal network)...")

	// First, determine the AWS SSH user by checking who we connected as
	userSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session to determine AWS user: %v", err)
	}
	awsUserOutput, err := userSession.Output("whoami")
	userSession.Close()
	if err != nil {
		// Fallback to preferred username or ec2-user
		if awsUsername != "" {
			// Use preferred username
		} else {
			awsUsername = "ec2-user"
		}
	} else {
		awsUsername = strings.TrimSpace(string(awsUserOutput))
	}

	// Build the SCP command to run on bastion
	// scp -i {keyPath} -o StrictHostKeyChecking=no {awsUser}@{awsHost}:{remotePath} /tmp/
	scpCmd := fmt.Sprintf("scp -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null %s@%s:%s %s",
		keyPath, awsUsername, awsHost, remotePath, bastionTempPath)
	logger.Debug("SCP command: %s", scpCmd)

	scpSession, err := bastionClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create bastion session for SCP: %v", err)
	}

	scpStartTime := time.Now()
	scpOutput, scpErr := scpSession.CombinedOutput(scpCmd)
	scpSession.Close()
	scpDuration := time.Since(scpStartTime)

	if scpErr != nil {
		logger.Error("SCP failed: %v\nOutput: %s", scpErr, strings.TrimSpace(string(scpOutput)))
		return fmt.Errorf("SCP from AWS to bastion failed: %v", scpErr)
	}
	logger.Info("Step 1 complete: File copied to bastion in %s", scpDuration.Round(time.Millisecond))

	// Remove the file from AWS immediately after successful SCP to bastion
	awsRemoveSession, err := awsClient.NewSession()
	if err == nil {
		awsRemoveCmd := fmt.Sprintf("rm -f %s", remotePath)
		awsRemoveSession.Run(awsRemoveCmd)
		awsRemoveSession.Close()
		logger.Debug("Removed file from AWS: %s", remotePath)
	}

	// Ensure cleanup of temp file on bastion when done
	defer func() {
		cleanSession, err := bastionClient.NewSession()
		if err == nil {
			cleanSession.Run(fmt.Sprintf("rm -f %s", bastionTempPath))
			cleanSession.Close()
			logger.Debug("Cleaned up temp file on bastion: %s", bastionTempPath)
		}
	}()

	// Get file size from bastion
	statSession, err := bastionClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for file stat: %v", err)
	}
	statOutput, err := statSession.Output(fmt.Sprintf("stat -c%%s %s", bastionTempPath))
	statSession.Close()
	if err != nil {
		return fmt.Errorf("failed to stat file on bastion: %v", err)
	}
	fileSize, err := strconv.ParseInt(strings.TrimSpace(string(statOutput)), 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse file size: %v", err)
	}
	logger.Debug("File size on bastion: %d bytes (%.2f MB)", fileSize, float64(fileSize)/(1024*1024))

	// --- STEP 2: Download from Bastion to Local ---
	// Choose download method based on configuration/flags
	scpDownloadStartTime := time.Now()

	if downloadMethod == "sftp" {
		// Use parallel SFTP (8 connections)
		logger.Info("Step 2: Downloading from bastion to local via parallel SFTP (%.2f MB)...", float64(fileSize)/(1024*1024))
		err = downloadFromBastionParallel(connParams, bastionTempPath, localPath, fileSize, numChunks, remoteFileName)
	} else {
		// Use native SCP (default - much faster)
		logger.Info("Step 2: Downloading from bastion to local via native SCP (%.2f MB)...", float64(fileSize)/(1024*1024))
		err = downloadFromBastionWithSCP(connParams, bastionTempPath, localPath, fileSize, remoteFileName)

		// If SCP fails, fallback to parallel SFTP
		if err != nil {
			logger.Warn("Native SCP failed (%v), falling back to parallel SFTP...", err)
			scpDownloadStartTime = time.Now() // Reset timer for SFTP attempt
			err = downloadFromBastionParallel(connParams, bastionTempPath, localPath, fileSize, numChunks, remoteFileName)
		}
	}

	if err != nil {
		return fmt.Errorf("failed to download from bastion: %v", err)
	}
	sftpDuration := time.Since(scpDownloadStartTime)

	// Final verification
	if err := performFinalVerification(localPath, fileSize); err != nil {
		return err
	}

	// Verify on disk
	finalFile, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("failed to verify final file: %v", err)
	}
	if finalFile.Size() != fileSize {
		return fmt.Errorf("file size mismatch: expected %d bytes, got %d bytes", fileSize, finalFile.Size())
	}

	// Calculate and display timing
	totalDuration := time.Since(startTime)
	logger.Info("Download timing: SCP (AWS->Bastion): %s, SCP (Bastion->Local): %s, Total: %s",
		scpDuration.Round(time.Millisecond),
		sftpDuration.Round(time.Millisecond),
		totalDuration.Round(time.Millisecond))

	bytesPerSecond := float64(fileSize) / totalDuration.Seconds()
	var speedStr string
	if bytesPerSecond >= 1024*1024 {
		speedStr = fmt.Sprintf("%.2f MB/s", bytesPerSecond/(1024*1024))
	} else if bytesPerSecond >= 1024 {
		speedStr = fmt.Sprintf("%.2f KB/s", bytesPerSecond/1024)
	} else {
		speedStr = fmt.Sprintf("%.2f B/s", bytesPerSecond)
	}
	logger.Debug("Download completed in %s (avg. %s)", totalDuration.Round(time.Millisecond), speedStr)

	return nil
}

// progressWriter wraps an io.Writer and tracks bytes written for progress reporting
type progressWriter struct {
	w           io.Writer
	written     *int64
	mu          *sync.Mutex
	atomicTotal *int64 // optional: for chunk downloads, atomically update shared total
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	if pw.mu != nil {
		pw.mu.Lock()
		*pw.written += int64(n)
		pw.mu.Unlock()
	}
	if pw.atomicTotal != nil {
		atomic.AddInt64(pw.atomicTotal, int64(n))
	}
	return n, err
}

// progressReader wraps an io.Reader to track progress and periodically flush writes
type progressReader struct {
	reader       io.Reader
	totalBytes   *int64 // Global progress counter
	chunkBytes   *int64 // Chunk-specific counter
	flushWriter  *bufio.Writer
	flushCounter int
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		atomic.AddInt64(pr.totalBytes, int64(n))
		*pr.chunkBytes += int64(n)

		// Flush every 2MB to prevent buffer buildup
		pr.flushCounter += n
		if pr.flushCounter >= 2*1024*1024 {
			pr.flushWriter.Flush()
			pr.flushCounter = 0
		}
	}
	return n, err
}

// simpleProgressReader wraps an io.Reader to track progress atomically
type simpleProgressReader struct {
	reader     io.Reader
	totalBytes *int64 // Global atomic progress counter
}

func (spr *simpleProgressReader) Read(p []byte) (int, error) {
	n, err := spr.reader.Read(p)
	if n > 0 {
		atomic.AddInt64(spr.totalBytes, int64(n))
	}
	return n, err
}

// downloadFromBastionParallel downloads a file from bastion using parallel SFTP connections
// Each chunk creates its own independent SSH connection with optimized SFTP settings
func downloadFromBastionParallel(connParams *ConnectionParams, bastionPath, localPath string, fileSize int64, numChunks int, fileName string) error {
	// Calculate chunk size
	chunkSize := fileSize / int64(numChunks)
	if chunkSize < 1024*1024 {
		chunkSize = 1024 * 1024 // Minimum 1MB per chunk
		numChunks = int(fileSize / chunkSize)
		if numChunks == 0 {
			numChunks = 1
		}
	}

	// Create chunk info
	type chunkInfo struct {
		index       int
		startOffset int64
		endOffset   int64
		tempFile    string
		err         error
	}

	tempFilePath := localPath + ".part"
	chunks := make([]chunkInfo, numChunks)
	for i := 0; i < numChunks; i++ {
		startOffset := int64(i) * chunkSize
		endOffset := startOffset + chunkSize
		if i == numChunks-1 || endOffset > fileSize {
			endOffset = fileSize
		}
		chunks[i] = chunkInfo{
			index:       i,
			startOffset: startOffset,
			endOffset:   endOffset,
			tempFile:    fmt.Sprintf("%s.chunk%d", tempFilePath, i),
		}
	}

	// Clean up chunk files on exit or error
	defer func() {
		for _, chunk := range chunks {
			if _, err := os.Stat(chunk.tempFile); err == nil {
				os.Remove(chunk.tempFile)
			}
		}
	}()

	// Set up progress display with rectangle bar
	var totalBytesRead int64
	progressTicker := time.NewTicker(200 * time.Millisecond)
	const barWidth = 20
	fmt.Printf("Downloading %s [", fileName)
	for i := 0; i < barWidth; i++ {
		fmt.Print(" ")
	}
	fmt.Printf("] 0%% 0.00/%.2f MB", float64(fileSize)/(1024*1024))

	stopAnimation := make(chan bool)
	go func() {
		for {
			select {
			case <-progressTicker.C:
				currentBytes := atomic.LoadInt64(&totalBytesRead)
				var percent int
				if fileSize > 0 {
					percent = int(float64(currentBytes) / float64(fileSize) * 100)
				}
				if percent > 100 {
					percent = 100
				}

				// Create rectangle progress bar with simple characters
				filled := (percent * barWidth) / 100
				bar := "["
				for i := 0; i < barWidth; i++ {
					if i < filled {
						bar += "="
					} else {
						bar += " "
					}
				}
				bar += "]"

				fmt.Printf("\rDownloading %s %s %d%% %.2f/%.2f MB",
					fileName, bar, percent,
					float64(currentBytes)/(1024*1024),
					float64(fileSize)/(1024*1024))
			case <-stopAnimation:
				progressTicker.Stop()
				return
			}
		}
	}()

	defer func() {
		select {
		case <-stopAnimation:
		default:
			close(stopAnimation)
		}
		finalBytes := atomic.LoadInt64(&totalBytesRead)
		bar := "["
		for i := 0; i < barWidth; i++ {
			bar += "="
		}
		bar += "]"
		fmt.Printf("\rDownloading %s %s 100%% %.2f/%.2f MB - Complete\n",
			fileName, bar,
			float64(finalBytes)/(1024*1024),
			float64(fileSize)/(1024*1024))
	}()

	logger.Debug("Downloading %d chunks to separate files", numChunks)
	for i, chunk := range chunks {
		logger.Debug("Chunk %d: %.2f MB (bytes %d to %d) -> %s",
			i, float64(chunk.endOffset-chunk.startOffset)/(1024*1024),
			chunk.startOffset, chunk.endOffset, chunk.tempFile)
	}

	// Download chunks in parallel - each to its own file
	var wg sync.WaitGroup
	for i := range chunks {
		wg.Add(1)
		go func(chunk *chunkInfo) {
			defer wg.Done()

			// Skip if chunk has no data
			if chunk.startOffset >= fileSize {
				return
			}

			logger.Debug("Starting chunk %d download to %s", chunk.index, chunk.tempFile)

			// Create independent SSH connection for this chunk
			chunkSSHClient, err := sshConnectBastion(
				connParams.BastionUsername,
				connParams.BastionPassword,
				connParams.BastionHost,
				connParams.BastionPort,
			)
			if err != nil {
				chunk.err = fmt.Errorf("chunk %d: failed to create SSH connection: %v", chunk.index, err)
				return
			}
			defer chunkSSHClient.Close()

			// Create SFTP client with concurrent request pipelining for better throughput
			chunkSFTP, err := sftp.NewClient(chunkSSHClient,
				sftp.MaxPacket(32*1024),
				sftp.UseConcurrentReads(true),         // Enable pipelined reads
				sftp.MaxConcurrentRequestsPerFile(64), // Pipeline up to 64 requests
			)
			if err != nil {
				chunk.err = fmt.Errorf("chunk %d: failed to create SFTP client: %v", chunk.index, err)
				return
			}
			defer chunkSFTP.Close()

			// Open remote file
			remoteFile, err := chunkSFTP.Open(bastionPath)
			if err != nil {
				chunk.err = fmt.Errorf("chunk %d: failed to open remote file: %v", chunk.index, err)
				return
			}
			defer remoteFile.Close()

			// Seek to chunk start offset
			_, err = remoteFile.Seek(chunk.startOffset, io.SeekStart)
			if err != nil {
				chunk.err = fmt.Errorf("chunk %d: failed to seek: %v", chunk.index, err)
				return
			}

			// Create chunk temp file
			chunkFile, err := os.OpenFile(chunk.tempFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				chunk.err = fmt.Errorf("chunk %d: failed to create temp file: %v", chunk.index, err)
				return
			}
			defer chunkFile.Close()

			// Download chunk with large buffer and progress tracking
			bytesToRead := chunk.endOffset - chunk.startOffset
			limitedReader := io.LimitReader(remoteFile, bytesToRead)

			// Create progress tracking wrapper
			progressTracker := &simpleProgressReader{
				reader:     limitedReader,
				totalBytes: &totalBytesRead,
			}

			// Use 2MB buffer for efficient copying
			buffer := make([]byte, 2*1024*1024)
			n, err := io.CopyBuffer(chunkFile, progressTracker, buffer)
			if err != nil {
				chunk.err = fmt.Errorf("chunk %d: copy error: %v", chunk.index, err)
				return
			}

			// Verify chunk was fully read
			if n != bytesToRead {
				chunk.err = fmt.Errorf("chunk %d: incomplete read: expected %d bytes, got %d", chunk.index, bytesToRead, n)
				return
			}

			// Sync chunk to disk
			if err := chunkFile.Sync(); err != nil {
				chunk.err = fmt.Errorf("chunk %d: sync error: %v", chunk.index, err)
				return
			}

			logger.Debug("Completed chunk %d: %d bytes written to %s", chunk.index, n, chunk.tempFile)
		}(&chunks[i])
	}

	// Wait for all chunks
	wg.Wait()

	// Check for errors
	for _, chunk := range chunks {
		if chunk.err != nil {
			return chunk.err
		}
	}

	// Verify all chunk files exist and have correct sizes
	for _, chunk := range chunks {
		chunkFileInfo, err := os.Stat(chunk.tempFile)
		if err != nil {
			return fmt.Errorf("chunk %d temp file missing: %v", chunk.index, err)
		}

		expectedSize := chunk.endOffset - chunk.startOffset
		if chunkFileInfo.Size() != expectedSize {
			return fmt.Errorf("chunk %d size mismatch: expected %d bytes, got %d bytes",
				chunk.index, expectedSize, chunkFileInfo.Size())
		}

		logger.Debug("Verified chunk %d: %s (%d bytes)", chunk.index, chunk.tempFile, chunkFileInfo.Size())
	}

	// Combine chunks into final file
	logger.Debug("Combining %d chunk files into final file", numChunks)
	finalFile, err := os.OpenFile(tempFilePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create final file: %v", err)
	}
	defer finalFile.Close()

	// Pre-allocate final file
	if err := finalFile.Truncate(fileSize); err != nil {
		logger.Debug("Failed to pre-allocate final file: %v", err)
	}

	var totalCombined int64
	combineBuffer := make([]byte, 4*1024*1024) // 4MB buffer for combining

	for _, chunk := range chunks {
		logger.Debug("Combining chunk %d from %s", chunk.index, chunk.tempFile)

		chunkFile, err := os.Open(chunk.tempFile)
		if err != nil {
			return fmt.Errorf("failed to open chunk file %s: %v", chunk.tempFile, err)
		}

		// Copy chunk data to final file
		for {
			n, readErr := chunkFile.Read(combineBuffer)
			if readErr != nil && readErr != io.EOF {
				chunkFile.Close()
				return fmt.Errorf("failed to read from chunk file %s: %v", chunk.tempFile, readErr)
			}

			if n == 0 {
				break
			}

			written, writeErr := finalFile.Write(combineBuffer[:n])
			if writeErr != nil {
				chunkFile.Close()
				return fmt.Errorf("failed to write to final file: %v", writeErr)
			}

			if written != n {
				chunkFile.Close()
				return fmt.Errorf("partial write to final file: wrote %d of %d bytes", written, n)
			}

			totalCombined += int64(written)
		}

		chunkFile.Close()
		logger.Debug("Combined chunk %d: %d bytes", chunk.index, chunk.endOffset-chunk.startOffset)
	}

	// Verify combined file size
	if totalCombined != fileSize {
		return fmt.Errorf("combined file size mismatch: expected %d bytes, got %d bytes", fileSize, totalCombined)
	}

	// Sync final file to disk
	if err := finalFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync final file: %v", err)
	}
	finalFile.Close()

	// Verify final file on disk
	finalFileInfo, err := os.Stat(tempFilePath)
	if err != nil {
		return fmt.Errorf("failed to stat final file: %v", err)
	}
	if finalFileInfo.Size() != fileSize {
		return fmt.Errorf("final file size mismatch: expected %d bytes, got %d bytes", fileSize, finalFileInfo.Size())
	}

	// Rename to final path
	if err := os.Rename(tempFilePath, localPath); err != nil {
		return fmt.Errorf("failed to rename temp file: %v", err)
	}

	logger.Debug("Successfully combined all chunks into %s", localPath)
	return nil
}

// downloadFromBastionWithSCP uses native scp command to download from bastion (much faster than SFTP)
func downloadFromBastionWithSCP(connParams *ConnectionParams, bastionPath, localPath string, fileSize int64, fileName string) error {
	// Create temporary SSH key for passwordless scp
	tempKeyPath := filepath.Join(os.TempDir(), fmt.Sprintf("logcollector_temp_key_%d", time.Now().Unix()))
	tempPubKeyPath := tempKeyPath + ".pub"

	logger.Debug("Generating temporary SSH key pair for SCP transfer")

	// Generate ED25519 key pair (fast and secure)
	keyGenCmd := exec.Command("ssh-keygen", "-t", "ed25519", "-f", tempKeyPath, "-N", "", "-q")
	if err := keyGenCmd.Run(); err != nil {
		logger.Warn("Failed to generate temp SSH key, falling back to SFTP: %v", err)
		// Fall back to SFTP method
		return downloadFromBastionParallel(connParams, bastionPath, localPath, fileSize, 8, fileName)
	}
	defer os.Remove(tempKeyPath)
	defer os.Remove(tempPubKeyPath)

	// Read the public key
	pubKeyBytes, err := ioutil.ReadFile(tempPubKeyPath)
	if err != nil {
		logger.Warn("Failed to read temp public key, falling back to SFTP: %v", err)
		return downloadFromBastionParallel(connParams, bastionPath, localPath, fileSize, 8, fileName)
	}
	pubKey := strings.TrimSpace(string(pubKeyBytes))

	// Add public key to bastion's authorized_keys via existing SSH connection
	logger.Debug("Adding temporary key to bastion authorized_keys")
	addKeySession, err := connParams.BastionClient.NewSession()
	if err != nil {
		logger.Warn("Failed to create session for key setup, falling back to SFTP: %v", err)
		return downloadFromBastionParallel(connParams, bastionPath, localPath, fileSize, 8, fileName)
	}
	addKeyCmd := fmt.Sprintf("mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '%s' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys", pubKey)
	if err := addKeySession.Run(addKeyCmd); err != nil {
		addKeySession.Close()
		logger.Warn("Failed to add temp key to bastion, falling back to SFTP: %v", err)
		return downloadFromBastionParallel(connParams, bastionPath, localPath, fileSize, 8, fileName)
	}
	addKeySession.Close()

	// Ensure key is removed from authorized_keys when done
	defer func() {
		logger.Debug("Removing temporary key from bastion authorized_keys")
		removeKeySession, err := connParams.BastionClient.NewSession()
		if err == nil {
			// Use last 30 chars of pubkey as unique identifier to remove
			keyIdentifier := pubKey
			if len(pubKey) > 30 {
				keyIdentifier = pubKey[len(pubKey)-30:]
			}
			removeKeyCmd := fmt.Sprintf("grep -v '%s' ~/.ssh/authorized_keys > ~/.ssh/authorized_keys.tmp 2>/dev/null && mv ~/.ssh/authorized_keys.tmp ~/.ssh/authorized_keys || true", keyIdentifier)
			removeKeySession.Run(removeKeyCmd)
			removeKeySession.Close()
		}
	}()

	// Execute native SCP command with the temporary key
	logger.Info("Downloading via native SCP (expect 2-4 MB/s)...")
	logger.Debug("Executing: scp -i %s %s@%s:%s %s", tempKeyPath, connParams.BastionUsername, connParams.BastionHost, bastionPath, localPath)

	// Set platform-specific null device for known_hosts file
	knownHostsFile := "/dev/null"
	if runtime.GOOS == "windows" {
		knownHostsFile = "NUL"
	}

	scpCmd := exec.Command("scp",
		"-i", tempKeyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", fmt.Sprintf("UserKnownHostsFile=%s", knownHostsFile),
		"-P", fmt.Sprintf("%d", connParams.BastionPort),
		fmt.Sprintf("%s@%s:%s", connParams.BastionUsername, connParams.BastionHost, bastionPath),
		localPath,
	)

	// Capture output for debugging
	output, err := scpCmd.CombinedOutput()
	if err != nil {
		logger.Error("SCP command failed: %v, output: %s", err, string(output))
		return fmt.Errorf("SCP download failed: %v", err)
	}

	logger.Debug("SCP download completed successfully")
	return nil
}

// performFinalVerification does a thorough check of the downloaded file to ensure it has proper content
func performFinalVerification(filePath string, expectedSize int64) error {
	logger.Debug("Verifying %s...", filepath.Base(filePath))

	// Try to open and read the file
	verifyFile, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("cannot open file for verification: %v", err)
	}
	defer verifyFile.Close()

	// Check file size
	stat, err := verifyFile.Stat()
	if err != nil {
		return fmt.Errorf("cannot stat file during verification: %v", err)
	}

	actualSize := stat.Size()
	logger.Debug("- File size: %d bytes (%.2f MB)", actualSize, float64(actualSize)/(1024*1024))

	if actualSize != expectedSize {
		return fmt.Errorf("file size mismatch during verification: expected %d bytes, got %d bytes",
			expectedSize, actualSize)
	}
	logger.Debug("Size check passed")

	// Skip content checks for very small files (likely empty)
	// This is done by reading a larger sample from various parts of the file
	zeroCheckPositions := []struct {
		position int64
		label    string
	}{
		{0, "start"},
	}

	// Add more positions for larger files
	if actualSize > 1024*1024 { // For files > 1MB
		zeroCheckPositions = append(zeroCheckPositions,
			struct {
				position int64
				label    string
			}{actualSize / 4, "25%"},
			struct {
				position int64
				label    string
			}{actualSize / 2, "50%"},
			struct {
				position int64
				label    string
			}{actualSize * 3 / 4, "75%"},
			struct {
				position int64
				label    string
			}{actualSize - 4096, "end"})
	} else if actualSize > 4096 {
		// For smaller files, just check start, middle, end
		zeroCheckPositions = append(zeroCheckPositions,
			struct {
				position int64
				label    string
			}{actualSize / 2, "middle"},
			struct {
				position int64
				label    string
			}{actualSize - 1024, "end"})
	}

	// Check each position
	suspiciousPositions := 0
	for _, posInfo := range zeroCheckPositions {
		// Make sure position is valid
		if posInfo.position < 0 {
			posInfo.position = 0
		}
		if posInfo.position >= actualSize {
			posInfo.position = actualSize - 1
		}

		// Read sample
		sampleSize := int64(4096)
		if posInfo.position+sampleSize > actualSize {
			sampleSize = actualSize - posInfo.position
		}

		sample := make([]byte, sampleSize)
		_, err = verifyFile.ReadAt(sample, posInfo.position)
		if err != nil && err != io.EOF {
			logger.Debug("Warning: Failed to read sample at %s position: %v", posInfo.label, err)
			continue
		}

		// Count non-zero bytes
		nonZeroCount := 0
		for _, b := range sample {
			if b != 0 {
				nonZeroCount++
			}
		}

		zeroPercentage := 100.0 - (float64(nonZeroCount) / float64(len(sample)) * 100.0)
		logger.Debug("- %s position: %.1f%% zero bytes", posInfo.label, zeroPercentage)

		if zeroPercentage > 95 {
			suspiciousPositions++
			logger.Debug("WARNING: File appears to be mostly zeros at %s position (%.1f%% zeros)",
				posInfo.label, zeroPercentage)
		}
	}

	// If multiple positions have suspicious content, treat as an error
	if suspiciousPositions > 0 && suspiciousPositions == len(zeroCheckPositions) {
		return fmt.Errorf("file verification failed: content appears to be empty at all checked positions")
	}

	logger.Debug("Content validation passed")
	return nil
}

// isCommandSafeForSystemInfo validates that a command is safe (read-only)
func isCommandSafeForSystemInfo(command string) (bool, string) {
	// Normalize command to lowercase for checking
	cmdLower := strings.ToLower(strings.TrimSpace(command))

	// Remove sudo prefix for checking
	cmdLower = strings.TrimPrefix(cmdLower, "sudo ")
	cmdLower = strings.TrimPrefix(cmdLower, "sudo su - -c ")
	cmdLower = strings.Trim(cmdLower, "'\"")

	// List of forbidden keywords that indicate config changes or destructive operations
	forbiddenKeywords := []string{
		"configure", "config", "set", "create", "delete", "remove", "rm",
		"restart", "reboot", "shutdown", "kill", "stop", "start",
		"update", "upgrade", "patch", "apply", "edit", "modify",
		"add", "insert", "drop", "truncate", "destroy",
		"exec", "execute", "run", "launch",
		"scale", "rollout", "deploy",
	}

	// Check for forbidden keywords
	for _, keyword := range forbiddenKeywords {
		// Check if keyword appears as a standalone word (not part of another word)
		if strings.Contains(cmdLower, " "+keyword+" ") ||
			strings.HasPrefix(cmdLower, keyword+" ") ||
			strings.HasSuffix(cmdLower, " "+keyword) ||
			cmdLower == keyword {
			return false, fmt.Sprintf("Command contains forbidden keyword '%s' - only read-only commands (show/get/describe/list) are allowed", keyword)
		}
	}

	// Additional check for common safe command patterns
	safePatterns := []string{"kubectl get", "kubectl describe", "kubectl top", "kubectl logs", "show", "display", "list", "cat", "grep", "find", "ls", "ps", "netstat", "df", "du"}
	hasSafePattern := false
	for _, pattern := range safePatterns {
		if strings.Contains(cmdLower, pattern) {
			hasSafePattern = true
			break
		}
	}

	// If no safe pattern found, warn but allow (for custom commands)
	if !hasSafePattern {
		logger.Debug("Command does not match common safe patterns but no forbidden keywords detected: %s", command)
	}

	return true, ""
}

// collectSystemInfo executes system information commands and saves outputs to files on the remote server
func collectSystemInfo(awsClient *ssh.Client, systemInfoConfig struct {
	Enabled        bool                `yaml:"enabled"`
	OutputDir      string              `yaml:"outputDir"`
	CommandTimeout int                 `yaml:"commandTimeout"`
	Commands       []SystemInfoCommand `yaml:"commands"`
}, environment, username, tempDir, finalLogFileName string) error {
	if !systemInfoConfig.Enabled || len(systemInfoConfig.Commands) == 0 {
		logger.Debug("System info collection is disabled or no commands configured")
		return nil
	}

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  SYSTEM INFO - Collecting cluster information")
	logger.Info("%s", strings.Repeat("=", 70))

	// Validate and set timeout (default: 180, min: 60, max: 300)
	timeoutSeconds := systemInfoConfig.CommandTimeout
	if timeoutSeconds < 60 {
		logger.Warn("Command timeout %d seconds is too low, using minimum: 60 seconds", timeoutSeconds)
		timeoutSeconds = 60
	} else if timeoutSeconds > 300 {
		logger.Warn("Command timeout %d seconds is too high, using maximum: 300 seconds", timeoutSeconds)
		timeoutSeconds = 300
	} else if timeoutSeconds == 0 {
		timeoutSeconds = 180 // default
		logger.Debug("Using default command timeout: 180 seconds")
	} else {
		logger.Debug("Using configured command timeout: %d seconds", timeoutSeconds)
	}

	logger.Info("Starting general system information collection (timeout: %d seconds per command)...", timeoutSeconds)

	// Create the general info directory on the remote server inside the log collection directory
	logDir := fmt.Sprintf("%s/%s", tempDir, finalLogFileName)
	generalOutputDir := fmt.Sprintf("%s/%s", logDir, systemInfoConfig.OutputDir)

	logger.Debug("TempDir: %s", tempDir)
	logger.Debug("FinalLogFileName: %s", finalLogFileName)
	logger.Debug("LogDir: %s", logDir)
	logger.Debug("GeneralOutputDir: %s", generalOutputDir)

	// Create the general info directory on remote server
	session, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for general info directory: %v", err)
	}
	defer session.Close()

	mkdirCmd := fmt.Sprintf("mkdir -p %s", generalOutputDir)
	if err := executeCommandAsRoot(session, mkdirCmd); err != nil {
		return fmt.Errorf("failed to create general info directory %s on remote server: %v", generalOutputDir, err)
	}

	logger.Info("Created general info directory on remote server: %s", generalOutputDir)

	// Detect XIQ namespace: check if 'xiq' namespace exists, otherwise use environment name
	actualNamespace := environment
	nsSession, nsErr := awsClient.NewSession()
	if nsErr == nil {
		nsOutput, nsCheckErr := nsSession.CombinedOutput("sudo su - -c 'kubectl get namespaces --no-headers -o custom-columns=NAME:.metadata.name'")
		nsSession.Close()
		if nsCheckErr == nil {
			for _, ns := range strings.Split(strings.TrimSpace(string(nsOutput)), "\n") {
				if strings.TrimSpace(ns) == "xiq" {
					actualNamespace = "xiq"
					logger.Debug("Found 'xiq' namespace - using it for general info commands")
					break
				}
			}
		}
		if actualNamespace != "xiq" {
			logger.Debug("No 'xiq' namespace found - using environment namespace '%s' for general info commands", environment)
		}
	} else {
		logger.Debug("Failed to create session for namespace check: %v", nsErr)
	}

	// Add timestamp to filenames if desired
	timestamp := time.Now().Format("20060102_150405")

	successCount := 0
	totalCommands := len(systemInfoConfig.Commands)

	for i, cmd := range systemInfoConfig.Commands {
		logger.Info("Executing command %d/%d: %s", i+1, totalCommands, cmd.Name)

		// Apply template replacement to the command
		command := cmd.Command
		command = strings.ReplaceAll(command, "{environment}", actualNamespace)
		command = strings.ReplaceAll(command, "{username}", username)

		// Validate command safety
		isSafe, reason := isCommandSafeForSystemInfo(command)
		if !isSafe {
			logger.Warn("SKIPPING command '%s': %s", cmd.Name, reason)
			logger.Warn("  Command: %s", command)
			continue
		}

		// Create session for this command
		cmdSession, err := awsClient.NewSession()
		if err != nil {
			logger.Error("Failed to create session for command '%s': %v", cmd.Name, err)
			continue
		}

		// Execute the command as root with configurable timeout
		logger.Debug("Executing: %s", command)

		type cmdResult struct {
			output []byte
			err    error
		}
		resultChan := make(chan cmdResult, 1)

		// Run command in goroutine
		go func() {
			output, err := cmdSession.CombinedOutput(fmt.Sprintf("sudo su - -c '%s'", command))
			resultChan <- cmdResult{output: output, err: err}
		}()

		// Wait for command or timeout
		var output []byte
		select {
		case result := <-resultChan:
			output = result.output
			err = result.err
			cmdSession.Close()
		case <-time.After(time.Duration(timeoutSeconds) * time.Second):
			logger.Warn("Command '%s' timed out after %d seconds - interrupting", cmd.Name, timeoutSeconds)
			// Close the session to terminate the command
			cmdSession.Close()
			err = fmt.Errorf("command timed out after %d seconds", timeoutSeconds)
			output = []byte(fmt.Sprintf("Command timed out after %d seconds", timeoutSeconds))
			// Try to drain the result channel to avoid goroutine leak
			select {
			case <-resultChan:
			default:
			}
		}

		if err != nil {
			logger.Warn("Command '%s' failed: %v", cmd.Name, err)
			// Still save the output (might contain partial results or error info)
		}

		// Create filename from command name (sanitize for filesystem)
		filename := sanitizeFilename(cmd.Name) + "_" + timestamp + ".txt"
		filePath := fmt.Sprintf("%s/%s", generalOutputDir, filename)

		// Prepare file content with metadata
		fileContent := "# General System Information\n"
		fileContent += fmt.Sprintf("# Command: %s\n", cmd.Command)
		fileContent += fmt.Sprintf("# Description: %s\n", cmd.Description)
		fileContent += fmt.Sprintf("# Executed on: %s\n", time.Now().Format("2006-01-02 15:04:05"))
		fileContent += fmt.Sprintf("# Environment: %s\n", environment)
		fileContent += fmt.Sprintf("# Status: %s\n", func() string {
			if err != nil {
				return "Failed"
			}
			return "Success"
		}())
		fileContent += fmt.Sprintf("#%s\n\n", strings.Repeat("-", 60))

		if err != nil {
			fileContent += fmt.Sprintf("ERROR: %v\n\n", err)
		}

		fileContent += string(output)

		// Write to file on remote server using a new session
		writeSession, writeErr := awsClient.NewSession()
		if writeErr != nil {
			logger.Error("Failed to create session for writing file %s: %v", filename, writeErr)
			continue
		}

		// Use stdin pipe to stream content to the remote file, avoiding ARG_MAX limits
		// for large command outputs (e.g. pods_all_namespaces)
		stdinPipe, pipeErr := writeSession.StdinPipe()
		if pipeErr != nil {
			logger.Error("Failed to create stdin pipe for file %s: %v", filename, pipeErr)
			writeSession.Close()
			continue
		}

		// Start the remote command that reads from stdin
		writeCmd := fmt.Sprintf("sudo su - -c 'cat > %s'", filePath)
		if startErr := writeSession.Start(writeCmd); startErr != nil {
			logger.Error("Failed to start write command for file %s: %v", filename, startErr)
			stdinPipe.Close()
			writeSession.Close()
			continue
		}

		// Write content via stdin pipe (no size limit)
		_, pipeWriteErr := io.WriteString(stdinPipe, fileContent)
		stdinPipe.Close() // Must close to signal EOF to cat

		if pipeWriteErr != nil {
			logger.Error("Failed to write content for file %s: %v", filename, pipeWriteErr)
			writeSession.Close()
			continue
		}

		// Wait for the remote command to finish
		if waitErr := writeSession.Wait(); waitErr != nil {
			logger.Error("Failed to write file %s on remote server: %v", filename, waitErr)
			writeSession.Close()
			continue
		}
		writeSession.Close()

		// Verify file was created and get its size
		verifySession, verifyErr := awsClient.NewSession()
		if verifyErr == nil {
			sizeCmd := fmt.Sprintf("ls -la %s 2>/dev/null | awk '{print $5}' || echo '0'", filePath)
			if sizeOutput, sizeErr := verifySession.CombinedOutput(fmt.Sprintf("sudo su - -c '%s'", sizeCmd)); sizeErr == nil {
				fileSize := strings.TrimSpace(string(sizeOutput))
				if err != nil {
					logger.Warn("Saved %s (%s bytes) on remote server - command failed but output captured", filename, fileSize)
				} else {
					logger.Debug("Saved %s (%s bytes) on remote server", filename, fileSize)
					successCount++
				}
			} else {
				if err == nil {
					successCount++
				}
				logger.Debug("Saved %s on remote server", filename)
			}
			verifySession.Close()
		} else {
			if err == nil {
				successCount++
			}
			logger.Debug("Saved %s on remote server", filename)
		}
	}

	logger.Info("General info collection completed: %d/%d commands successful", successCount, totalCommands)
	logger.Info("General info files saved to remote directory: %s", generalOutputDir)

	return nil
}

// collectTemporalWorkflowInfo collects Temporal workflow debugging information from the admin pod
func collectTemporalWorkflowInfo(awsClient *ssh.Client, temporalConfig struct {
	Enabled              bool                `yaml:"enabled"`
	WorkflowIdPrefix     string              `yaml:"workflowIdPrefix"`
	NumberOfWorkflows    int                 `yaml:"numberOfWorkflows"`
	Namespace            string              `yaml:"namespace"`
	KubeNamespace        string              `yaml:"kubeNamespace"`
	FilterByOwnerID      bool                `yaml:"filterByOwnerID"`
	WorkflowIdKeyword    string              `yaml:"workflowIdKeyword"`
	WorkflowActivitySets map[string][]string `yaml:"workflowActivitySets"`
	OwnerID              string              `yaml:"-"`
}, forceAllActivities bool, environment, username, tempDir, finalLogFileName string) error {
	if !temporalConfig.Enabled {
		logger.Debug("Temporal workflow collection is disabled")
		return nil
	}

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  TEMPORAL WORKFLOWS - Collecting workflow execution data")
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("Starting Temporal workflow information collection...")

	// Set defaults
	temporalNamespace := temporalConfig.Namespace
	if temporalNamespace == "" {
		temporalNamespace = "configuration"
	}
	numberOfWorkflows := temporalConfig.NumberOfWorkflows
	if numberOfWorkflows <= 0 {
		numberOfWorkflows = 3
	}
	if numberOfWorkflows > 20 {
		numberOfWorkflows = 20
	}

	kubeNamespace := temporalConfig.KubeNamespace
	if kubeNamespace == "" {
		kubeNamespace = "common"
	}

	// Create the Temporal output directory on the remote server inside the log collection directory
	logDir := fmt.Sprintf("%s/%s", tempDir, finalLogFileName)
	temporalOutputDir := fmt.Sprintf("%s/Temporal", logDir)

	session, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for temporal directory: %v", err)
	}
	defer session.Close()

	mkdirCmd := fmt.Sprintf("mkdir -p %s", temporalOutputDir)
	if err := executeCommandAsRoot(session, mkdirCmd); err != nil {
		return fmt.Errorf("failed to create temporal directory: %v", err)
	}
	logger.Info("Created Temporal output directory: %s", temporalOutputDir)

	// Step 1: Find the temporal admin pod
	logger.Info("Discovering Temporal admin pod in '%s' namespace...", kubeNamespace)
	podSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for pod discovery: %v", err)
	}

	podCmd := fmt.Sprintf("kubectl get pods -n %s --no-headers | grep temporal-admintools | grep Running | head -1 | awk '{print \\$1}'", kubeNamespace)
	podOutput, err := podSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", podCmd))
	podSession.Close()
	if err != nil {
		// Log the output for debugging, then return error
		logger.Debug("Pod discovery command output: %s", strings.TrimSpace(string(podOutput)))
		return fmt.Errorf("failed to discover temporal admin pod: %v", err)
	}

	adminPod := strings.TrimSpace(string(podOutput))
	if adminPod == "" {
		return fmt.Errorf("no running temporal-admintools pod found in '%s' namespace", kubeNamespace)
	}
	logger.Info("Found Temporal admin pod: %s", adminPod)

	// Step 2: List workflows
	logger.Info("Listing workflows in namespace '%s'...", temporalNamespace)

	// Optional ownerID filter — uses the temporal CLI visibility query:
	//   temporal workflow list --query 'OwnerId="<ownerID>"'
	ownerQueryFlag := ""
	if temporalConfig.FilterByOwnerID && temporalConfig.OwnerID != "" {
		ownerQueryFlag = fmt.Sprintf(` --query 'OwnerId=\"%s\"'`, temporalConfig.OwnerID)
		logger.Info("Filtering workflows by ownerID: %s", temporalConfig.OwnerID)
	} else if temporalConfig.FilterByOwnerID {
		logger.Warn("filterByOwnerID is enabled but no ownerID was resolved — listing all workflows")
	}

	// First: get the plain text tabular listing (human-readable, used for parsing workflow IDs)
	listSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for workflow listing: %v", err)
	}

	listCmd := fmt.Sprintf("kubectl exec %s -n %s -- temporal workflow list --namespace %s%s 2>/dev/null",
		adminPod, kubeNamespace, temporalNamespace, ownerQueryFlag)
	listOutput, err := listSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", listCmd))
	listSession.Close()
	if err != nil {
		return fmt.Errorf("failed to list temporal workflows: %v\nOutput: %s", err, string(listOutput))
	}

	// Save the tabular workflow listing as workflow_list.txt
	writeFileToRemote(awsClient, fmt.Sprintf("%s/workflow_list.txt", temporalOutputDir),
		fmt.Sprintf("# Temporal Workflow List\n# Namespace: %s\n# Collected: %s\n# Filter: %s\n%s\n\n%s",
			temporalNamespace, time.Now().Format("2006-01-02 15:04:05"),
			func() string {
				var parts []string
				if temporalConfig.FilterByOwnerID && temporalConfig.OwnerID != "" {
					parts = append(parts, "ownerID="+temporalConfig.OwnerID)
				}
				if temporalConfig.WorkflowIdPrefix != "" {
					parts = append(parts, "prefix="+temporalConfig.WorkflowIdPrefix)
				}
				if temporalConfig.WorkflowIdKeyword != "" {
					parts = append(parts, "workflowIdKeyword="+temporalConfig.WorkflowIdKeyword)
				}
				if len(parts) == 0 {
					return "none"
				}
				return strings.Join(parts, ", ")
			}(),
			strings.Repeat("-", 60),
			string(listOutput)))

	// Step 3: Extract workflow IDs from the tabular listing
	listOutputStr := string(listOutput)
	logger.Debug("Workflow list output (first 500 chars): %s", func() string {
		if len(listOutputStr) > 500 {
			return listOutputStr[:500] + "..."
		}
		return listOutputStr
	}())
	if temporalConfig.WorkflowIdKeyword != "" {
		logger.Info("Filtering workflows to those containing '%s' in their ID (workflowIdKeyword)", temporalConfig.WorkflowIdKeyword)
	}
	workflowIDs := extractWorkflowIDs(listOutputStr, temporalConfig.WorkflowIdPrefix, temporalConfig.WorkflowIdKeyword, numberOfWorkflows)
	if len(workflowIDs) == 0 {
		logger.Warn("No workflow IDs found matching the criteria")
		writeFileToRemote(awsClient, fmt.Sprintf("%s/no_workflows_found.txt", temporalOutputDir),
			fmt.Sprintf("No workflows found matching criteria.\nPrefix filter: '%s'\nWorkflow ID keyword filter: '%s'\nNamespace: %s\nRaw listing output:\n%s\n",
				temporalConfig.WorkflowIdPrefix, temporalConfig.WorkflowIdKeyword, temporalNamespace, listOutputStr))
		return nil
	}

	logger.Info("Found %d workflow(s) to collect information for", len(workflowIDs))
	for i, wfID := range workflowIDs {
		logger.Info("  %d. %s", i+1, wfID)
	}

	// Step 4: For each workflow, collect detailed information.
	if err := collectWorkflowDetails(awsClient, adminPod, kubeNamespace, temporalNamespace, workflowIDs, temporalConfig.WorkflowActivitySets, forceAllActivities, temporalOutputDir, "deploy_workflows"); err != nil {
		return err
	}

	logger.Info("Temporal workflow collection completed: %d workflow(s) collected", len(workflowIDs))
	logger.Info("Temporal data saved to remote directory: %s", temporalOutputDir)

	return nil
}

// collectWorkflowDetails fetches and writes detailed information (input/output/activities/detailed
// history) for each given workflow ID. Instead of issuing one remote `kubectl exec` per field
// (input/output/activities/etc, which was fragile due to nested shell quoting and repeated
// round-trips), it fetches the full event history JSON once per workflow and decodes everything
// locally. Shared by collectTemporalWorkflowInfo (deploy-site/deploy-device style workflows) and
// collectZtfOnboardWorkflows (ztf-onboard-<serial>-<ownerID>-<hash> workflows).
func collectWorkflowDetails(awsClient *ssh.Client, adminPod, kubeNamespace, temporalNamespace string, workflowIDs []string, workflowActivitySets map[string][]string, forceAllActivities bool, temporalOutputDir, reportLabel string) error {
	var flaggedActivities []TemporalActivityIssue
	var missingActivities []TemporalMissingActivity
	statusesChecked := 0
	// Same success set the analytics report uses, so the two reports cannot disagree.
	successStatuses := globalTemporalAnalysisConfig.successStatusSet()

	for i, workflowID := range workflowIDs {
		logger.Info("Collecting data for workflow %d/%d: %s", i+1, len(workflowIDs), workflowID)

		// Create a sanitized filename from workflow ID
		safeWfID := sanitizeFilename(workflowID)
		wfOutputFile := fmt.Sprintf("%s/%s.txt", temporalOutputDir, safeWfID)

		logger.Debug("  Fetching workflow event history...")
		historyCmd := fmt.Sprintf(`kubectl exec %s -n %s -- temporal workflow show --namespace %s --workflow-id "%s" --output json 2>&1`,
			adminPod, kubeNamespace, temporalNamespace, workflowID)
		historyOutput := executeTemporalCommand(awsClient, historyCmd)

		var history struct {
			Events []map[string]interface{} `json:"events"`
		}
		historyParsed := false
		if trimmed := strings.TrimSpace(historyOutput); strings.HasPrefix(trimmed, "{") {
			if jsonErr := json.Unmarshal([]byte(trimmed), &history); jsonErr == nil {
				historyParsed = true
			} else {
				logger.Warn("  Failed to parse workflow history JSON for %s: %v", workflowID, jsonErr)
			}
		} else {
			logger.Warn("  Workflow history output for %s was not valid JSON", workflowID)
		}

		var wfContent strings.Builder
		wfContent.WriteString("# Temporal Workflow Details\n")
		wfContent.WriteString(fmt.Sprintf("# Workflow ID: %s\n", workflowID))
		wfContent.WriteString(fmt.Sprintf("# Namespace: %s\n", temporalNamespace))
		wfContent.WriteString(fmt.Sprintf("# Collected: %s\n", time.Now().Format("2006-01-02 15:04:05")))
		wfContent.WriteString(fmt.Sprintf("#%s\n\n", strings.Repeat("-", 60)))

		if !historyParsed {
			wfContent.WriteString("Failed to fetch or parse workflow event history.\n\n")
			wfContent.WriteString(historyOutput + "\n")
			writeFileToRemote(awsClient, wfOutputFile, wfContent.String())
			logger.Warn("  Skipping activity collection for %s (history unavailable)", workflowID)
			continue
		}

		// 4a: Workflow Input
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n")
		wfContent.WriteString("  WORKFLOW INPUT\n")
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n\n")
		wfContent.WriteString(extractWorkflowStartInput(history.Events) + "\n\n")

		// 4b: Workflow Output
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n")
		wfContent.WriteString("  WORKFLOW OUTPUT\n")
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n\n")
		workflowOutput := extractWorkflowCompletedOutput(history.Events)
		wfContent.WriteString(workflowOutput + "\n\n")

		resultIssues, resultChecked := extractWorkflowResultFailures(workflowOutput, successStatuses)
		statusesChecked += resultChecked
		for i := range resultIssues {
			resultIssues[i].WorkflowID = workflowID
		}
		flaggedActivities = append(flaggedActivities, resultIssues...)

		// 4c: List all activities discovered in the history (informational overview)
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n")
		wfContent.WriteString("  ACTIVITIES\n")
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n\n")
		discoveredActivities := listScheduledActivities(history.Events)
		if len(discoveredActivities) == 0 {
			wfContent.WriteString("No activities found for this workflow.\n\n")
		} else {
			for _, a := range discoveredActivities {
				wfContent.WriteString(fmt.Sprintf("%s  %s  %s\n", a.EventID, a.EventType, a.Name))
			}
			wfContent.WriteString("\n")
		}

		// 4d: Resolve the activity set for this workflow from config.yaml's
		// workflowActivitySets (matched by longest workflow ID prefix). For each
		// configured activity, write separate {Activity}_input.txt / _output.txt / _status.txt files.
		// A configured list containing "ALL" (case-insensitive) collects every activity
		// discovered in the workflow's event history instead of naming each one.
		activityNames, matchedKey := resolveActivitySetForWorkflow(workflowID, workflowActivitySets)
		usingAll := forceAllActivities
		if !usingAll {
			for _, n := range activityNames {
				if strings.EqualFold(strings.TrimSpace(n), "ALL") {
					usingAll = true
					break
				}
			}
		}
		if usingAll {
			activityNames = nil
		}
		if len(activityNames) == 0 {
			// No configured activity set matched (or "ALL"/--temporal --all was specified) —
			// fall back to all activities discovered in the history
			seen := make(map[string]bool)
			for _, a := range discoveredActivities {
				if a.Name != "" && !seen[a.Name] {
					seen[a.Name] = true
					activityNames = append(activityNames, a.Name)
				}
			}
			if forceAllActivities {
				logger.Info("  --temporal --all: collecting all %d discovered activities for workflow %s", len(activityNames), workflowID)
			} else if usingAll {
				logger.Info("  workflowActivitySets['%s'] = ALL — collecting all %d discovered activities for workflow %s", matchedKey, len(activityNames), workflowID)
			} else if len(activityNames) > 0 {
				logger.Warn("  No workflowActivitySets entry matched workflow ID '%s' — falling back to %d discovered activities", workflowID, len(activityNames))
			}
		} else {
			logger.Info("  Matched workflowActivitySets['%s'] (%d activities) for workflow %s", matchedKey, len(activityNames), workflowID)
		}

		if len(activityNames) > 0 {
			activitiesDir := fmt.Sprintf("%s/%s_activities", temporalOutputDir, safeWfID)
			if mkdirActSession, mkErr := awsClient.NewSession(); mkErr == nil {
				executeCommandAsRoot(mkdirActSession, fmt.Sprintf("mkdir -p %s", activitiesDir))
				mkdirActSession.Close()
			}

			var summaryLines []string
			for _, activityName := range activityNames {
				occurrences := findScheduledEventsByName(history.Events, activityName)
				if len(occurrences) == 0 {
					logger.Warn("  Activity '%s' not found in workflow %s history", activityName, workflowID)
					summaryLines = append(summaryLines, fmt.Sprintf("%s | NOT_FOUND", activityName))
					missingActivities = append(missingActivities, TemporalMissingActivity{WorkflowID: workflowID, Activity: activityName})
					continue
				}

				for idx, schedEvent := range occurrences {
					attempt := idx + 1
					sid := asString(schedEvent["eventId"])

					inputText := "No input data found"
					if attrs, ok := schedEvent["activityTaskScheduledEventAttributes"].(map[string]interface{}); ok {
						inputText = decodePayloadsField(attrs, "input")
					}

					outputText := "No output data found or workflow still running"
					var eventTypes []string
					var failureText string
					for _, rev := range findEventsByScheduledID(history.Events, sid) {
						et, _ := rev["eventType"].(string)
						if et != "" {
							eventTypes = append(eventTypes, et)
						}
						switch et {
						case "EVENT_TYPE_ACTIVITY_TASK_COMPLETED":
							if compAttrs, ok := rev["activityTaskCompletedEventAttributes"].(map[string]interface{}); ok {
								outputText = decodePayloadsField(compAttrs, "result")
							}
						case "EVENT_TYPE_ACTIVITY_TASK_FAILED":
							if failAttrs, ok := rev["activityTaskFailedEventAttributes"].(map[string]interface{}); ok {
								if failure, ok := failAttrs["failure"]; ok {
									if b, mErr := json.MarshalIndent(failure, "", "  "); mErr == nil {
										failureText = string(b)
									}
								}
							}
						}
					}

					statusSummary := "SCHEDULED_ONLY_OR_NO_STATUS"
					if len(eventTypes) > 0 {
						statusSummary = strings.Join(eventTypes, ", ")
					}

					// Flag activity outputs reporting a non-success status (e.g. ProvisionConfiguration
					// returning anything other than COMPLETED_SUCCESS).
					payloadIssues, checked := extractActivityStatusFailures(outputText, successStatuses)
					statusesChecked += checked
					for i := range payloadIssues {
						payloadIssues[i].WorkflowID = workflowID
						payloadIssues[i].Activity = activityName
						payloadIssues[i].ScheduledID = sid
						payloadIssues[i].Attempt = attempt
						logger.Warn("  [FLAGGED] %s", formatTemporalActivityIssue(payloadIssues[i]))
					}
					flaggedActivities = append(flaggedActivities, payloadIssues...)

					var statusBuilder strings.Builder
					statusBuilder.WriteString(fmt.Sprintf("Activity: %s\n", activityName))
					statusBuilder.WriteString(fmt.Sprintf("Scheduled Event ID: %s\n", sid))
					statusBuilder.WriteString(fmt.Sprintf("Attempt: %d\n", attempt))
					statusBuilder.WriteString(fmt.Sprintf("Status: %s\n", statusSummary))
					for _, pi := range payloadIssues {
						statusBuilder.WriteString(fmt.Sprintf("Output Status: FLAGGED (%s)\n", pi.Status))
						if pi.StatusMessage != "" {
							statusBuilder.WriteString(fmt.Sprintf("Output Status Message: %s\n", pi.StatusMessage))
						}
					}
					if failureText != "" {
						statusBuilder.WriteString("\nFailure Details:\n")
						statusBuilder.WriteString(failureText + "\n")
					}

					base := sanitizeFilename(activityName)
					suffix := ""
					if len(occurrences) > 1 {
						suffix = fmt.Sprintf("_attempt%d", attempt)
					}

					writeFileToRemote(awsClient, fmt.Sprintf("%s/%s%s_input.txt", activitiesDir, base, suffix), inputText)
					writeFileToRemote(awsClient, fmt.Sprintf("%s/%s%s_output.txt", activitiesDir, base, suffix), outputText)
					writeFileToRemote(awsClient, fmt.Sprintf("%s/%s%s_status.txt", activitiesDir, base, suffix), statusBuilder.String())

					summaryLine := fmt.Sprintf("%s | sid=%s | attempt=%d | status=%s", activityName, sid, attempt, statusSummary)
					for _, pi := range payloadIssues {
						summaryLine += fmt.Sprintf(" | FLAGGED output status=%s", pi.Status)
					}
					summaryLines = append(summaryLines, summaryLine)
				}
			}

			writeFileToRemote(awsClient, fmt.Sprintf("%s/summary.txt", activitiesDir), strings.Join(summaryLines, "\n")+"\n")
			logger.Info("  Saved %d activity file set(s) to: %s", len(summaryLines), activitiesDir)
		}

		// Write the complete workflow overview file
		writeFileToRemote(awsClient, wfOutputFile, wfContent.String())
		logger.Info("  Saved workflow data to: %s", wfOutputFile)

		// 4e: Detailed workflow event history (--detailed flag) — separate file
		logger.Debug("  Collecting detailed workflow event history...")
		detailedCmd := fmt.Sprintf(`kubectl exec %s -n %s -- temporal workflow show --namespace %s --workflow-id "%s" --detailed`,
			adminPod, kubeNamespace, temporalNamespace, workflowID)
		detailedOutput := executeTemporalCommand(awsClient, detailedCmd)

		var detailedContent strings.Builder
		detailedContent.WriteString("# Temporal Workflow Detailed Event History\n")
		detailedContent.WriteString(fmt.Sprintf("# Workflow ID: %s\n", workflowID))
		detailedContent.WriteString(fmt.Sprintf("# Namespace: %s\n", temporalNamespace))
		detailedContent.WriteString(fmt.Sprintf("# Collected: %s\n", time.Now().Format("2006-01-02 15:04:05")))
		detailedContent.WriteString(fmt.Sprintf("#%s\n\n", strings.Repeat("-", 60)))
		detailedContent.WriteString(decodeDetailedHistoryPayloads(detailedOutput) + "\n")

		detailedOutputFile := fmt.Sprintf("%s/%s_detailed.txt", temporalOutputDir, safeWfID)
		writeFileToRemote(awsClient, detailedOutputFile, detailedContent.String())
		logger.Info("  Saved detailed workflow history to: %s", detailedOutputFile)
	}

	writeTemporalActivityStatusReport(awsClient, temporalOutputDir, reportLabel, workflowIDs, flaggedActivities, missingActivities, statusesChecked)

	return nil
}

// writeTemporalActivityStatusReport prints the flagged-activity verdict to the console and saves
// the same summary alongside the collected workflow data so it travels with the archive.
// statusesChecked and missing distinguish a genuine all-clear from "nothing was actually validated".
func writeTemporalActivityStatusReport(awsClient *ssh.Client, temporalOutputDir, reportLabel string, workflowIDs []string, flagged []TemporalActivityIssue, missing []TemporalMissingActivity, statusesChecked int) {
	var report strings.Builder
	report.WriteString("# Temporal Workflow Activity Status Validation\n")
	report.WriteString(fmt.Sprintf("# Scope: %s\n", reportLabel))
	report.WriteString(fmt.Sprintf("# Collected: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	report.WriteString(fmt.Sprintf("# Workflows checked: %d\n", len(workflowIDs)))
	report.WriteString(fmt.Sprintf("# Activity outputs validated: %d\n", statusesChecked))
	report.WriteString(fmt.Sprintf("# Activities flagged: %d\n", len(flagged)))
	report.WriteString(fmt.Sprintf("# Activities that never ran: %d\n", len(missing)))
	report.WriteString(fmt.Sprintf("#%s\n\n", strings.Repeat("-", 60)))

	logger.Info("%s", strings.Repeat("-", 70))
	switch {
	case len(flagged) > 0:
		logger.Warn("  TEMPORAL ANALYSIS (%s): %d of %d checked status value(s) were non-success", reportLabel, len(flagged), statusesChecked)
		report.WriteString("FLAGGED ACTIVITIES:\n")
		for _, issue := range flagged {
			logger.Warn("    - %s", formatTemporalActivityIssue(issue))
			report.WriteString("  " + formatTemporalActivityIssue(issue) + "\n")
			report.WriteString(fmt.Sprintf("    scheduledEventId=%s attempt=%d deviceId=%s\n", issue.ScheduledID, issue.Attempt, issue.DeviceID))
		}
	case statusesChecked == 0:
		logger.Warn("  TEMPORAL ANALYSIS (%s): NOT VALIDATED - no collected activity output contained a status field", reportLabel)
		report.WriteString("NOT VALIDATED: no collected activity output contained a status field.\n")
		report.WriteString("This is not an all-clear.\n")
	default:
		logger.Info("  TEMPORAL ANALYSIS (%s): all %d checked status value(s) were successful", reportLabel, statusesChecked)
		report.WriteString(fmt.Sprintf("All %d checked status value(s) (activity outputs + workflow results) were successful.\n", statusesChecked))
	}

	if len(missing) > 0 {
		order, byActivity := groupMissingByActivity(missing)
		logger.Warn("  MISSING ACTIVITIES (%s): expected to run, but never started. Their status could NOT be checked.", reportLabel)
		report.WriteString("\nMISSING ACTIVITIES\n")
		report.WriteString("These activities were expected to run but never started, so no status was\n")
		report.WriteString("available to validate. This is NOT the same as a successful activity.\n\n")
		for _, activity := range order {
			wfs := byActivity[activity]
			logger.Warn("    '%s' never ran in %d of %d workflow(s):", activity, len(wfs), len(workflowIDs))
			report.WriteString(fmt.Sprintf("  '%s' never ran in %d of %d workflow(s):\n", activity, len(wfs), len(workflowIDs)))
			for i, wf := range wfs {
				logger.Warn("        %d. %s", i+1, wf)
				report.WriteString(fmt.Sprintf("      %d. %s\n", i+1, wf))
			}
			report.WriteString("\n")
		}
	}
	logger.Info("%s", strings.Repeat("-", 70))

	fileLabel := sanitizeFilename(reportLabel)
	writeFileToRemote(awsClient, fmt.Sprintf("%s/temporal_activity_status_report_%s.txt", temporalOutputDir, fileLabel), report.String())
}

// collectZtfOnboardWorkflows collects Temporal ZTF (Zero-Touch onboarding) workflow data.
// Workflow IDs follow the shape ztf-onboard-<serial>-<ownerID>-<hash>, e.g.
// "ztf-onboard-JA142040G-00471-1029-2817cb". If ztfConfig.SerialNumbers is set (comma-separated),
// only workflows for those specific serials are collected (multiple serials = multiple filters
// applied, OR'd together). If empty, falls back to the most recent NumberOfWorkflows matching
// workflows (newest-first, same convention as temporalWorkflowCollection.numberOfWorkflows).
func collectZtfOnboardWorkflows(awsClient *ssh.Client, ztfConfig ZtfOnboardWorkflowConfig, workflowActivitySets map[string][]string, forceAllActivities bool, tempDir, finalLogFileName string) error {
	if !ztfConfig.Enabled {
		logger.Debug("ZTF onboarding workflow collection is disabled")
		return nil
	}

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  ZTF ONBOARDING WORKFLOWS - Collecting onboarding workflow data")
	logger.Info("%s", strings.Repeat("=", 70))

	workflowIdPrefix := ztfConfig.WorkflowIdPrefix
	if workflowIdPrefix == "" {
		workflowIdPrefix = "ztf-onboard-"
	}

	temporalNamespace := ztfConfig.Namespace
	if temporalNamespace == "" {
		temporalNamespace = "configuration"
	}

	kubeNamespace := ztfConfig.KubeNamespace
	if kubeNamespace == "" {
		kubeNamespace = "common"
	}

	numberOfWorkflows := ztfConfig.NumberOfWorkflows
	if numberOfWorkflows <= 0 {
		numberOfWorkflows = 3
	}
	if numberOfWorkflows > 20 {
		numberOfWorkflows = 20
	}

	// Parse comma-separated serial numbers (trimmed, blanks dropped)
	var serialNumbers []string
	for _, s := range strings.Split(ztfConfig.SerialNumbers, ",") {
		if s = strings.TrimSpace(s); s != "" {
			serialNumbers = append(serialNumbers, s)
		}
	}

	// Create the Temporal output directory on the remote server inside the log collection directory
	logDir := fmt.Sprintf("%s/%s", tempDir, finalLogFileName)
	temporalOutputDir := fmt.Sprintf("%s/Temporal", logDir)

	dirSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for ztf onboard directory: %v", err)
	}
	mkdirCmd := fmt.Sprintf("mkdir -p %s", temporalOutputDir)
	mkdirErr := executeCommandAsRoot(dirSession, mkdirCmd)
	dirSession.Close()
	if mkdirErr != nil {
		return fmt.Errorf("failed to create temporal directory: %v", mkdirErr)
	}

	// Discover the temporal-admintools pod
	logger.Info("Discovering Temporal admin pod in '%s' namespace...", kubeNamespace)
	podSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for pod discovery: %v", err)
	}
	podCmd := fmt.Sprintf("kubectl get pods -n %s --no-headers | grep temporal-admintools | grep Running | head -1 | awk '{print \\$1}'", kubeNamespace)
	podOutput, err := podSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", podCmd))
	podSession.Close()
	if err != nil {
		logger.Debug("Pod discovery command output: %s", strings.TrimSpace(string(podOutput)))
		return fmt.Errorf("failed to discover temporal admin pod: %v", err)
	}
	adminPod := strings.TrimSpace(string(podOutput))
	if adminPod == "" {
		return fmt.Errorf("no running temporal-admintools pod found in '%s' namespace", kubeNamespace)
	}
	logger.Info("Found Temporal admin pod: %s", adminPod)

	// ZTF workflows have no OwnerId search attribute in Temporal (unlike deploy workflows),
	// so a server-side OwnerId query returns zero ZTF results. Filter client-side instead,
	// using the numeric ownerID embedded as a dash-bounded token in the workflow ID string
	// (e.g. "-1204-" in "ztf-onboard-JA062313G-00275-1204-ebc4b4").
	numericOwnerID := ""
	if ztfConfig.FilterByOwnerID && ztfConfig.OwnerID != "" {
		numericOwnerID = ztfConfig.OwnerID
		logger.Info("Filtering ZTF onboarding workflows by ownerID: %s", numericOwnerID)
	} else if ztfConfig.FilterByOwnerID {
		logger.Warn("filterByOwnerID is enabled but no ownerID was resolved — collecting all ZTF workflows")
	}

	listSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for ztf onboard workflow listing: %v", err)
	}
	listCmd := fmt.Sprintf("kubectl exec %s -n %s -- temporal workflow list --namespace %s 2>/dev/null",
		adminPod, kubeNamespace, temporalNamespace)
	listOutput, err := listSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", listCmd))
	listSession.Close()
	if err != nil {
		return fmt.Errorf("failed to list temporal workflows: %v\nOutput: %s", err, string(listOutput))
	}
	listOutputStr := string(listOutput)

	// Extract all workflow IDs matching the ztf-onboard prefix first (no count cutoff yet —
	// the cutoff/serial filtering below decides the final set).
	allMatchingIDs := extractWorkflowIDs(listOutputStr, workflowIdPrefix, "", 1000)
	if numericOwnerID != "" {
		ownerToken := "-" + numericOwnerID + "-"
		var filtered []string
		for _, id := range allMatchingIDs {
			if strings.Contains(id, ownerToken) {
				filtered = append(filtered, id)
			}
		}
		logger.Info("Owner ID filter (%s): %d of %d ZTF workflow(s) match", numericOwnerID, len(filtered), len(allMatchingIDs))
		allMatchingIDs = filtered
	}

	var selectedIDs []string
	var filterDescription string
	if len(serialNumbers) > 0 {
		logger.Info("Filtering ZTF onboarding workflows to serial number(s): %s", strings.Join(serialNumbers, ", "))
		selectedIDs = filterWorkflowIDsBySerialNumbers(allMatchingIDs, serialNumbers)
		filterDescription = "serialNumbers=" + strings.Join(serialNumbers, ",")
	} else {
		logger.Info("No serialNumbers configured — collecting the most recent %d ZTF onboarding workflow(s)", numberOfWorkflows)
		if len(allMatchingIDs) > numberOfWorkflows {
			selectedIDs = allMatchingIDs[:numberOfWorkflows]
		} else {
			selectedIDs = allMatchingIDs
		}
		filterDescription = fmt.Sprintf("most recent %d (no serialNumbers configured)", numberOfWorkflows)
	}

	writeFileToRemote(awsClient, fmt.Sprintf("%s/ztf_onboard_workflow_list.txt", temporalOutputDir),
		fmt.Sprintf("# ZTF Onboarding Workflow List\n# Namespace: %s\n# Collected: %s\n# Prefix: %s\n# Filter: %s\n%s\n\n%s",
			temporalNamespace, time.Now().Format("2006-01-02 15:04:05"), workflowIdPrefix, filterDescription,
			strings.Repeat("-", 60), listOutputStr))

	if len(selectedIDs) == 0 {
		logger.Warn("No ZTF onboarding workflows found matching criteria")
		writeFileToRemote(awsClient, fmt.Sprintf("%s/no_ztf_onboard_workflows_found.txt", temporalOutputDir),
			fmt.Sprintf("No ZTF onboarding workflows found.\nPrefix: %s\nSerial numbers: %s\nNamespace: %s\n",
				workflowIdPrefix, ztfConfig.SerialNumbers, temporalNamespace))
		return nil
	}

	logger.Info("Found %d ZTF onboarding workflow(s) to collect information for", len(selectedIDs))
	for i, wfID := range selectedIDs {
		logger.Info("  %d. %s", i+1, wfID)
	}

	if err := collectWorkflowDetails(awsClient, adminPod, kubeNamespace, temporalNamespace, selectedIDs, workflowActivitySets, forceAllActivities, temporalOutputDir, "ztf_onboard"); err != nil {
		return err
	}

	logger.Info("ZTF onboarding workflow collection completed: %d workflow(s) collected", len(selectedIDs))
	logger.Info("ZTF onboarding data saved to remote directory: %s", temporalOutputDir)

	return nil
}

// filterWorkflowIDsBySerialNumbers keeps workflow IDs containing "-<serial>-" (case-insensitive)
// for ANY of the given serial numbers. Matching on the dash-bounded substring (rather than
// positional splitting) correctly handles serial numbers that themselves contain dashes
// (e.g. "JA142040G-00471" in "ztf-onboard-JA142040G-00471-1029-2817cb").
func filterWorkflowIDsBySerialNumbers(workflowIDs []string, serialNumbers []string) []string {
	var filtered []string
	for _, id := range workflowIDs {
		lowerID := strings.ToLower(id)
		for _, serial := range serialNumbers {
			needle := "-" + strings.ToLower(serial) + "-"
			if strings.Contains(lowerID, needle) {
				filtered = append(filtered, id)
				break
			}
		}
	}
	return filtered
}

// executeTemporalCommand runs a command via SSH and returns the output string
func executeTemporalCommand(awsClient *ssh.Client, command string) string {
	session, err := awsClient.NewSession()
	if err != nil {
		return fmt.Sprintf("ERROR: Failed to create SSH session: %v", err)
	}
	defer session.Close()

	// Use heredoc with single-quoted delimiter to avoid ALL quoting issues.
	// Commands contain nested single quotes (bash -c '...') and double quotes (jq expressions),
	// so neither sudo su - -c '...' nor "..." wrapping works safely.
	// <<'TEMPORAL_EOF' passes the command verbatim with zero shell expansion.
	heredocCmd := fmt.Sprintf("sudo bash <<'TEMPORAL_EOF'\n%s\nTEMPORAL_EOF", command)
	output, err := session.CombinedOutput(heredocCmd)
	if err != nil {
		// Still return output - it may contain useful partial data or error messages
		if len(output) > 0 {
			return string(output)
		}
		return fmt.Sprintf("ERROR: Command failed: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// ============================================================================
// Temporal event history helpers — parse `temporal workflow show -o json`
// output locally (no jq/python3 dependency on the remote host) and decode
// Payload data (base64 + optional zlib compression + protobuf Payload wrapper).
// ============================================================================

// activityScheduledRef describes one activity scheduled event discovered in a workflow history
type activityScheduledRef struct {
	EventID   string
	EventType string
	Name      string
}

// TemporalActivityIssue flags a Temporal workflow activity whose output payload reported a
// non-success status (e.g. ProvisionConfiguration returning anything but COMPLETED_SUCCESS).
type TemporalActivityIssue struct {
	WorkflowID    string
	Activity      string
	ScheduledID   string
	Attempt       int
	Status        string
	StatusMessage string
	DeviceSerial  string
	DeviceID      string
	Source        string // relative file path, populated by the analytics scan
}

// TemporalAnalysisConfig controls validation of Temporal workflow activity output statuses.
// Pointer fields distinguish "absent from YAML" (nil, treated as enabled) from an explicit false,
// so configs written before this block existed keep working unchanged.
type TemporalAnalysisConfig struct {
	Enabled                 *bool                  `yaml:"enabled"`
	SuccessStatuses         []string               `yaml:"successStatuses"`
	ReportMissingActivities *bool                  `yaml:"reportMissingActivities"`
	HTMLReport              *bool                  `yaml:"htmlReport"`
	HTMLAutoOpen            *bool                  `yaml:"htmlAutoOpen"` // Open the generated HTML report in the default browser once it's written
	HTMLMaxPayloadKB        int                    `yaml:"htmlMaxPayloadKB"`
	JiraDefectLookup        JiraDefectLookupConfig `yaml:"jiraDefectLookup"` // Search JIRA for existing defects matching a failure's error message
}

// JiraDefectLookupConfig controls searching JIRA for pre-existing defects matching a failure's
// error message, so the report can show "known defect XCP-123 (Open)" instead of the reader
// filing a duplicate. Requires jira.baseUrl/email/apiToken to already be configured. Off by
// default since it makes live network calls during report generation.
type JiraDefectLookupConfig struct {
	Enabled    *bool    `yaml:"enabled"`
	Projects   []string `yaml:"projects"`   // JIRA project keys to search, e.g. ["XCP", "NVO"]
	MaxResults int      `yaml:"maxResults"` // Max matching issues to show per error message (default 3)
}

func (c JiraDefectLookupConfig) isEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

func (c JiraDefectLookupConfig) projectList() []string {
	if len(c.Projects) == 0 {
		return []string{"XCP", "NVO"}
	}
	return c.Projects
}

func (c JiraDefectLookupConfig) maxResultsOrDefault() int {
	if c.MaxResults > 0 {
		return c.MaxResults
	}
	return 3
}

func (c TemporalAnalysisConfig) isEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c TemporalAnalysisConfig) shouldReportMissing() bool {
	return c.ReportMissingActivities == nil || *c.ReportMissingActivities
}

func (c TemporalAnalysisConfig) wantsHTMLReport() bool {
	return c.HTMLReport == nil || *c.HTMLReport
}

func (c TemporalAnalysisConfig) wantsHTMLAutoOpen() bool {
	return c.HTMLAutoOpen == nil || *c.HTMLAutoOpen
}

// htmlPayloadLimitBytes caps how much of each activity payload is embedded in the HTML report.
func (c TemporalAnalysisConfig) htmlPayloadLimitBytes() int {
	if c.HTMLMaxPayloadKB > 0 {
		return c.HTMLMaxPayloadKB * 1024
	}
	return 64 * 1024
}

// globalTemporalAnalysisConfig lets the collection-time status report honour the same
// successStatuses as the analytics report. It is set once from main() after the config loads;
// collectWorkflowDetails is reached through collectKubernetesLogs' 20-parameter signature, so
// threading it positionally would be far more invasive than this. The zero value falls back to
// the built-in defaults, so an unset value behaves exactly as before.
var globalTemporalAnalysisConfig TemporalAnalysisConfig

// globalJiraConfig lets the HTML flow report look up existing JIRA defects without threading
// jira.baseUrl/email/apiToken through the analysis call chain. Set once from main() after the
// config loads, same pattern as globalTemporalAnalysisConfig above.
var globalJiraConfig JiraConfig

// successStatusSet returns the configured success values lowercased, falling back to the strict
// default of COMPLETED_SUCCESS only.
func (c TemporalAnalysisConfig) successStatusSet() map[string]bool {
	set := make(map[string]bool)
	for _, s := range c.SuccessStatuses {
		if t := strings.ToLower(strings.TrimSpace(s)); t != "" {
			set[t] = true
		}
	}
	if len(set) == 0 {
		return defaultTemporalSuccessStatuses()
	}
	return set
}

// TemporalAnalysisResult carries activity status validation output to the report writers.
type TemporalAnalysisResult struct {
	Enabled         bool
	Issues          []TemporalActivityIssue
	Missing         []TemporalMissingActivity
	StatusesChecked int
	Flows           []TemporalWorkflowFlow
}

// TemporalFlowStep is one activity occurrence in a workflow's execution sequence.
type TemporalFlowStep struct {
	ScheduledID  int
	Activity     string
	Attempt      int
	EventStatus  string
	OutputStatus string
	Flagged      bool
	NeverRan     bool

	// Why the step was flagged, lifted out of the output payload so the reader does not have to
	// dig through it. Set by applyIssueFlagsToFlows.
	StatusMessage string
	DeviceSerial  string

	// Populated only when the HTML report is enabled, for the hover/copy panels.
	Input  string
	Output string
	Detail string
}

// TemporalWorkflowFlow is a single workflow's ordered execution sequence, rendered as a flow so
// it is obvious where execution actually stopped — including activities that never started.
type TemporalWorkflowFlow struct {
	WorkflowID string
	Steps      []TemporalFlowStep
	Result     string
	ResultOK   bool
	HasResult  bool
}

// parseWorkflowFlowSummary reads an activities summary.txt into ordered flow steps. Lines look
// like "GetConfiguration | sid=23 | attempt=1 | status=EVENT_TYPE_..." or "Name | NOT_FOUND".
func parseWorkflowFlowSummary(content string) []TemporalFlowStep {
	var steps []TemporalFlowStep
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "|")
		step := TemporalFlowStep{Activity: strings.TrimSpace(fields[0])}
		if step.Activity == "" {
			continue
		}
		for _, f := range fields[1:] {
			f = strings.TrimSpace(f)
			switch {
			case f == "NOT_FOUND":
				step.NeverRan = true
			case strings.HasPrefix(f, "sid="):
				step.ScheduledID, _ = strconv.Atoi(strings.TrimPrefix(f, "sid="))
			case strings.HasPrefix(f, "attempt="):
				step.Attempt, _ = strconv.Atoi(strings.TrimPrefix(f, "attempt="))
			case strings.HasPrefix(f, "FLAGGED output status="):
				step.OutputStatus = strings.TrimPrefix(f, "FLAGGED output status=")
				step.Flagged = true
			case strings.HasPrefix(f, "status="):
				step.EventStatus = strings.TrimPrefix(f, "status=")
			}
		}
		steps = append(steps, step)
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].ScheduledID < steps[j].ScheduledID })
	return steps
}

// summarizeEventStatus reduces the recorded event-type chain to its terminal state, since events
// are appended in execution order (SCHEDULED, STARTED, COMPLETED).
func summarizeEventStatus(raw string) string {
	switch {
	case raw == "":
		return "UNKNOWN"
	case raw == "SCHEDULED_ONLY_OR_NO_STATUS":
		return "SCHEDULED (never completed)"
	}
	parts := strings.Split(raw, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	return strings.TrimPrefix(last, "EVENT_TYPE_ACTIVITY_TASK_")
}

// renderTemporalWorkflowFlow draws one workflow as an ASCII execution flow. ASCII only, so it
// survives the Windows console, Jira monospace blocks, and plain-text mail alike.
func renderTemporalWorkflowFlow(w io.Writer, flow TemporalWorkflowFlow) {
	fmt.Fprintf(w, "  %s\n", flow.WorkflowID)
	fmt.Fprintln(w, "    START")

	width := 0
	for _, s := range flow.Steps {
		if len(s.Activity) > width {
			width = len(s.Activity)
		}
	}

	ran := 0
	for _, s := range flow.Steps {
		if s.NeverRan {
			continue
		}
		// One connector per group, not per activity, so the sequence reads as a solid block.
		if ran == 0 {
			fmt.Fprintln(w, "      |")
		}
		ran++
		status := summarizeEventStatus(s.EventStatus)
		marker := "+->"
		if s.Flagged || status == "FAILED" || status == "TIMED_OUT" {
			marker = "!->"
		}
		line := fmt.Sprintf("      %s [%3d] %-*s  %s", marker, s.ScheduledID, width, s.Activity, status)
		if s.Attempt > 1 {
			line += fmt.Sprintf(" (attempt %d)", s.Attempt)
		}
		if s.Flagged {
			line += fmt.Sprintf("  <== output status=%s", s.OutputStatus)
		}
		fmt.Fprintln(w, line)
	}
	if ran == 0 {
		fmt.Fprintln(w, "      |")
		fmt.Fprintln(w, "      (no activities recorded)")
	}

	skipped := 0
	for _, s := range flow.Steps {
		if !s.NeverRan {
			continue
		}
		if skipped == 0 {
			fmt.Fprintln(w, "      |")
		}
		skipped++
		fmt.Fprintf(w, "      XXX [  -] %-*s  NEVER SCHEDULED\n", width, s.Activity)
	}

	fmt.Fprintln(w, "      |")
	switch {
	case !flow.HasResult:
		fmt.Fprintln(w, "    END   (no workflow result recorded)")
	case flow.ResultOK:
		fmt.Fprintf(w, "    END   %s\n", flow.Result)
	default:
		fmt.Fprintf(w, "    END   %s  <== FAILURE\n", flow.Result)
	}
	fmt.Fprintln(w)
}

// loadFlowStepPayloads attaches each step's collected input/output/status text so the HTML report
// can show it on hover and copy it on click. Only called when the HTML report is enabled, since
// activity payloads can be hundreds of KB each.
func loadFlowStepPayloads(activitiesDir string, steps []TemporalFlowStep, limit int) {
	read := func(activity, attempt, suffix string) string {
		name := activity + suffix
		if attempt != "" {
			name = activity + "_attempt" + attempt + suffix
		}
		b, err := ioutil.ReadFile(filepath.Join(activitiesDir, name))
		if err != nil {
			return ""
		}
		text := strings.TrimSpace(string(b))
		if len(text) > limit {
			text = text[:limit] + fmt.Sprintf("\n\n... [truncated, %d more bytes - see %s]", len(text)-limit, name)
		}
		return text
	}
	// Files are written either bare or with an _attemptN infix depending on retries.
	pick := func(activity, attempt, suffix string) string {
		if v := read(activity, "", suffix); v != "" {
			return v
		}
		return read(activity, attempt, suffix)
	}

	for i := range steps {
		s := &steps[i]
		if s.NeverRan {
			continue
		}
		attempt := strconv.Itoa(s.Attempt)
		s.Input = pick(s.Activity, attempt, "_input.txt")
		s.Output = pick(s.Activity, attempt, "_output.txt")
		s.Detail = pick(s.Activity, attempt, "_status.txt")
	}
}

// flowHTMLBlock is one rendered node in the HTML flow diagram.
type flowHTMLBlock struct {
	Activity string
	SID      string
	Attempt  int
	Status   string
	State    string // ok | fail | skip
	Reason   string
	Device   string
	Input    string
	Output   string
	Detail   string
	// Existing JIRA defects whose text matches Reason, so the reader knows not to file a
	// duplicate. DefectsChecked distinguishes "looked, found none" from "lookup not run",
	// DefectsExact a verified phrase hit from a weaker keyword guess, and DefectQuery records
	// what was actually searched so a questionable match can be audited from the report itself.
	Defects        []JiraDefectMatch
	DefectsChecked bool
	DefectsExact   bool
	DefectsFailed  bool
	DefectQuery    string
}

// JiraDefectMatch is a candidate pre-existing defect found by searching JIRA for a failure's
// error message text.
type JiraDefectMatch struct {
	Key         string
	Status      string
	StatusClass string // open | progress | done - drives the chip colour in the HTML report
	Summary     string
	URL         string
}

// jiraDefectLookupResult is one memoized lookup. Exact means the matches came from the
// exact-phrase query and can safely be called duplicates; without it they are keyword guesses
// that a human still has to confirm.
type jiraDefectLookupResult struct {
	Matches []JiraDefectMatch
	Exact   bool
	Failed  bool
	Query   string
}

// jiraDefectLookupCache memoizes lookups per normalized error message for the lifetime of the
// process, since the same fault reason commonly repeats across many workflows/steps in one
// report and each lookup is a live JIRA API call.
var jiraDefectLookupCache = struct {
	sync.Mutex
	m map[string]jiraDefectLookupResult
}{m: make(map[string]jiraDefectLookupResult)}

// findExistingJiraDefects searches JIRA (projects from cfg.projectList()) for issues whose text
// matches the given error message, so the report can flag "known defect" instead of letting the
// reader file a duplicate. Returns an empty result when lookup is disabled, the message is empty,
// or the search fails - a failed lookup must never break report generation.
func findExistingJiraDefects(jiraConfig JiraConfig, cfg JiraDefectLookupConfig, errorMessage string) jiraDefectLookupResult {
	if !cfg.isEnabled() {
		return jiraDefectLookupResult{}
	}
	text := strings.Join(strings.Fields(errorMessage), " ")
	if text == "" {
		return jiraDefectLookupResult{}
	}

	cacheKey := strings.ToLower(text)
	jiraDefectLookupCache.Lock()
	if cached, ok := jiraDefectLookupCache.m[cacheKey]; ok {
		jiraDefectLookupCache.Unlock()
		return cached
	}
	jiraDefectLookupCache.Unlock()

	result, err := searchJiraForDefects(jiraConfig, cfg, text)
	if err != nil {
		if logger != nil {
			logger.Debug("JIRA defect lookup skipped: %v", err)
		}
		// Cache the failure too so a persistent error isn't retried per step, but keep it
		// distinguishable from a genuine "no defect exists" answer.
		result = jiraDefectLookupResult{Failed: true, Query: result.Query}
	}

	jiraDefectLookupCache.Lock()
	jiraDefectLookupCache.m[cacheKey] = result
	jiraDefectLookupCache.Unlock()
	return result
}

// errorPhraseMarkers are common wrapper prefixes ("failed to X: reason: Y: error message: Z")
// found in this codebase's error strings. The ROOT CAUSE text after the last one present is what
// actually gets pasted into a Jira ticket - the outer wrapper (action/URL/HTTP status) is specific
// to this run and won't appear in an existing defect's summary/description.
var errorPhraseMarkers = []string{"error message:", "message:", "reason:"}

// extractDistinctiveErrorPhrase strips a known wrapper prefix so the JQL search matches the
// stable root-cause text instead of a one-off "Failed to X: Reason: POST http://..." preamble.
func extractDistinctiveErrorPhrase(msg string) string {
	lower := strings.ToLower(msg)
	for _, marker := range errorPhraseMarkers {
		if idx := strings.LastIndex(lower, marker); idx >= 0 {
			return strings.TrimSpace(msg[idx+len(marker):])
		}
	}
	return msg
}

// luceneSpecialReplacer blanks the characters that break or distort JIRA's Lucene text query.
// Every one of them is also a tokenizer separator, so removing them cannot change which issues
// match - it only keeps the query parseable and removes the need to escape anything.
var luceneSpecialReplacer = strings.NewReplacer(
	`\`, " ", `"`, " ", `'`, " ", `+`, " ", `-`, " ", `&`, " ", `|`, " ", `!`, " ",
	`(`, " ", `)`, " ", `{`, " ", `}`, " ", `[`, " ", `]`, " ", `^`, " ", `~`, " ",
	`*`, " ", `?`, " ", `:`, " ", `/`, " ",
)

// jiraSearchPhrase reduces a raw error string to the word sequence used for both JIRA queries:
// the root-cause tail, stripped of query-syntax characters, and capped at a whole word (a very
// long literal hurts recall, and a truncated half-word can never phrase-match).
func jiraSearchPhrase(msg string) string {
	msg = luceneSpecialReplacer.Replace(extractDistinctiveErrorPhrase(msg))
	msg = strings.Join(strings.Fields(msg), " ")
	const maxLen = 120
	if len(msg) > maxLen {
		msg = msg[:maxLen]
		if cut := strings.LastIndex(msg, " "); cut > 0 {
			msg = msg[:cut]
		}
	}
	return strings.TrimSpace(msg)
}

// errorStopWords are words that appear in almost any sentence and so must not count toward the
// keyword-overlap score. Deliberately limited to grammatical filler: pruning domain words like
// "already"/"exists" would leave only the nouns, and any issue mentioning the same component
// would then look like a match.
var errorStopWords = map[string]bool{
	"the": true, "an": true, "and": true, "or": true, "of": true, "to": true, "in": true,
	"on": true, "at": true, "for": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "it": true, "its": true, "this": true, "that": true,
	"with": true, "from": true, "by": true, "as": true, "not": true, "no": true,
	"if": true, "then": true, "there": true,
}

// errorTokens lowercases and splits text into alphanumeric words, dropping single characters.
func errorTokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

// distinctiveErrorTokens is the deduplicated, stop-word-free vocabulary of an error phrase - the
// set a candidate issue is scored against.
func distinctiveErrorTokens(phrase string) []string {
	seen := make(map[string]bool)
	tokens := make([]string, 0, 8)
	for _, t := range errorTokens(phrase) {
		if len(t) < 2 || errorStopWords[t] || seen[t] {
			continue
		}
		seen[t] = true
		tokens = append(tokens, t)
	}
	return tokens
}

// keywordOverlap is the share of an error's distinctive words present in a candidate issue's
// own text. Whole-word comparison, so "server" cannot be satisfied by "serverless".
func keywordOverlap(keywords []string, text string) float64 {
	if len(keywords) == 0 {
		return 0
	}
	have := make(map[string]bool)
	for _, t := range errorTokens(text) {
		have[t] = true
	}
	hits := 0
	for _, k := range keywords {
		if have[k] {
			hits++
		}
	}
	return float64(hits) / float64(len(keywords))
}

// adfPlainText flattens a JIRA description into searchable text. Cloud's v3 API returns rich
// text as an Atlassian Document Format node tree rather than a string, so the description has to
// be walked - and it has to be searched, because the root-cause text of a defect is far more
// often in the description than in the summary.
func adfPlainText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain
	}
	var doc interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	var sb strings.Builder
	var walk func(node interface{}, depth int)
	walk = func(node interface{}, depth int) {
		if depth > 32 {
			return
		}
		switch v := node.(type) {
		case map[string]interface{}:
			if text, ok := v["text"].(string); ok {
				sb.WriteString(text)
				sb.WriteByte(' ')
			}
			walk(v["content"], depth+1)
		case []interface{}:
			for _, item := range v {
				walk(item, depth+1)
			}
		}
	}
	walk(doc, 0)
	return sb.String()
}

// jiraStatusClass buckets a JIRA status name into open/progress/done for chip colouring. Falls
// back to "open" for anything unrecognized so an unresolved-looking defect is never hidden as done.
func jiraStatusClass(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "closed", "resolved", "complete", "completed":
		return "done"
	case "in progress", "in review", "in-progress", "reviewing":
		return "progress"
	default:
		return "open"
	}
}

// minKeywordOverlap is the share of an error's distinctive words a candidate must contain before
// a fallback (non-phrase) match is worth showing. JIRA's text search returns relevance-ranked
// results with no notion of "good enough", so without a floor it happily returns the three
// least-bad issues in the project for an error that has no existing defect at all.
const minKeywordOverlap = 0.6

// jiraIssueCandidate pairs a search hit with the text it is verified against.
type jiraIssueCandidate struct {
	Match      JiraDefectMatch
	SearchText string // summary + description, the fields a root cause is actually written into
}

// searchJiraForDefects looks for a pre-existing defect matching errorMessage in two tiers: an
// exact-phrase query, whose hits JIRA has already proven contain the phrase, and - only if that
// finds nothing - a keyword query whose hits are re-verified here against each issue's own
// summary and description before being shown.
func searchJiraForDefects(jiraConfig JiraConfig, cfg JiraDefectLookupConfig, errorMessage string) (jiraDefectLookupResult, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(jiraConfig.BaseURL), "/")
	email := strings.TrimSpace(jiraConfig.Email)
	if baseURL == "" || email == "" {
		return jiraDefectLookupResult{}, fmt.Errorf("JIRA baseUrl/email not configured")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return jiraDefectLookupResult{}, fmt.Errorf("JIRA baseUrl %q is missing a scheme", baseURL)
	}

	phrase := jiraSearchPhrase(errorMessage)
	if phrase == "" {
		return jiraDefectLookupResult{}, nil
	}

	apiToken, err := getJIRAApiToken(&jiraConfig, logger)
	if err != nil {
		return jiraDefectLookupResult{}, fmt.Errorf("failed to retrieve JIRA API token: %v", err)
	}
	apiToken = strings.TrimSpace(apiToken)
	if apiToken == "" {
		return jiraDefectLookupResult{}, fmt.Errorf("JIRA API token not available")
	}

	projects := strings.Join(cfg.projectList(), ", ")
	limit := cfg.maxResultsOrDefault()
	// Recorded verbatim in the report so a reader can judge a match without reading this code.
	query := fmt.Sprintf("%s for phrase %q", projects, phrase)

	// Tier 1: exact phrase (nested quotes are JQL's phrase syntax). JIRA matches it server-side
	// across summary, description AND comments, so a hit here needs no further verification -
	// which is also how a defect that only mentions the error in a comment still gets found.
	// Deliberately no ORDER BY: an explicit sort overrides the text-relevance ranking that puts
	// the best match first.
	exactJQL := fmt.Sprintf(`project in (%s) AND text ~ "\"%s\""`, projects, phrase)
	exactHits, err := jiraJQLSearch(baseURL, email, apiToken, exactJQL, limit)
	if err != nil {
		// A rejected phrase query must not cost us the fallback.
		if logger != nil {
			logger.Debug("JIRA exact-phrase defect search failed, falling back to keywords: %v", err)
		}
	} else if len(exactHits) > 0 {
		return jiraDefectLookupResult{Matches: candidateMatches(exactHits, limit), Exact: true, Query: query}, nil
	}

	// Tier 2: keyword match. Anything below two distinctive words is too generic to attribute to
	// a specific defect, so no guess is better than a wrong one.
	keywords := distinctiveErrorTokens(phrase)
	if len(keywords) < 2 {
		return jiraDefectLookupResult{Query: query}, nil
	}
	overFetch := limit * 4
	if overFetch > 20 {
		overFetch = 20
	}
	looseJQL := fmt.Sprintf(`project in (%s) AND text ~ "%s"`, projects, phrase)
	looseHits, err := jiraJQLSearch(baseURL, email, apiToken, looseJQL, overFetch)
	if err != nil {
		return jiraDefectLookupResult{Query: query}, err
	}
	verified := make([]jiraIssueCandidate, 0, len(looseHits))
	for _, hit := range looseHits {
		if keywordOverlap(keywords, hit.SearchText) >= minKeywordOverlap {
			verified = append(verified, hit)
		}
	}
	return jiraDefectLookupResult{Matches: candidateMatches(verified, limit), Query: query}, nil
}

func candidateMatches(candidates []jiraIssueCandidate, limit int) []JiraDefectMatch {
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	matches := make([]JiraDefectMatch, 0, len(candidates))
	for _, c := range candidates {
		matches = append(matches, c.Match)
	}
	return matches
}

// jiraJQLSearch runs one JQL search using the same JIRA credentials as file attachment.
func jiraJQLSearch(baseURL, email, apiToken, jql string, maxResults int) ([]jiraIssueCandidate, error) {
	// Atlassian removed the legacy GET /rest/api/3/search endpoint (HTTP 410) in favor of
	// POST /rest/api/3/search/jql, which takes the same JQL but as a JSON body.
	apiURL := fmt.Sprintf("%s/rest/api/3/search/jql", baseURL)
	reqBody, err := json.Marshal(struct {
		JQL        string   `json:"jql"`
		MaxResults int      `json:"maxResults"`
		Fields     []string `json:"fields"`
	}{JQL: jql, MaxResults: maxResults, Fields: []string{"summary", "status", "description"}})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", email, apiToken)))
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("unexpected redirect to %s (check jira.baseUrl)", req.URL)
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JIRA search returned %d: %s", resp.StatusCode, extractJiraErrorMessage(body))
	}

	var parsed struct {
		Issues []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary     string          `json:"summary"`
				Description json.RawMessage `json:"description"`
				Status      struct {
					Name string `json:"name"`
				} `json:"status"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse JIRA search response: %v", err)
	}

	candidates := make([]jiraIssueCandidate, 0, len(parsed.Issues))
	for _, iss := range parsed.Issues {
		candidates = append(candidates, jiraIssueCandidate{
			Match: JiraDefectMatch{
				Key:         iss.Key,
				Status:      iss.Fields.Status.Name,
				StatusClass: jiraStatusClass(iss.Fields.Status.Name),
				Summary:     iss.Fields.Summary,
				URL:         fmt.Sprintf("%s/browse/%s", baseURL, iss.Key),
			},
			SearchText: iss.Fields.Summary + " " + adfPlainText(iss.Fields.Description),
		})
	}
	return candidates, nil
}

type flowHTMLWorkflow struct {
	ID   string
	Kind string
	// OK is the overall verdict (also false for a skipped activity); ResultOK is only about the
	// workflow's own result payload. Keeping them apart avoids labelling a COMPLETED_SUCCESS
	// result as a FAILURE.
	OK        bool
	ResultOK  bool
	HasResult bool
	Result    string
	RanCount  int
	SkipCount int
	Blocks    []flowHTMLBlock
}

type flowHTMLData struct {
	Generated       string
	Source          string
	Version         string
	StatusesChecked int
	FlaggedCount    int
	MissingCount    int
	FailedCount     int
	// The most repeated fault reason, so the header can name one root cause instead of making
	// the reader diff every block.
	PrimaryFailure      string
	PrimaryFailureCount int
	Workflows           []flowHTMLWorkflow
}

// buildFlowHTMLData converts the analysis result into the view model used by the HTML template.
func buildFlowHTMLData(temporal TemporalAnalysisResult, source, version string, taCfg TemporalAnalysisConfig) flowHTMLData {
	data := flowHTMLData{
		Generated:       time.Now().Format("2006-01-02 15:04:05"),
		Source:          source,
		Version:         version,
		StatusesChecked: temporal.StatusesChecked,
		FlaggedCount:    len(temporal.Issues),
		MissingCount:    len(temporal.Missing),
	}

	for _, flow := range temporal.Flows {
		wf := flowHTMLWorkflow{
			ID:        flow.WorkflowID,
			Kind:      workflowKindLabel(flow.WorkflowID),
			OK:        !flow.HasResult || flow.ResultOK,
			ResultOK:  !flow.HasResult || flow.ResultOK,
			HasResult: flow.HasResult,
			Result:    flow.Result,
		}

		for _, s := range flow.Steps {
			if s.NeverRan {
				continue
			}
			block := flowHTMLBlock{
				Activity: s.Activity,
				Attempt:  s.Attempt,
				SID:      strconv.Itoa(s.ScheduledID),
				Status:   summarizeEventStatus(s.EventStatus),
				Reason:   s.StatusMessage,
				Device:   s.DeviceSerial,
				Input:    s.Input,
				Output:   s.Output,
				Detail:   s.Detail,
			}
			if s.Flagged {
				block.State = "fail"
				block.Status = s.OutputStatus
			} else if block.Status == "FAILED" || block.Status == "TIMED_OUT" {
				block.State = "fail"
			} else {
				block.State = "ok"
			}
			if block.Reason != "" {
				block.DefectsChecked = taCfg.JiraDefectLookup.isEnabled()
				lookup := findExistingJiraDefects(globalJiraConfig, taCfg.JiraDefectLookup, block.Reason)
				block.Defects = lookup.Matches
				block.DefectsExact = lookup.Exact
				block.DefectsFailed = lookup.Failed
				block.DefectQuery = lookup.Query
			}
			wf.RanCount++
			wf.Blocks = append(wf.Blocks, block)
		}

		// Never-scheduled activities carry no scheduled ID, so they sort to the front of Steps.
		// Append them after the executed chain to match the text report's ordering.
		for _, s := range flow.Steps {
			if !s.NeverRan {
				continue
			}
			wf.SkipCount++
			wf.Blocks = append(wf.Blocks, flowHTMLBlock{
				Activity: s.Activity,
				SID:      "-",
				Status:   "NEVER SCHEDULED",
				State:    "skip",
			})
		}

		// A workflow with a skipped activity is not clean even when every recorded status passed.
		if wf.SkipCount > 0 {
			wf.OK = false
		}
		if !wf.OK {
			data.FailedCount++
		}
		data.Workflows = append(data.Workflows, wf)
	}
	data.PrimaryFailure, data.PrimaryFailureCount = dominantFailure(data.Workflows)
	return data
}

// dominantFailure returns the most repeated step fault reason across every workflow.
func dominantFailure(workflows []flowHTMLWorkflow) (string, int) {
	counts := map[string]int{}
	for _, wf := range workflows {
		for _, b := range wf.Blocks {
			if reason := strings.TrimSpace(b.Reason); reason != "" {
				counts[reason]++
			}
		}
	}
	var best string
	bestN := 0
	for reason, n := range counts {
		// Ties break on text so map iteration order cannot change the header between runs.
		if n > bestN || (n == bestN && reason < best) {
			best, bestN = reason, n
		}
	}
	return best, bestN
}

// workflowKindLabel derives a short badge from the workflow ID prefix.
func workflowKindLabel(id string) string {
	switch {
	case strings.HasPrefix(id, "deploy-site-"):
		return "deploy-site"
	case strings.HasPrefix(id, "deploy-device-"):
		return "deploy-device"
	case strings.HasPrefix(id, "ztf-onboard-"):
		return "ztf-onboard"
	}
	if idx := strings.Index(id, "-"); idx > 0 {
		return id[:idx]
	}
	return "workflow"
}

// writeTemporalFlowHTML renders the interactive flow report. Payload text is emitted into hidden
// elements as ordinary escaped HTML text (never into script literals), so cluster-supplied values
// cannot break out into markup or JS.
func writeTemporalFlowHTML(path string, data flowHTMLData) error {
	tmpl, err := template.New("flow").Parse(temporalFlowHTMLTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse HTML template: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create HTML report: %v", err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	defer w.Flush()
	return tmpl.Execute(w, data)
}

// temporalFlowHTMLTemplate is a fully self-contained report: no external CSS/JS/fonts, so it can
// be attached to a ticket and opened offline. Payloads are emitted as escaped text inside hidden
// <pre> nodes and read back via textContent, never interpolated into script source.
const temporalFlowHTMLTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Temporal Flow Analysis</title>
<style>
  :root {
    --void:#04060b; --deep:#070c14; --panel:#0b131e; --edge:#16283a;
    --txt:#c6dae6; --dim:#5f7789;
    --ok:#22e0b1; --fail:#e0566d; --warn:#ffb02e; --beam:#35d6ff;
    --mono: ui-monospace, "Cascadia Mono", "SF Mono", Consolas, "Courier New", monospace;
  }
  * { box-sizing:border-box; }
  body { margin:0; background:var(--void); color:var(--txt); font:13px/1.55 var(--mono);
         overflow-x:hidden; -webkit-font-smoothing:antialiased; }
  .bg { position:fixed; inset:0; z-index:0; pointer-events:none;
        background-image:linear-gradient(rgba(53,214,255,.05) 1px, transparent 1px),
                         linear-gradient(90deg, rgba(53,214,255,.05) 1px, transparent 1px);
        background-size:44px 44px;
        -webkit-mask-image:radial-gradient(ellipse 90% 60% at 50% 0%, #000 10%, transparent 75%);
        mask-image:radial-gradient(ellipse 90% 60% at 50% 0%, #000 10%, transparent 75%); }
  .vignette { position:fixed; inset:0; z-index:0; pointer-events:none;
        background:radial-gradient(ellipse 120% 80% at 50% -10%, rgba(53,214,255,.07), transparent 60%); }
  .hud,.cmdbar,.shell,.signature { position:relative; z-index:1; }

  .hud { display:flex; gap:20px; align-items:center; flex-wrap:wrap; justify-content:space-between;
         padding:18px 26px; border-bottom:1px solid var(--edge);
         background:linear-gradient(180deg, rgba(53,214,255,.06), transparent); }
  .brand { display:flex; gap:14px; align-items:center; }
  .hud h1 { margin:0; font-size:15px; letter-spacing:.28em; font-weight:600; }
  .sub { margin:5px 0 0; font-size:10px; letter-spacing:.1em; color:var(--dim); overflow-wrap:anywhere; }
  .sigil { width:38px; height:38px; border-radius:50%; position:relative; flex:0 0 auto;
           border:1px solid rgba(53,214,255,.35); display:grid; place-items:center; }
  .sigil::before { content:""; position:absolute; inset:-4px; border-radius:50%;
           border:1px solid rgba(53,214,255,.14); border-top-color:var(--beam);
           animation:spin 4s linear infinite; }
  .sigil i { width:9px; height:9px; border-radius:50%; background:var(--beam);
           box-shadow:0 0 14px var(--beam); }
  @keyframes spin { to { transform:rotate(360deg); } }

  .gauges { display:flex; gap:8px; flex-wrap:wrap; }
  .g { min-width:98px; padding:8px 13px; border:1px solid var(--edge); border-radius:3px;
       background:linear-gradient(180deg, rgba(53,214,255,.05), transparent); }
  .g b { display:block; font-size:23px; line-height:1.1; }
  .g span { display:block; font-size:9.5px; color:var(--dim); letter-spacing:.16em; margin-top:2px; }
  .g.bad { border-color:rgba(224,86,109,.32); background:linear-gradient(180deg, rgba(224,86,109,.07), transparent); }
  .g.bad b { color:var(--fail); text-shadow:0 0 9px rgba(224,86,109,.35); }
  .g.warn { border-color:rgba(255,176,46,.4); }
  .g.warn b { color:var(--warn); }
  .g.good b { color:var(--ok); text-shadow:0 0 16px rgba(34,224,177,.4); }

  .signature { padding:13px 26px; border-bottom:1px solid rgba(224,86,109,.22);
       background:linear-gradient(90deg, rgba(224,86,109,.08), transparent 70%); }
  .sigl { font-size:9.5px; letter-spacing:.2em; color:var(--fail); margin-bottom:5px; }
  .sigt { font-size:12.5px; color:#dcb3ba; overflow-wrap:anywhere; white-space:pre-wrap; }

  .cmdbar { display:flex; gap:12px; align-items:center; flex-wrap:wrap;
            padding:11px 26px; border-bottom:1px solid var(--edge);
            background:var(--deep); position:sticky; top:0; z-index:6; }
  .field { display:flex; align-items:center; gap:7px; border:1px solid var(--edge);
           border-radius:2px; padding:5px 10px; background:var(--void); }
  .field:focus-within { border-color:var(--beam); box-shadow:0 0 0 1px rgba(53,214,255,.25); }
  .pfx { color:var(--beam); }
  #q { background:transparent; border:0; outline:none; color:var(--txt); font:inherit; min-width:230px; }
  .btn { background:var(--void); border:1px solid var(--edge); color:var(--txt); font:inherit;
         font-size:11px; letter-spacing:.1em; padding:6px 13px; border-radius:2px; cursor:pointer; }
  .btn:hover { border-color:var(--beam); color:var(--beam); }
  .btn.danger { border-color:rgba(224,86,109,.35); color:var(--fail); }
  .btn.danger:hover { background:rgba(224,86,109,.09); }
  .tgl { display:flex; align-items:center; gap:6px; font-size:11px; letter-spacing:.08em;
         color:var(--dim); cursor:pointer; user-select:none; }
  .keys { margin-left:auto; font-size:9.5px; letter-spacing:.12em; color:var(--dim); }

  .shell { display:grid; grid-template-columns:304px 1fr; align-items:start; }
  .roster { position:sticky; top:47px; max-height:calc(100vh - 47px); overflow:auto;
            padding:14px 12px 40px; border-right:1px solid var(--edge);
            display:flex; flex-direction:column; gap:7px; }
  .slot { display:flex; gap:10px; text-align:left; width:100%; padding:10px 11px; cursor:pointer;
          background:var(--panel); border:1px solid var(--edge); border-radius:3px; color:var(--txt);
          font:inherit; transition:border-color .15s, background .15s; }
  .slot:hover { border-color:var(--beam); }
  .slot.on { border-color:var(--beam); background:linear-gradient(90deg, rgba(53,214,255,.13), var(--panel)); }
  .led { width:8px; height:8px; border-radius:50%; margin-top:5px; flex:0 0 auto;
         background:var(--ok); box-shadow:0 0 10px var(--ok); }
  .slot.bad .led { background:var(--fail); box-shadow:0 0 7px var(--fail);
         animation:beat 1.6s ease-in-out infinite; }
  .slotbody { min-width:0; flex:1 1 auto; }
  .wfref { display:block; font-size:11.5px; overflow-wrap:anywhere; line-height:1.35; }
  .slotmeta { display:block; font-size:9.5px; letter-spacing:.1em; color:var(--dim); margin-top:3px; }
  .strip { display:flex; gap:2px; margin-top:7px; }
  .strip i { height:3px; flex:1 1 auto; border-radius:1px; background:var(--ok); }
  .strip i.fail { background:var(--fail); }
  .strip i.skip { background:var(--warn); }

  .stage { padding:0 26px 90px; min-width:0; }
  /* Pinned under the command bar so a long chain never leaves you guessing whose it is. */
  .wfbar { display:flex; align-items:center; gap:12px; flex-wrap:wrap; margin-bottom:16px;
           position:sticky; top:47px; z-index:4; background:var(--void);
           padding:16px 0 12px; border-bottom:1px solid var(--edge); }
  .wfbar h2 { margin:0; font-size:14px; font-weight:600; overflow-wrap:anywhere; flex:1 1 auto; }
  .verdict { font-size:10px; letter-spacing:.2em; padding:5px 12px; border-radius:2px; }
  .verdict.ok { color:var(--ok); border:1px solid rgba(34,224,177,.4); }
  .verdict.bad { color:var(--fail); border:1px solid rgba(224,86,109,.4); background:rgba(224,86,109,.07); }
  .tag { font-size:9.5px; letter-spacing:.14em; color:var(--dim); border:1px solid var(--edge);
         padding:3px 9px; border-radius:2px; }

  .chain { padding:4px 0 10px; }
  .cap { display:inline-flex; align-items:center; padding:5px 14px; border-radius:2px;
         font-size:10px; letter-spacing:.2em; color:var(--dim);
         border:1px dashed var(--edge); background:var(--deep); }
  .cap.ok { color:var(--ok); border-color:rgba(34,224,177,.4); border-style:solid; }
  .cap.bad { color:var(--fail); border-color:rgba(224,86,109,.4); border-style:solid;
             background:rgba(224,86,109,.06); }
  .link { width:1px; height:16px; margin-left:26px;
          background:linear-gradient(180deg, rgba(53,214,255,.55), rgba(53,214,255,.15)); }
  .link.dead { background:repeating-linear-gradient(180deg, rgba(224,86,109,.5) 0 3px, transparent 3px 7px); }
  .cell { display:flex; align-items:center; gap:13px; padding:11px 15px; cursor:pointer;
          border:1px solid var(--edge); border-radius:3px; background:var(--panel);
          position:relative; transition:border-color .15s, transform .15s, box-shadow .15s; }
  .cell::before { content:""; position:absolute; left:0; top:0; bottom:0; width:2px; background:var(--dim); }
  .cell:hover,.cell:focus { outline:none; border-color:var(--beam); transform:translateX(3px);
          box-shadow:-6px 0 22px rgba(53,214,255,.13); }
  .cell.ok::before { background:var(--ok); box-shadow:0 0 12px rgba(34,224,177,.6); }
  .cell.fail::before { background:var(--fail); box-shadow:0 0 9px rgba(224,86,109,.45); }
  .cell.skip::before { background:var(--warn); }
  .cell.fail { border-color:rgba(224,86,109,.3);
               background:linear-gradient(90deg, rgba(224,86,109,.08), var(--panel) 55%); }
  .cell.skip { border-style:dashed; opacity:.9; }
  .idx { min-width:30px; text-align:right; font-size:10.5px; color:var(--dim); }
  .cbody { flex:1 1 auto; display:flex; align-items:center; gap:9px; min-width:0; flex-wrap:wrap; }
  .nm { font-weight:600; font-size:13.5px; overflow-wrap:anywhere; }
  .retry { font-size:9.5px; letter-spacing:.12em; color:var(--warn);
           border:1px solid rgba(255,176,46,.35); border-radius:2px; padding:1px 6px; }
  .stat { font-size:10px; letter-spacing:.13em; white-space:nowrap; color:var(--dim); }
  .cell.ok .stat { color:var(--ok); }
  .cell.fail .stat { color:var(--fail); }
  .cell.skip .stat { color:var(--warn); }
  .pulse { position:absolute; right:-1px; top:-1px; bottom:-1px; width:3px; border-radius:0 3px 3px 0; }
  .cell.fail .pulse { background:var(--fail); animation:beat 1.9s ease-in-out infinite; }
  @keyframes beat { 0%,100%{opacity:.35} 50%{opacity:.85} }

  .fault { margin:7px 0 0 26px; padding:10px 13px; border-left:2px solid var(--fail);
           background:linear-gradient(90deg, rgba(224,86,109,.07), transparent);
           font-size:12px; line-height:1.5; color:#dcb3ba;
           white-space:pre-wrap; overflow-wrap:anywhere; }
  .fault .dev { display:inline-block; font-size:9.5px; letter-spacing:.14em; color:var(--warn);
           border:1px solid rgba(255,176,46,.35); border-radius:2px; padding:1px 7px; margin-bottom:6px; }
  .fault .msg { display:block; }

  .defects { margin-top:9px; padding-top:9px; border-top:1px dashed rgba(224,86,109,.3);
             display:flex; flex-wrap:wrap; align-items:center; gap:8px; }
  .defects.none { border-top-color:var(--edge); }
  .defects.weak { border-top-color:rgba(255,176,46,.35); }
  .defhdr { font-size:9.5px; letter-spacing:.1em; color:var(--dim); white-space:normal; }
  .defects.none .defhdr { font-style:italic; }
  .defects.weak .defhdr { color:var(--warn); }
  .defq { flex-basis:100%; font-size:9.5px; letter-spacing:.04em; color:var(--dim); opacity:.7;
          white-space:normal; overflow-wrap:anywhere; }
  .defchip { display:inline-block; font-size:11px; font-weight:600; letter-spacing:.03em;
             padding:3px 9px; border-radius:11px; text-decoration:none; white-space:nowrap;
             border:1px solid var(--edge); transition:transform .12s, box-shadow .12s; }
  .defchip:hover { transform:translateY(-1px); }
  .defchip.open { color:var(--fail); border-color:rgba(224,86,109,.45); background:rgba(224,86,109,.08); }
  .defchip.progress { color:var(--warn); border-color:rgba(255,176,46,.45); background:rgba(255,176,46,.08); }
  .defchip.done { color:var(--ok); border-color:rgba(34,224,177,.45); background:rgba(34,224,177,.08); }

  .drawer { position:fixed; top:0; right:0; width:min(680px, 94vw); height:100%; z-index:40;
            background:linear-gradient(180deg,#060b13,#040709); border-left:1px solid var(--beam);
            box-shadow:-24px 0 60px rgba(0,0,0,.7); display:flex; flex-direction:column;
            transform:translateX(100%); transition:transform .22s ease; pointer-events:none; }
  .drawer.open { transform:translateX(0); pointer-events:auto; }
  .dhead { display:flex; gap:12px; align-items:flex-start; padding:16px 18px 12px;
           border-bottom:1px solid var(--edge); }
  .dhead > div { min-width:0; flex:1 1 auto; }
  .dtitle { font-size:14px; font-weight:600; overflow-wrap:anywhere; }
  .dsub { font-size:10px; letter-spacing:.14em; color:var(--dim); margin-top:4px; overflow-wrap:anywhere; }
  .dtabs { display:flex; gap:6px; padding:11px 18px; border-bottom:1px solid var(--edge); }
  .tab { background:transparent; border:1px solid var(--edge); color:var(--dim); font:inherit;
         font-size:10px; letter-spacing:.14em; padding:5px 13px; border-radius:2px; cursor:pointer; }
  .tab.on { color:var(--beam); border-color:var(--beam); background:rgba(53,214,255,.1); }
  .dtabs .btn { margin-left:auto; }
  .dbody { flex:1 1 auto; margin:0; padding:16px 18px 40px; overflow:auto; font-size:11.5px;
           line-height:1.55; white-space:pre-wrap; overflow-wrap:anywhere; color:#b9d2e0; }

  .tip { position:fixed; z-index:50; max-width:600px; display:none; pointer-events:none;
         background:#03060a; border:1px solid rgba(53,214,255,.5); border-radius:3px;
         padding:10px 12px; box-shadow:0 12px 40px rgba(0,0,0,.75); }
  .tip .tth { font-size:9.5px; letter-spacing:.16em; color:var(--beam); margin-bottom:6px; }
  .tip pre { margin:0; font-size:11px; max-height:240px; overflow:hidden; white-space:pre-wrap;
             overflow-wrap:anywhere; color:var(--txt); }
  .toast { position:fixed; bottom:26px; left:50%; transform:translateX(-50%); z-index:60;
           display:none; padding:9px 20px; border-radius:2px; font-size:11px; letter-spacing:.12em;
           background:var(--ok); color:#02120d; font-weight:700; }
  .void { color:var(--dim); letter-spacing:.16em; font-size:11px; }
  .vault { display:none; }
  .hidden { display:none !important; }
  @media (prefers-reduced-motion: reduce) {
    *,*::before,*::after { animation:none !important; transition:none !important; }
  }
  @media (max-width: 980px) {
    .shell { grid-template-columns:1fr; }
    .roster { position:static; max-height:none; border-right:0; border-bottom:1px solid var(--edge); }
  }
</style>
</head>
<body>
<div class="bg"></div>
<div class="vignette"></div>

<header class="hud">
  <div class="brand">
    <div class="sigil"><i></i></div>
    <div>
      <h1>TEMPORAL FLOW ANALYSIS</h1>
      <p class="sub">{{.Source}} &nbsp;/&nbsp; {{.Generated}} &nbsp;/&nbsp; LOGCOLLECTOR {{.Version}}</p>
    </div>
  </div>
  <div class="gauges">
    <div class="g"><b>{{len .Workflows}}</b><span>WORKFLOWS</span></div>
    <div class="g {{if .FailedCount}}bad{{else}}good{{end}}"><b>{{.FailedCount}}</b><span>FAULTED</span></div>
    <div class="g"><b>{{.StatusesChecked}}</b><span>CHECKS</span></div>
    <div class="g {{if .FlaggedCount}}bad{{end}}"><b>{{.FlaggedCount}}</b><span>FLAGGED</span></div>
    <div class="g {{if .MissingCount}}warn{{end}}"><b>{{.MissingCount}}</b><span>NEVER RAN</span></div>
  </div>
</header>

{{if .PrimaryFailure}}
<section class="signature">
  <div class="sigl">DOMINANT FAULT SIGNATURE &nbsp;/&nbsp; {{.PrimaryFailureCount}} OCCURRENCE(S)</div>
  <div class="sigt">{{.PrimaryFailure}}</div>
</section>
{{end}}

<div class="cmdbar">
  <div class="field"><span class="pfx">&gt;</span><input type="search" id="q" placeholder="filter workflow or activity" autocomplete="off"></div>
  <label class="tgl"><input type="checkbox" id="onlybad"><span>faults only</span></label>
  <button class="btn danger" type="button" id="jump">jump to fault</button>
  <span class="keys">/ SEARCH &nbsp; &uarr;&darr; MOVE &nbsp; ENTER INSPECT &nbsp; ESC CLOSE</span>
</div>

<div class="shell">
  <nav class="roster" id="roster" aria-label="Workflow roster">
    {{range $i, $w := .Workflows}}
    <button class="slot {{if $w.OK}}ok{{else}}bad{{end}}" type="button" data-idx="{{$i}}" data-ok="{{$w.OK}}">
      <span class="led"></span>
      <span class="slotbody">
        <span class="wfref">{{$w.ID}}</span>
        <span class="slotmeta">{{$w.Kind}} &nbsp;/&nbsp; {{$w.RanCount}} RAN{{if $w.SkipCount}} &nbsp;/&nbsp; {{$w.SkipCount}} SKIPPED{{end}}</span>
        <span class="strip">{{range $w.Blocks}}<i class="{{.State}}"></i>{{end}}</span>
      </span>
    </button>
    {{end}}
  </nav>

  <main class="stage" id="stage">
    {{if not .Workflows}}<p class="void">NO TEMPORAL WORKFLOWS FOUND IN THIS COLLECTION</p>{{end}}
    {{range $i, $w := .Workflows}}
    <section class="wf" data-idx="{{$i}}" data-ok="{{$w.OK}}" hidden>
      <div class="wfbar">
        <span class="verdict {{if $w.OK}}ok{{else}}bad{{end}}">{{if $w.OK}}NOMINAL{{else}}FAULT{{end}}</span>
        <h2>{{$w.ID}}</h2>
        <span class="tag">{{$w.Kind}}</span>
      </div>
      <div class="chain">
        <div class="cap">INITIATE</div>
        {{range $w.Blocks}}
        <div class="link"></div>
        <article class="cell {{.State}}" tabindex="0" data-activity="{{.Activity}}" data-status="{{.Status}}">
          <span class="idx">{{.SID}}</span>
          <span class="cbody">
            <span class="nm">{{.Activity}}</span>
            {{if gt .Attempt 1}}<span class="retry">RETRY {{.Attempt}}</span>{{end}}
          </span>
          <span class="stat">{{.Status}}</span>
          <span class="pulse"></span>
        </article>
        {{if .Reason}}
        <div class="fault">
          {{if .Device}}<span class="dev">{{.Device}}</span>{{end}}
          <span class="msg">{{.Reason}}</span>
          {{if .DefectsChecked}}
          <div class="defects{{if not .Defects}} none{{end}}{{if and .Defects (not .DefectsExact)}} weak{{end}}">
            {{if .DefectsFailed}}
            <span class="defhdr">JIRA defect lookup failed - see the collector log.</span>
            {{else if .Defects}}
              {{if .DefectsExact}}
              <span class="defhdr">KNOWN DEFECT{{if gt (len .Defects) 1}}S{{end}} - DO NOT FILE A DUPLICATE:</span>
              {{else}}
              <span class="defhdr">POSSIBLY RELATED - CONFIRM BEFORE TREATING AS A DUPLICATE:</span>
              {{end}}
              {{range .Defects}}
              <a class="defchip {{.StatusClass}}" href="{{.URL}}" target="_blank" rel="noopener noreferrer" title="{{.Summary}}">{{.Key}} &middot; {{.Status}}</a>
              {{end}}
            {{else}}
            <span class="defhdr">No matching defect found in JIRA.</span>
            {{end}}
            {{if .DefectQuery}}<span class="defq">searched {{.DefectQuery}}</span>{{end}}
          </div>
          {{end}}
        </div>
        {{end}}
        <div class="vault" hidden>
          <pre data-k="detail">{{.Detail}}</pre>
          <pre data-k="input">{{.Input}}</pre>
          <pre data-k="output">{{.Output}}</pre>
        </div>
        {{end}}
        <div class="link {{if not $w.ResultOK}}dead{{end}}"></div>
        <div class="cap {{if $w.ResultOK}}ok{{else}}bad{{end}}">{{if $w.HasResult}}{{$w.Result}}{{else}}NO WORKFLOW RESULT RECORDED{{end}}</div>
      </div>
    </section>
    {{end}}
  </main>
</div>

<aside class="drawer" id="drawer">
  <div class="dhead">
    <div>
      <div class="dtitle" id="dtitle"></div>
      <div class="dsub" id="dsub"></div>
    </div>
    <button class="btn" type="button" id="dclose">close</button>
  </div>
  <div class="dtabs">
    <button class="tab on" type="button" data-k="output">OUTPUT</button>
    <button class="tab" type="button" data-k="input">INPUT</button>
    <button class="tab" type="button" data-k="detail">EVENTS</button>
    <button class="btn" type="button" id="dcopy">copy</button>
  </div>
  <pre class="dbody" id="dbody"></pre>
</aside>

<div class="tip" id="tip"><div class="tth"></div><pre></pre></div>
<div class="toast" id="toast"></div>

<script>
(function () {
  var stage = document.getElementById("stage");
  var roster = document.getElementById("roster");
  var drawer = document.getElementById("drawer");
  var dtitle = document.getElementById("dtitle");
  var dsub = document.getElementById("dsub");
  var dbody = document.getElementById("dbody");
  var tip = document.getElementById("tip");
  var tipHead = tip.querySelector(".tth");
  var tipBody = tip.querySelector("pre");
  var toast = document.getElementById("toast");
  var q = document.getElementById("q");
  var onlybad = document.getElementById("onlybad");
  var slots = Array.prototype.slice.call(document.querySelectorAll(".slot"));
  var views = Array.prototype.slice.call(document.querySelectorAll(".wf"));
  var toastTimer = null;
  var current = -1;
  var activeCell = null;
  var activeKind = "output";

  function flash(msg) {
    toast.textContent = msg;
    toast.style.display = "block";
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { toast.style.display = "none"; }, 1500);
  }

  // Each cell is followed by an optional .fault line and then its .vault, so walk forward
  // instead of assuming a fixed sibling position.
  function payload(cell, kind) {
    var el = cell.nextElementSibling;
    while (el && !el.classList.contains("vault")) {
      if (el.classList.contains("cell")) { return ""; }
      el = el.nextElementSibling;
    }
    if (!el) { return ""; }
    var pre = el.querySelector("[data-k=" + kind + "]");
    return pre ? pre.textContent : "";
  }

  function best(cell) {
    return payload(cell, "output") || payload(cell, "input") || payload(cell, "detail");
  }

  // file:// is a secure context in Chrome/Edge/Firefox, so the async Clipboard API works from a
  // user gesture. If it is refused, select the visible text instead of shipping a hidden-input shim.
  function copyText(text) {
    if (!text) { flash("NOTHING TO COPY"); return; }
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(
        function () { flash("COPIED " + text.length + " CHARS"); },
        selectBody
      );
    } else {
      selectBody();
    }
  }

  function selectBody() {
    if (!dbody.textContent) { flash("CLIPBOARD BLOCKED"); return; }
    var sel = window.getSelection();
    var range = document.createRange();
    range.selectNodeContents(dbody);
    sel.removeAllRanges();
    sel.addRange(range);
    flash("SELECTED - PRESS CTRL+C");
  }

  // Break the spine after the first fault so the eye lands on where execution stopped being sound.
  function markDeadLinks(view) {
    var broken = false;
    Array.prototype.forEach.call(view.querySelectorAll(".chain > *"), function (el) {
      if (broken && el.classList.contains("link")) { el.classList.add("dead"); }
      if (el.classList.contains("cell") &&
          (el.classList.contains("fail") || el.classList.contains("skip"))) { broken = true; }
    });
  }

  function select(idx) {
    if (idx === current || !views[idx]) { return; }
    current = idx;
    views.forEach(function (v, i) { v.hidden = (i !== idx); });
    slots.forEach(function (s, i) { s.classList.toggle("on", i === idx); });
    closeDrawer();
  }

  function setTab(kind) {
    activeKind = kind;
    Array.prototype.forEach.call(document.querySelectorAll(".tab"), function (t) {
      t.classList.toggle("on", t.getAttribute("data-k") === kind);
    });
    var text = activeCell ? payload(activeCell, kind) : "";
    dbody.textContent = text || "NOT COLLECTED";
  }

  function openDrawer(cell) {
    activeCell = cell;
    dtitle.textContent = cell.getAttribute("data-activity");
    dsub.textContent = "SID " + cell.querySelector(".idx").textContent.trim() +
                       "  /  " + cell.getAttribute("data-status");
    setTab(activeKind);
    // The drawer supersedes the preview, and the click's own mouseover would otherwise leave
    // the tooltip parked on top of it.
    tip.style.display = "none";
    drawer.classList.add("open");
  }

  function closeDrawer() { drawer.classList.remove("open"); }

  roster.addEventListener("click", function (e) {
    var slot = e.target.closest(".slot");
    if (slot) { select(+slot.getAttribute("data-idx")); }
  });

  stage.addEventListener("click", function (e) {
    var cell = e.target.closest(".cell");
    if (cell) { openDrawer(cell); }
  });

  drawer.addEventListener("click", function (e) {
    var tab = e.target.closest(".tab");
    if (tab) { setTab(tab.getAttribute("data-k")); return; }
    if (e.target.closest("#dcopy")) {
      var text = dbody.textContent === "NOT COLLECTED" ? "" : dbody.textContent;
      copyText(text);
      return;
    }
    if (e.target.closest("#dclose")) { closeDrawer(); }
  });

  stage.addEventListener("mouseover", function (e) {
    var cell = e.target.closest(".cell");
    if (!cell || drawer.classList.contains("open")) { return; }
    var text = best(cell);
    tipHead.textContent = cell.getAttribute("data-activity") + "  /  " + cell.getAttribute("data-status");
    tipBody.textContent = text ? text.slice(0, 1200) : "NO PAYLOAD COLLECTED FOR THIS ACTIVITY";
    tip.style.display = "block";
  });

  stage.addEventListener("mouseout", function (e) {
    if (e.target.closest(".cell")) { tip.style.display = "none"; }
  });

  document.addEventListener("mousemove", function (e) {
    if (tip.style.display !== "block") { return; }
    var pad = 18;
    var r = tip.getBoundingClientRect();
    var x = e.clientX + pad;
    var y = e.clientY + pad;
    if (x + r.width > window.innerWidth) { x = e.clientX - r.width - pad; }
    if (y + r.height > window.innerHeight) { y = window.innerHeight - r.height - pad; }
    tip.style.left = Math.max(pad, x) + "px";
    tip.style.top = Math.max(pad, y) + "px";
  });

  // Precomputed per workflow and deliberately excluding .vault, so typing does not rescan
  // megabytes of payload text on every keystroke.
  var haystacks = views.map(function (v) {
    var parts = [];
    Array.prototype.forEach.call(v.querySelectorAll(".wfbar, .cell, .fault, .cap"), function (n) {
      parts.push(n.textContent);
    });
    return parts.join(" ").toLowerCase();
  });

  function applyFilter() {
    var term = q.value.trim().toLowerCase();
    var badOnly = onlybad.checked;
    var firstVisible = -1;
    slots.forEach(function (s, i) {
      var isBad = s.getAttribute("data-ok") === "false";
      var show = (!badOnly || isBad) && (!term || haystacks[i].indexOf(term) >= 0);
      s.classList.toggle("hidden", !show);
      if (show && firstVisible < 0) { firstVisible = i; }
    });
    if (firstVisible >= 0 && slots[current] && slots[current].classList.contains("hidden")) {
      select(firstVisible);
    }
  }

  q.addEventListener("input", applyFilter);
  onlybad.addEventListener("change", applyFilter);

  document.getElementById("jump").addEventListener("click", function () {
    var cell = views[current] ? views[current].querySelector(".cell.fail, .cell.skip") : null;
    if (!cell) {
      for (var i = 0; i < views.length; i++) {
        if (views[i].getAttribute("data-ok") === "false") {
          select(i);
          cell = views[i].querySelector(".cell.fail, .cell.skip");
          break;
        }
      }
    }
    if (!cell) { flash("NO FAULTS DETECTED"); return; }
    cell.scrollIntoView({ block: "center", behavior: "smooth" });
    cell.focus({ preventScroll: true });
  });

  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") { closeDrawer(); tip.style.display = "none"; return; }
    if (e.key === "/" && document.activeElement !== q) { e.preventDefault(); q.focus(); return; }
    if (document.activeElement === q || !views[current]) { return; }
    var cells = Array.prototype.slice.call(views[current].querySelectorAll(".cell"));
    var pos = cells.indexOf(document.activeElement);
    if (e.key === "ArrowDown" || e.key === "j") {
      e.preventDefault();
      var next = cells[pos < 0 ? 0 : Math.min(cells.length - 1, pos + 1)];
      if (next) { next.focus(); next.scrollIntoView({ block: "nearest" }); }
    } else if (e.key === "ArrowUp" || e.key === "k") {
      e.preventDefault();
      var prev = cells[pos < 0 ? 0 : Math.max(0, pos - 1)];
      if (prev) { prev.focus(); prev.scrollIntoView({ block: "nearest" }); }
    } else if (e.key === "Enter" && pos >= 0) {
      e.preventDefault();
      openDrawer(cells[pos]);
    }
  });

  views.forEach(markDeadLinks);

  // Open on the first faulted workflow: the reason the report was generated.
  var start = 0;
  for (var i = 0; i < views.length; i++) {
    if (views[i].getAttribute("data-ok") === "false") { start = i; break; }
  }
  select(start);
})();
</script>
</body>
</html>
`

// writeTemporalFlowHTMLReport emits the interactive flow report next to the text report. Failure
// to write it is logged but never fails the run, since the text report already carries the data.
func writeTemporalFlowHTMLReport(reportPath, source string, temporal TemporalAnalysisResult, taCfg TemporalAnalysisConfig) {
	if !temporal.Enabled || !taCfg.wantsHTMLReport() || len(temporal.Flows) == 0 {
		return
	}
	htmlPath := strings.TrimSuffix(reportPath, filepath.Ext(reportPath)) + "_temporal_flow.html"
	data := buildFlowHTMLData(temporal, filepath.Base(source), appVersion, taCfg)
	if err := writeTemporalFlowHTML(htmlPath, data); err != nil {
		logger.Warn("Failed to write Temporal flow HTML report: %v", err)
		return
	}
	logger.Info("Temporal flow HTML report generated: %s", htmlPath)

	if taCfg.wantsHTMLAutoOpen() {
		if absPath, err := filepath.Abs(htmlPath); err == nil {
			openBrowser(absPath)
		} else {
			openBrowser(htmlPath)
		}
	}
}

// TemporalMissingActivity records a configured activity that never appeared in a workflow's
// event history. A missing activity is not an all-clear — it means nothing was validated.
type TemporalMissingActivity struct {
	WorkflowID string
	Activity   string
}

// groupMissingByActivity collapses the flat missing list into "activity -> workflows it is
// missing from", which reads far better than one line per workflow/activity combination.
func groupMissingByActivity(missing []TemporalMissingActivity) ([]string, map[string][]string) {
	order := []string{}
	byActivity := make(map[string][]string)
	for _, m := range missing {
		if _, seen := byActivity[m.Activity]; !seen {
			order = append(order, m.Activity)
		}
		byActivity[m.Activity] = append(byActivity[m.Activity], m.WorkflowID)
	}
	return order, byActivity
}

// temporalSuccessStatuses lists activity output "status" values treated as successful.
// Anything else found in an activity output payload is flagged.
func defaultTemporalSuccessStatuses() map[string]bool {
	return map[string]bool{"completed_success": true}
}

// extractJSONObjects returns every top-level {...} block found in text, tolerating surrounding
// prose, multiple concatenated objects, and JSON arrays of objects.
func extractJSONObjects(text string) []string {
	var objects []string
	depth, start := 0, -1
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					objects = append(objects, text[start:i+1])
					start = -1
				}
			}
		}
	}
	return objects
}

// extractActivityStatusFailures parses an activity output payload and returns one issue per JSON
// object whose "status" field is not a recognized success value, plus the number of status fields
// actually inspected. A zero count means the payload carried nothing to validate — which is not
// the same as a clean result.
func extractActivityStatusFailures(outputText string, successStatuses map[string]bool) (issues []TemporalActivityIssue, statusesChecked int) {
	for _, raw := range extractJSONObjects(outputText) {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			continue
		}
		status, isString := obj["status"].(string)
		if !isString {
			continue
		}
		status = strings.TrimSpace(status)
		if status == "" {
			continue
		}
		statusesChecked++
		if successStatuses[strings.ToLower(status)] {
			continue
		}
		issues = append(issues, TemporalActivityIssue{
			Status:        status,
			StatusMessage: strings.TrimSpace(asString(obj["status_message"])),
			DeviceSerial:  asString(obj["device_serial_number"]),
			DeviceID:      asString(obj["device_id"]),
		})
	}
	return issues, statusesChecked
}

// jsonStatusField is a status-like key found anywhere in a decoded workflow result payload.
type jsonStatusField struct {
	Key   string
	Value string
	Owner map[string]interface{}
}

// collectStatusFields walks a decoded JSON value and returns every status-like key carrying a
// non-empty string value. Workflow results nest the real verdict under varying names and depths
// (e.g. "Status" at the top level, or "BatchExec.batch_status"), so a fixed top-level "status"
// lookup misses them. Map keys are visited in sorted order to keep reporting deterministic.
func collectStatusFields(v interface{}, out *[]jsonStatusField) {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := t[k]
			lk := strings.ToLower(k)
			if lk == "status" || strings.HasSuffix(lk, "_status") {
				if s, ok := val.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						*out = append(*out, jsonStatusField{Key: k, Value: s, Owner: t})
					}
				}
			}
			collectStatusFields(val, out)
		}
	case []interface{}:
		for _, item := range t {
			collectStatusFields(item, out)
		}
	}
}

// workflowResultStatuses returns every status-like field found in a workflow result payload.
func workflowResultStatuses(outputText string) []jsonStatusField {
	var fields []jsonStatusField
	for _, raw := range extractJSONObjects(outputText) {
		var obj interface{}
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			continue
		}
		collectStatusFields(obj, &fields)
	}
	return fields
}

// extractWorkflowResultFailures validates the workflow-level result payload. Temporal can report
// a workflow as COMPLETED while the business result inside says otherwise (e.g. batch_status
// COMPLETED_FAILURE), so this is checked independently of the per-activity outputs.
func extractWorkflowResultFailures(outputText string, successStatuses map[string]bool) (issues []TemporalActivityIssue, statusesChecked int) {
	for _, f := range workflowResultStatuses(outputText) {
		statusesChecked++
		if successStatuses[strings.ToLower(f.Value)] {
			continue
		}
		issues = append(issues, TemporalActivityIssue{
			Activity:      fmt.Sprintf("(workflow result: %s)", f.Key),
			Status:        f.Value,
			StatusMessage: workflowResultMessage(f.Owner),
			DeviceSerial:  asString(f.Owner["device_serial_number"]),
			DeviceID:      asString(f.Owner["device_id"]),
		})
	}
	return issues, statusesChecked
}

// summarizeWorkflowResult renders a workflow result as "key=value" text plus whether every status
// in it was successful.
func summarizeWorkflowResult(outputText string, successStatuses map[string]bool) (string, bool, bool) {
	fields := workflowResultStatuses(outputText)
	if len(fields) == 0 {
		return "", false, false
	}
	parts := make([]string, 0, len(fields))
	allOK := true
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%s=%s", f.Key, f.Value))
		if !successStatuses[strings.ToLower(f.Value)] {
			allOK = false
		}
	}
	return strings.Join(parts, ", "), allOK, true
}

// workflowResultMessage picks the most useful explanatory text sitting alongside a status field.
func workflowResultMessage(owner map[string]interface{}) string {
	for _, key := range []string{"status_message", "statusMessage", "message", "Message", "error", "Error", "reason"} {
		if msg := strings.TrimSpace(asString(owner[key])); msg != "" {
			return msg
		}
	}
	return ""
}

// extractWorkflowOutputSection returns the WORKFLOW OUTPUT block of a collected workflow overview
// file, i.e. the text between that banner and the next one.
func extractWorkflowOutputSection(content string) (string, bool) {
	const banner = "  WORKFLOW OUTPUT\n"
	idx := strings.Index(content, banner)
	if idx < 0 {
		return "", false
	}
	rest := content[idx+len(banner):]
	if nl := strings.Index(rest, "\n"); nl >= 0 {
		rest = rest[nl+1:] // drop the banner's closing ==== rule
	}
	if end := strings.Index(rest, "\n===="); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest), true
}

// formatTemporalActivityIssue renders a flagged activity as a single console/report line.
func formatTemporalActivityIssue(issue TemporalActivityIssue) string {
	line := fmt.Sprintf("workflow=%s | activity=%s | status=%s", issue.WorkflowID, issue.Activity, issue.Status)
	if issue.DeviceSerial != "" {
		line += fmt.Sprintf(" | serial=%s", issue.DeviceSerial)
	}
	if issue.StatusMessage != "" {
		line += fmt.Sprintf(" | message=%s", issue.StatusMessage)
	}
	return line
}

// asString normalizes a generic JSON value (string, float64, etc.) to its string form
func asString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// extractWorkflowStartInput returns the decoded input payload from the workflow start event
func extractWorkflowStartInput(events []map[string]interface{}) string {
	if len(events) == 0 {
		return "No input data found"
	}
	attrs, ok := events[0]["workflowExecutionStartedEventAttributes"].(map[string]interface{})
	if !ok {
		return "No input data found"
	}
	return decodePayloadsField(attrs, "input")
}

// extractWorkflowCompletedOutput returns the decoded result payload from the workflow completed event, if any
func extractWorkflowCompletedOutput(events []map[string]interface{}) string {
	for _, ev := range events {
		if et, _ := ev["eventType"].(string); et == "EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED" {
			if attrs, ok := ev["workflowExecutionCompletedEventAttributes"].(map[string]interface{}); ok {
				return decodePayloadsField(attrs, "result")
			}
		}
	}
	return "No output data found or workflow still running"
}

// listScheduledActivities returns every activity scheduled event found in the history, in order
func listScheduledActivities(events []map[string]interface{}) []activityScheduledRef {
	var out []activityScheduledRef
	for _, ev := range events {
		attrs, ok := ev["activityTaskScheduledEventAttributes"].(map[string]interface{})
		if !ok {
			continue
		}
		name := ""
		if at, ok := attrs["activityType"].(map[string]interface{}); ok {
			name, _ = at["name"].(string)
		}
		et, _ := ev["eventType"].(string)
		out = append(out, activityScheduledRef{
			EventID:   asString(ev["eventId"]),
			EventType: et,
			Name:      name,
		})
	}
	return out
}

// findScheduledEventsByName returns all activityTaskScheduled events whose activity type name matches
func findScheduledEventsByName(events []map[string]interface{}, activityName string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, ev := range events {
		attrs, ok := ev["activityTaskScheduledEventAttributes"].(map[string]interface{})
		if !ok {
			continue
		}
		at, ok := attrs["activityType"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := at["name"].(string)
		if name == activityName {
			out = append(out, ev)
		}
	}
	return out
}

// findEventsByScheduledID returns started/completed/failed/timedOut/cancelRequested events
// whose scheduledEventId matches the given scheduled event id (sid)
func findEventsByScheduledID(events []map[string]interface{}, sid string) []map[string]interface{} {
	attrKeys := []string{
		"activityTaskStartedEventAttributes",
		"activityTaskCompletedEventAttributes",
		"activityTaskFailedEventAttributes",
		"activityTaskTimedOutEventAttributes",
		"activityTaskCancelRequestedEventAttributes",
	}
	var out []map[string]interface{}
	for _, ev := range events {
		for _, key := range attrKeys {
			attrs, ok := ev[key].(map[string]interface{})
			if !ok {
				continue
			}
			if v, ok := attrs["scheduledEventId"]; ok && asString(v) == sid {
				out = append(out, ev)
				break
			}
		}
	}
	return out
}

// resolveActivitySetForWorkflow finds the workflowActivitySets entry whose key is the
// longest prefix match of the workflow ID (e.g. key "deploy-site" matches
// "deploy-site-CP1-20260720-054850-usr1007-f82a9e-batch-1"). Returns the matched
// activity list and the matched key (empty if no match).
func resolveActivitySetForWorkflow(workflowID string, sets map[string][]string) ([]string, string) {
	bestKey := ""
	for key := range sets {
		if key == "" {
			continue
		}
		if strings.HasPrefix(workflowID, key) && len(key) > len(bestKey) {
			bestKey = key
		}
	}
	if bestKey == "" {
		return nil, ""
	}
	return sets[bestKey], bestKey
}

// decodePayloadsField decodes the first payload's data from a Temporal event attributes
// field (e.g. attrs["input"] or attrs["result"]), mirroring `.input.payloads[0].data`
func decodePayloadsField(container map[string]interface{}, field string) string {
	fieldVal, ok := container[field].(map[string]interface{})
	if !ok {
		return "No payload found"
	}
	payloadsArr, ok := fieldVal["payloads"].([]interface{})
	if !ok || len(payloadsArr) == 0 {
		return "No payload found"
	}
	p0, ok := payloadsArr[0].(map[string]interface{})
	if !ok {
		return "No payload found"
	}
	data, _ := p0["data"].(string)
	if data == "" {
		return "No payload found"
	}
	return decodeTemporalPayloadData(data)
}

// decodeTemporalPayloadData decodes a base64-encoded Temporal Payload data blob.
// Payloads may be zlib-compressed and wrapped in a protobuf "Payload" message
// (field 2 = raw data bytes); this mirrors the decode logic used by the reference
// temporal_log.sh script, implemented natively in Go (no python3/jq dependency).
func decodeTemporalPayloadData(data string) string {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		if raw2, err2 := base64.URLEncoding.DecodeString(data); err2 == nil {
			raw = raw2
		} else {
			return fmt.Sprintf("Base64 decode failed: %v", err)
		}
	}

	// Repeatedly zlib-inflate while the zlib magic header is present
	for len(raw) >= 2 && raw[0] == 0x78 && (raw[1] == 0x9c || raw[1] == 0x01 || raw[1] == 0xda || raw[1] == 0x5e) {
		zr, zErr := zlib.NewReader(bytes.NewReader(raw))
		if zErr != nil {
			break
		}
		decompressed, readErr := io.ReadAll(zr)
		zr.Close()
		if readErr != nil {
			break
		}
		raw = decompressed
	}

	// Try to locate a length-delimited protobuf field 2 (the wrapped raw data) and use that if present
	if value, found := scanProtobufField2(raw); found {
		if pretty, ok := tryPrettyJSON(value); ok {
			return pretty
		}
		return truncateText(string(value), 2000)
	}

	if pretty, ok := tryPrettyJSON(raw); ok {
		return pretty
	}
	return truncateText(string(raw), 2000)
}

// scanProtobufField2 does a minimal protobuf wire-format scan looking for a
// length-delimited field number 2 (matches the python reference implementation)
func scanProtobufField2(raw []byte) ([]byte, bool) {
	pos := 0
	for pos < len(raw) {
		tag, newPos, ok := readVarint(raw, pos)
		if !ok {
			return nil, false
		}
		pos = newPos
		field := tag >> 3
		wire := tag & 7

		switch wire {
		case 2:
			length, newPos2, ok := readVarint(raw, pos)
			if !ok {
				return nil, false
			}
			pos = newPos2
			if int(length) < 0 || pos+int(length) > len(raw) {
				return nil, false
			}
			value := raw[pos : pos+int(length)]
			pos += int(length)
			if field == 2 {
				return value, true
			}
		case 0:
			_, newPos3, ok := readVarint(raw, pos)
			if !ok {
				return nil, false
			}
			pos = newPos3
		case 1:
			pos += 8
		case 5:
			pos += 4
		default:
			return nil, false
		}
	}
	return nil, false
}

// readVarint reads a protobuf varint starting at pos, returning the value and next position
func readVarint(buf []byte, pos int) (uint64, int, bool) {
	var result uint64
	var shift uint
	for pos < len(buf) {
		b := buf[pos]
		pos++
		result |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, pos, true
		}
		shift += 7
		if shift > 63 {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

// tryPrettyJSON attempts to parse b as JSON and returns an indented pretty-printed string
func tryPrettyJSON(b []byte) (string, bool) {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", false
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", false
	}
	return string(pretty), true
}

// truncateText truncates s to at most n bytes (used as a fallback when payload isn't JSON)
func truncateText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// maxDetailedPayloadChars caps each decoded payload rendered into a _detailed.txt history.
// Full-fidelity payloads are already saved per activity in <Activity>_input.txt/_output.txt,
// so a single 700KB config blob does not need to be inlined here as well.
const maxDetailedPayloadChars = 4000

// decodeDetailedHistoryPayloads rewrites `temporal workflow show --detailed` output so the
// base64/zlib "payloads[N].data" blobs read as JSON. Unrecognized lines pass through unchanged.
func decodeDetailedHistoryPayloads(text string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		clean := strings.TrimRight(line, "\r")
		indent := clean[:len(clean)-len(strings.TrimLeft(clean, " \t"))]

		// Temporal's trailing "Results:" block renders the payload as inline JSON
		// ({"metadata":{"encoding":"..."},"data":"..."}) instead of a dotted key path.
		if idx := strings.Index(clean, `{"metadata"`); idx >= 0 {
			if decoded, ok := decodeInlinePayloadJSON(clean[idx:]); ok {
				out = append(out, strings.TrimRight(clean[:idx], " \t")+" (decoded)")
				out = append(out, indentLines(renderDecodedPayload(decoded), indent+"    ")...)
				continue
			}
		}

		key, value, found := strings.Cut(clean, ":")
		trimmedKey := strings.TrimSpace(key)
		if !found || !strings.Contains(trimmedKey, "payloads[") {
			out = append(out, line)
			continue
		}
		value = strings.TrimSpace(value)

		switch {
		case strings.HasSuffix(trimmedKey, ".data") && value != "":
			out = append(out, indent+trimmedKey+": (decoded)")
			out = append(out, indentLines(renderDecodedPayload(decodeTemporalPayloadData(value)), indent+"    ")...)
		case strings.HasSuffix(trimmedKey, ".metadata.encoding") && value != "":
			// Short mime-type marker such as base64("binary/zlib")
			if b, err := base64.StdEncoding.DecodeString(value); err == nil && isPrintableASCII(b) {
				out = append(out, indent+trimmedKey+": "+string(b))
			} else {
				out = append(out, line)
			}
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// decodeInlinePayloadJSON decodes a Temporal Payload serialized inline as JSON.
func decodeInlinePayloadJSON(s string) (string, bool) {
	var p struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &p); err != nil || p.Data == "" {
		return "", false
	}
	return decodeTemporalPayloadData(p.Data), true
}

// renderDecodedPayload caps a decoded payload so one large config blob cannot dominate the file.
func renderDecodedPayload(decoded string) string {
	if len(decoded) <= maxDetailedPayloadChars {
		return decoded
	}
	return decoded[:maxDetailedPayloadChars] +
		fmt.Sprintf("\n... [truncated %d chars - full payload in the matching _activities file]",
			len(decoded)-maxDetailedPayloadChars)
}

func indentLines(s, indent string) []string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}
	return lines
}

func isPrintableASCII(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

// extractWorkflowIDs parses workflow listing output and extracts workflow IDs
// Supports both JSON output and plain text tabular output from `temporal workflow list`.
// workflowIdKeyword (if non-empty) further restricts the result to workflow IDs containing
// that arbitrary substring (case-insensitive) — e.g. "batch" to prefer batched instances like
// "deploy-site-CP1-...-batch-1" over their non-batch parent "deploy-site-CP1-...", though any
// keyword can be used depending on your workflow ID naming convention.
// The filter is applied before the maxCount cutoff so the most recent N *matching*
// workflows are returned (temporal workflow list returns newest-first).
func extractWorkflowIDs(listOutput string, prefixFilter string, workflowIdKeyword string, maxCount int) []string {
	var workflowIDs []string
	trimmed := strings.TrimSpace(listOutput)

	// Try JSON parsing first — temporal workflow list --output json returns one JSON object per line (JSONL)
	// or could return a JSON array. Look for workflowId or WorkflowId fields.
	if strings.Contains(trimmed, "workflowId") || strings.Contains(trimmed, "WorkflowId") || strings.Contains(trimmed, "workflow_id") {
		lines := strings.Split(trimmed, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// Extract workflowId from JSON-like content using simple string matching
			// Handles formats like: "workflowId": "deploy-profile-..."
			for _, key := range []string{`"workflowId"`, `"WorkflowId"`, `"workflow_id"`} {
				idx := strings.Index(line, key)
				if idx == -1 {
					continue
				}
				// Find the value after the key
				rest := line[idx+len(key):]
				// Skip : and whitespace
				rest = strings.TrimLeft(rest, ": \t")
				// Extract quoted value
				if len(rest) > 0 && rest[0] == '"' {
					rest = rest[1:]
					endIdx := strings.Index(rest, "\"")
					if endIdx > 0 {
						wfID := rest[:endIdx]
						if prefixFilter == "" || strings.HasPrefix(wfID, prefixFilter) {
							workflowIDs = append(workflowIDs, wfID)
						}
					}
				}
				break
			}
		}
		// Deduplicate (same workflow ID might appear in multiple JSON fields)
		seen := make(map[string]bool)
		unique := []string{}
		for _, id := range workflowIDs {
			if !seen[id] {
				seen[id] = true
				unique = append(unique, id)
			}
		}
		workflowIDs = unique
	}

	// If JSON parsing didn't find anything, try tabular format
	if len(workflowIDs) == 0 {
		lines := strings.Split(trimmed, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			// Skip header lines
			if strings.HasPrefix(line, "Status") || strings.HasPrefix(line, "------") || strings.HasPrefix(line, "#") {
				continue
			}

			// Try to extract workflow ID from tabular format:
			// "Running  deploy-profile-Test_profile-20260105-141248  WorkflowType  2025-01-05T..."
			// "Completed  workflow-id  WorkflowType  2025-01-05T..."
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				possibleStatus := strings.ToLower(fields[0])
				if possibleStatus == "running" || possibleStatus == "completed" || possibleStatus == "failed" ||
					possibleStatus == "canceled" || possibleStatus == "terminated" || possibleStatus == "timedout" ||
					possibleStatus == "continueasnew" {
					workflowID := fields[1]
					if prefixFilter != "" && !strings.HasPrefix(workflowID, prefixFilter) {
						continue
					}
					workflowIDs = append(workflowIDs, workflowID)
				}
			}
		}
	}

	// Apply the workflow ID keyword filter (case-insensitive substring match) before the
	// maxCount cutoff, so we keep the most recent N *matching* workflows rather than
	// truncating first and filtering away entries that would have made the cut.
	if workflowIdKeyword != "" {
		lowerKeyword := strings.ToLower(workflowIdKeyword)
		var filtered []string
		for _, id := range workflowIDs {
			if strings.Contains(strings.ToLower(id), lowerKeyword) {
				filtered = append(filtered, id)
			}
		}
		workflowIDs = filtered
	}

	// Limit to maxCount
	if len(workflowIDs) > maxCount {
		workflowIDs = workflowIDs[:maxCount]
	}

	return workflowIDs
}

// collectTemporalScheduleInfo collects Temporal schedule information from the admin pod
func collectTemporalScheduleInfo(awsClient *ssh.Client, temporalConfig struct {
	Enabled           bool   `yaml:"enabled"`
	NumberOfSchedules int    `yaml:"numberOfSchedules"`
	Namespace         string `yaml:"namespace"`
}, environment, username, tempDir, finalLogFileName string) error {
	if !temporalConfig.Enabled {
		logger.Debug("Temporal schedule collection is disabled")
		return nil
	}

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  TEMPORAL SCHEDULES - Collecting schedule information")
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("Starting Temporal schedule information collection...")

	// Set defaults
	temporalNamespace := temporalConfig.Namespace
	if temporalNamespace == "" {
		temporalNamespace = "configuration"
	}
	numberOfSchedules := temporalConfig.NumberOfSchedules
	if numberOfSchedules <= 0 {
		numberOfSchedules = 5
	}
	if numberOfSchedules > 20 {
		numberOfSchedules = 20
	}

	// Use the same Temporal output directory that was created for workflow collection
	logDir := fmt.Sprintf("%s/%s", tempDir, finalLogFileName)
	temporalOutputDir := fmt.Sprintf("%s/Temporal", logDir)

	// Ensure the directory exists (it should already exist from workflow collection, but check anyway)
	session, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for temporal directory: %v", err)
	}
	defer session.Close()

	mkdirCmd := fmt.Sprintf("mkdir -p %s", temporalOutputDir)
	if err := executeCommandAsRoot(session, mkdirCmd); err != nil {
		return fmt.Errorf("failed to create temporal directory: %v", err)
	}

	// Step 1: Find the temporal admin pod (same as workflow collection)
	logger.Info("Discovering Temporal admin pod in 'common' namespace...")
	podSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for pod discovery: %v", err)
	}

	podCmd := "kubectl get pods -n common --no-headers | grep temporal-admintools | grep Running | head -1 | awk '{print \\$1}'"
	podOutput, err := podSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", podCmd))
	podSession.Close()
	if err != nil {
		logger.Debug("Pod discovery command output: %s", strings.TrimSpace(string(podOutput)))
		return fmt.Errorf("failed to discover temporal admin pod: %v", err)
	}

	adminPod := strings.TrimSpace(string(podOutput))
	if adminPod == "" {
		return fmt.Errorf("no running temporal-admintools pod found in 'common' namespace")
	}
	logger.Info("Found Temporal admin pod: %s", adminPod)

	// Step 2: List schedules
	logger.Info("Listing schedules in namespace '%s'...", temporalNamespace)

	listSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for schedule listing: %v", err)
	}

	listCmd := fmt.Sprintf("kubectl exec %s -n common -- temporal schedule list --namespace %s 2>/dev/null",
		adminPod, temporalNamespace)
	listOutput, err := listSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", listCmd))
	listSession.Close()
	if err != nil {
		return fmt.Errorf("failed to list temporal schedules: %v\\nOutput: %s", err, string(listOutput))
	}

	// Save the schedule listing as schedule_list.txt
	writeFileToRemote(awsClient, fmt.Sprintf("%s/schedule_list.txt", temporalOutputDir),
		fmt.Sprintf("# Temporal Schedule List\n# Namespace: %s\n# Collected: %s\n%s\n\n%s",
			temporalNamespace, time.Now().Format("2006-01-02 15:04:05"),
			strings.Repeat("-", 60),
			string(listOutput)))

	// Step 3: Extract schedule IDs from the listing
	listOutputStr := string(listOutput)
	logger.Debug("Schedule list output (first 500 chars): %s", func() string {
		if len(listOutputStr) > 500 {
			return listOutputStr[:500] + "..."
		}
		return listOutputStr
	}())

	scheduleIDs := extractScheduleIDs(listOutputStr, numberOfSchedules)
	if len(scheduleIDs) == 0 {
		logger.Warn("No schedule IDs found")
		writeFileToRemote(awsClient, fmt.Sprintf("%s/no_schedules_found.txt", temporalOutputDir),
			fmt.Sprintf("No schedules found.\nNamespace: %s\nRaw listing output:\n%s\n",
				temporalNamespace, listOutputStr))
		return nil
	}

	logger.Info("Found %d schedule(s) to collect information for", len(scheduleIDs))
	logger.Debug("Extracted schedule IDs: %v", scheduleIDs)
	for i, schedID := range scheduleIDs {
		logger.Info("  %d. %s", i+1, schedID)
	}

	// Step 4: For each schedule, collect detailed information using `temporal schedule describe`
	for i, scheduleID := range scheduleIDs {
		logger.Info("Collecting data for schedule %d/%d: %s", i+1, len(scheduleIDs), scheduleID)

		// Create a sanitized filename from schedule ID
		safeSchedID := sanitizeFilename(scheduleID)
		schedOutputFile := fmt.Sprintf("%s/schedule_%s.txt", temporalOutputDir, safeSchedID)

		var schedContent strings.Builder
		schedContent.WriteString("# Temporal Schedule Details\n")
		schedContent.WriteString(fmt.Sprintf("# Schedule ID: %s\n", scheduleID))
		schedContent.WriteString(fmt.Sprintf("# Namespace: %s\n", temporalNamespace))
		schedContent.WriteString(fmt.Sprintf("# Collected: %s\n", time.Now().Format("2006-01-02 15:04:05")))
		schedContent.WriteString(fmt.Sprintf("#%s\n\n", strings.Repeat("-", 60)))

		// Describe the schedule
		logger.Debug("  Collecting schedule details...")
		schedContent.WriteString("=" + strings.Repeat("=", 79) + "\n")
		schedContent.WriteString("  SCHEDULE DESCRIPTION\n")
		schedContent.WriteString("=" + strings.Repeat("=", 79) + "\n\n")

		describeCmd := fmt.Sprintf(`kubectl exec %s -n common -- temporal schedule describe --namespace %s --schedule-id "%s" 2>&1`,
			adminPod, temporalNamespace, scheduleID)
		describeOutput := executeTemporalCommand(awsClient, describeCmd)
		schedContent.WriteString(describeOutput + "\n\n")

		// Write the complete schedule file
		writeFileToRemote(awsClient, schedOutputFile, schedContent.String())
		logger.Info("  Saved schedule data to: %s", schedOutputFile)
	}

	logger.Info("Temporal schedule collection completed: %d schedule(s) collected", len(scheduleIDs))
	logger.Info("Temporal schedule data saved to remote directory: %s", temporalOutputDir)
	return nil
}

// extractScheduleIDs parses schedule listing output and extracts schedule IDs
// The `temporal schedule list` output format is typically:
//
//	ScheduleId          WorkflowType        ...
//	schedule-profile-   DeployProfile       ...
func extractScheduleIDs(listOutput string, maxCount int) []string {
	var scheduleIDs []string
	lines := strings.Split(strings.TrimSpace(listOutput), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Skip header line (contains "ScheduleId" or starts with common header indicators)
		if strings.Contains(strings.ToLower(line), "scheduleid") ||
			strings.HasPrefix(line, "Schedule ID") ||
			strings.HasPrefix(line, "---") ||
			strings.HasPrefix(line, "===") {
			continue
		}

		// The schedule ID is typically the first column - use Fields() to handle varying whitespace
		fields := strings.Fields(line)
		if len(fields) > 0 {
			schedID := fields[0]
			// Validate that it looks like a schedule ID (not empty, not a header remnant)
			if schedID != "" && !strings.Contains(schedID, "Schedule") {
				scheduleIDs = append(scheduleIDs, schedID)
				if len(scheduleIDs) >= maxCount {
					break
				}
			}
		}
	}

	return scheduleIDs
}

// ============================================================================
// Pod File Collection — Collect specific files from inside pods
// ============================================================================

// collectPodFiles collects specific files from pods based on configuration
func collectPodFiles(awsClient *ssh.Client, collections []PodFileCollection, tempDir, finalLogFileName string) error {
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  POD FILE COLLECTION - Collecting files from inside pods")
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("Collecting pod files from %d configuration(s)...", len(collections))

	for i, collection := range collections {
		logger.Info("[%d/%d] Processing: namespace=%s, podPrefix=%s", i+1, len(collections), collection.Namespace, collection.PodPrefix)

		// Step 1: Find pods matching the prefix
		pods, err := findPodsMatchingPrefix(awsClient, collection.Namespace, collection.PodPrefix)
		if err != nil {
			logger.Warn("Failed to find pods for %s/%s: %v", collection.Namespace, collection.PodPrefix, err)
			continue
		}

		if len(pods) == 0 {
			logger.Warn("No pods found matching prefix %s in namespace %s", collection.PodPrefix, collection.Namespace)
			continue
		}

		logger.Info("Found %d pod(s) matching prefix %s", len(pods), collection.PodPrefix)

		// Step 2: For each pod, collect files matching the patterns
		for _, pod := range pods {
			logger.Debug("  Processing pod: %s", pod)

			// Create output directory for this pod
			podOutputDir := fmt.Sprintf("%s/%s/PodFiles/%s/%s", tempDir, finalLogFileName, collection.Namespace, pod)
			mkdirSession, err := awsClient.NewSession()
			if err != nil {
				logger.Warn("  Failed to create session for mkdir: %v", err)
				continue
			}
			mkdirCmd := fmt.Sprintf("mkdir -p %s", podOutputDir)
			executeCommandAsRoot(mkdirSession, mkdirCmd)
			mkdirSession.Close()

			// Step 3: For each file pattern, find and copy files
			for _, pattern := range collection.FilePatterns {
				logger.Debug("    Looking for files matching: %s", pattern)

				// List files matching the pattern
				files, err := listFilesInPod(awsClient, collection.Namespace, pod, collection.LogPath, pattern, collection.MatchPodName)
				if err != nil {
					logger.Warn("    Failed to list files in pod %s: %v", pod, err)
					continue
				}

				if len(files) == 0 {
					logger.Debug("    No files found matching pattern: %s", pattern)
					continue
				}

				logger.Info("    Found %d file(s) matching pattern %s", len(files), pattern)

				// Copy each file
				for _, file := range files {
					logger.Debug("      Copying: %s", file)
					err := copyFileFromPod(awsClient, collection.Namespace, pod, file, podOutputDir)
					if err != nil {
						logger.Warn("      Failed to copy %s: %v", file, err)
					} else {
						logger.Debug("      ✓ Copied successfully")
					}
				}
			}
		}
	}

	logger.Info("Pod file collection completed")
	return nil
}

// findPodsMatchingPrefix finds all pods in a namespace matching the given prefix
func findPodsMatchingPrefix(awsClient *ssh.Client, namespace, podPrefix string) ([]string, error) {
	session, err := awsClient.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %v", err)
	}
	defer session.Close()

	// Use simpler approach: get all pods and filter in Go instead of complex shell quoting
	cmd := fmt.Sprintf("kubectl get pods -n %s --no-headers -o custom-columns=NAME:.metadata.name", namespace)
	logger.Debug("Executing command: sudo su - -c \"%s\"", cmd)

	output, err := session.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", cmd))
	if err != nil {
		logger.Debug("Command output: %s", string(output))
		return nil, fmt.Errorf("kubectl command failed: %v", err)
	}

	logger.Debug("Pod list output: %s", string(output))

	pods := []string{}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		pod := strings.TrimSpace(line)
		// Filter by prefix in Go instead of in shell
		if pod != "" && strings.HasPrefix(pod, podPrefix) {
			pods = append(pods, pod)
		}
	}

	logger.Debug("Found %d pods matching prefix '%s': %v", len(pods), podPrefix, pods)
	return pods, nil
}

// listFilesInPod lists files in a pod directory matching a pattern
// If matchPodName is true, only returns files whose basename starts with the pod name
func listFilesInPod(awsClient *ssh.Client, namespace, pod, logPath, pattern string, matchPodName bool) ([]string, error) {
	session, err := awsClient.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %v", err)
	}
	defer session.Close()

	// Ensure logPath ends with /
	if !strings.HasSuffix(logPath, "/") {
		logPath += "/"
	}

	// Use find command to list files matching pattern
	// Use double quotes for outer command, single quotes for pattern
	findCmd := fmt.Sprintf("kubectl exec -n %s %s -- find %s -maxdepth 1 -type f -name '%s' 2>/dev/null",
		namespace, pod, logPath, pattern)
	logger.Debug("Executing find command: %s", findCmd)

	output, err := session.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", findCmd))
	if err != nil {
		logger.Debug("Find command failed, trying ls fallback. Error: %v, Output: %s", err, string(output))
		// Try simpler ls-based approach as fallback
		lsCmd := fmt.Sprintf("kubectl exec -n %s %s -- ls %s%s 2>/dev/null",
			namespace, pod, logPath, pattern)
		logger.Debug("Executing ls command: %s", lsCmd)
		output2, err2 := session.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", lsCmd))
		if err2 != nil {
			logger.Debug("Ls command also failed. Error: %v, Output: %s", err2, string(output2))
			return nil, fmt.Errorf("both find and ls commands failed: %v, %v", err, err2)
		}
		output = output2
		logger.Debug("Ls command output: %s", string(output))
	} else {
		logger.Debug("Find command output: %s", string(output))
	}

	files := []string{}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		file := strings.TrimSpace(line)
		if file != "" && !strings.HasSuffix(file, "/") {
			// If file is just a basename, prepend the logPath
			if !strings.HasPrefix(file, "/") {
				file = logPath + file
			}

			// Filter by pod name if matchPodName is enabled
			if matchPodName {
				basename := filepath.Base(file)
				// Only include files where basename starts with pod name
				if !strings.HasPrefix(basename, pod) {
					logger.Debug("      Skipping %s (does not match pod name %s)", basename, pod)
					continue
				}
			}

			files = append(files, file)
		}
	}

	return files, nil
}

// copyFileFromPod copies a file from a pod to the local AWS server
func copyFileFromPod(awsClient *ssh.Client, namespace, pod, sourceFile, destDir string) error {
	session, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %v", err)
	}
	defer session.Close()

	// Extract filename from path
	fileName := filepath.Base(sourceFile)
	destFile := fmt.Sprintf("%s/%s", destDir, fileName)

	// Use kubectl exec cat to copy file content
	copyCmd := fmt.Sprintf("kubectl exec -n %s %s -- cat %s > %s 2>/dev/null",
		namespace, pod, sourceFile, destFile)

	logger.Debug("Copying file with command: %s", copyCmd)
	err = executeCommandAsRoot(session, copyCmd)
	if err != nil {
		logger.Debug("Copy command failed: %v", err)
		return fmt.Errorf("copy command failed: %v", err)
	}

	logger.Debug("Successfully copied %s to %s", sourceFile, destFile)
	return nil
}

// writeFileToRemote writes content to a file on the remote server via SSH using stdin pipe
func writeFileToRemote(awsClient *ssh.Client, filePath, content string) error {
	session, err := awsClient.NewSession()
	if err != nil {
		logger.Error("Failed to create session for writing file %s: %v", filePath, err)
		return err
	}
	defer session.Close()

	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %v", err)
	}

	writeCmd := fmt.Sprintf("sudo su - -c 'cat > %s'", filePath)
	if err := session.Start(writeCmd); err != nil {
		stdinPipe.Close()
		return fmt.Errorf("failed to start write command: %v", err)
	}

	_, err = io.WriteString(stdinPipe, content)
	stdinPipe.Close()
	if err != nil {
		return fmt.Errorf("failed to write content: %v", err)
	}

	if err := session.Wait(); err != nil {
		return fmt.Errorf("failed to write file on remote server: %v", err)
	}

	return nil
}

// sanitizeFilename removes or replaces characters that are not suitable for filenames
func sanitizeFilename(name string) string {
	// Replace spaces and other problematic characters
	filename := strings.ReplaceAll(name, " ", "_")
	filename = strings.ReplaceAll(filename, "/", "_")
	filename = strings.ReplaceAll(filename, "\\", "_")
	filename = strings.ReplaceAll(filename, ":", "_")
	filename = strings.ReplaceAll(filename, "*", "_")
	filename = strings.ReplaceAll(filename, "?", "_")
	filename = strings.ReplaceAll(filename, "\"", "_")
	filename = strings.ReplaceAll(filename, "<", "_")
	filename = strings.ReplaceAll(filename, ">", "_")
	filename = strings.ReplaceAll(filename, "|", "_")

	// Remove any double underscores and trim
	for strings.Contains(filename, "__") {
		filename = strings.ReplaceAll(filename, "__", "_")
	}
	filename = strings.Trim(filename, "_")

	// Ensure filename is not empty
	if filename == "" {
		filename = "unnamed_command"
	}

	return filename
}

// ============================================================================
// Log Analytics Feature - Analyze downloaded log archives for errors/issues
// ============================================================================

// LogMatch represents a single error/pattern match found in a log file
type LogMatch struct {
	FileName       string            // Name of the log file
	LineNumber     int               // 1-based line number of the match
	MatchedLine    string            // The actual line that matched
	Pattern        string            // The pattern that matched
	BeforeLines    []string          // Context lines before the match
	AfterLines     []string          // Context lines after the match
	CorrelationIDs map[string]string // Extracted correlation IDs: type -> ID (e.g., "transaction" -> "TXN-12345")
	Timestamp      time.Time         // Parsed timestamp from the log line
}

// CorrelatedIssue represents a group of related errors found across multiple files
type CorrelatedIssue struct {
	Pattern     string   // Common pattern or keyword
	Files       []string // Files where this pattern was found
	TotalCount  int      // Total occurrences across all files
	Severity    string   // Estimated severity: CRITICAL, HIGH, MEDIUM, LOW
	Description string   // Auto-generated description of the issue
}

// CorrelationIDIssue represents errors grouped by correlation ID (transaction ID, request ID, etc.)
type CorrelationIDIssue struct {
	CorrelationID   string        // The correlation ID value (e.g., "TXN-12345")
	CorrelationType string        // Type of correlation ID (transaction, request, trace, etc.)
	Matches         []LogMatch    // All log matches with this correlation ID
	Files           []string      // Files where this correlation ID appears
	StartTime       time.Time     // Earliest timestamp (root cause candidate)
	EndTime         time.Time     // Latest timestamp
	Duration        time.Duration // Time span from first to last occurrence
	Severity        string        // Estimated severity based on error patterns
	Description     string        // Auto-generated description
}

// FileAnalysisSummary holds analysis results for a single file
type FileAnalysisSummary struct {
	FileName      string
	TotalMatches  int
	Matches       []LogMatch
	PatternCounts map[string]int // count per pattern
}

// categorizeAnalyzedFiles logs statistics about the types of files being analyzed
func categorizeAnalyzedFiles(logFiles []string, extractDir string) {
	var podLogs, podFiles, systemInfo, temporalFiles, filteredLogs, dbQueryFiles, otherFiles []string

	for _, path := range logFiles {
		relPath, _ := filepath.Rel(extractDir, path)
		relPathLower := strings.ToLower(relPath)

		// Categorize based on path patterns
		switch {
		case strings.Contains(relPathLower, "/temporal/"):
			temporalFiles = append(temporalFiles, relPath)
		case strings.Contains(relPathLower, "/database/"):
			dbQueryFiles = append(dbQueryFiles, relPath)
		case strings.Contains(relPathLower, "/general_info/") || strings.Contains(relPathLower, "/systeminfo/"):
			systemInfo = append(systemInfo, relPath)
		case strings.Contains(relPathLower, "/filter/"):
			filteredLogs = append(filteredLogs, relPath)
		case strings.Contains(relPathLower, "/pods/") || strings.Contains(relPathLower, "-server.log") || strings.Contains(relPathLower, "-server_err.log"):
			podFiles = append(podFiles, relPath)
		case strings.HasSuffix(relPathLower, ".log") && (strings.Contains(relPathLower, "/xiq/") || strings.Contains(relPathLower, "/common/") || strings.Contains(relPathLower, "/nvo/") || strings.Contains(relPathLower, "/configuration/")):
			podLogs = append(podLogs, relPath)
		default:
			otherFiles = append(otherFiles, relPath)
		}
	}

	logger.Info("Analyzing %d file(s) across multiple categories:", len(logFiles))
	if len(podLogs) > 0 {
		logger.Info("  → Pod Logs (kubectl logs): %d files", len(podLogs))
	}
	if len(podFiles) > 0 {
		logger.Info("  → Pod Files (kubectl cp): %d files", len(podFiles))
	}
	if len(systemInfo) > 0 {
		logger.Info("  → System Info: %d files", len(systemInfo))
	}
	if len(temporalFiles) > 0 {
		logger.Info("  → Temporal Workflow Data: %d files", len(temporalFiles))
	}
	if len(dbQueryFiles) > 0 {
		logger.Info("  → Database Query Results: %d files", len(dbQueryFiles))
	}
	if len(filteredLogs) > 0 {
		logger.Info("  → Filtered Logs: %d files", len(filteredLogs))
	}
	if len(otherFiles) > 0 {
		logger.Info("  → Other Files: %d files", len(otherFiles))
	}
}

// PodStatusIssue represents a problematic pod status
type PodStatusIssue struct {
	PodName   string
	Namespace string
	Status    string
	Restarts  string
	Age       string
	IssueType string // "CrashLoopBackOff", "Evicted", "NotReady", "Error", "Pending", "ImagePullBackOff", etc.
	Severity  string // "CRITICAL", "HIGH", "MEDIUM"
}

// isAgeLessThanDays checks if a Kubernetes age string (e.g., "12d", "3h", "45m", "1d2h") is less than the specified number of days
func isAgeLessThanDays(age string, days int) bool {
	if age == "" {
		return false
	}

	// Parse age format: can be "12d", "3h", "45m", "1d2h", etc.
	totalHours := 0.0

	// Extract numbers and units
	var currentNum strings.Builder
	for i := 0; i < len(age); i++ {
		ch := age[i]
		if ch >= '0' && ch <= '9' {
			currentNum.WriteByte(ch)
		} else if ch == 'd' || ch == 'h' || ch == 'm' || ch == 's' {
			if currentNum.Len() > 0 {
				num := 0
				fmt.Sscanf(currentNum.String(), "%d", &num)
				currentNum.Reset()

				switch ch {
				case 'd':
					totalHours += float64(num * 24)
				case 'h':
					totalHours += float64(num)
				case 'm':
					totalHours += float64(num) / 60.0
				case 's':
					totalHours += float64(num) / 3600.0
				}
			}
		}
	}

	thresholdHours := float64(days * 24)
	return totalHours < thresholdHours
}

// analyzeTemporalActivities scans collected Temporal files and reports activity outputs and
// workflow results carrying a non-success status, activities that never ran, how many status
// values were actually checked (zero means nothing was validated), and each workflow's execution
// flow.
func analyzeTemporalActivities(logFiles []string, baseDir string, taCfg TemporalAnalysisConfig) TemporalAnalysisResult {
	successStatuses := taCfg.successStatusSet()
	reportMissing := taCfg.shouldReportMissing()
	result := TemporalAnalysisResult{Enabled: true}
	flows := map[string]*TemporalWorkflowFlow{}
	flowFor := func(id string) *TemporalWorkflowFlow {
		if f, ok := flows[id]; ok {
			return f
		}
		f := &TemporalWorkflowFlow{WorkflowID: id}
		flows[id] = f
		return f
	}

	for _, path := range logFiles {
		relPath, err := filepath.Rel(baseDir, path)
		if err != nil {
			relPath = path
		}
		relPath = strings.ReplaceAll(relPath, "\\", "/")
		isActivityFile := strings.Contains(relPath, "_activities/")
		// Workflow overview files live beside the _activities dirs; the banner check below is what
		// actually identifies them, this just avoids reading unrelated pod logs.
		maybeOverview := strings.HasSuffix(strings.ToLower(relPath), ".txt") && strings.Contains(strings.ToLower(relPath), "temporal")
		if !isActivityFile && !maybeOverview {
			continue
		}

		content, err := ioutil.ReadFile(path)
		if err != nil {
			continue
		}

		if !isActivityFile {
			section, isOverview := extractWorkflowOutputSection(string(content))
			if !isOverview {
				continue
			}
			wfID := strings.TrimSuffix(filepath.Base(relPath), ".txt")
			resultIssues, checked := extractWorkflowResultFailures(section, successStatuses)
			result.StatusesChecked += checked
			for i := range resultIssues {
				resultIssues[i].WorkflowID = wfID
				resultIssues[i].Source = relPath
			}
			result.Issues = append(result.Issues, resultIssues...)

			f := flowFor(wfID)
			f.Result, f.ResultOK, f.HasResult = summarizeWorkflowResult(section, successStatuses)
			continue
		}

		// summary.txt carries the execution sequence, including NOT_FOUND for activities that
		// were configured but never appeared in the workflow history.
		if strings.HasSuffix(relPath, "/summary.txt") {
			workflowID, _ := parseActivityOutputPath(relPath)
			steps := parseWorkflowFlowSummary(string(content))
			if taCfg.wantsHTMLReport() {
				loadFlowStepPayloads(filepath.Dir(path), steps, taCfg.htmlPayloadLimitBytes())
			}
			flowFor(workflowID).Steps = steps
			if reportMissing {
				for _, s := range steps {
					if s.NeverRan {
						result.Missing = append(result.Missing, TemporalMissingActivity{WorkflowID: workflowID, Activity: s.Activity})
					}
				}
			}
			continue
		}

		if !strings.HasSuffix(relPath, "_output.txt") {
			continue
		}
		found, checked := extractActivityStatusFailures(string(content), successStatuses)
		result.StatusesChecked += checked

		if len(found) == 0 {
			continue
		}

		workflowID, activity := parseActivityOutputPath(relPath)
		for i := range found {
			found[i].WorkflowID = workflowID
			found[i].Activity = activity
			found[i].Source = relPath
		}
		result.Issues = append(result.Issues, found...)
	}

	// summary.txt's FLAGGED marker is written at collection time, so it is absent in older
	// archives and stale whenever successStatuses changed since collection. Re-apply the flags
	// this run computed so the flow view can never contradict the issue list above it.
	applyIssueFlagsToFlows(flows, result.Issues)

	for _, f := range flows {
		result.Flows = append(result.Flows, *f)
	}
	sort.Slice(result.Flows, func(i, j int) bool { return result.Flows[i].WorkflowID < result.Flows[j].WorkflowID })
	return result
}

// applyIssueFlagsToFlows marks the flow steps that this analysis run flagged, matching on
// workflow + activity + attempt.
func applyIssueFlagsToFlows(flows map[string]*TemporalWorkflowFlow, issues []TemporalActivityIssue) {
	for _, issue := range issues {
		// Workflow-level results have no step of their own; they render on the END line.
		if !strings.Contains(issue.Source, "_activities/") {
			continue
		}
		flow, ok := flows[issue.WorkflowID]
		if !ok {
			continue
		}
		attempt := attemptFromActivityPath(issue.Source)
		for i := range flow.Steps {
			step := &flow.Steps[i]
			if step.NeverRan || step.Activity != issue.Activity {
				continue
			}
			// An unnumbered path means "only one attempt was collected", so flag every match.
			if attempt > 0 && step.Attempt != attempt {
				continue
			}
			step.Flagged = true
			step.OutputStatus = issue.Status
			if msg := strings.TrimSpace(issue.StatusMessage); msg != "" {
				step.StatusMessage = msg
			}
			if issue.DeviceSerial != "" {
				step.DeviceSerial = issue.DeviceSerial
			}
		}
	}
}

// attemptFromActivityPath returns the N in <Activity>_attemptN_output.txt, or 0 when absent.
func attemptFromActivityPath(relPath string) int {
	base := relPath[strings.LastIndex(relPath, "/")+1:]
	base = strings.TrimSuffix(base, "_output.txt")
	idx := strings.LastIndex(base, "_attempt")
	if idx < 0 {
		return 0
	}
	n, err := strconv.Atoi(base[idx+len("_attempt"):])
	if err != nil {
		return 0
	}
	return n
}

// parseActivityOutputPath derives the workflow ID and activity name from a collected activity
// output path of the form <workflowID>_activities/<Activity>[_attemptN]_output.txt.
func parseActivityOutputPath(relPath string) (workflowID, activity string) {
	parts := strings.Split(relPath, "/")
	activity = strings.TrimSuffix(parts[len(parts)-1], "_output.txt")
	if idx := strings.LastIndex(activity, "_attempt"); idx > 0 {
		if _, err := strconv.Atoi(activity[idx+len("_attempt"):]); err == nil {
			activity = activity[:idx]
		}
	}
	if len(parts) >= 2 {
		workflowID = strings.TrimSuffix(parts[len(parts)-2], "_activities")
	}
	return workflowID, activity
}

// analyzePodStatus parses kubectl get pods output and identifies problematic pods
func analyzePodStatus(logFiles []string, extractDir string) []PodStatusIssue {
	var issues []PodStatusIssue

	// Find files that might contain pod status
	for _, path := range logFiles {
		relPath, _ := filepath.Rel(extractDir, path)
		relPathLower := strings.ToLower(relPath)

		// Look for files with names like "kubectl_get_pods*.txt" or similar
		if !strings.Contains(relPathLower, "pod") || (!strings.Contains(relPathLower, "kubectl") && !strings.Contains(relPathLower, "general_info")) {
			continue
		}

		// Read the file
		content, err := ioutil.ReadFile(path)
		if err != nil {
			continue
		}

		// Process lines to find pod statuses
		lines := strings.Split(string(content), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "NAME") || strings.HasPrefix(line, "#") {
				continue // Skip header and comment lines
			}

			// Parse pod status line (format: NAME READY STATUS RESTARTS AGE)
			fields := strings.Fields(line)
			if len(fields) < 5 {
				continue
			}

			podName := fields[0]
			ready := fields[1]
			status := fields[2]
			restarts := fields[3]
			age := fields[4]
			namespace := "unknown"

			// Try to extract namespace from column if -A flag was used
			if len(fields) >= 6 && !strings.Contains(fields[1], "/") {
				namespace = fields[0]
				podName = fields[1]
				ready = fields[2]
				status = fields[3]
				restarts = fields[4]
				age = fields[5]
			}

			// Identify problem pods
			var issueType string
			var severity string

			statusLower := strings.ToLower(status)
			switch {
			case strings.Contains(statusLower, "crashloopbackoff"):
				issueType = "CrashLoopBackOff"
				severity = "CRITICAL"
			case strings.Contains(statusLower, "evicted"):
				issueType = "Evicted"
				severity = "CRITICAL"
			case strings.Contains(statusLower, "error"):
				issueType = "Error"
				severity = "CRITICAL"
			case strings.Contains(statusLower, "imagepullbackoff") || strings.Contains(statusLower, "errimagepull"):
				issueType = "ImagePullBackOff"
				severity = "HIGH"
			case strings.Contains(statusLower, "pending") && age != "" && !strings.Contains(age, "s") && !strings.Contains(age, "m"): // Pending for a long time
				issueType = "Pending (Long Duration)"
				severity = "HIGH"
			case strings.Contains(statusLower, "terminating") && age != "" && !strings.Contains(age, "s") && !strings.Contains(age, "m"):
				issueType = "Terminating (Stuck)"
				severity = "MEDIUM"
			case strings.Contains(statusLower, "oomkilled"):
				issueType = "OOMKilled"
				severity = "CRITICAL"
			case (!strings.Contains(ready, "/") || strings.HasPrefix(ready, "0/")) && !strings.Contains(statusLower, "completed"):
				// Only flag NotReady if: restarts > 0 OR age < 1 day
				restartCount := 0
				fmt.Sscanf(restarts, "%d", &restartCount)
				isYoung := isAgeLessThanDays(age, 1)
				if restartCount > 0 || isYoung {
					issueType = "NotReady"
					severity = "HIGH"
				}
			case status == "Running" && restarts != "0" && restarts != "":
				// Parse restart count
				restartCount := 0
				fmt.Sscanf(restarts, "%d", &restartCount)
				if restartCount >= 5 {
					issueType = "Excessive Restarts"
					severity = "MEDIUM"
				}
			default:
				continue // Not a problem pod
			}

			if issueType != "" {
				issues = append(issues, PodStatusIssue{
					PodName:   podName,
					Namespace: namespace,
					Status:    status,
					Restarts:  restarts,
					Age:       age,
					IssueType: issueType,
					Severity:  severity,
				})
			}
		}
	}

	return issues
}

// analyzeDownloadedLogs extracts a downloaded .tar.gz archive and analyzes all log files
// for error patterns, correlates issues across files, and generates a comprehensive report
func analyzeDownloadedLogs(archivePath, outputDir string, logAnalysisConfig struct {
	Enabled         bool     `yaml:"enabled"`
	OutputFile      string   `yaml:"outputFile"`
	ErrorPatterns   []string `yaml:"errorPatterns"`
	ExcludeKeywords []string `yaml:"excludeKeywords"`
	MaxMatches      int      `yaml:"maxMatches"`
	ContextLines    int      `yaml:"contextLines"`
	CorrelationKeys []struct {
		Pattern string `yaml:"pattern"`
		Type    string `yaml:"type"`
	} `yaml:"correlationKeys"`
	TimestampPatterns []string `yaml:"timestampPatterns"`
	ErrorGroups       []struct {
		Name     string   `yaml:"name"`
		Patterns []string `yaml:"patterns"`
		Severity string   `yaml:"severity"`
	} `yaml:"errorGroups"`
	TemporalAnalysis TemporalAnalysisConfig `yaml:"temporalAnalysis"`
}) error {
	if !logAnalysisConfig.Enabled {
		logger.Debug("Log analysis is disabled")
		return nil
	}

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  LOG ANALYTICS - Analyzing downloaded archive for errors & issues")
	logger.Info("%s", strings.Repeat("=", 70))

	// Set defaults
	if logAnalysisConfig.OutputFile == "" {
		logAnalysisConfig.OutputFile = "log_analytics_report.txt"
	}
	if logAnalysisConfig.MaxMatches <= 0 {
		logAnalysisConfig.MaxMatches = 20
	}
	if logAnalysisConfig.ContextLines < 0 {
		logAnalysisConfig.ContextLines = 2
	}
	if len(logAnalysisConfig.ErrorPatterns) == 0 {
		logAnalysisConfig.ErrorPatterns = []string{"error", "panic", "failure", "failed", "exception", "fatal", "critical", "timeout"}
	}

	// Compile regex patterns (case-insensitive)
	var compiledPatterns []*regexp.Regexp
	for _, pattern := range logAnalysisConfig.ErrorPatterns {
		re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(pattern))
		if err != nil {
			logger.Warn("Invalid error pattern '%s', skipping: %v", pattern, err)
			continue
		}
		compiledPatterns = append(compiledPatterns, re)
	}
	if len(compiledPatterns) == 0 {
		return fmt.Errorf("no valid error patterns compiled")
	}

	// Compile exclude keyword patterns (case-insensitive)
	var compiledExcludes []*regexp.Regexp
	for _, exclude := range logAnalysisConfig.ExcludeKeywords {
		re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(exclude))
		if err != nil {
			logger.Warn("Invalid exclude keyword '%s', skipping: %v", exclude, err)
			continue
		}
		compiledExcludes = append(compiledExcludes, re)
	}

	logger.Info("Analyzing with %d error patterns, %d exclude keywords, %d context lines before/after",
		len(compiledPatterns), len(compiledExcludes), logAnalysisConfig.ContextLines)
	logger.Info("Patterns: %s", strings.Join(logAnalysisConfig.ErrorPatterns, ", "))
	if len(logAnalysisConfig.ExcludeKeywords) > 0 {
		logger.Info("Excludes: %s", strings.Join(logAnalysisConfig.ExcludeKeywords, ", "))
	}

	// Compile correlation ID patterns
	var correlationRegexes []struct {
		regex *regexp.Regexp
		typ   string
	}
	for _, corrKey := range logAnalysisConfig.CorrelationKeys {
		re, err := regexp.Compile(corrKey.Pattern)
		if err != nil {
			logger.Warn("Invalid correlation pattern '%s', skipping: %v", corrKey.Pattern, err)
			continue
		}
		correlationRegexes = append(correlationRegexes, struct {
			regex *regexp.Regexp
			typ   string
		}{regex: re, typ: corrKey.Type})
	}
	if len(correlationRegexes) > 0 {
		logger.Info("Correlation ID tracking enabled: %d patterns", len(correlationRegexes))
	}

	// Compile timestamp patterns
	var timestampRegexes []*regexp.Regexp
	for _, tsPattern := range logAnalysisConfig.TimestampPatterns {
		re, err := regexp.Compile(tsPattern)
		if err != nil {
			logger.Warn("Invalid timestamp pattern '%s', skipping: %v", tsPattern, err)
			continue
		}
		timestampRegexes = append(timestampRegexes, re)
	}
	if len(timestampRegexes) > 0 {
		logger.Info("Timestamp extraction enabled: %d patterns", len(timestampRegexes))
	}

	// Step 1: Extract the archive to a temporary directory
	extractDir, err := extractTarGz(archivePath)
	if err != nil {
		return fmt.Errorf("failed to extract archive for analysis: %v", err)
	}
	defer os.RemoveAll(extractDir)
	logger.Info("Extracted archive to temporary directory for analysis")

	// Step 2: Discover all log/text files in the extracted archive
	var logFiles []string
	err = filepath.Walk(extractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip files we can't access
		}
		if info.IsDir() {
			return nil
		}
		// Analyze text-based files: .log, .txt, .json, .yaml, .yml, .xml, .csv, and files without extension
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".log", ".txt", ".json", ".yaml", ".yml", ".xml", ".csv", ".out", ".err", "":
			logFiles = append(logFiles, path)
		default:
			// Check if it might be a text file by reading a small portion
			if isLikelyTextFile(path) {
				logFiles = append(logFiles, path)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk extracted files: %v", err)
	}

	if len(logFiles) == 0 {
		logger.Warn("No log/text files found in the archive to analyze")
		return nil
	}

	// Log the different file categories being analyzed
	categorizeAnalyzedFiles(logFiles, extractDir)

	// Step 3: Analyze each file for error patterns
	var allSummaries []FileAnalysisSummary
	globalPatternCounts := make(map[string]int)       // pattern -> total count across all files
	patternFileMap := make(map[string]map[string]int) // pattern -> file -> count
	totalMatchesFound := 0

	for _, filePath := range logFiles {
		relPath, _ := filepath.Rel(extractDir, filePath)
		summary := analyzeFileForPatterns(filePath, relPath, compiledPatterns, compiledExcludes,
			logAnalysisConfig.ErrorPatterns, logAnalysisConfig.MaxMatches, logAnalysisConfig.ContextLines,
			correlationRegexes, timestampRegexes)

		if summary.TotalMatches > 0 {
			allSummaries = append(allSummaries, summary)
			totalMatchesFound += summary.TotalMatches

			// Aggregate pattern counts
			for pattern, count := range summary.PatternCounts {
				globalPatternCounts[pattern] += count
				if patternFileMap[pattern] == nil {
					patternFileMap[pattern] = make(map[string]int)
				}
				patternFileMap[pattern][summary.FileName] = count
			}
		}
	}

	logger.Info("Analysis complete: %d total matches across %d file(s)", totalMatchesFound, len(allSummaries))

	// Step 3b: Analyze pod status issues
	logger.Info("Analyzing Kubernetes pod status for issues...")
	podStatusIssues := analyzePodStatus(logFiles, extractDir)
	if len(podStatusIssues) > 0 {
		logger.Warn("Found %d problematic pod(s) with status issues", len(podStatusIssues))
	} else {
		logger.Info("No pod status issues detected")
	}

	// Step 3c: Validate Temporal workflow activity output statuses
	temporal := TemporalAnalysisResult{Enabled: logAnalysisConfig.TemporalAnalysis.isEnabled()}
	if temporal.Enabled {
		temporal = analyzeTemporalActivities(logFiles, extractDir, logAnalysisConfig.TemporalAnalysis)
		if len(temporal.Issues) > 0 {
			logger.Warn("Found %d non-success Temporal status value(s)", len(temporal.Issues))
		}
		if len(temporal.Missing) > 0 {
			order, _ := groupMissingByActivity(temporal.Missing)
			logger.Warn("Expected Temporal activity/activities never ran: %s", strings.Join(order, ", "))
		}
	} else {
		logger.Debug("Temporal activity status validation is disabled")
	}

	// Step 4: Correlate errors across files
	correlatedIssues := correlateErrors(allSummaries, globalPatternCounts, patternFileMap)

	// Step 4b: Correlate errors by correlation IDs (transaction IDs, request IDs, etc.)
	var correlationIDIssues []CorrelationIDIssue
	if len(correlationRegexes) > 0 {
		correlationIDIssues = correlateByCorrelationIDs(allSummaries)
		if len(correlationIDIssues) > 0 {
			logger.Info("Found %d correlation ID groups across files", len(correlationIDIssues))
		}
	}

	// Store patternFileMap for report generator
	globalPatternFileMap = patternFileMap

	// Step 5: Generate the report
	// Derive report filename with timestamp from the archive name
	// e.g., app_log_20250710_120000.tar.gz -> log_analytics_report_20250710_120000.txt
	reportFileName := logAnalysisConfig.OutputFile
	archiveBase := filepath.Base(archivePath)
	archiveBase = strings.TrimSuffix(archiveBase, ".tar.gz")
	archiveBase = strings.TrimSuffix(archiveBase, ".gz")
	// Extract timestamp portion: look for _YYYYMMDD_HHMMSS pattern
	tsRegex := regexp.MustCompile(`(\d{8}_\d{6})`)
	if matches := tsRegex.FindString(archiveBase); matches != "" {
		// Insert timestamp before .txt extension
		ext := filepath.Ext(reportFileName)
		base := strings.TrimSuffix(reportFileName, ext)
		reportFileName = fmt.Sprintf("%s_%s%s", base, matches, ext)
	}
	reportPath := filepath.Join(outputDir, reportFileName)
	err = generateAnalyticsReport(reportPath, allSummaries, correlatedIssues,
		globalPatternCounts, logAnalysisConfig, totalMatchesFound, archivePath, correlationIDIssues, podStatusIssues, temporal)
	if err != nil {
		return fmt.Errorf("failed to generate analytics report: %v", err)
	}

	logger.Info("Log analytics report generated: %s", reportPath)
	writeTemporalFlowHTMLReport(reportPath, archivePath, temporal, logAnalysisConfig.TemporalAnalysis)

	// Print a summary to the console
	printAnalyticsSummary(allSummaries, correlatedIssues, totalMatchesFound, temporal)

	return nil
}

// analyzeLocalDirectory performs standalone log analysis on a local directory (or single file).
// It recursively scans for log/text files and runs the same analysis pipeline used during
// log downloads, without requiring any SSH connections or remote operations.
// Usage: logcollector --analyze /path/to/logs [--outdir /path/to/report]
func analyzeLocalDirectory(targetPath string, reportOutputDir string, logAnalysisConfig struct {
	Enabled         bool     `yaml:"enabled"`
	OutputFile      string   `yaml:"outputFile"`
	ErrorPatterns   []string `yaml:"errorPatterns"`
	ExcludeKeywords []string `yaml:"excludeKeywords"`
	MaxMatches      int      `yaml:"maxMatches"`
	ContextLines    int      `yaml:"contextLines"`
	CorrelationKeys []struct {
		Pattern string `yaml:"pattern"`
		Type    string `yaml:"type"`
	} `yaml:"correlationKeys"`
	TimestampPatterns []string `yaml:"timestampPatterns"`
	ErrorGroups       []struct {
		Name     string   `yaml:"name"`
		Patterns []string `yaml:"patterns"`
		Severity string   `yaml:"severity"`
	} `yaml:"errorGroups"`
	TemporalAnalysis TemporalAnalysisConfig `yaml:"temporalAnalysis"`
}) error {
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  LOG ANALYZER — Standalone Local Log Analysis")
	logger.Info("%s", strings.Repeat("=", 70))

	// Validate target path exists
	info, err := os.Stat(targetPath)
	if err != nil {
		return fmt.Errorf("cannot access path '%s': %v", targetPath, err)
	}

	// Set defaults
	if logAnalysisConfig.OutputFile == "" {
		logAnalysisConfig.OutputFile = "log_analysis_summary.txt"
	}
	if logAnalysisConfig.MaxMatches <= 0 {
		logAnalysisConfig.MaxMatches = 99
	}
	if logAnalysisConfig.ContextLines < 0 {
		logAnalysisConfig.ContextLines = 2
	}
	if len(logAnalysisConfig.ErrorPatterns) == 0 {
		logAnalysisConfig.ErrorPatterns = []string{"error", "panic", "failure", "failed", "exception", "fatal", "critical", "timeout", "connection refused", "unable to", "cannot", "permission denied"}
	}

	// Compile regex patterns (case-insensitive)
	var compiledPatterns []*regexp.Regexp
	for _, pattern := range logAnalysisConfig.ErrorPatterns {
		re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(pattern))
		if err != nil {
			logger.Warn("Invalid error pattern '%s', skipping: %v", pattern, err)
			continue
		}
		compiledPatterns = append(compiledPatterns, re)
	}
	if len(compiledPatterns) == 0 {
		return fmt.Errorf("no valid error patterns compiled")
	}

	// Compile exclude keyword patterns (case-insensitive)
	var compiledExcludes []*regexp.Regexp
	for _, exclude := range logAnalysisConfig.ExcludeKeywords {
		re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(exclude))
		if err != nil {
			logger.Warn("Invalid exclude keyword '%s', skipping: %v", exclude, err)
			continue
		}
		compiledExcludes = append(compiledExcludes, re)
	}

	// Compile correlation ID patterns
	var correlationRegexes []struct {
		regex *regexp.Regexp
		typ   string
	}
	for _, corrKey := range logAnalysisConfig.CorrelationKeys {
		re, err := regexp.Compile(corrKey.Pattern)
		if err != nil {
			logger.Warn("Invalid correlation pattern '%s', skipping: %v", corrKey.Pattern, err)
			continue
		}
		correlationRegexes = append(correlationRegexes, struct {
			regex *regexp.Regexp
			typ   string
		}{regex: re, typ: corrKey.Type})
	}

	// Compile timestamp patterns
	var timestampRegexes []*regexp.Regexp
	for _, tsPattern := range logAnalysisConfig.TimestampPatterns {
		re, err := regexp.Compile(tsPattern)
		if err != nil {
			logger.Warn("Invalid timestamp pattern '%s', skipping: %v", tsPattern, err)
			continue
		}
		timestampRegexes = append(timestampRegexes, re)
	}

	logger.Info("Target path: %s", targetPath)
	logger.Info("Analyzing with %d error patterns, %d exclude keywords, %d context lines",
		len(compiledPatterns), len(compiledExcludes), logAnalysisConfig.ContextLines)
	logger.Info("Patterns: %s", strings.Join(logAnalysisConfig.ErrorPatterns, ", "))
	if len(logAnalysisConfig.ExcludeKeywords) > 0 {
		logger.Info("Excludes: %s", strings.Join(logAnalysisConfig.ExcludeKeywords, ", "))
	}
	if len(correlationRegexes) > 0 {
		logger.Info("Correlation ID tracking enabled: %d patterns", len(correlationRegexes))
	}

	// Discover log/text files
	var logFiles []string
	baseDir := targetPath // used for relative path display

	if !info.IsDir() {
		// Single file mode
		baseDir = filepath.Dir(targetPath)
		lowerPath := strings.ToLower(targetPath)
		if strings.HasSuffix(lowerPath, ".tar.gz") || strings.HasSuffix(lowerPath, ".tgz") {
			// Extract tar.gz archive and analyze its contents
			logger.Info("Archive mode: extracting %s for analysis...", filepath.Base(targetPath))
			extractDir, extractErr := extractTarGz(targetPath)
			if extractErr != nil {
				return fmt.Errorf("failed to extract archive '%s': %v", filepath.Base(targetPath), extractErr)
			}
			baseDir = extractDir
			// Walk the extracted directory for analyzable files
			filepath.Walk(extractDir, func(ePath string, eInfo os.FileInfo, eErr error) error {
				if eErr != nil || eInfo.IsDir() {
					return nil
				}
				ext := strings.ToLower(filepath.Ext(ePath))
				switch ext {
				case ".log", ".txt", ".json", ".yaml", ".yml", ".xml", ".csv", ".out", ".err", "":
					logFiles = append(logFiles, ePath)
				default:
					if isLikelyTextFile(ePath) {
						logFiles = append(logFiles, ePath)
					}
				}
				return nil
			})
		} else {
			logFiles = append(logFiles, targetPath)
			logger.Info("Single file mode: %s", filepath.Base(targetPath))
		}
	} else {
		// Recursive directory scan
		err = filepath.Walk(targetPath, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil // Skip inaccessible files
			}
			if fi.IsDir() {
				return nil
			}

			// Check for .tar.gz archives — extract and add their contents
			if strings.HasSuffix(strings.ToLower(path), ".tar.gz") || strings.HasSuffix(strings.ToLower(path), ".tgz") {
				logger.Info("Found archive: %s — extracting for analysis...", filepath.Base(path))
				extractDir, extractErr := extractTarGz(path)
				if extractErr != nil {
					logger.Warn("Failed to extract %s: %v (skipping)", filepath.Base(path), extractErr)
					return nil
				}
				// Walk the extracted directory
				filepath.Walk(extractDir, func(ePath string, eInfo os.FileInfo, eErr error) error {
					if eErr != nil || eInfo.IsDir() {
						return nil
					}
					ext := strings.ToLower(filepath.Ext(ePath))
					switch ext {
					case ".log", ".txt", ".json", ".yaml", ".yml", ".xml", ".csv", ".out", ".err", "":
						logFiles = append(logFiles, ePath)
					default:
						if isLikelyTextFile(ePath) {
							logFiles = append(logFiles, ePath)
						}
					}
					return nil
				})
				return nil
			}

			// Regular text/log files
			ext := strings.ToLower(filepath.Ext(path))
			switch ext {
			case ".log", ".txt", ".json", ".yaml", ".yml", ".xml", ".csv", ".out", ".err", "":
				logFiles = append(logFiles, path)
			default:
				if isLikelyTextFile(path) {
					logFiles = append(logFiles, path)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to walk directory: %v", err)
		}
	}

	if len(logFiles) == 0 {
		logger.Warn("No log/text files found to analyze in: %s", targetPath)
		return nil
	}

	logger.Info("Found %d file(s) to analyze", len(logFiles))

	// Categorize files for informational logging
	categorizeAnalyzedFiles(logFiles, baseDir)

	// Analyze each file
	var allSummaries []FileAnalysisSummary
	globalPatternCounts := make(map[string]int)
	patternFileMap := make(map[string]map[string]int)
	totalMatchesFound := 0

	for _, filePath := range logFiles {
		relPath, err := filepath.Rel(baseDir, filePath)
		if err != nil {
			relPath = filePath // fallback to absolute path
		}
		summary := analyzeFileForPatterns(filePath, relPath, compiledPatterns, compiledExcludes,
			logAnalysisConfig.ErrorPatterns, logAnalysisConfig.MaxMatches, logAnalysisConfig.ContextLines,
			correlationRegexes, timestampRegexes)

		if summary.TotalMatches > 0 {
			allSummaries = append(allSummaries, summary)
			totalMatchesFound += summary.TotalMatches

			for pattern, count := range summary.PatternCounts {
				globalPatternCounts[pattern] += count
				if patternFileMap[pattern] == nil {
					patternFileMap[pattern] = make(map[string]int)
				}
				patternFileMap[pattern][summary.FileName] = count
			}
		}
	}

	logger.Info("Analysis complete: %d total matches across %d file(s)", totalMatchesFound, len(allSummaries))

	// Pod status analysis
	podStatusIssues := analyzePodStatus(logFiles, baseDir)
	if len(podStatusIssues) > 0 {
		logger.Warn("Found %d problematic pod(s) with status issues", len(podStatusIssues))
	}

	// Temporal workflow activity output status validation
	temporal := TemporalAnalysisResult{Enabled: logAnalysisConfig.TemporalAnalysis.isEnabled()}
	if temporal.Enabled {
		temporal = analyzeTemporalActivities(logFiles, baseDir, logAnalysisConfig.TemporalAnalysis)
		if len(temporal.Issues) > 0 {
			logger.Warn("Found %d non-success Temporal status value(s)", len(temporal.Issues))
		}
		if len(temporal.Missing) > 0 {
			order, _ := groupMissingByActivity(temporal.Missing)
			logger.Warn("Expected Temporal activity/activities never ran: %s", strings.Join(order, ", "))
		}
	} else {
		logger.Debug("Temporal activity status validation is disabled")
	}

	// Correlate errors across files
	correlatedIssues := correlateErrors(allSummaries, globalPatternCounts, patternFileMap)

	// Correlate by correlation IDs
	var correlationIDIssues []CorrelationIDIssue
	if len(correlationRegexes) > 0 {
		correlationIDIssues = correlateByCorrelationIDs(allSummaries)
		if len(correlationIDIssues) > 0 {
			logger.Info("Found %d correlation ID groups across files", len(correlationIDIssues))
		}
	}

	// Store for report generator
	globalPatternFileMap = patternFileMap

	// Generate report
	reportFileName := logAnalysisConfig.OutputFile
	timestamp := time.Now().Format("20060102_150405")
	ext := filepath.Ext(reportFileName)
	base := strings.TrimSuffix(reportFileName, ext)
	reportFileName = fmt.Sprintf("%s_%s%s", base, timestamp, ext)

	reportPath := filepath.Join(reportOutputDir, reportFileName)
	err = generateAnalyticsReport(reportPath, allSummaries, correlatedIssues,
		globalPatternCounts, logAnalysisConfig, totalMatchesFound, targetPath, correlationIDIssues, podStatusIssues, temporal)
	if err != nil {
		return fmt.Errorf("failed to generate analytics report: %v", err)
	}

	logger.Info("")
	logger.Info("Log analytics report generated: %s", reportPath)
	writeTemporalFlowHTMLReport(reportPath, targetPath, temporal, logAnalysisConfig.TemporalAnalysis)

	// Print console summary
	printAnalyticsSummary(allSummaries, correlatedIssues, totalMatchesFound, temporal)

	return nil
}

// extractTarGz extracts a .tar.gz archive to a temporary directory and returns the path
func extractTarGz(archivePath string) (string, error) {
	// Create temp directory for extraction
	extractDir, err := ioutil.TempDir("", "log_analysis_")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %v", err)
	}

	// Open the archive
	file, err := os.Open(archivePath)
	if err != nil {
		os.RemoveAll(extractDir)
		return "", fmt.Errorf("failed to open archive: %v", err)
	}
	defer file.Close()

	// Create gzip reader
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		os.RemoveAll(extractDir)
		return "", fmt.Errorf("failed to create gzip reader: %v", err)
	}
	defer gzReader.Close()

	// Create tar reader
	tarReader := tar.NewReader(gzReader)

	// Extract all files
	fileCount := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.RemoveAll(extractDir)
			return "", fmt.Errorf("error reading tar: %v", err)
		}

		// Sanitize the path to prevent directory traversal attacks
		targetPath := filepath.Join(extractDir, header.Name)
		if !strings.HasPrefix(filepath.Clean(targetPath), filepath.Clean(extractDir)) {
			logger.Warn("Skipping suspicious path in archive: %s", header.Name)
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				logger.Warn("Failed to create directory %s: %v", targetPath, err)
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			parentDir := filepath.Dir(targetPath)
			if err := os.MkdirAll(parentDir, 0755); err != nil {
				logger.Warn("Failed to create parent dir %s: %v", parentDir, err)
				continue
			}

			outFile, err := os.Create(targetPath)
			if err != nil {
				logger.Warn("Failed to create file %s: %v", targetPath, err)
				continue
			}

			// Limit extraction size per file (100 MB)
			limited := io.LimitReader(tarReader, 100*1024*1024)
			if _, err := io.Copy(outFile, limited); err != nil {
				outFile.Close()
				logger.Warn("Failed to extract %s: %v", targetPath, err)
				continue
			}
			outFile.Close()
			fileCount++
		}
	}

	logger.Debug("Extracted %d files from archive", fileCount)
	return extractDir, nil
}

// isLikelyTextFile checks if a file is likely a text file by reading its first 512 bytes
func isLikelyTextFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false
	}
	if n == 0 {
		return false
	}
	buf = buf[:n]

	// Check for binary content (null bytes or too many non-printable chars)
	nonPrintable := 0
	for _, b := range buf {
		if b == 0 {
			return false // Null byte = binary
		}
		if b < 32 && b != '\n' && b != '\r' && b != '\t' {
			nonPrintable++
		}
	}
	return float64(nonPrintable)/float64(n) < 0.1
}

// analyzeFileForPatterns scans a file for error patterns and returns matches with context
// Lines matching any exclude pattern are skipped (false positive filtering)
func analyzeFileForPatterns(filePath, displayName string, compiledPatterns []*regexp.Regexp,
	compiledExcludes []*regexp.Regexp, patternNames []string, maxMatches, contextLines int,
	correlationRegexes []struct {
		regex *regexp.Regexp
		typ   string
	}, timestampRegexes []*regexp.Regexp) FileAnalysisSummary {

	summary := FileAnalysisSummary{
		FileName:      displayName,
		PatternCounts: make(map[string]int),
	}

	file, err := os.Open(filePath)
	if err != nil {
		logger.Debug("Cannot open file for analysis: %s: %v", displayName, err)
		return summary
	}
	defer file.Close()

	// Read all lines into memory for context extraction
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if len(lines) == 0 {
		return summary
	}

	// Scan for pattern matches
	matchCount := 0
	for lineIdx, line := range lines {
		if matchCount >= maxMatches {
			break
		}

		// Check if this line matches any exclude keyword — skip if so
		excluded := false
		for _, exRe := range compiledExcludes {
			if exRe.MatchString(line) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		for patIdx, re := range compiledPatterns {
			if re.MatchString(line) {
				patternName := patternNames[patIdx]

				// Extract context lines
				var beforeLines []string
				var afterLines []string

				startBefore := lineIdx - contextLines
				if startBefore < 0 {
					startBefore = 0
				}
				for i := startBefore; i < lineIdx; i++ {
					beforeLines = append(beforeLines, lines[i])
				}

				endAfter := lineIdx + contextLines + 1
				if endAfter > len(lines) {
					endAfter = len(lines)
				}
				for i := lineIdx + 1; i < endAfter; i++ {
					afterLines = append(afterLines, lines[i])
				}

				// Extract correlation IDs from the line
				correlationIDs := make(map[string]string)
				for _, corrRegex := range correlationRegexes {
					if matches := corrRegex.regex.FindStringSubmatch(line); len(matches) > 0 {
						// Use first submatch if available, otherwise full match
						if len(matches) > 1 {
							correlationIDs[corrRegex.typ] = matches[1]
						} else {
							correlationIDs[corrRegex.typ] = matches[0]
						}
					}
				}

				// Extract timestamp from the line
				var timestamp time.Time
				for _, tsRegex := range timestampRegexes {
					if tsMatch := tsRegex.FindString(line); tsMatch != "" {
						parsedTime := parseTimestamp(tsMatch)
						if !parsedTime.IsZero() {
							timestamp = parsedTime
							break
						}
					}
				}

				match := LogMatch{
					FileName:       displayName,
					LineNumber:     lineIdx + 1, // 1-based
					MatchedLine:    line,
					Pattern:        patternName,
					BeforeLines:    beforeLines,
					AfterLines:     afterLines,
					CorrelationIDs: correlationIDs,
					Timestamp:      timestamp,
				}

				summary.Matches = append(summary.Matches, match)
				summary.PatternCounts[patternName]++
				matchCount++

				break // Only count each line once (first matching pattern wins)
			}
		}
	}

	summary.TotalMatches = len(summary.Matches)
	return summary
}

// correlateErrors finds patterns that appear across multiple files and estimates severity
func correlateErrors(summaries []FileAnalysisSummary, globalPatternCounts map[string]int,
	patternFileMap map[string]map[string]int) []CorrelatedIssue {

	var issues []CorrelatedIssue

	// Sort patterns by total count (most frequent first)
	type patternCount struct {
		pattern string
		count   int
	}
	var sortedPatterns []patternCount
	for pattern, count := range globalPatternCounts {
		sortedPatterns = append(sortedPatterns, patternCount{pattern, count})
	}
	sort.Slice(sortedPatterns, func(i, j int) bool {
		return sortedPatterns[i].count > sortedPatterns[j].count
	})

	for _, pc := range sortedPatterns {
		pattern := pc.pattern
		fileMap := patternFileMap[pattern]

		// Collect files where this pattern appears
		var files []string
		for f := range fileMap {
			files = append(files, f)
		}
		sort.Strings(files)

		// Determine severity based on pattern and frequency
		severity := determineSeverity(pattern, pc.count, len(files))

		// Generate description
		description := generateIssueDescription(pattern, pc.count, files)

		issue := CorrelatedIssue{
			Pattern:     pattern,
			Files:       files,
			TotalCount:  pc.count,
			Severity:    severity,
			Description: description,
		}
		issues = append(issues, issue)
	}

	return issues
}

// correlateByCorrelationIDs groups errors by correlation IDs (transaction IDs, request IDs, etc.)
// This identifies errors that are related through the same transaction/request across multiple services/files
func correlateByCorrelationIDs(summaries []FileAnalysisSummary) []CorrelationIDIssue {
	// Map: correlationID -> list of matches with that ID
	correlationMap := make(map[string][]LogMatch)

	// Collect all matches with correlation IDs
	for _, summary := range summaries {
		for _, match := range summary.Matches {
			if len(match.CorrelationIDs) > 0 {
				// A match might have multiple correlation IDs (e.g., both TXN-123 and REQ-ABC)
				for typ, id := range match.CorrelationIDs {
					// Use type:id as the key to avoid collisions
					key := fmt.Sprintf("%s:%s", typ, id)
					correlationMap[key] = append(correlationMap[key], match)
				}
			}
		}
	}

	var issues []CorrelationIDIssue

	for key, matches := range correlationMap {
		// Skip if only one occurrence (not very useful for correlation)
		if len(matches) < 2 {
			continue
		}

		// Sort by timestamp to establish timeline
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].Timestamp.IsZero() || matches[j].Timestamp.IsZero() {
				return false // Keep original order if no timestamp
			}
			return matches[i].Timestamp.Before(matches[j].Timestamp)
		})

		// Extract correlation type and ID from key
		parts := strings.SplitN(key, ":", 2)
		corrType := parts[0]
		corrID := parts[1]

		// Collect unique files
		fileSet := make(map[string]bool)
		var startTime, endTime time.Time
		for _, match := range matches {
			fileSet[match.FileName] = true
			if !match.Timestamp.IsZero() {
				if startTime.IsZero() || match.Timestamp.Before(startTime) {
					startTime = match.Timestamp
				}
				if endTime.IsZero() || match.Timestamp.After(endTime) {
					endTime = match.Timestamp
				}
			}
		}

		var files []string
		for file := range fileSet {
			files = append(files, file)
		}
		sort.Strings(files)

		// Determine severity based on error patterns in matches
		severity := "MEDIUM"
		for _, match := range matches {
			patternLower := strings.ToLower(match.Pattern)
			switch patternLower {
			case "panic", "fatal", "critical":
				severity = "CRITICAL"
			case "exception":
				if severity != "CRITICAL" {
					severity = "HIGH"
				}
			}
		}

		// Generate description
		duration := time.Duration(0)
		if !startTime.IsZero() && !endTime.IsZero() {
			duration = endTime.Sub(startTime)
		}

		description := fmt.Sprintf("Correlation ID '%s' (%s) appears in %d error(s) across %d file(s)",
			corrID, corrType, len(matches), len(files))
		if duration > 0 {
			description += fmt.Sprintf(" over %v", duration)
		}
		description += ". This indicates a transaction/request that encountered errors across multiple services."

		issue := CorrelationIDIssue{
			CorrelationID:   corrID,
			CorrelationType: corrType,
			Matches:         matches,
			Files:           files,
			StartTime:       startTime,
			EndTime:         endTime,
			Duration:        duration,
			Severity:        severity,
			Description:     description,
		}
		issues = append(issues, issue)
	}

	// Sort by severity and then by number of occurrences
	sort.Slice(issues, func(i, j int) bool {
		sevOrder := map[string]int{"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2, "LOW": 3}
		if sevOrder[issues[i].Severity] != sevOrder[issues[j].Severity] {
			return sevOrder[issues[i].Severity] < sevOrder[issues[j].Severity]
		}
		return len(issues[i].Matches) > len(issues[j].Matches)
	})

	return issues
}

// determineSeverity estimates the severity of an issue based on pattern and frequency
func determineSeverity(pattern string, totalCount, fileCount int) string {
	patternLower := strings.ToLower(pattern)

	// Critical patterns
	switch patternLower {
	case "panic", "fatal", "critical":
		return "CRITICAL"
	}

	// High severity patterns
	switch patternLower {
	case "exception", "permission denied":
		return "HIGH"
	}

	// If a pattern appears across many files, it's likely more severe
	if fileCount >= 3 && totalCount >= 10 {
		if patternLower == "error" || patternLower == "failure" || patternLower == "failed" {
			return "HIGH"
		}
	}

	// Medium severity
	switch patternLower {
	case "error", "failure", "failed", "unable to", "cannot":
		return "MEDIUM"
	case "timeout", "connection refused":
		return "MEDIUM"
	}

	return "LOW"
}

// generateIssueDescription creates a human-readable description for a correlated issue
func generateIssueDescription(pattern string, totalCount int, files []string) string {
	fileStr := strings.Join(files, ", ")
	if len(files) > 3 {
		fileStr = strings.Join(files[:3], ", ") + fmt.Sprintf(" and %d more", len(files)-3)
	}

	if len(files) > 1 {
		return fmt.Sprintf("Pattern '%s' found %d times across %d files (%s). "+
			"This cross-file occurrence suggests a systemic issue that may have cascading effects.",
			pattern, totalCount, len(files), fileStr)
	}
	return fmt.Sprintf("Pattern '%s' found %d times in %s.",
		pattern, totalCount, fileStr)
}

// generateAnalyticsReport writes the comprehensive log analytics report to a file
func generateAnalyticsReport(reportPath string, summaries []FileAnalysisSummary,
	correlatedIssues []CorrelatedIssue, globalPatternCounts map[string]int,
	config struct {
		Enabled         bool     `yaml:"enabled"`
		OutputFile      string   `yaml:"outputFile"`
		ErrorPatterns   []string `yaml:"errorPatterns"`
		ExcludeKeywords []string `yaml:"excludeKeywords"`
		MaxMatches      int      `yaml:"maxMatches"`
		ContextLines    int      `yaml:"contextLines"`
		CorrelationKeys []struct {
			Pattern string `yaml:"pattern"`
			Type    string `yaml:"type"`
		} `yaml:"correlationKeys"`
		TimestampPatterns []string `yaml:"timestampPatterns"`
		ErrorGroups       []struct {
			Name     string   `yaml:"name"`
			Patterns []string `yaml:"patterns"`
			Severity string   `yaml:"severity"`
		} `yaml:"errorGroups"`
		TemporalAnalysis TemporalAnalysisConfig `yaml:"temporalAnalysis"`
	}, totalMatches int, archivePath string, correlationIDIssues []CorrelationIDIssue, podStatusIssues []PodStatusIssue, temporal TemporalAnalysisResult) error {

	file, err := os.Create(reportPath)
	if err != nil {
		return fmt.Errorf("failed to create report file: %v", err)
	}
	defer file.Close()

	w := bufio.NewWriter(file)
	defer w.Flush()

	// Header
	fmt.Fprintln(w, strings.Repeat("=", 80))
	fmt.Fprintln(w, "  LOG ANALYTICS REPORT")
	fmt.Fprintln(w, strings.Repeat("=", 80))
	fmt.Fprintf(w, "  Generated:     %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "  Archive:       %s\n", filepath.Base(archivePath))
	fmt.Fprintf(w, "  Patterns:      %s\n", strings.Join(config.ErrorPatterns, ", "))
	if len(config.ExcludeKeywords) > 0 {
		fmt.Fprintf(w, "  Excludes:      %s\n", strings.Join(config.ExcludeKeywords, ", "))
	}
	fmt.Fprintf(w, "  Context Lines: %d before / %d after\n", config.ContextLines, config.ContextLines)
	fmt.Fprintf(w, "  Max Matches:   %d per file\n", config.MaxMatches)
	fmt.Fprintf(w, "  Files Analyzed: %d with matches out of total scanned\n", len(summaries))
	fmt.Fprintf(w, "  Total Matches: %d\n", totalMatches)
	if len(podStatusIssues) > 0 {
		fmt.Fprintf(w, "  Pod Status Issues: %d problematic pods detected\n", len(podStatusIssues))
	}
	if len(temporal.Issues) > 0 {
		fmt.Fprintf(w, "  Temporal Status Issues: %d non-success status value(s)\n", len(temporal.Issues))
	}
	if len(temporal.Missing) > 0 {
		missingOrder, _ := groupMissingByActivity(temporal.Missing)
		fmt.Fprintf(w, "  Temporal Activities That Never Ran: %s\n", strings.Join(missingOrder, ", "))
	}
	fmt.Fprintln(w, strings.Repeat("=", 80))
	fmt.Fprintln(w)

	// ---- SECTION 0: POD STATUS ISSUES (if any) ----
	if len(podStatusIssues) > 0 {
		fmt.Fprintln(w, strings.Repeat("*", 80))
		fmt.Fprintln(w, "  SECTION 0: KUBERNETES POD STATUS ISSUES")
		fmt.Fprintln(w, strings.Repeat("*", 80))
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  The following pods have problematic statuses that require attention:")
		fmt.Fprintln(w)

		// Group by severity
		criticalPods := []PodStatusIssue{}
		highPods := []PodStatusIssue{}
		mediumPods := []PodStatusIssue{}

		for _, issue := range podStatusIssues {
			switch issue.Severity {
			case "CRITICAL":
				criticalPods = append(criticalPods, issue)
			case "HIGH":
				highPods = append(highPods, issue)
			case "MEDIUM":
				mediumPods = append(mediumPods, issue)
			}
		}

		if len(criticalPods) > 0 {
			fmt.Fprintln(w, "  CRITICAL ISSUES:", len(criticalPods), "pod(s)")
			fmt.Fprintln(w, "  "+strings.Repeat("-", 76))
			for _, issue := range criticalPods {
				fmt.Fprintf(w, "    • Pod: %s (Namespace: %s)\n", issue.PodName, issue.Namespace)
				fmt.Fprintf(w, "      Issue: %s | Status: %s | Restarts: %s | Age: %s\n",
					issue.IssueType, issue.Status, issue.Restarts, issue.Age)
			}
			fmt.Fprintln(w)
		}

		if len(highPods) > 0 {
			fmt.Fprintln(w, "  HIGH PRIORITY ISSUES:", len(highPods), "pod(s)")
			fmt.Fprintln(w, "  "+strings.Repeat("-", 76))
			for _, issue := range highPods {
				fmt.Fprintf(w, "    • Pod: %s (Namespace: %s)\n", issue.PodName, issue.Namespace)
				fmt.Fprintf(w, "      Issue: %s | Status: %s | Restarts: %s | Age: %s\n",
					issue.IssueType, issue.Status, issue.Restarts, issue.Age)
			}
			fmt.Fprintln(w)
		}

		if len(mediumPods) > 0 {
			fmt.Fprintln(w, "  MEDIUM PRIORITY ISSUES:", len(mediumPods), "pod(s)")
			fmt.Fprintln(w, "  "+strings.Repeat("-", 76))
			for _, issue := range mediumPods {
				fmt.Fprintf(w, "    • Pod: %s (Namespace: %s)\n", issue.PodName, issue.Namespace)
				fmt.Fprintf(w, "      Issue: %s | Status: %s | Restarts: %s | Age: %s\n",
					issue.IssueType, issue.Status, issue.Restarts, issue.Age)
			}
			fmt.Fprintln(w)
		}

		fmt.Fprintln(w, "  "+strings.Repeat("=", 76))
		fmt.Fprintln(w)
	}

	// ---- SECTION 0A: TEMPORAL ANALYSIS ----
	if temporal.Enabled {
		fmt.Fprintln(w, strings.Repeat("*", 80))
		fmt.Fprintln(w, "  SECTION 0A: TEMPORAL ANALYSIS (Activity Output + Workflow Result Status)")
		fmt.Fprintln(w, strings.Repeat("*", 80))
		fmt.Fprintln(w)

		if len(temporal.Issues) == 0 {
			if temporal.StatusesChecked == 0 {
				fmt.Fprintln(w, "  NOT VALIDATED: no collected activity output contained a status field.")
				fmt.Fprintln(w, "  This is not an all-clear.")
			} else {
				fmt.Fprintf(w, "  All %d checked status value(s) were successful.\n", temporal.StatusesChecked)
			}
			fmt.Fprintln(w)
		} else {
			fmt.Fprintf(w, "  %d of %d checked status value(s) were non-success:\n", len(temporal.Issues), temporal.StatusesChecked)
			fmt.Fprintln(w)

			// Group flagged activities by workflow so a failing deployment reads as one block
			workflowOrder := []string{}
			byWorkflow := map[string][]TemporalActivityIssue{}
			for _, issue := range temporal.Issues {
				if _, seen := byWorkflow[issue.WorkflowID]; !seen {
					workflowOrder = append(workflowOrder, issue.WorkflowID)
				}
				byWorkflow[issue.WorkflowID] = append(byWorkflow[issue.WorkflowID], issue)
			}

			for _, workflowID := range workflowOrder {
				fmt.Fprintf(w, "  Workflow: %s\n", workflowID)
				fmt.Fprintln(w, "  "+strings.Repeat("-", 76))
				for _, issue := range byWorkflow[workflowID] {
					fmt.Fprintf(w, "    • Activity: %s | Status: %s\n", issue.Activity, issue.Status)
					if issue.DeviceSerial != "" || issue.DeviceID != "" {
						fmt.Fprintf(w, "      Device: serial=%s id=%s\n", issue.DeviceSerial, issue.DeviceID)
					}
					if issue.StatusMessage != "" {
						fmt.Fprintf(w, "      Status Message: %s\n", issue.StatusMessage)
					}
					if issue.Source != "" {
						fmt.Fprintf(w, "      Source: %s\n", issue.Source)
					}
				}
				fmt.Fprintln(w)
			}

			fmt.Fprintln(w, "  "+strings.Repeat("=", 76))
			fmt.Fprintln(w)
		}

		if len(temporal.Missing) > 0 {
			order, byActivity := groupMissingByActivity(temporal.Missing)
			fmt.Fprintf(w, "  ACTIVITIES THAT NEVER RAN (%d)\n", len(temporal.Missing))
			fmt.Fprintln(w, "  These activities were expected to run but never started, so no status was")
			fmt.Fprintln(w, "  available to validate. This is NOT the same as a successful activity.")
			fmt.Fprintln(w, "  "+strings.Repeat("-", 76))
			for _, activity := range order {
				wfs := byActivity[activity]
				fmt.Fprintf(w, "    '%s' never ran in %d workflow(s):\n", activity, len(wfs))
				for i, wf := range wfs {
					fmt.Fprintf(w, "        %d. %s\n", i+1, wf)
				}
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, "  "+strings.Repeat("=", 76))
			fmt.Fprintln(w)
		}

		if len(temporal.Flows) > 0 {
			fmt.Fprintf(w, "  WORKFLOW EXECUTION FLOW (%d workflow(s))\n", len(temporal.Flows))
			fmt.Fprintln(w, "  Each workflow in execution order, so it is visible where it stopped.")
			fmt.Fprintln(w, "  Legend:  +-> ran   !-> failed/flagged   XXX never scheduled")
			fmt.Fprintln(w, "  "+strings.Repeat("-", 76))
			fmt.Fprintln(w)
			for _, flow := range temporal.Flows {
				renderTemporalWorkflowFlow(w, flow)
			}
			fmt.Fprintln(w, "  "+strings.Repeat("=", 76))
			fmt.Fprintln(w)
		}
	}

	// ---- SECTION 1: EXECUTIVE SUMMARY ----
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w, "  SECTION 1: EXECUTIVE SUMMARY")
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w)

	if totalMatches == 0 {
		fmt.Fprintln(w, "  No unexpected log messages found. All logs appear clean.")
		fmt.Fprintln(w)
		return nil
	}

	// Count by severity
	severityCounts := map[string]int{"CRITICAL": 0, "HIGH": 0, "MEDIUM": 0, "LOW": 0}
	for _, issue := range correlatedIssues {
		severityCounts[issue.Severity] += issue.TotalCount
	}

	fmt.Fprintf(w, "  CRITICAL:  %d occurrences\n", severityCounts["CRITICAL"])
	fmt.Fprintf(w, "  HIGH:      %d occurrences\n", severityCounts["HIGH"])
	fmt.Fprintf(w, "  MEDIUM:    %d occurrences\n", severityCounts["MEDIUM"])
	fmt.Fprintf(w, "  LOW:       %d occurrences\n", severityCounts["LOW"])
	fmt.Fprintln(w)

	// Top issues
	fmt.Fprintln(w, "  Top Issues:")
	for i, issue := range correlatedIssues {
		if i >= 5 {
			break
		}
		crossFileIndicator := ""
		if len(issue.Files) > 1 {
			crossFileIndicator = fmt.Sprintf(" [CROSS-FILE: %d files]", len(issue.Files))
		}
		fmt.Fprintf(w, "    %d. [%s] '%s' - %d occurrences%s\n",
			i+1, issue.Severity, issue.Pattern, issue.TotalCount, crossFileIndicator)
	}
	fmt.Fprintln(w)

	// ---- SECTION 2: CORRELATED ISSUES (CROSS-FILE) ----
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w, "  SECTION 2: CORRELATED ISSUES (Cross-File Error Analysis)")
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w)

	crossFileIssues := 0
	for _, issue := range correlatedIssues {
		if len(issue.Files) <= 1 {
			continue
		}
		crossFileIssues++
		fmt.Fprintf(w, "  CORRELATED ISSUE #%d\n", crossFileIssues)
		fmt.Fprintf(w, "  Pattern:    '%s'\n", issue.Pattern)
		fmt.Fprintf(w, "  Severity:   %s\n", issue.Severity)
		fmt.Fprintf(w, "  Occurrences: %d total across %d files\n", issue.TotalCount, len(issue.Files))
		fmt.Fprintln(w, "  Affected Files:")
		for _, f := range issue.Files {
			fmt.Fprintf(w, "    - %s\n", f)
		}
		fmt.Fprintf(w, "  Assessment: %s\n", issue.Description)
		fmt.Fprintln(w, "  "+strings.Repeat("-", 70))
		fmt.Fprintln(w)
	}

	if crossFileIssues == 0 {
		fmt.Fprintln(w, "  No cross-file correlated issues detected.")
		fmt.Fprintln(w, "  Errors appear to be isolated to individual files.")
		fmt.Fprintln(w)
	}

	// ---- SECTION 2A: TRANSACTION/REQUEST CORRELATION ----
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w, "  SECTION 2A: TRANSACTION/REQUEST CORRELATION ANALYSIS")
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w)

	if len(correlationIDIssues) > 0 {
		fmt.Fprintf(w, "  Found %d transaction(s)/request(s) with errors across multiple files.\n", len(correlationIDIssues))
		fmt.Fprintln(w, "  This analysis correlates errors by their correlation IDs (transaction IDs,")
		fmt.Fprintln(w, "  request IDs, trace IDs) to identify cascading failures and root causes.")
		fmt.Fprintln(w)

		for idx, issue := range correlationIDIssues {
			fmt.Fprintf(w, "  TRANSACTION/REQUEST #%d\n", idx+1)
			fmt.Fprintf(w, "  Correlation ID: %s (%s)\n", issue.CorrelationID, issue.CorrelationType)
			fmt.Fprintf(w, "  Severity:       %s\n", issue.Severity)
			fmt.Fprintf(w, "  Occurrences:    %d error(s) across %d file(s)\n", len(issue.Matches), len(issue.Files))

			if !issue.StartTime.IsZero() && !issue.EndTime.IsZero() {
				fmt.Fprintf(w, "  Time Span:      %s to %s (%v duration)\n",
					issue.StartTime.Format("15:04:05.000"),
					issue.EndTime.Format("15:04:05.000"),
					issue.Duration)
			}

			fmt.Fprintln(w, "  Affected Files:")
			for _, f := range issue.Files {
				fmt.Fprintf(w, "    - %s\n", f)
			}

			// Show error timeline if we have timestamps
			hasTimestamps := false
			for _, match := range issue.Matches {
				if !match.Timestamp.IsZero() {
					hasTimestamps = true
					break
				}
			}

			if hasTimestamps {
				fmt.Fprintln(w, "  Error Timeline (chronological order):")
				for i, match := range issue.Matches {
					timeStr := ""
					if !match.Timestamp.IsZero() {
						timeStr = match.Timestamp.Format("15:04:05.000")
					} else {
						timeStr = "??:??:??.???"
					}

					rootCauseIndicator := ""
					if i == 0 && !match.Timestamp.IsZero() {
						rootCauseIndicator = " [POTENTIAL ROOT CAUSE]"
					}

					fmt.Fprintf(w, "    [%s] %s - Pattern: '%s'%s\n",
						timeStr,
						filepath.Base(match.FileName),
						match.Pattern,
						rootCauseIndicator)

					// Show a snippet of the matched line
					snippet := match.MatchedLine
					if len(snippet) > 80 {
						snippet = snippet[:77] + "..."
					}
					fmt.Fprintf(w, "               %s\n", strings.TrimSpace(snippet))
				}
			}

			fmt.Fprintf(w, "  Assessment: %s\n", issue.Description)
			fmt.Fprintln(w, "  "+strings.Repeat("-", 70))
			fmt.Fprintln(w)
		}
	} else {
		fmt.Fprintln(w, "  No correlation IDs detected in error logs.")
		fmt.Fprintln(w, "  Either no correlation IDs are present, or errors do not share common")
		fmt.Fprintln(w, "  transaction/request identifiers across files.")
		fmt.Fprintln(w)
	}

	// ---- SECTION 3: PER-FILE ANALYSIS WITH CONTEXT ----
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w, "  SECTION 3: PER-FILE ANALYSIS (With Before/After Context)")
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w)

	// Sort summaries by total matches (most first)
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].TotalMatches > summaries[j].TotalMatches
	})

	for fileIdx, summary := range summaries {
		fmt.Fprintf(w, "  FILE %d: %s\n", fileIdx+1, summary.FileName)
		fmt.Fprintf(w, "  Total Matches: %d\n", summary.TotalMatches)

		// Pattern breakdown for this file
		fmt.Fprintln(w, "  Pattern Breakdown:")
		for pattern, count := range summary.PatternCounts {
			fmt.Fprintf(w, "    - '%s': %d\n", pattern, count)
		}
		fmt.Fprintln(w, "  "+strings.Repeat("-", 70))
		fmt.Fprintln(w)

		// Each match with before/after context
		for matchIdx, match := range summary.Matches {
			fmt.Fprintf(w, "    Match %d/%d [Pattern: '%s'] at Line %d:\n",
				matchIdx+1, summary.TotalMatches, match.Pattern, match.LineNumber)
			fmt.Fprintln(w)

			// Before context
			if len(match.BeforeLines) > 0 {
				fmt.Fprintln(w, "      --- BEFORE ---")
				for i, line := range match.BeforeLines {
					beforeLineNum := match.LineNumber - len(match.BeforeLines) + i
					fmt.Fprintf(w, "      %4d | %s\n", beforeLineNum, line)
				}
			}

			// The matched line (highlighted)
			fmt.Fprintln(w, "      >>> MATCHED LINE <<<")
			fmt.Fprintf(w, "      %4d | %s\n", match.LineNumber, match.MatchedLine)

			// After context
			if len(match.AfterLines) > 0 {
				fmt.Fprintln(w, "      --- AFTER ---")
				for i, line := range match.AfterLines {
					afterLineNum := match.LineNumber + 1 + i
					fmt.Fprintf(w, "      %4d | %s\n", afterLineNum, line)
				}
			}

			fmt.Fprintln(w)
		}

		fmt.Fprintln(w, "  "+strings.Repeat("=", 70))
		fmt.Fprintln(w)
	}

	// ---- SECTION 4: PATTERN FREQUENCY TABLE ----
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w, "  SECTION 4: PATTERN FREQUENCY TABLE")
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w)

	// Sort by count
	type patternEntry struct {
		pattern string
		count   int
		files   int
	}
	var patternTable []patternEntry
	for pattern, count := range globalPatternCounts {
		fileCount := 0
		if fm, ok := globalPatternFileMap[pattern]; ok {
			fileCount = len(fm)
		}
		patternTable = append(patternTable, patternEntry{pattern, count, fileCount})
	}
	sort.Slice(patternTable, func(i, j int) bool {
		return patternTable[i].count > patternTable[j].count
	})

	fmt.Fprintf(w, "  %-25s %10s %10s\n", "PATTERN", "COUNT", "FILES")
	fmt.Fprintf(w, "  %-25s %10s %10s\n", strings.Repeat("-", 25), strings.Repeat("-", 10), strings.Repeat("-", 10))
	for _, entry := range patternTable {
		fmt.Fprintf(w, "  %-25s %10d %10d\n", entry.pattern, entry.count, entry.files)
	}
	fmt.Fprintln(w)

	// ---- SECTION 5: FILES WITHOUT ISSUES ----
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w, "  SECTION 5: RECOMMENDATION")
	fmt.Fprintln(w, strings.Repeat("*", 80))
	fmt.Fprintln(w)

	if severityCounts["CRITICAL"] > 0 {
		fmt.Fprintln(w, "  ⚠ CRITICAL issues detected! Immediate investigation required.")
		fmt.Fprintln(w, "  Focus on 'panic' and 'fatal' errors first - these indicate application crashes.")
		fmt.Fprintln(w)
	}
	if crossFileIssues > 0 {
		fmt.Fprintln(w, "  Cross-file correlated issues detected. These errors appear in multiple log files")
		fmt.Fprintln(w, "  simultaneously, suggesting a systemic problem (e.g., infrastructure, connectivity,")
		fmt.Fprintln(w, "  or deployment issue) rather than an isolated bug.")
		fmt.Fprintln(w)
	}
	if severityCounts["CRITICAL"] == 0 && severityCounts["HIGH"] == 0 && crossFileIssues == 0 {
		fmt.Fprintln(w, "  No critical or cross-file issues detected. The errors found appear to be")
		fmt.Fprintln(w, "  isolated and low-severity. Review individual file sections above for details.")
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, strings.Repeat("=", 80))
	fmt.Fprintln(w, "  END OF LOG ANALYTICS REPORT")
	fmt.Fprintln(w, strings.Repeat("=", 80))

	return nil
}

// globalPatternFileMap is used by the report generator to access pattern-file mapping
var globalPatternFileMap map[string]map[string]int

// truncateLine truncates a line to the given max length
func truncateLine(line string, maxLen int) string {
	if len(line) <= maxLen {
		return line
	}
	return line[:maxLen] + "..."
}

// printAnalyticsSummary outputs a concise summary to the console
func printAnalyticsSummary(summaries []FileAnalysisSummary, correlatedIssues []CorrelatedIssue, totalMatches int, temporal TemporalAnalysisResult) {
	logger.Info("%s", "="+strings.Repeat("=", 50))
	logger.Info("  LOG ANALYTICS SUMMARY")
	logger.Info("%s", "="+strings.Repeat("=", 50))

	if temporal.Enabled {
		logger.Info("  TEMPORAL ANALYSIS:")
		switch {
		case len(temporal.Issues) > 0:
			logger.Warn("    %d of %d checked status value(s) were non-success:", len(temporal.Issues), temporal.StatusesChecked)
			for _, issue := range temporal.Issues {
				logger.Warn("      - %s", formatTemporalActivityIssue(issue))
			}
		case temporal.StatusesChecked == 0:
			logger.Warn("    NOT VALIDATED - no collected activity output contained a status field")
		default:
			logger.Info("    All %d checked status value(s) were successful", temporal.StatusesChecked)
		}
		if len(temporal.Missing) > 0 {
			order, byActivity := groupMissingByActivity(temporal.Missing)
			logger.Warn("    MISSING ACTIVITIES: expected to run, but never started. Their status could NOT be checked.")
			for _, activity := range order {
				wfs := byActivity[activity]
				logger.Warn("      '%s' never ran in %d workflow(s):", activity, len(wfs))
				for i, wf := range wfs {
					logger.Warn("          %d. %s", i+1, wf)
				}
			}
		}
	}

	if totalMatches == 0 {
		logger.Info("  No issues found - all logs appear clean!")
		return
	}

	logger.Info("  Total unexpected messages found: %d", totalMatches)
	logger.Info("  Files with issues: %d", len(summaries))

	// Count cross-file issues
	crossFileCount := 0
	for _, issue := range correlatedIssues {
		if len(issue.Files) > 1 {
			crossFileCount++
		}
	}
	if crossFileCount > 0 {
		logger.Info("  Cross-file correlated issues: %d", crossFileCount)
	}

	// Show severity breakdown
	severityCounts := map[string]int{}
	for _, issue := range correlatedIssues {
		severityCounts[issue.Severity] += issue.TotalCount
	}
	if severityCounts["CRITICAL"] > 0 {
		logger.Warn("  ⚠ CRITICAL: %d occurrences", severityCounts["CRITICAL"])
	}
	if severityCounts["HIGH"] > 0 {
		logger.Warn("  HIGH: %d occurrences", severityCounts["HIGH"])
	}

	// Show top 3 affected files
	logger.Info("  Top affected files:")
	for i, s := range summaries {
		if i >= 3 {
			break
		}
		logger.Info("    %d. %s (%d matches)", i+1, s.FileName, s.TotalMatches)
	}
}

// ============================================================================
// Post-Download Message Filter — Filter logs into a separate filtered_logs_{timestamp} directory
// ============================================================================

// filterDownloadedLogs extracts a .tar.gz archive, applies message filters to each log file,
// and writes filtered versions into a filtered_logs_{timestamp} directory preserving the archive's directory structure.
//
// Filter logic (Loki-style strict inclusion):
//   - keyValueFilters: A line is kept ONLY if it contains the key AND the value matches.
//     Lines that don't contain the key are dropped (not passed through).
//   - specificStrings: If specified, lines MUST contain at least one of these strings to be included.
//   - When both keyValueFilters and specificStrings are configured, a line must pass ALL filters.
//   - combineReplicas: If enabled, merges logs from replica pods into single files (e.g., nvo-edge-abc + nvo-edge-xyz → nvo-edge.log)
//   - sortByTimestamp: If enabled, sorts combined logs chronologically by timestamp
func filterDownloadedLogs(archivePath, outputDir string, filterConfig struct {
	Enabled              bool `yaml:"enabled"`
	FilterDuringDownload bool `yaml:"filterDuringDownload"`
	KeyValueFilters      []struct {
		Key   string `yaml:"key"`
		Value string `yaml:"value"`
	} `yaml:"keyValueFilters"`
	SpecificStrings  []string `yaml:"specificStrings"`
	CombineReplicas  bool     `yaml:"combineReplicas"`
	ReplicaPattern   string   `yaml:"replicaPattern"`
	SortByTimestamp  bool     `yaml:"sortByTimestamp"`
	TimestampPattern string   `yaml:"timestampPattern"`
}) error {
	if !filterConfig.Enabled {
		return nil
	}

	hasKeyValueFilters := len(filterConfig.KeyValueFilters) > 0
	hasSpecificStrings := len(filterConfig.SpecificStrings) > 0

	if !hasKeyValueFilters && !hasSpecificStrings {
		logger.Debug("Message filter enabled but no filters configured, skipping")
		return nil
	}

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  MESSAGE FILTER - Filtering downloaded logs")
	logger.Info("%s", strings.Repeat("=", 70))

	if hasKeyValueFilters {
		for _, kv := range filterConfig.KeyValueFilters {
			if kv.Value == "" {
				logger.Info("  Key-Value Filter: key=%q value=(empty) → SKIPPED (no value to filter on)", kv.Key)
			} else {
				logger.Info("  Key-Value Filter: key=%q value=%q (Loki-style: keep ONLY lines with matching key=value)", kv.Key, kv.Value)
			}
		}
	}
	if hasSpecificStrings {
		logger.Info("  Specific Strings: %s (only keep lines containing these)", strings.Join(filterConfig.SpecificStrings, ", "))
	}
	if filterConfig.CombineReplicas {
		logger.Info("  Replica Merging: ENABLED (combining replica pod logs into single files)")
		if filterConfig.SortByTimestamp {
			logger.Info("  Timestamp Sorting: ENABLED (chronological order within combined files)")
		}
	}

	// Extract archive to temp dir
	extractDir, err := extractTarGz(archivePath)
	if err != nil {
		return fmt.Errorf("failed to extract archive for filtering: %v", err)
	}
	defer os.RemoveAll(extractDir)

	// Derive the archive timestamp for the filter subdirectory name
	archiveBase := filepath.Base(archivePath)
	archiveBase = strings.TrimSuffix(archiveBase, ".tar.gz")
	archiveBase = strings.TrimSuffix(archiveBase, ".gz")

	// Extract timestamp from archive name (e.g., app_log_20260217_095735 -> 20260217_095735)
	// Use it to create filtered_logs_{timestamp}/
	tsRe := regexp.MustCompile(`(\d{8}_\d{6})$`)
	tsMatch := tsRe.FindString(archiveBase)
	filterDirName := archiveBase // fallback: use full archive base name
	if tsMatch != "" {
		filterDirName = "filtered_logs_" + tsMatch
	}

	// Create filter output directory: outputDir/filtered_logs_{timestamp}/
	filterBaseDir := filepath.Join(outputDir, filterDirName)

	// Discover all files (ONLY kubectl logs and pod file collection logs, NOT system info/database/version files)
	var logFiles []string
	err = filepath.Walk(extractDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}

		// Skip system directories (System_Info, Database, App_Version_Info, etc.)
		// Message filtering should ONLY apply to kubectl logs and pod file collection
		relPath, _ := filepath.Rel(extractDir, path)
		pathLower := strings.ToLower(relPath)

		// Skip if path contains system directories
		skipDirs := []string{"system_info", "database", "app_version_info", "version_info"}
		shouldSkip := false
		for _, skipDir := range skipDirs {
			if strings.Contains(pathLower, skipDir) {
				shouldSkip = true
				break
			}
		}

		if shouldSkip {
			logger.Debug("Skipping system file from message filtering: %s", relPath)
			return nil
		}

		// Only filter text/log files (kubectl logs and pod file collection)
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".log", ".txt", ".json", ".yaml", ".yml", ".xml", ".csv", ".out", ".err", "":
			logFiles = append(logFiles, path)
		default:
			if isLikelyTextFile(path) {
				logFiles = append(logFiles, path)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk extracted files: %v", err)
	}

	if len(logFiles) == 0 {
		logger.Warn("No log files found in archive to filter")
		return nil
	}

	logger.Info("Filtering %d file(s)...", len(logFiles))

	// Compile key-value filter patterns
	type kvFilter struct {
		keyPattern  *regexp.Regexp
		fullPattern *regexp.Regexp
	}
	var kvFilters []kvFilter
	for _, kv := range filterConfig.KeyValueFilters {
		if kv.Value == "" {
			continue
		}
		keyRe, err := regexp.Compile("(?i)" + regexp.QuoteMeta(kv.Key))
		if err != nil {
			logger.Warn("Invalid key pattern '%s', skipping", kv.Key)
			continue
		}

		// Build flexible key pattern: allow optional '_' at camelCase transitions
		// so "ownerID" also matches "owner_id", "OwnerID", etc.
		// Also allow optional '\"' or '"' between key and delimiter for JSON format ("key":value)
		// and escaped JSON (\"key\":value) common in nested log messages
		var flexKeyBuf strings.Builder
		for i := 0; i < len(kv.Key); i++ {
			if i > 0 && kv.Key[i] >= 'A' && kv.Key[i] <= 'Z' && kv.Key[i-1] >= 'a' && kv.Key[i-1] <= 'z' {
				flexKeyBuf.WriteString("_?")
			}
			flexKeyBuf.WriteString(regexp.QuoteMeta(string(kv.Key[i])))
		}
		flexKey := flexKeyBuf.String()

		escapedVal := regexp.QuoteMeta(kv.Value)
		// Pattern: key (with optional _ at case transitions) + optional \" or " + whitespace + : or = + whitespace + optional \" or " + value + optional \" or "
		fullRe, err := regexp.Compile("(?i)" + flexKey + `\\?"?\s*[:=]\s*\\?"?` + escapedVal + `\\?"?`)
		if err != nil {
			logger.Warn("Invalid key-value pattern for key='%s' value='%s', skipping", kv.Key, kv.Value)
			continue
		}
		logger.Debug("  Key-value regex: %s", fullRe.String())
		kvFilters = append(kvFilters, kvFilter{keyPattern: keyRe, fullPattern: fullRe})
	}

	// Compile replica pattern if combining replicas
	var replicaRegex *regexp.Regexp
	if filterConfig.CombineReplicas && filterConfig.ReplicaPattern != "" {
		replicaRegex, err = regexp.Compile(filterConfig.ReplicaPattern)
		if err != nil {
			logger.Warn("Invalid replica pattern '%s', disabling replica combining: %v", filterConfig.ReplicaPattern, err)
			filterConfig.CombineReplicas = false
		}
	}

	// Compile timestamp pattern if sorting by timestamp
	var timestampRegex *regexp.Regexp
	if filterConfig.SortByTimestamp && filterConfig.TimestampPattern != "" {
		timestampRegex, err = regexp.Compile(filterConfig.TimestampPattern)
		if err != nil {
			logger.Warn("Invalid timestamp pattern '%s', disabling timestamp sorting: %v", filterConfig.TimestampPattern, err)
			filterConfig.SortByTimestamp = false
		}
	}

	// Structure to hold filtered lines with optional timestamp
	type FilteredLine struct {
		Line       string
		Timestamp  time.Time
		SourceFile string
	}

	// Map: baseName -> []FilteredLine (for replica combining)
	baseNameGroups := make(map[string][]FilteredLine)
	filesProcessed := 0
	totalLinesKept := 0
	totalLinesRemoved := 0

	// Process each file: filter and collect lines
	for _, srcPath := range logFiles {
		relPath, _ := filepath.Rel(extractDir, srcPath)

		// Read source file
		srcFile, err := os.Open(srcPath)
		if err != nil {
			logger.Warn("Cannot read %s for filtering: %v", relPath, err)
			continue
		}

		scanner := bufio.NewScanner(srcFile)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		var filteredLines []FilteredLine
		linesRead := 0
		linesKept := 0

		for scanner.Scan() {
			line := scanner.Text()
			linesRead++

			keep := true

			// Apply key-value filters
			for _, kv := range kvFilters {
				if !kv.fullPattern.MatchString(line) {
					keep = false
					break
				}
			}

			// Apply specific string filters
			if keep && hasSpecificStrings {
				found := false
				for _, ss := range filterConfig.SpecificStrings {
					if strings.Contains(line, ss) {
						found = true
						break
					}
				}
				if !found {
					keep = false
				}
			}

			if keep {
				fl := FilteredLine{
					Line:       line,
					SourceFile: relPath,
				}

				// Try to extract timestamp if sorting is enabled
				if filterConfig.SortByTimestamp && timestampRegex != nil {
					if tsMatch := timestampRegex.FindString(line); tsMatch != "" {
						// Try to parse timestamp with common formats
						parsedTime := parseTimestamp(tsMatch)
						if !parsedTime.IsZero() {
							fl.Timestamp = parsedTime
						}
					}
				}

				filteredLines = append(filteredLines, fl)
				linesKept++
			}
		}
		srcFile.Close()

		if err := scanner.Err(); err != nil {
			logger.Warn("Error scanning %s: %v", relPath, err)
			continue
		}

		linesRemoved := linesRead - linesKept
		totalLinesKept += linesKept
		totalLinesRemoved += linesRemoved

		if linesKept > 0 {
			filesProcessed++

			// Determine base name for grouping
			baseName := relPath
			if filterConfig.CombineReplicas && replicaRegex != nil {
				// Strip replica suffix to get base name
				// Handles two cases:
				// 1. Pod name in directory: "xiq/nvo-edge-7c59fdd6d6-7sqwc/app.log" -> "xiq/nvo-edge/app.log"
				// 2. Pod name in filename: "nvo/nvo-network-7c895646cd-7lgcp-server.log" -> "nvo/nvo-network-server.log"
				// Note: Kubernetes pods have TWO suffixes (ReplicaSet hash + Pod hash)
				// We need to strip both to merge all replicas together
				dir := filepath.Dir(relPath)
				file := filepath.Base(relPath)
				podDir := filepath.Base(dir)

				dirChanged := false
				fileChanged := false

				// Strip ALL replica suffixes from pod directory name (loop until no more matches)
				// This handles cases like "nvo-edge-7c59fdd6d6-7sqwc" -> "nvo-edge"
				origPodDir := podDir
				for replicaRegex.MatchString(podDir) {
					podDir = replicaRegex.ReplaceAllString(podDir, "")
				}
				if podDir != origPodDir {
					dirChanged = true
				}

				// Strip ALL replica suffixes from filename (for pod file collections)
				// This handles cases like "nvo-network-7c895646cd-7lgcp-server.log" -> "nvo-network-server.log"
				origFile := file
				for replicaRegex.MatchString(file) {
					file = replicaRegex.ReplaceAllString(file, "")
				}
				if file != origFile {
					fileChanged = true
				}

				// Rebuild path if either directory or filename changed
				if dirChanged || fileChanged {
					if dirChanged {
						dir = filepath.Join(filepath.Dir(dir), podDir)
					}
					baseName = filepath.Join(dir, file)
				}
			}

			// Add to group
			baseNameGroups[baseName] = append(baseNameGroups[baseName], filteredLines...)

			if linesRemoved > 0 {
				logger.Debug("  Filtered %s: kept %d/%d lines (removed %d)", relPath, linesKept, linesRead, linesRemoved)
			}
		} else if linesRead > 0 {
			logger.Debug("  Skipped %s: all %d lines filtered out", relPath, linesRead)
		}
	}

	// Write combined files
	filesWritten := 0
	replicasCombined := 0

	for baseName, lines := range baseNameGroups {
		destPath := filepath.Join(filterBaseDir, baseName)

		// Count source files for this base name (replica count)
		sourceFiles := make(map[string]bool)
		for _, fl := range lines {
			sourceFiles[fl.SourceFile] = true
		}

		// Sort by timestamp if enabled
		if filterConfig.SortByTimestamp {
			sort.SliceStable(lines, func(i, j int) bool {
				// If both have timestamps, sort by time
				if !lines[i].Timestamp.IsZero() && !lines[j].Timestamp.IsZero() {
					return lines[i].Timestamp.Before(lines[j].Timestamp)
				}
				// If only one has timestamp, put timestamped line first
				if !lines[i].Timestamp.IsZero() {
					return true
				}
				if !lines[j].Timestamp.IsZero() {
					return false
				}
				// Neither has timestamp, maintain original order
				return false
			})
		}

		// Create output directory
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			logger.Warn("Failed to create filter dir for %s: %v", baseName, err)
			continue
		}

		// Write combined file
		destFile, err := os.Create(destPath)
		if err != nil {
			logger.Warn("Failed to create filtered file %s: %v", baseName, err)
			continue
		}

		w := bufio.NewWriter(destFile)
		for _, fl := range lines {
			fmt.Fprintln(w, fl.Line)
		}
		w.Flush()
		destFile.Close()
		filesWritten++

		if len(sourceFiles) > 1 {
			replicasCombined++
			logger.Debug("  Combined %d replica(s) into %s (%d lines)", len(sourceFiles), baseName, len(lines))
		}
	}

	logger.Info("Message filtering complete:")
	logger.Info("  Files processed: %d", filesProcessed)
	logger.Info("  Output files written: %d", filesWritten)
	if filterConfig.CombineReplicas && replicasCombined > 0 {
		logger.Info("  Replica groups combined: %d", replicasCombined)
	}
	logger.Info("  Total lines kept: %d, removed: %d", totalLinesKept, totalLinesRemoved)
	logger.Info("  Filtered logs saved to: %s", filterBaseDir)

	return nil
}

// parseTimestamp attempts to parse a timestamp string in various common formats
func parseTimestamp(ts string) time.Time {
	formats := []string{
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05.000000",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.000",
		"2006-01-02 15:04:05.000000",
		"2006-01-02 15:04:05",
		"[2006-01-02 15:04:05]",
		"[2006-01-02T15:04:05]",
	}

	// Remove brackets if present
	ts = strings.Trim(ts, "[]")

	for _, format := range formats {
		if t, err := time.Parse(format, ts); err == nil {
			return t
		}
	}
	return time.Time{}
}

// collectAppVersionsStandalone collects application version information and saves it locally
// Returns the path to the created file and any error
func collectAppVersionsStandalone(awsClient *ssh.Client, config *Config, outputDir string) (string, error) {
	if !config.AppVersionCollection.Enabled {
		logger.Info("App version collection is disabled in config")
		return "", nil
	}

	logger.Debug("Starting standalone application version collection...")
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  APP VERSION - Collecting application version information")
	logger.Info("%s", strings.Repeat("=", 70))

	// First, let's check what namespaces are available
	logger.Debug("Checking available namespaces...")
	nsSession, err := awsClient.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session for namespace check: %v", err)
	}
	nsCmd := "kubectl get namespaces --no-headers | awk '{print \\$1}'"
	nsOutput, err := nsSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", nsCmd))
	nsSession.Close()

	var availableNamespaces []string
	xiqNamespaceExists := false

	if err != nil {
		logger.Warn("Failed to get namespaces: %v", err)
		logger.Debug("Namespace command output: %s", string(nsOutput))
	} else {
		availableNamespaces = strings.Split(strings.TrimSpace(string(nsOutput)), "\n")
		logger.Debug("Available namespaces: %v", availableNamespaces)

		// Check if 'xiq' namespace exists
		for _, ns := range availableNamespaces {
			if strings.TrimSpace(ns) == "xiq" {
				xiqNamespaceExists = true
				break
			}
		}
	}

	// Determine XIQ namespace: use 'xiq' if it exists, otherwise use environment name
	xiqNamespace := config.Environment
	if xiqNamespaceExists {
		xiqNamespace = "xiq"
		logger.Debug("Found dedicated 'xiq' namespace - will use it for XIQ applications")
	} else {
		logger.Info("No dedicated 'xiq' namespace found - will use environment namespace '%s' for XIQ applications", config.Environment)
	}

	// Apply template replacement to output filename
	outputFileName := config.AppVersionCollection.OutputFileName
	outputFileName = strings.ReplaceAll(outputFileName, "{environment}", config.Environment)
	outputFileName = strings.ReplaceAll(outputFileName, "{username}", config.Username)

	// Replace {timestamp} placeholder — use override if provided, else current time
	timestampStr := config.archiveTimestamp
	if timestampStr == "" {
		timestampStr = time.Now().Format("20060102_150405")
	}
	outputFileName = strings.ReplaceAll(outputFileName, "{timestamp}", timestampStr)

	// Use the provided output directory (already has template replacements applied)
	// If empty, fall back to current directory
	if outputDir == "" {
		outputDir = "."
	}

	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create output directory: %v", err)
	}

	// Create local output file path
	localFilePath := filepath.Join(outputDir, outputFileName)

	logger.Debug("Output file will be saved to: %s", localFilePath)

	// Prepare the header content like the format in the user request
	timestamp := time.Now().Format("Mon Jan 2 15:04:05 MST 2006")
	envName := strings.ToUpper(config.Environment)
	content := fmt.Sprintf("### %s Cloud testbed ###\n\n", envName)
	content += fmt.Sprintf("Date and Time: %s\n\n", timestamp)
	content += fmt.Sprintf("### %s RDC components versions ###\n", envName)
	content += "+----------------+---------------------------------------------------+---------------------+\n"
	content += "|  Namespace     |  Pod                                              |  Version            |\n"
	content += "+----------------+---------------------------------------------------+---------------------+\n"

	// Store version info with namespace for grouping
	type versionEntry struct {
		namespace string
		line      string
	}
	var allVersionInfo []versionEntry
	totalPods := 0
	successCount := 0

	// Process each namespace
	for _, ns := range config.AppVersionCollection.Namespaces {
		// Apply template replacement to namespace
		namespace := ns.Namespace
		// For {environment} placeholder, use xiq namespace if it exists, otherwise use environment name
		namespace = strings.ReplaceAll(namespace, "{environment}", xiqNamespace)
		namespace = strings.ReplaceAll(namespace, "{username}", config.Username)

		logger.Debug("Checking namespace: %s (%s)", namespace, ns.Description)

		// Process each pod prefix in the namespace
		for _, podPrefix := range ns.PodPrefixes {
			// Apply template replacement to pod prefix
			prefix := podPrefix
			prefix = strings.ReplaceAll(prefix, "{environment}", config.Environment)
			prefix = strings.ReplaceAll(prefix, "{username}", config.Username)

			logger.Debug("Looking for pods with prefix: %s in namespace: %s", prefix, namespace)

			// Create a new session for each kubectl command to avoid "Stdout already set" errors
			session, err := awsClient.NewSession()
			if err != nil {
				logger.Warn("Failed to create session for namespace %s with prefix %s: %v", namespace, prefix, err)
				continue
			}

			// Get pods matching the prefix with timeout for better performance - faster timeout
			listPodsCmd := fmt.Sprintf("timeout 15 kubectl get pods -n %s --no-headers | grep '^%s' | awk '{print \\$1}'", namespace, prefix)
			logger.Debug("Executing: sudo su - -c \"%s\"", listPodsCmd)
			podsOutput, err := session.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", listPodsCmd))
			session.Close()

			if err != nil {
				logger.Warn("Failed to list pods in namespace %s with prefix %s: %v", namespace, prefix, err)
				logger.Debug("Command output: %s", string(podsOutput))
				continue
			}

			podOutputStr := strings.TrimSpace(string(podsOutput))

			// Check for "No resources found" or empty output
			if podOutputStr == "" || strings.Contains(podOutputStr, "No resources found") || strings.Contains(podOutputStr, "Error from server") {
				logger.Debug("No pods found with prefix %s in namespace %s", prefix, namespace)
				continue
			}

			pods := strings.Split(podOutputStr, "\n")
			if len(pods) == 0 {
				logger.Debug("No pods found with prefix %s in namespace %s", prefix, namespace)
				continue
			}

			// Only get version info from the first pod of each prefix type (representative)
			if len(pods) > 0 && pods[0] != "" {
				podName := strings.TrimSpace(pods[0])
				logger.Debug("Getting version info for representative pod: %s (found %d total pods with prefix %s)", podName, len(pods), prefix)

				totalPods++

				// Create a new session for each describe command
				describeSession, err := awsClient.NewSession()
				if err != nil {
					logger.Warn("Failed to create session for describing pod %s: %v", podName, err)
					continue
				}

				// Get image information from pod description using specific grep patterns
				var describeCmd string

				// Use specific grep patterns based on pod name to extract version information
				switch {
				case strings.HasPrefix(podName, "cs-perfmonitor"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*perfmonitor:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "hmweb"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*hm-webapp:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "teconfig"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*task-engine:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "nvo-edge"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*edge:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "nvo-network"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*network:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "cas-api-service"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*cas-api-service:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "cls-api-service"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*cls-api-service:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "cns-api-service"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*cns-api-service:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "cs-configstate"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*configstate:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "cs-metaflow-cacheupdater"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*metaflow-cacheupdater:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "cs-tagging"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*tagging:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "device-connector"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*device-connector:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "xiq-app"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*xiq-app:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "xiq-api"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*xiq-api:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				case strings.HasPrefix(podName, "xiq-ui"):
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image: .*xiq-ui:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				default:
					// Fallback to generic image extraction for unknown pod types
					describeCmd = fmt.Sprintf("kubectl describe pod %s -n %s | grep 'Image:' | head -1 | awk -F':' '{print \\$NF}' | tr -d ' '", podName, namespace)
				}

				logger.Debug("Using kubectl command: %s", describeCmd)
				imageOutput, err := describeSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", describeCmd))
				describeSession.Close()

				var version string
				if err != nil {
					logger.Warn("Failed to describe pod %s: %v", podName, err)
					logger.Debug("Command output: %s", string(imageOutput))
					version = "ERROR"
				} else {
					imageInfo := strings.TrimSpace(string(imageOutput))
					logger.Debug("Raw image output for %s: '%s'", podName, imageInfo)
					if imageInfo == "" {
						version = "Unknown"
					} else {
						version = imageInfo
					}
				}

				// Format the table row
				versionLine := fmt.Sprintf("|  %-13s |  %-48s |  %-18s |", namespace, podName, version)
				allVersionInfo = append(allVersionInfo, versionEntry{namespace: namespace, line: versionLine})
				successCount++

				logger.Debug("Retrieved version for %s: %s", podName, version)
			}
		}
	}

	// Add all version info to content with separators between namespaces
	var lastNamespace string
	for i, entry := range allVersionInfo {
		// Add separator line when namespace changes (but not for the first entry)
		if i > 0 && entry.namespace != lastNamespace {
			content += "+----------------+---------------------------------------------------+---------------------+\n"
		}
		content += entry.line + "\n"
		lastNamespace = entry.namespace
	}

	// Close the table
	content += "+----------------+---------------------------------------------------+---------------------+\n"

	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create output directory %s: %v", outputDir, err)
	}

	// Write to local file
	if err := ioutil.WriteFile(localFilePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write app versions file %s: %v", localFilePath, err)
	}

	// Get file info for logging
	fileInfo, err := os.Stat(localFilePath)
	var fileSize string
	if err == nil {
		fileSize = fmt.Sprintf("%d bytes", fileInfo.Size())
	} else {
		fileSize = "unknown size"
	}

	logger.Debug("App versions saved to: %s (%s)", localFilePath, fileSize)

	// Print to log if requested
	if config.AppVersionCollection.PrintToLog {
		logger.Info("Application Version Information:")
		logger.Info("%s", strings.Repeat("=", 80))
		lines := strings.Split(content, "\n")
		for _, line := range lines {
			if line != "" {
				logger.Info("%s", line)
			}
		}
		logger.Info("%s", strings.Repeat("=", 80))
	}

	logger.Info("Standalone app version collection completed: %d/%d pods processed successfully", successCount, totalPods)
	logger.Info("Version information saved locally to: %s", localFilePath)

	return localFilePath, nil
}

// ================================================================
// ================================================================
// Credential Management Functions
// Platform-specific implementations in:
//   - credentials_windows.go (Windows Credential Manager)
//   - credentials_other.go (Linux/macOS stubs - use env vars or config)
// ================================================================

// splitJiraIssueKeys parses a comma-separated list of JIRA issue keys (e.g.
// "XCP-1234, XCP-2345, NVO-1234"), trimming whitespace and dropping empty entries.
func splitJiraIssueKeys(csv string) []string {
	var keys []string
	for _, part := range strings.Split(csv, ",") {
		key := strings.TrimSpace(part)
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// attachFilesToMultipleJiraIssues attaches the given files to one or more JIRA issues.
// issueIDsCSV accepts a single issue key ("XCP-1234") or a comma-separated list
// ("XCP-1234, XCP-2345, NVO-1234"), attaching the same set of files to every issue listed.
func attachFilesToMultipleJiraIssues(jiraConfig JiraConfig, issueIDsCSV string, filePaths []string, logger *Logger) error {
	issueKeys := splitJiraIssueKeys(issueIDsCSV)
	if len(issueKeys) == 0 {
		return fmt.Errorf("no JIRA issue keys provided")
	}

	if len(issueKeys) == 1 {
		return attachFilesToJira(jiraConfig, issueKeys[0], filePaths, logger)
	}

	logger.Info("Attaching files to %d JIRA issues: %s", len(issueKeys), strings.Join(issueKeys, ", "))

	var failedIssues []string
	for _, issueKey := range issueKeys {
		if err := attachFilesToJira(jiraConfig, issueKey, filePaths, logger); err != nil {
			logger.Error("Failed to attach files to JIRA issue %s: %v", issueKey, err)
			failedIssues = append(failedIssues, issueKey)
		}
	}

	if len(failedIssues) > 0 {
		return fmt.Errorf("failed to attach files to %d/%d JIRA issue(s): %v", len(failedIssues), len(issueKeys), failedIssues)
	}
	return nil
}

// attachFilesToJira uploads files to a JIRA issue using the JIRA REST API
func attachFilesToJira(jiraConfig JiraConfig, issueKey string, filePaths []string, logger *Logger) error {
	// Normalize config values defensively — a stray trailing slash on baseUrl (e.g.
	// "https://foo.atlassian.net/") produces a double-slash API URL that Jira Cloud's
	// edge redirects (301), which silently turns the POST upload into a GET and
	// makes the "attach" appear to fail with no useful error. Likewise trim
	// whitespace/newlines that can sneak in via YAML/env var copy-paste.
	jiraConfig.Email = strings.TrimSpace(jiraConfig.Email)
	jiraConfig.BaseURL = strings.TrimRight(strings.TrimSpace(jiraConfig.BaseURL), "/")
	issueKey = strings.TrimSpace(issueKey)

	// Validate basic configuration
	if jiraConfig.Email == "" {
		return fmt.Errorf("JIRA email not configured")
	}

	if jiraConfig.BaseURL == "" {
		return fmt.Errorf("JIRA baseUrl not configured")
	}

	if !strings.HasPrefix(jiraConfig.BaseURL, "http://") && !strings.HasPrefix(jiraConfig.BaseURL, "https://") {
		return fmt.Errorf("JIRA baseUrl %q is missing a scheme (expected e.g. \"https://yourcompany.atlassian.net\")", jiraConfig.BaseURL)
	}

	if issueKey == "" {
		return fmt.Errorf("JIRA issue key is empty")
	}

	// Get API token from multiple sources (env var, keychain, config, prompt)
	apiToken, err := getJIRAApiToken(&jiraConfig, logger)
	if err != nil {
		return fmt.Errorf("failed to retrieve JIRA API token: %v", err)
	}
	apiToken = strings.TrimSpace(apiToken)

	// Filter out non-existent files
	existingFiles := []string{}
	for _, path := range filePaths {
		if _, err := os.Stat(path); err == nil {
			existingFiles = append(existingFiles, path)
		} else {
			logger.Debug("Skipping non-existent file: %s", path)
		}
	}

	if len(existingFiles) == 0 {
		return fmt.Errorf("no files found to attach")
	}

	// Upload files in parallel
	logger.Info("Uploading %d file(s) to JIRA issue %s...", len(existingFiles), issueKey)

	type uploadResult struct {
		fileName string
		err      error
	}

	resultChan := make(chan uploadResult, len(existingFiles))
	var wg sync.WaitGroup

	// Launch parallel upload goroutines
	for _, filePath := range existingFiles {
		wg.Add(1)
		go func(fPath string) {
			defer wg.Done()
			fileName := filepath.Base(fPath)

			logger.Info("Attaching file: %s", fileName)

			err := attachSingleFileToJira(jiraConfig, issueKey, fPath, apiToken, logger)

			if err != nil {
				logger.Error("Failed to attach %s: %v", fileName, err)
				resultChan <- uploadResult{fileName: fileName, err: err}
			} else {
				logger.Info("Successfully attached: %s", fileName)
				resultChan <- uploadResult{fileName: fileName, err: nil}
			}
		}(filePath)
	}

	// Wait for all uploads to complete
	wg.Wait()
	close(resultChan)

	// Collect results
	successCount := 0
	failedFiles := []string{}

	for result := range resultChan {
		if result.err == nil {
			successCount++
		} else {
			failedFiles = append(failedFiles, result.fileName)
		}
	}

	// Print summary
	if successCount > 0 {
		logger.Info("Successfully attached %d file(s) to JIRA issue %s", successCount, issueKey)
	}

	if len(failedFiles) > 0 {
		return fmt.Errorf("failed to attach %d file(s): %v", len(failedFiles), failedFiles)
	}

	return nil
}

// attachSingleFileToJira uploads a single file to a JIRA issue
func attachSingleFileToJira(jiraConfig JiraConfig, issueKey string, filePath string, apiToken string, logger *Logger) error {
	// Build the API endpoint
	apiURL := fmt.Sprintf("%s/rest/api/3/issue/%s/attachments", jiraConfig.BaseURL, issueKey)

	// Create multipart form data
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}

	fileName := filepath.Base(filePath)
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		file.Close()
		return fmt.Errorf("failed to create form file: %w", err)
	}

	_, err = io.Copy(part, file)
	file.Close()
	if err != nil {
		return fmt.Errorf("failed to copy file to form: %w", err)
	}

	err = writer.Close()
	if err != nil {
		return fmt.Errorf("failed to close multipart writer: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", apiURL, body)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Atlassian-Token", "no-check")

	// Set Basic Authentication
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", jiraConfig.Email, apiToken)))
	req.Header.Set("Authorization", fmt.Sprintf("Basic %s", auth))

	// Send request with extended timeout for large file uploads.
	// CheckRedirect fails fast with a clear error instead of silently letting Go's
	// http.Client turn a 301/302 redirect (e.g. from a misconfigured baseUrl) into a
	// bodyless GET, which would otherwise look like an unexplained upload failure.
	client := &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("unexpected redirect to %s (check jira.baseUrl for typos/trailing slash)", req.URL)
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, _ := io.ReadAll(resp.Body)

	// Check response status
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		logger.Debug("JIRA API Response for %s: %s", fileName, string(respBody))
		return nil
	}

	// Special handling for 401 errors on large files - may indicate token expiration
	jiraMsg := extractJiraErrorMessage(respBody)
	if resp.StatusCode == 401 {
		fileInfo, _ := os.Stat(filePath)
		if fileInfo != nil && fileInfo.Size() > 50*1024*1024 { // > 50MB
			return fmt.Errorf("JIRA API authentication failed (401) for large file (%d MB) - API token may have expired during upload. Try splitting into smaller files or refresh credentials", fileInfo.Size()/(1024*1024))
		}
		return fmt.Errorf("JIRA API authentication failed (401) for issue %s: %s - the API token/email is invalid, revoked, or has insufficient permissions", issueKey, jiraMsg)
	}

	// 403: authenticated but not permitted (e.g. no "add attachments" permission on this project/issue)
	if resp.StatusCode == 403 {
		return fmt.Errorf("JIRA API request forbidden (403) for issue %s: %s - account %s likely lacks 'add attachments' permission on this project", issueKey, jiraMsg, jiraConfig.Email)
	}

	// 404: issue key doesn't exist, was moved/renamed, or isn't visible to this account.
	// This is Jira's deliberately vague response for both "doesn't exist" and "no permission"
	// (to avoid leaking issue existence), so it almost always means one of:
	//   1. The issue key is mistyped, or belongs to a different Atlassian site than jira.baseUrl
	//   2. The account tied to the API token lacks "Browse Projects" permission for this issue
	if resp.StatusCode == 404 {
		return fmt.Errorf("JIRA issue %s not found (404): %s - verify the issue key is correct, that it exists under %s (not a different Atlassian site), and that %s can view it in a browser", issueKey, jiraMsg, jiraConfig.BaseURL, jiraConfig.Email)
	}

	// 413: file exceeds the Jira instance's configured maximum attachment size
	if resp.StatusCode == 413 {
		fileInfo, _ := os.Stat(filePath)
		sizeMB := int64(0)
		if fileInfo != nil {
			sizeMB = fileInfo.Size() / (1024 * 1024)
		}
		return fmt.Errorf("JIRA rejected %s as too large (413, %d MB) - this exceeds the Jira instance's maximum attachment size. Ask a JIRA admin to raise the limit, or split/compress the file further", fileName, sizeMB)
	}

	// 429: Atlassian Cloud rate limit hit, common when uploading several files in parallel
	if resp.StatusCode == 429 {
		retryAfter := resp.Header.Get("Retry-After")
		return fmt.Errorf("JIRA API rate limit exceeded (429) while uploading %s (Retry-After=%s) - too many attachment requests in a short time", fileName, retryAfter)
	}

	return fmt.Errorf("JIRA API request failed with status %d: %s", resp.StatusCode, jiraMsg)
}

// extractJiraErrorMessage parses a Jira REST API error response body, which is
// typically shaped like {"errorMessages":["..."],"errors":{"field":"..."}}, and
// returns a short human-readable summary. Falls back to the raw (truncated) body
// if it isn't in the expected shape, so nothing is silently dropped.
func extractJiraErrorMessage(body []byte) string {
	var parsed struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		text := strings.TrimSpace(string(body))
		if len(text) > 300 {
			text = text[:300] + "..."
		}
		if text == "" {
			return "(empty response body)"
		}
		return text
	}

	var parts []string
	parts = append(parts, parsed.ErrorMessages...)
	for field, msg := range parsed.Errors {
		parts = append(parts, fmt.Sprintf("%s: %s", field, msg))
	}
	if len(parts) == 0 {
		return "(no error details returned)"
	}
	return strings.Join(parts, "; ")
}

// ============================================================================
// Network Device Log Collection Functions
// ============================================================================

// DeviceSession represents an active SSH session to a network device with
// interactive shell support for sending CLI commands
type DeviceSession struct {
	Client   *ssh.Client
	Session  *ssh.Session
	Stdin    io.WriteCloser
	Stdout   *bufio.Reader
	Device   NetworkDevice
	Logger   *Logger
	byteChan chan byte  // Single reader goroutine sends bytes here
	errChan  chan error // Reader goroutine sends errors here
}

// startReader launches a single goroutine that continuously reads from Stdout
// and sends bytes through byteChan. This prevents goroutine leaks from
// multiple concurrent ReadByte() callers.
func (ds *DeviceSession) startReader() {
	ds.byteChan = make(chan byte, 65536)
	ds.errChan = make(chan error, 1)
	go func() {
		for {
			b, err := ds.Stdout.ReadByte()
			if err != nil {
				ds.errChan <- err
				return
			}
			ds.byteChan <- b
		}
	}()
}

// connectToExosDevice establishes an SSH connection to an EXOS switch
// using password authentication and returns a connected DeviceSession
func connectToExosDevice(device NetworkDevice, logger *Logger) (*DeviceSession, error) {
	logger.Info("Connecting to EXOS device '%s' at %s:%d...", device.Name, device.IPAddress, device.Port)

	port := device.Port
	if port == 0 {
		port = 22
	}

	config := &ssh.ClientConfig{
		User: device.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(device.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
		Config: ssh.Config{
			Ciphers: []string{
				"aes128-ctr", "aes192-ctr", "aes256-ctr",
				"aes128-gcm@openssh.com", "aes256-gcm@openssh.com",
			},
		},
	}

	addr := net.JoinHostPort(device.IPAddress, strconv.Itoa(port))

	// Establish TCP connection with timeout
	tcpConn, err := net.DialTimeout("tcp", addr, config.Timeout)
	if err != nil {
		return nil, fmt.Errorf("TCP connection to %s failed: %v", addr, err)
	}

	// Optimize TCP connection
	if tcp, ok := tcpConn.(*net.TCPConn); ok {
		tcp.SetKeepAlive(true)
		tcp.SetKeepAlivePeriod(30 * time.Second)
		tcp.SetNoDelay(true)
	}

	// Create SSH connection
	clientConn, chans, reqs, err := ssh.NewClientConn(tcpConn, addr, config)
	if err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("SSH handshake with %s failed: %v", device.Name, err)
	}

	client := ssh.NewClient(clientConn, chans, reqs)
	logger.Info("SSH connection established to '%s' (%s)", device.Name, device.IPAddress)

	// Open an interactive session with PTY for CLI interaction
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to create SSH session on %s: %v", device.Name, err)
	}

	// Request a PTY (pseudo-terminal) for interactive CLI
	modes := ssh.TerminalModes{
		ssh.ECHO:          0,     // Disable echo
		ssh.TTY_OP_ISPEED: 14400, // Input speed
		ssh.TTY_OP_OSPEED: 14400, // Output speed
	}
	if err := session.RequestPty("xterm", 80, 200, modes); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("PTY request failed on %s: %v", device.Name, err)
	}

	// Set up stdin/stdout pipes for interactive communication
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("failed to create stdin pipe on %s: %v", device.Name, err)
	}

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("failed to create stdout pipe on %s: %v", device.Name, err)
	}
	stdout := bufio.NewReaderSize(stdoutPipe, 65536)

	// Start interactive shell
	if err := session.Shell(); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("failed to start shell on %s: %v", device.Name, err)
	}

	ds := &DeviceSession{
		Client:  client,
		Session: session,
		Stdin:   stdin,
		Stdout:  stdout,
		Device:  device,
		Logger:  logger,
	}

	// Start the single reader goroutine — all reads go through byteChan
	ds.startReader()

	// Wait for initial prompt
	if err := ds.waitForPrompt(15 * time.Second); err != nil {
		ds.Close()
		return nil, fmt.Errorf("timed out waiting for initial prompt on %s: %v", device.Name, err)
	}

	logger.Info("Interactive session ready on '%s'", device.Name)
	return ds, nil
}

// ansiRegex strips ANSI escape sequences (colors, cursor movement, etc.)
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\].*?\x07|\x1b\[[\?]?[0-9;]*[hlm]`)

// stripAnsiCodes removes ANSI escape sequences from a string
func stripAnsiCodes(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// exosPromptRegex matches EXOS CLI prompts like:
//
//	"DeviceName.1 # "
//	"* DeviceName.2 # "
//	"DeviceName # "
//	"DeviceName.1 >"
//	"* (CIT_33.6.0.289) DeviceName.1 # "   ← firmware version prefix
var exosPromptRegex = regexp.MustCompile(`(?m)^[\*\s]*(?:\([^\)]*\)\s+)?[\w][\w\-\.]*\s*[#>]\s*$`)

// vossPromptRegex matches VOSS CLI prompts like:
//
//	"Man-flo-0035:1>"        ← normal mode
//	"Man-flo-0035:1#"        ← enable mode
//	"Man-flo-0035:1(config)#" ← config mode
//	"Man-flo-0035:1(config-if)#" ← interface config mode
var vossPromptRegex = regexp.MustCompile(`(?m)^[\w][\w\-\.]*:\d+(?:\([^\)]*\))?[#>]\s*$`)

// isDevicePrompt checks if the buffered output ends with a CLI prompt for the given device type
func isDevicePrompt(output string, deviceType string) bool {
	// Strip ANSI escape codes and carriage returns before checking
	cleaned := stripAnsiCodes(output)
	cleaned = strings.ReplaceAll(cleaned, "\r", "")

	lines := strings.Split(cleaned, "\n")
	if len(lines) == 0 {
		return false
	}
	lastLine := lines[len(lines)-1]
	if strings.TrimSpace(lastLine) == "" && len(lines) > 1 {
		lastLine = lines[len(lines)-2]
	}

	switch strings.ToLower(deviceType) {
	case "voss":
		return vossPromptRegex.MatchString(lastLine)
	default: // exos
		return exosPromptRegex.MatchString(lastLine)
	}
}

// isExosPrompt checks if the buffered output ends with an EXOS CLI prompt
func isExosPrompt(output string) bool {
	return isDevicePrompt(output, "exos")
}

// getPromptRegex returns the prompt regex for the given device type
func getPromptRegex(deviceType string) *regexp.Regexp {
	switch strings.ToLower(deviceType) {
	case "voss":
		return vossPromptRegex
	default:
		return exosPromptRegex
	}
}

// waitForPrompt reads output until a CLI prompt pattern is detected
// EXOS prompts typically end with "# " or "> " possibly preceded by the device name
func (ds *DeviceSession) waitForPrompt(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var buf bytes.Buffer
	lastLogLen := 0

	for {
		// Periodic debug logging every 256 bytes
		if buf.Len()-lastLogLen >= 256 {
			ds.Logger.Debug("waitForPrompt: received %d bytes so far", buf.Len())
			lastLogLen = buf.Len()
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timeout after %v waiting for prompt (received %d bytes: %q)",
				timeout, buf.Len(), truncateString(buf.String(), 200))
		}

		select {
		case b := <-ds.byteChan:
			buf.WriteByte(b)
			if isDevicePrompt(buf.String(), ds.Device.Type) {
				ds.Logger.Debug("Detected prompt after %d bytes", buf.Len())
				return nil
			}
		case err := <-ds.errChan:
			if err == io.EOF {
				return fmt.Errorf("connection closed while waiting for prompt")
			}
			return err
		case <-time.After(remaining):
			return fmt.Errorf("timeout waiting for prompt (received %d bytes)", buf.Len())
		}
	}
}

// drainBuffer reads and discards any pending data in byteChan.
// Non-blocking: only drains bytes already available in the channel.
func (ds *DeviceSession) drainBuffer() {
	drained := 0
	for {
		select {
		case <-ds.byteChan:
			drained++
		default:
			if drained > 0 {
				ds.Logger.Debug("drainBuffer: discarded %d bytes", drained)
			}
			return
		}
	}
}

// sendCommand sends a CLI command to the device and reads the response until
// the next prompt appears. Uses a settling delay to avoid false prompt detection.
// Returns the command output (excluding the echoed command and trailing prompt).
func (ds *DeviceSession) sendCommand(command string, timeout time.Duration) (string, error) {
	ds.Logger.Debug("Sending command to '%s': %s", ds.Device.Name, command)

	// Step 1: Drain any pending output from previous commands
	ds.drainBuffer()

	// Step 2: Send the command
	_, err := fmt.Fprintf(ds.Stdin, "%s\n", command)
	if err != nil {
		return "", fmt.Errorf("failed to send command '%s': %v", command, err)
	}

	// Step 3: Read response until prompt, with settling delay to avoid false positives
	var output bytes.Buffer
	deadline := time.Now().Add(timeout)
	settleDelay := 500 * time.Millisecond

	for {
		// Check for prompt BEFORE waiting for new data (handles the case where
		// settling was interrupted by trailing bytes that complete a valid prompt)
		if output.Len() > 0 && isDevicePrompt(output.String(), ds.Device.Type) {
			// Settling: wait to make sure no more data is coming.
			settleDeadline := time.Now().Add(settleDelay)
			settled := true
			for time.Now().Before(settleDeadline) {
				settleRemaining := time.Until(settleDeadline)
				if settleRemaining <= 0 {
					break
				}
				select {
				case mb := <-ds.byteChan:
					output.WriteByte(mb)
					settled = false
				case <-time.After(settleRemaining):
					// No more data within settle period — it's a real prompt
				}
				if !settled {
					break
				}
			}

			if settled {
				result := cleanCommandOutput(output.String(), command, ds.Device.Type)
				ds.Logger.Debug("Command '%s' completed (%d bytes output)", command, len(result))
				return result, nil
			}
			// Not settled — more data arrived; loop back to re-check prompt
			continue
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			// ================================================================
			// TIMEOUT RECOVERY: Try Ctrl+C to interrupt the stuck command
			// ================================================================
			ds.Logger.Warn("Command '%s' timed out after %v, sending Ctrl+C to interrupt...", command, timeout)

			recoveredOutput := output.String()
			if ds.recoverWithCtrlC() {
				ds.Logger.Info("Session recovered after Ctrl+C for command '%s'", command)
				return recoveredOutput, fmt.Errorf("command '%s' timed out after %v (session recovered via Ctrl+C, collected %d bytes)",
					command, timeout, len(recoveredOutput))
			}

			// Ctrl+C didn't work — session is unrecoverable
			ds.Logger.Error("Session unrecoverable after Ctrl+C timeout for command '%s', session will be closed", command)
			return recoveredOutput, fmt.Errorf("SESSION_UNRECOVERABLE: command '%s' timed out and Ctrl+C failed (collected %d bytes)",
				command, len(recoveredOutput))
		}

		select {
		case b := <-ds.byteChan:
			output.WriteByte(b)

		case readErr := <-ds.errChan:
			if readErr == io.EOF {
				return output.String(), fmt.Errorf("connection closed during command '%s'", command)
			}
			return output.String(), readErr

		case <-time.After(remaining):
			// Will be handled by the remaining <= 0 check on next iteration
			ds.Logger.Warn("Command '%s' timed out after %v, sending Ctrl+C to interrupt...", command, timeout)

			recoveredOutput := output.String()
			if ds.recoverWithCtrlC() {
				ds.Logger.Info("Session recovered after Ctrl+C for command '%s'", command)
				return recoveredOutput, fmt.Errorf("command '%s' timed out after %v (session recovered via Ctrl+C, collected %d bytes)",
					command, timeout, len(recoveredOutput))
			}

			ds.Logger.Error("Session unrecoverable after Ctrl+C timeout for command '%s', session will be closed", command)
			return recoveredOutput, fmt.Errorf("SESSION_UNRECOVERABLE: command '%s' timed out and Ctrl+C failed (collected %d bytes)",
				command, len(recoveredOutput))
		}
	}
}

// recoverWithCtrlC sends Ctrl+C (0x03) to interrupt a stuck command and waits
// up to 10 seconds for the device prompt to reappear. Returns true if the
// session was recovered (prompt detected), false if the session is unrecoverable.
func (ds *DeviceSession) recoverWithCtrlC() bool {
	ctrlCRecoveryTimeout := 10 * time.Second

	// Send Ctrl+C (ETX, byte 0x03)
	_, err := ds.Stdin.Write([]byte{0x03})
	if err != nil {
		ds.Logger.Error("Failed to send Ctrl+C to '%s': %v", ds.Device.Name, err)
		return false
	}
	ds.Logger.Debug("Sent Ctrl+C to '%s', waiting up to %v for prompt...", ds.Device.Name, ctrlCRecoveryTimeout)

	// Wait for prompt to reappear
	var recovery bytes.Buffer
	deadline := time.Now().Add(ctrlCRecoveryTimeout)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			ds.Logger.Error("Prompt not detected within %v after Ctrl+C on '%s'", ctrlCRecoveryTimeout, ds.Device.Name)
			return false
		}

		select {
		case b := <-ds.byteChan:
			recovery.WriteByte(b)
			if isDevicePrompt(recovery.String(), ds.Device.Type) {
				// Wait a brief settle period to confirm
				settleDeadline := time.Now().Add(300 * time.Millisecond)
				settled := true
				for time.Now().Before(settleDeadline) {
					sr := time.Until(settleDeadline)
					if sr <= 0 {
						break
					}
					select {
					case mb := <-ds.byteChan:
						recovery.WriteByte(mb)
						settled = false
					case <-time.After(sr):
					}
					if !settled {
						// More data came, re-check if still a prompt
						if !isDevicePrompt(recovery.String(), ds.Device.Type) {
							settled = true // reset and continue main loop
						}
						break
					}
				}
				if settled && isDevicePrompt(recovery.String(), ds.Device.Type) {
					ds.Logger.Debug("Prompt detected after Ctrl+C on '%s'", ds.Device.Name)
					return true
				}
			}

		case readErr := <-ds.errChan:
			ds.Logger.Error("Connection error after Ctrl+C on '%s': %v", ds.Device.Name, readErr)
			return false

		case <-time.After(remaining):
			ds.Logger.Error("Prompt not detected within %v after Ctrl+C on '%s'", ctrlCRecoveryTimeout, ds.Device.Name)
			return false
		}
	}
}

// cleanCommandOutput removes the echoed command from the beginning and the
// trailing prompt from the output, returning just the command results
func cleanCommandOutput(raw, command, deviceType string) string {
	// Strip ANSI codes and carriage returns for clean parsing
	cleaned := stripAnsiCodes(raw)
	cleaned = strings.ReplaceAll(cleaned, "\r", "")
	lines := strings.Split(cleaned, "\n")

	// Find start index: skip the echoed command line
	startIdx := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == command || strings.Contains(trimmed, command) {
			startIdx = i + 1
			break
		}
	}

	// Find end index: remove trailing prompt line(s)
	promptRegex := getPromptRegex(deviceType)
	endIdx := len(lines)
	for i := len(lines) - 1; i >= startIdx; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		// Use regex for prompt detection instead of loose suffix matching
		if promptRegex.MatchString(lines[i]) {
			endIdx = i
			continue
		}
		break
	}

	if startIdx >= endIdx {
		return ""
	}

	return strings.Join(lines[startIdx:endIdx], "\n")
}

// disableExosPaging sends the "disable clipaging" command to prevent paged output
func (ds *DeviceSession) disableExosPaging(pagingCommand string) error {
	ds.Logger.Info("Disabling CLI paging on '%s'...", ds.Device.Name)

	if pagingCommand == "" {
		pagingCommand = "disable clipaging"
	}

	_, err := ds.sendCommand(pagingCommand, 10*time.Second)
	if err != nil {
		return fmt.Errorf("failed to disable paging on %s: %v", ds.Device.Name, err)
	}

	ds.Logger.Info("CLI paging disabled on '%s'", ds.Device.Name)
	return nil
}

// collectExosDiagnostics executes diagnostic commands on an EXOS device and writes
// the combined output to a single file with headers, separators, and a summary footer.
// Each command's output is separated by a clear header showing the command name and timestamp.
// On timeout, the timeout status is logged in the file and execution continues to next command.
func collectExosDiagnostics(ds *DeviceSession, device NetworkDevice, dlc DeviceLogCollection, outputDir string) error {
	logger := ds.Logger

	// Determine which commands to run
	var commands []DeviceCommand
	if device.Diagnostics.UseDefaults {
		// Use hardcoded default EXOS commands
		commands = append(commands, getDefaultExosCommands()...)
	}
	if len(device.Diagnostics.AdditionalCommands) > 0 {
		commands = append(commands, device.Diagnostics.AdditionalCommands...)
	}

	if len(commands) == 0 {
		logger.Warn("No diagnostic commands configured for device '%s'", device.Name)
		return nil
	}

	logger.Info("Collecting diagnostics from '%s': %d commands to execute", device.Name, len(commands))

	// Create output file
	diagFileName := fmt.Sprintf("%s_diagnostics_%s.txt", device.Name, time.Now().Format("20060102_150405"))
	diagFilePath := filepath.Join(outputDir, diagFileName)

	file, err := os.Create(diagFilePath)
	if err != nil {
		return fmt.Errorf("failed to create diagnostics file %s: %v", diagFilePath, err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	defer writer.Flush()

	commandTimeout := time.Duration(dlc.CLISettings.CommandTimeout) * time.Second
	if commandTimeout == 0 {
		commandTimeout = 180 * time.Second
	}
	commandDelay := time.Duration(dlc.CLISettings.CommandDelay) * time.Second

	// Write file header
	fmt.Fprintf(writer, "================================================================================\n")
	fmt.Fprintf(writer, "  EXOS Device Diagnostics Report\n")
	fmt.Fprintf(writer, "================================================================================\n")
	fmt.Fprintf(writer, "  Device Name:    %s\n", device.Name)
	fmt.Fprintf(writer, "  Device IP:      %s\n", device.IPAddress)
	fmt.Fprintf(writer, "  Device Type:    %s\n", device.Type)
	fmt.Fprintf(writer, "  Collection Time: %s\n", time.Now().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(writer, "  Total Commands:  %d\n", len(commands))
	fmt.Fprintf(writer, "  Command Timeout: %v\n", commandTimeout)
	fmt.Fprintf(writer, "================================================================================\n\n")

	// Execute each command
	successCount := 0
	failCount := 0
	timeoutCount := 0
	skippedCount := 0
	sessionDead := false

	for i, cmd := range commands {
		// If session is unrecoverable, skip remaining commands
		if sessionDead {
			skippedCount++
			logger.Warn("[%d/%d] Skipping '%s' — session unrecoverable", i+1, len(commands), cmd.Name)
			fmt.Fprintf(writer, "--------------------------------------------------------------------------------\n")
			fmt.Fprintf(writer, "  Command %d/%d: %s\n", i+1, len(commands), cmd.Name)
			fmt.Fprintf(writer, "  *** SKIPPED — session unrecoverable after previous Ctrl+C timeout ***\n")
			fmt.Fprintf(writer, "--------------------------------------------------------------------------------\n\n")
			writer.Flush()
			continue
		}

		cmdStartTime := time.Now()

		logger.Info("[%d/%d] Executing: %s (%s)", i+1, len(commands), cmd.Name, cmd.Command)

		// Write command header in output file
		fmt.Fprintf(writer, "--------------------------------------------------------------------------------\n")
		fmt.Fprintf(writer, "  Command %d/%d: %s\n", i+1, len(commands), cmd.Name)
		fmt.Fprintf(writer, "  Command:     %s\n", cmd.Command)
		if cmd.Description != "" {
			fmt.Fprintf(writer, "  Description: %s\n", cmd.Description)
		}
		fmt.Fprintf(writer, "  Start Time:  %s\n", cmdStartTime.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(writer, "--------------------------------------------------------------------------------\n\n")

		// Send command and collect output
		output, err := ds.sendCommand(cmd.Command, commandTimeout)
		cmdDuration := time.Since(cmdStartTime)

		if err != nil {
			if strings.Contains(err.Error(), "SESSION_UNRECOVERABLE") {
				timeoutCount++
				sessionDead = true
				logger.Error("Command '%s' caused unrecoverable session — Ctrl+C failed, skipping remaining commands", cmd.Name)
				fmt.Fprintf(writer, "*** COMMAND TIMED OUT — SESSION UNRECOVERABLE (Ctrl+C failed) ***\n")
				fmt.Fprintf(writer, "Partial output collected (%d bytes):\n\n", len(output))
			} else if strings.Contains(err.Error(), "timed out") {
				timeoutCount++
				logger.Warn("Command '%s' timed out after %v (session recovered via Ctrl+C)", cmd.Name, commandTimeout)
				fmt.Fprintf(writer, "*** COMMAND TIMED OUT after %v (recovered via Ctrl+C) ***\n", commandTimeout)
				fmt.Fprintf(writer, "Partial output collected (%d bytes):\n\n", len(output))
			} else {
				failCount++
				logger.Error("Command '%s' failed: %v", cmd.Name, err)
				fmt.Fprintf(writer, "*** COMMAND FAILED: %v ***\n\n", err)
			}
		} else {
			successCount++
		}

		// Write command output
		if output != "" {
			fmt.Fprintf(writer, "%s\n", output)
		}

		// Write command footer
		fmt.Fprintf(writer, "\n  [Duration: %v | Status: ", cmdDuration)
		if err == nil {
			fmt.Fprintf(writer, "SUCCESS")
		} else if strings.Contains(err.Error(), "SESSION_UNRECOVERABLE") {
			fmt.Fprintf(writer, "UNRECOVERABLE")
		} else if strings.Contains(err.Error(), "timed out") {
			fmt.Fprintf(writer, "TIMEOUT (recovered)")
		} else {
			fmt.Fprintf(writer, "FAILED")
		}
		fmt.Fprintf(writer, "]\n\n")

		// Flush after each command to preserve data on failure
		writer.Flush()

		// Delay between commands (skip after last command)
		if i < len(commands)-1 && commandDelay > 0 && !sessionDead {
			time.Sleep(commandDelay)
		}
	}

	// Write summary footer
	fmt.Fprintf(writer, "\n================================================================================\n")
	fmt.Fprintf(writer, "  Summary\n")
	fmt.Fprintf(writer, "================================================================================\n")
	fmt.Fprintf(writer, "  Total Commands:  %d\n", len(commands))
	fmt.Fprintf(writer, "  Successful:      %d\n", successCount)
	fmt.Fprintf(writer, "  Failed:          %d\n", failCount)
	fmt.Fprintf(writer, "  Timed Out:       %d\n", timeoutCount)
	if skippedCount > 0 {
		fmt.Fprintf(writer, "  Skipped:         %d (session unrecoverable)\n", skippedCount)
	}
	fmt.Fprintf(writer, "  Completed:       %s\n", time.Now().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(writer, "================================================================================\n")

	logger.Info("Diagnostics saved to: %s", diagFilePath)
	logger.Info("Results: %d succeeded, %d failed, %d timed out", successCount, failCount, timeoutCount)
	if skippedCount > 0 {
		logger.Warn("%d commands skipped due to unrecoverable session", skippedCount)
	}

	if sessionDead {
		return fmt.Errorf("SESSION_UNRECOVERABLE: diagnostics aborted after Ctrl+C timeout")
	}

	return nil
}

// collectExosLogFiles downloads log files from an EXOS device using SFTP.
// It first attempts to use the configured compression command to create a tar.gz
// archive on the device, then downloads it. If compression fails, it auto-falls
// back to downloading individual log files specified in fallbackFiles.
func collectExosLogFiles(ds *DeviceSession, device NetworkDevice, dlc DeviceLogCollection, outputDir string) error {
	logger := ds.Logger
	logConfig := device.Logs

	logger.Info("Starting log file collection from '%s'...", device.Name)

	// Try compressed download first if enabled
	if logConfig.CompressionEnabled && logConfig.CompressionCommand != "" {
		logger.Info("Attempting compressed log collection...")

		// Execute compression command on device via CLI
		cmdTimeout := time.Duration(dlc.CLISettings.CommandTimeout) * time.Second
		if cmdTimeout == 0 {
			cmdTimeout = 180 * time.Second
		}

		output, err := ds.sendCommand(logConfig.CompressionCommand, cmdTimeout)
		if err != nil {
			logger.Warn("Compression command failed on '%s': %v", device.Name, err)
			logger.Warn("Output: %s", truncateString(output, 200))
			logger.Info("Falling back to individual file download...")
		} else {
			logger.Debug("Compression command output: %s", truncateString(output, 200))

			// Download compressed file via SFTP
			remotePath := logConfig.CompressedFilePath
			// Include device IP in filename for unique JIRA attachments across devices
			deviceIP := strings.ReplaceAll(device.IPAddress, ".", "_")
			remoteBase := filepath.Base(remotePath)
			ext := filepath.Ext(remoteBase)                                          // .gz
			nameWithoutExt := strings.TrimSuffix(remoteBase, ext)                    // nos_logs.tar
			ext2 := filepath.Ext(nameWithoutExt)                                     // .tar
			baseName := strings.TrimSuffix(nameWithoutExt, ext2)                     // nos_logs
			localFileName := fmt.Sprintf("%s_%s%s%s", baseName, deviceIP, ext2, ext) // nos_logs_10_127_34_23.tar.gz
			localPath := filepath.Join(outputDir, localFileName)

			if err := downloadFileFromDeviceSFTP(ds.Client, remotePath, localPath, logger); err != nil {
				logger.Warn("Failed to download compressed file '%s' from '%s': %v", remotePath, device.Name, err)
				logger.Info("Falling back to individual file download...")
			} else {
				logger.Info("Downloaded compressed logs: %s", localPath)

				// Cleanup compressed file on device if configured
				if logConfig.RemoveCompressedFile {
					rmCmd := fmt.Sprintf("run script shell rm -f %s", remotePath)
					if _, err := ds.sendCommand(rmCmd, 10*time.Second); err != nil {
						logger.Warn("Failed to cleanup compressed file on device: %v", err)
					} else {
						logger.Debug("Cleaned up compressed file on device: %s", remotePath)
					}
				}

				return nil // Compressed download succeeded
			}
		}
	}

	// Fallback: download individual files
	if len(logConfig.FallbackFiles) == 0 {
		logger.Warn("No fallback files configured for device '%s'", device.Name)
		return fmt.Errorf("no log files to download from %s", device.Name)
	}

	logger.Info("Downloading %d individual log files from '%s'...", len(logConfig.FallbackFiles), device.Name)

	successCount := 0
	failCount := 0

	deviceIP := strings.ReplaceAll(device.IPAddress, ".", "_")
	for _, remotePath := range logConfig.FallbackFiles {
		// Include device IP in filename for unique JIRA attachments across devices
		remoteBase := filepath.Base(remotePath)
		ext := filepath.Ext(remoteBase)
		baseName := strings.TrimSuffix(remoteBase, ext)
		localFileName := fmt.Sprintf("%s_%s%s", baseName, deviceIP, ext) // agent_10_127_34_23.log
		localPath := filepath.Join(outputDir, localFileName)

		logger.Info("Downloading: %s", remotePath)
		if err := downloadFileFromDeviceSFTP(ds.Client, remotePath, localPath, logger); err != nil {
			logger.Warn("Failed to download '%s' from '%s': %v (continuing...)", remotePath, device.Name, err)
			failCount++
			continue
		}

		logger.Info("Downloaded: %s -> %s", remotePath, localPath)
		successCount++
	}

	logger.Info("Log file download complete: %d succeeded, %d failed", successCount, failCount)

	if successCount == 0 {
		return fmt.Errorf("all %d log file downloads failed from %s", failCount, device.Name)
	}

	return nil
}

// ============================================================================
// VOSS Device Support
// ============================================================================

// connectToVossDevice establishes an SSH connection to a VOSS switch
// using password authentication and returns a connected DeviceSession.
// VOSS requires: login → enable mode → config mode → disable paging
func connectToVossDevice(device NetworkDevice, logger *Logger) (*DeviceSession, error) {
	logger.Info("Connecting to VOSS device '%s' at %s:%d...", device.Name, device.IPAddress, device.Port)

	port := device.Port
	if port == 0 {
		port = 22
	}

	config := &ssh.ClientConfig{
		User: device.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(device.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
		Config: ssh.Config{
			Ciphers: []string{
				"aes128-ctr", "aes192-ctr", "aes256-ctr",
				"aes128-gcm@openssh.com", "aes256-gcm@openssh.com",
				"aes128-cbc", "aes256-cbc",
			},
		},
	}

	addr := net.JoinHostPort(device.IPAddress, strconv.Itoa(port))

	// Establish TCP connection with timeout
	tcpConn, err := net.DialTimeout("tcp", addr, config.Timeout)
	if err != nil {
		return nil, fmt.Errorf("TCP connection to %s failed: %v", addr, err)
	}

	// Optimize TCP connection
	if tcp, ok := tcpConn.(*net.TCPConn); ok {
		tcp.SetKeepAlive(true)
		tcp.SetKeepAlivePeriod(30 * time.Second)
		tcp.SetNoDelay(true)
	}

	// Create SSH connection
	clientConn, chans, reqs, err := ssh.NewClientConn(tcpConn, addr, config)
	if err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("SSH handshake with %s failed: %v", device.Name, err)
	}

	client := ssh.NewClient(clientConn, chans, reqs)
	logger.Info("SSH connection established to '%s' (%s)", device.Name, device.IPAddress)

	// Open an interactive session with PTY for CLI interaction
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to create SSH session on %s: %v", device.Name, err)
	}

	// Request a PTY (pseudo-terminal) for interactive CLI
	modes := ssh.TerminalModes{
		ssh.ECHO:          0,     // Disable echo
		ssh.TTY_OP_ISPEED: 14400, // Input speed
		ssh.TTY_OP_OSPEED: 14400, // Output speed
	}
	if err := session.RequestPty("xterm", 80, 200, modes); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("PTY request failed on %s: %v", device.Name, err)
	}

	// Set up stdin/stdout pipes for interactive communication
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("failed to create stdin pipe on %s: %v", device.Name, err)
	}

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("failed to create stdout pipe on %s: %v", device.Name, err)
	}
	stdout := bufio.NewReaderSize(stdoutPipe, 65536)

	// Start interactive shell
	if err := session.Shell(); err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("failed to start shell on %s: %v", device.Name, err)
	}

	ds := &DeviceSession{
		Client:  client,
		Session: session,
		Stdin:   stdin,
		Stdout:  stdout,
		Device:  device,
		Logger:  logger,
	}

	// Start the single reader goroutine
	ds.startReader()

	// Wait for initial prompt (VOSS shows login banner then prompt like "hostname:1>")
	if err := ds.waitForPrompt(15 * time.Second); err != nil {
		ds.Close()
		return nil, fmt.Errorf("timed out waiting for initial prompt on %s: %v", device.Name, err)
	}

	logger.Info("Interactive session ready on '%s'", device.Name)
	return ds, nil
}

// initVossSession enters enable mode and config mode, then disables paging.
// VOSS requires: enable → configure terminal → terminal more disable
// All commands are hardcoded as they are standard VOSS commands
func (ds *DeviceSession) initVossSession(dlc DeviceLogCollection) error {
	logger := ds.Logger

	// Step 1: Enter enable mode (hardcoded VOSS command)
	logger.Info("Entering enable mode on '%s'...", ds.Device.Name)
	if _, err := ds.sendCommand("enable", 10*time.Second); err != nil {
		return fmt.Errorf("failed to enter enable mode on %s: %v", ds.Device.Name, err)
	}
	logger.Info("Enable mode active on '%s'", ds.Device.Name)

	// Step 2: Enter config mode (hardcoded VOSS command)
	logger.Info("Entering config mode on '%s'...", ds.Device.Name)
	if _, err := ds.sendCommand("configure terminal", 10*time.Second); err != nil {
		return fmt.Errorf("failed to enter config mode on %s: %v", ds.Device.Name, err)
	}
	logger.Info("Config mode active on '%s'", ds.Device.Name)

	// Step 3: Disable CLI paging (hardcoded VOSS command)
	logger.Info("Disabling CLI paging on '%s'...", ds.Device.Name)
	if _, err := ds.sendCommand("terminal more disable", 10*time.Second); err != nil {
		return fmt.Errorf("failed to disable paging on %s: %v", ds.Device.Name, err)
	}
	logger.Info("CLI paging disabled on '%s'", ds.Device.Name)

	return nil
}

// collectVossDiagnostics executes diagnostic commands on a VOSS device and writes
// the combined output to a single file with headers, separators, and a summary footer.
func collectVossDiagnostics(ds *DeviceSession, device NetworkDevice, dlc DeviceLogCollection, outputDir string) error {
	logger := ds.Logger

	// Determine which commands to run
	var commands []DeviceCommand
	if device.Diagnostics.UseDefaults {
		// Use hardcoded default VOSS commands
		commands = append(commands, getDefaultVossCommands()...)
	}
	if len(device.Diagnostics.AdditionalCommands) > 0 {
		commands = append(commands, device.Diagnostics.AdditionalCommands...)
	}

	if len(commands) == 0 {
		logger.Warn("No diagnostic commands configured for VOSS device '%s'", device.Name)
		return nil
	}

	logger.Info("Collecting diagnostics from '%s': %d commands to execute", device.Name, len(commands))

	// Create output file
	diagFileName := fmt.Sprintf("%s_diagnostics_%s.txt", device.Name, time.Now().Format("20060102_150405"))
	diagFilePath := filepath.Join(outputDir, diagFileName)

	file, err := os.Create(diagFilePath)
	if err != nil {
		return fmt.Errorf("failed to create diagnostics file %s: %v", diagFilePath, err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	defer writer.Flush()

	commandTimeout := time.Duration(dlc.CLISettings.CommandTimeout) * time.Second
	if commandTimeout == 0 {
		commandTimeout = 180 * time.Second
	}
	commandDelay := time.Duration(dlc.CLISettings.CommandDelay) * time.Second

	// Write file header
	fmt.Fprintf(writer, "================================================================================\n")
	fmt.Fprintf(writer, "  VOSS Device Diagnostics Report\n")
	fmt.Fprintf(writer, "================================================================================\n")
	fmt.Fprintf(writer, "  Device Name:    %s\n", device.Name)
	fmt.Fprintf(writer, "  Device IP:      %s\n", device.IPAddress)
	fmt.Fprintf(writer, "  Device Type:    %s\n", device.Type)
	fmt.Fprintf(writer, "  Collection Time: %s\n", time.Now().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(writer, "  Total Commands:  %d\n", len(commands))
	fmt.Fprintf(writer, "  Command Timeout: %v\n", commandTimeout)
	fmt.Fprintf(writer, "================================================================================\n\n")

	// Execute each command
	successCount := 0
	failCount := 0
	timeoutCount := 0
	skippedCount := 0
	sessionDead := false

	for i, cmd := range commands {
		// If session is unrecoverable, skip remaining commands
		if sessionDead {
			skippedCount++
			logger.Warn("[%d/%d] Skipping '%s' — session unrecoverable", i+1, len(commands), cmd.Name)
			fmt.Fprintf(writer, "--------------------------------------------------------------------------------\n")
			fmt.Fprintf(writer, "  Command %d/%d: %s\n", i+1, len(commands), cmd.Name)
			fmt.Fprintf(writer, "  *** SKIPPED — session unrecoverable after previous Ctrl+C timeout ***\n")
			fmt.Fprintf(writer, "--------------------------------------------------------------------------------\n\n")
			writer.Flush()
			continue
		}

		cmdStartTime := time.Now()

		logger.Info("[%d/%d] Executing: %s (%s)", i+1, len(commands), cmd.Name, cmd.Command)

		// Write command header in output file
		fmt.Fprintf(writer, "--------------------------------------------------------------------------------\n")
		fmt.Fprintf(writer, "  Command %d/%d: %s\n", i+1, len(commands), cmd.Name)
		fmt.Fprintf(writer, "  Command:     %s\n", cmd.Command)
		if cmd.Description != "" {
			fmt.Fprintf(writer, "  Description: %s\n", cmd.Description)
		}
		fmt.Fprintf(writer, "  Start Time:  %s\n", cmdStartTime.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(writer, "--------------------------------------------------------------------------------\n\n")

		// Send command and collect output
		output, err := ds.sendCommand(cmd.Command, commandTimeout)
		cmdDuration := time.Since(cmdStartTime)

		if err != nil {
			if strings.Contains(err.Error(), "SESSION_UNRECOVERABLE") {
				timeoutCount++
				sessionDead = true
				logger.Error("Command '%s' caused unrecoverable session — Ctrl+C failed, skipping remaining commands", cmd.Name)
				fmt.Fprintf(writer, "*** COMMAND TIMED OUT — SESSION UNRECOVERABLE (Ctrl+C failed) ***\n")
				fmt.Fprintf(writer, "Partial output collected (%d bytes):\n\n", len(output))
			} else if strings.Contains(err.Error(), "timed out") {
				timeoutCount++
				logger.Warn("Command '%s' timed out after %v (session recovered via Ctrl+C)", cmd.Name, commandTimeout)
				fmt.Fprintf(writer, "*** COMMAND TIMED OUT after %v (recovered via Ctrl+C) ***\n", commandTimeout)
				fmt.Fprintf(writer, "Partial output collected (%d bytes):\n\n", len(output))
			} else {
				failCount++
				logger.Error("Command '%s' failed: %v", cmd.Name, err)
				fmt.Fprintf(writer, "*** COMMAND FAILED: %v ***\n\n", err)
			}
		} else {
			successCount++
		}

		// Write command output
		if output != "" {
			fmt.Fprintf(writer, "%s\n", output)
		}

		// Write command footer
		fmt.Fprintf(writer, "\n  [Duration: %v | Status: ", cmdDuration)
		if err == nil {
			fmt.Fprintf(writer, "SUCCESS")
		} else if strings.Contains(err.Error(), "SESSION_UNRECOVERABLE") {
			fmt.Fprintf(writer, "UNRECOVERABLE")
		} else if strings.Contains(err.Error(), "timed out") {
			fmt.Fprintf(writer, "TIMEOUT (recovered)")
		} else {
			fmt.Fprintf(writer, "FAILED")
		}
		fmt.Fprintf(writer, "]\n\n")

		// Flush after each command to preserve data on failure
		writer.Flush()

		// Delay between commands (skip after last command)
		if i < len(commands)-1 && commandDelay > 0 && !sessionDead {
			time.Sleep(commandDelay)
		}
	}

	// Write summary footer
	fmt.Fprintf(writer, "\n================================================================================\n")
	fmt.Fprintf(writer, "  Summary\n")
	fmt.Fprintf(writer, "================================================================================\n")
	fmt.Fprintf(writer, "  Total Commands:  %d\n", len(commands))
	fmt.Fprintf(writer, "  Successful:      %d\n", successCount)
	fmt.Fprintf(writer, "  Failed:          %d\n", failCount)
	fmt.Fprintf(writer, "  Timed Out:       %d\n", timeoutCount)
	if skippedCount > 0 {
		fmt.Fprintf(writer, "  Skipped:         %d (session unrecoverable)\n", skippedCount)
	}
	fmt.Fprintf(writer, "  Completed:       %s\n", time.Now().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(writer, "================================================================================\n")

	logger.Info("Diagnostics saved to: %s", diagFilePath)
	logger.Info("Results: %d succeeded, %d failed, %d timed out", successCount, failCount, timeoutCount)
	if skippedCount > 0 {
		logger.Warn("%d commands skipped due to unrecoverable session", skippedCount)
	}

	if sessionDead {
		return fmt.Errorf("SESSION_UNRECOVERABLE: diagnostics aborted after Ctrl+C timeout")
	}

	return nil
}

// collectVossLogFiles downloads log files from a VOSS device using SFTP.
// VOSS only supports one session per SSH connection, so we open a dedicated
// SSH connection for SFTP file transfers (separate from the interactive CLI session).
func collectVossLogFiles(ds *DeviceSession, device NetworkDevice, dlc DeviceLogCollection, outputDir string) error {
	logger := ds.Logger
	logConfig := device.Logs

	if !logConfig.Enabled {
		logger.Info("Log file collection disabled for VOSS device '%s'", device.Name)
		return nil
	}

	// VOSS doesn't support shell-level tar compression like EXOS, so we download individual files.
	if logConfig.CompressionEnabled {
		logger.Info("Note: VOSS devices do not support on-device compression. Downloading individual files...")
	}

	files := logConfig.FallbackFiles
	if len(files) == 0 {
		logger.Warn("No log files configured for VOSS device '%s'", device.Name)
		return fmt.Errorf("no log files to download from %s", device.Name)
	}

	logger.Info("Downloading %d log files from VOSS device '%s' via SFTP...", len(files), device.Name)

	// VOSS only supports one session per SSH client, so we need a dedicated connection for SFTP
	port := device.Port
	if port == 0 {
		port = 22
	}
	sftpConfig := &ssh.ClientConfig{
		User: device.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(device.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
		Config: ssh.Config{
			Ciphers: []string{
				"aes128-ctr", "aes192-ctr", "aes256-ctr",
				"aes128-gcm@openssh.com", "aes256-gcm@openssh.com",
				"aes128-cbc", "aes256-cbc",
			},
		},
	}
	addr := net.JoinHostPort(device.IPAddress, strconv.Itoa(port))
	sftpSSHClient, err := ssh.Dial("tcp", addr, sftpConfig)
	if err != nil {
		return fmt.Errorf("failed to open SFTP SSH connection to %s: %v", device.Name, err)
	}
	defer sftpSSHClient.Close()
	logger.Debug("Opened dedicated SFTP SSH connection to '%s'", device.Name)

	// Create a single SFTP client and reuse it for all downloads.
	// VOSS only supports one session per SSH connection, so we must not
	// close and re-create the SFTP client between files.
	sftpClient, err := sftp.NewClient(sftpSSHClient)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client for %s: %v", device.Name, err)
	}
	defer sftpClient.Close()
	logger.Debug("SFTP client ready for '%s'", device.Name)

	successCount := 0
	failCount := 0
	deviceIP := strings.ReplaceAll(device.IPAddress, ".", "_")

	for _, remotePath := range files {
		// Include device IP in filename for unique JIRA attachments
		remoteBase := filepath.Base(remotePath)
		ext := filepath.Ext(remoteBase)
		baseName := strings.TrimSuffix(remoteBase, ext)
		localFileName := fmt.Sprintf("%s_%s%s", baseName, deviceIP, ext)
		localPath := filepath.Join(outputDir, localFileName)

		logger.Info("Downloading: %s", remotePath)
		if err := downloadFileSFTPClient(sftpClient, remotePath, localPath, logger); err != nil {
			logger.Warn("Failed to download '%s' from '%s': %v (continuing...)", remotePath, device.Name, err)
			failCount++
			continue
		}

		logger.Info("Downloaded: %s -> %s", remotePath, localPath)
		successCount++
	}

	logger.Info("VOSS log file download complete: %d succeeded, %d failed", successCount, failCount)

	if successCount == 0 {
		return fmt.Errorf("all %d log file downloads failed from %s", failCount, device.Name)
	}

	return nil
}

// downloadFileFromDeviceSFTP uses SFTP to download a file directly from a network device.
// This is used for collecting log files from EXOS and VOSS switches.
// NOTE: This creates a NEW SFTP session per call. For VOSS devices (single session limit),
// use downloadFileSFTPClient with a pre-created SFTP client instead.
func downloadFileFromDeviceSFTP(client *ssh.Client, remotePath, localPath string, logger *Logger) error {
	// Create SFTP client on the existing SSH connection
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %v", err)
	}
	defer sftpClient.Close()

	return downloadFileSFTPClient(sftpClient, remotePath, localPath, logger)
}

// downloadFileSFTPClient downloads a file using an existing SFTP client.
// This allows reusing a single SFTP session for multiple file downloads,
// which is required for VOSS devices that only support one session per SSH connection.
func downloadFileSFTPClient(sftpClient *sftp.Client, remotePath, localPath string, logger *Logger) error {

	// Open remote file
	remoteFile, err := sftpClient.Open(remotePath)
	if err != nil {
		return fmt.Errorf("failed to open remote file %s: %v", remotePath, err)
	}
	defer remoteFile.Close()

	// Get file info for size
	stat, err := remoteFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat remote file %s: %v", remotePath, err)
	}

	logger.Debug("Remote file %s size: %d bytes", remotePath, stat.Size())

	// Create local file
	localFile, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file %s: %v", localPath, err)
	}
	defer localFile.Close()

	// Copy with buffered IO
	written, err := io.Copy(localFile, remoteFile)
	if err != nil {
		// Clean up partial file
		os.Remove(localPath)
		return fmt.Errorf("failed to download %s: %v", remotePath, err)
	}

	logger.Debug("Downloaded %d bytes: %s -> %s", written, remotePath, localPath)
	return nil
}

// compressDirectoryToTarGz compresses an entire directory into a single .tar.gz file.
// The archive preserves the directory structure relative to the source directory.
func compressDirectoryToTarGz(sourceDir, outputPath string, logger *Logger) error {
	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create archive file %s: %v", outputPath, err)
	}
	defer outFile.Close()

	gzWriter := gzip.NewWriter(outFile)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	baseDir := filepath.Base(sourceDir)

	err = filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Build the relative path inside the archive using the directory name as root
		relPath, err := filepath.Rel(filepath.Dir(sourceDir), path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %v", path, err)
		}

		// Use forward slashes in tar headers for cross-platform compatibility
		relPath = filepath.ToSlash(relPath)

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("failed to create tar header for %s: %v", path, err)
		}
		header.Name = relPath

		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header for %s: %v", path, err)
		}

		// Only write file contents for regular files
		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open %s: %v", path, err)
		}
		defer file.Close()

		if _, err := io.Copy(tarWriter, file); err != nil {
			return fmt.Errorf("failed to write %s to archive: %v", path, err)
		}

		return nil
	})

	if err != nil {
		// Clean up partial archive on error
		os.Remove(outputPath)
		return fmt.Errorf("failed to compress directory %s: %v", baseDir, err)
	}

	logger.Debug("Compressed directory '%s' to: %s", baseDir, outputPath)
	return nil
}

// Close cleanly shuts down the device session
func (ds *DeviceSession) Close() {
	if ds.Stdin != nil {
		// Try to send exit command gracefully
		fmt.Fprintf(ds.Stdin, "exit\n")
		ds.Stdin.Close()
	}
	if ds.Session != nil {
		ds.Session.Close()
	}
	if ds.Client != nil {
		ds.Client.Close()
	}
	ds.Logger.Debug("Session closed for device '%s'", ds.Device.Name)
}

// truncateString truncates a string to a maximum length, appending "..." if truncated
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// getDefaultExosLogConfig returns the hardcoded default log file configuration for EXOS devices
func getDefaultExosLogConfig() DeviceLogConfig {
	return DeviceLogConfig{
		Enabled:            true,
		CompressionEnabled: true,
		CompressionCommand: "run script shell tar -czf /usr/local/tmp/eciq/nos_logs.tar.gz -C /usr/local/tmp/eciq/ openapi_server.log hiveagent.log agent.log",
		CompressedFilePath: "/usr/local/tmp/eciq/nos_logs.tar.gz",
		FallbackFiles: []string{
			"/usr/local/tmp/eciq/agent.log",
			"/usr/local/tmp/eciq/hiveagent.log",
			"/usr/local/tmp/eciq/openapi_server.log",
		},
		RemoveCompressedFile: true,
		RemoveOldLogs:        false,
	}
}

// getDefaultVossLogConfig returns the hardcoded default log file configuration for VOSS devices
func getDefaultVossLogConfig() DeviceLogConfig {
	return DeviceLogConfig{
		Enabled:            true,
		CompressionEnabled: false,
		FallbackFiles: []string{
			"/intflash/openapi/openapi_server.log",
			"/intflash/config.cfg",
		},
	}
}

// getDefaultDiagnosticConfig returns the default diagnostic configuration
func getDefaultDiagnosticConfig() DeviceDiagnosticConfig {
	return DeviceDiagnosticConfig{
		Enabled:     true,
		UseDefaults: true,
	}
}

// collectDeviceLogsFromDevice connects to a single EXOS device, disables paging,
// collects diagnostics and log files within a global timeout.
func collectDeviceLogsFromDevice(device NetworkDevice, dlc DeviceLogCollection, outputDir string, logger *Logger) error {
	// Apply global timeout per device
	globalTimeout := time.Duration(dlc.GlobalTimeout) * time.Second
	if globalTimeout == 0 {
		globalTimeout = 600 * time.Second
	}

	type result struct {
		err error
	}
	resultChan := make(chan result, 1)

	go func() {
		resultChan <- result{err: collectDeviceLogsFromDeviceInner(device, dlc, outputDir, logger)}
	}()

	select {
	case res := <-resultChan:
		return res.err
	case <-time.After(globalTimeout):
		logger.Error("Device '%s' global timeout exceeded (%v)", device.Name, globalTimeout)
		return fmt.Errorf("device %s: global timeout (%v) exceeded", device.Name, globalTimeout)
	}
}

// collectDeviceLogsFromDeviceInner performs the actual device collection work
func collectDeviceLogsFromDeviceInner(device NetworkDevice, dlc DeviceLogCollection, outputDir string, logger *Logger) error {
	startTime := time.Now()

	// Apply default credentials based on device type if not specified
	deviceType := strings.ToLower(device.Type)
	if device.Port == 0 {
		device.Port = 22
	}
	switch deviceType {
	case "exos":
		if device.Username == "" {
			device.Username = "admin"
		}
		// Password defaults to "" for EXOS (no change needed since zero-value is "")
	case "voss":
		if device.Username == "" {
			device.Username = "rwa"
		}
		if device.Password == "" {
			device.Password = "rwa"
		}
	}

	// Apply default NOS log/diagnostic configs when defaultNosLogFiles is enabled
	if dlc.DefaultNosLogFiles.Enabled {
		switch deviceType {
		case "exos":
			if len(device.Logs.FallbackFiles) == 0 && device.Logs.CompressedFilePath == "" {
				device.Logs = getDefaultExosLogConfig()
				logger.Debug("Applied default EXOS log file configuration for '%s'", device.Name)
			}
		case "voss":
			if len(device.Logs.FallbackFiles) == 0 {
				device.Logs = getDefaultVossLogConfig()
				logger.Debug("Applied default VOSS log file configuration for '%s'", device.Name)
			}
		}
		// Apply default diagnostics if not explicitly configured
		if !device.Diagnostics.Enabled && !device.Diagnostics.UseDefaults {
			device.Diagnostics = getDefaultDiagnosticConfig()
			logger.Debug("Applied default diagnostic configuration for '%s'", device.Name)
		}
	}

	logger.Info("========================================")
	logger.Info("Starting collection from device: %s (%s)", device.Name, device.IPAddress)
	logger.Info("Device type: %s", device.Type)
	logger.Info("========================================")

	// Device-specific output directory (created only after successful connection)
	deviceOutputDir := filepath.Join(outputDir, device.Name)

	switch deviceType {
	case "exos":
		// Connect to EXOS device
		ds, err := connectToExosDevice(device, logger)
		if err != nil {
			return fmt.Errorf("failed to connect to device %s: %v", device.Name, err)
		}
		defer ds.Close()

		// Create output directory only after successful connection
		if err := os.MkdirAll(deviceOutputDir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory %s: %v", deviceOutputDir, err)
		}

		// Disable paging (hardcoded EXOS command)
		if err := ds.disableExosPaging("disable clipaging"); err != nil {
			return fmt.Errorf("failed to disable paging on %s: %v", device.Name, err)
		}

		// Collect diagnostics if enabled
		if device.Diagnostics.Enabled {
			if err := collectExosDiagnostics(ds, device, dlc, deviceOutputDir); err != nil {
				logger.Error("Diagnostic collection failed on '%s': %v", device.Name, err)
				if strings.Contains(err.Error(), "SESSION_UNRECOVERABLE") {
					logger.Warn("Session to '%s' is unrecoverable, skipping log file collection", device.Name)
					break
				}
			}
		} else {
			logger.Info("Diagnostic collection disabled for device '%s'", device.Name)
		}

		// Collect logs if enabled
		if device.Logs.Enabled {
			if err := collectExosLogFiles(ds, device, dlc, deviceOutputDir); err != nil {
				logger.Error("Log file collection failed on '%s': %v", device.Name, err)
			}
		} else {
			logger.Info("Log file collection disabled for device '%s'", device.Name)
		}

	case "voss":
		// Connect to VOSS device
		ds, err := connectToVossDevice(device, logger)
		if err != nil {
			return fmt.Errorf("failed to connect to device %s: %v", device.Name, err)
		}
		defer ds.Close()

		// Create output directory only after successful connection
		if err := os.MkdirAll(deviceOutputDir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory %s: %v", deviceOutputDir, err)
		}

		// Initialize VOSS session: enable → config → disable paging
		if err := ds.initVossSession(dlc); err != nil {
			return fmt.Errorf("failed to initialize VOSS session on %s: %v", device.Name, err)
		}

		// Collect diagnostics if enabled
		if device.Diagnostics.Enabled {
			if err := collectVossDiagnostics(ds, device, dlc, deviceOutputDir); err != nil {
				logger.Error("Diagnostic collection failed on '%s': %v", device.Name, err)
				if strings.Contains(err.Error(), "SESSION_UNRECOVERABLE") {
					logger.Warn("Session to '%s' is unrecoverable, skipping log file collection", device.Name)
					break
				}
			}
		} else {
			logger.Info("Diagnostic collection disabled for device '%s'", device.Name)
		}

		// Collect logs if enabled
		if device.Logs.Enabled {
			if err := collectVossLogFiles(ds, device, dlc, deviceOutputDir); err != nil {
				logger.Error("Log file collection failed on '%s': %v", device.Name, err)
			}
		} else {
			logger.Info("Log file collection disabled for device '%s'", device.Name)
		}

	default:
		logger.Warn("Device type '%s' is not supported (supported: 'exos', 'voss')", device.Type)
		return fmt.Errorf("unsupported device type: %s", device.Type)
	}

	elapsed := time.Since(startTime)
	logger.Info("Device '%s' collection completed in %v", device.Name, elapsed)
	return nil
}

// processDeviceLogCollection orchestrates the collection of logs from all
// enabled network devices, either sequentially or in parallel.
// baseOutputDir is the root output directory (e.g., C:\Logs). If empty, uses the
// configured outputDir or "DeviceLogs" as a relative path.
// timestamp is an optional shared timestamp (e.g., from the K8s archive in --all mode).
// If empty, a new timestamp is generated. Returns the final output directory path.
func processDeviceLogCollection(config Config, baseOutputDir string, timestamp string, logger *Logger) (string, error) {
	dlc := config.DeviceLogCollection

	if !dlc.Enabled {
		logger.Warn("Device log collection is disabled in config.yaml")
		return "", nil
	}

	// Count enabled devices
	enabledDevices := []NetworkDevice{}
	for _, device := range dlc.Devices {
		if device.Enabled {
			enabledDevices = append(enabledDevices, device)
		}
	}

	if len(enabledDevices) == 0 {
		logger.Warn("No enabled devices found in config.yaml deviceLogCollection.devices")
		logger.Info("Please enable at least one device (set enabled: true) to collect logs")
		return "", nil
	}

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  DEVICE LOG COLLECTION - Network device diagnostics")
	logger.Info("%s", strings.Repeat("=", 70))

	// Determine timestamp
	if timestamp == "" {
		timestamp = time.Now().Format("20060102_150405")
	}

	// Build output directory: baseOutputDir/Device_timestamp
	deviceLogFolderName := fmt.Sprintf("Device_%s", timestamp)
	var timestampDir string
	if baseOutputDir != "" {
		timestampDir = filepath.Join(baseOutputDir, deviceLogFolderName)
	} else {
		// Standalone mode: use configured outputDir or default
		outputDir := dlc.OutputDir
		if outputDir == "" {
			outputDir = "."
		}
		timestampDir = filepath.Join(outputDir, deviceLogFolderName)
	}

	logger.Info("========================================")
	logger.Info("Network Device Log Collection")
	logger.Info("Enabled devices: %d of %d", len(enabledDevices), len(dlc.Devices))
	logger.Info("Parallel downloads: %v", dlc.ParallelDownloads)
	logger.Info("Output directory: %s", timestampDir)
	logger.Info("========================================")

	if err := os.MkdirAll(timestampDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create output directory %s: %v", timestampDir, err)
	}

	var collectionErr error

	if dlc.ParallelDownloads && len(enabledDevices) > 1 {
		// Parallel collection with goroutines
		logger.Info("Processing %d devices in parallel...", len(enabledDevices))
		var wg sync.WaitGroup
		errChan := make(chan error, len(enabledDevices))

		for _, device := range enabledDevices {
			wg.Add(1)
			go func(dev NetworkDevice) {
				defer wg.Done()
				if err := collectDeviceLogsFromDevice(dev, dlc, timestampDir, logger); err != nil {
					logger.Error("Device '%s' failed: %v", dev.Name, err)
					errChan <- fmt.Errorf("device %s: %v", dev.Name, err)
				}
			}(device)
		}

		wg.Wait()
		close(errChan)

		// Collect errors
		var errors []string
		for err := range errChan {
			errors = append(errors, err.Error())
		}
		if len(errors) > 0 {
			collectionErr = fmt.Errorf("%d device(s) failed: %s", len(errors), strings.Join(errors, "; "))
		}
	} else {
		// Sequential collection
		logger.Info("Processing %d device(s) sequentially...", len(enabledDevices))
		var errors []string
		for _, device := range enabledDevices {
			if err := collectDeviceLogsFromDevice(device, dlc, timestampDir, logger); err != nil {
				logger.Error("Device '%s' failed: %v", device.Name, err)
				errors = append(errors, fmt.Errorf("device %s: %v", device.Name, err).Error())
			}
		}
		if len(errors) > 0 {
			collectionErr = fmt.Errorf("%d device(s) failed: %s", len(errors), strings.Join(errors, "; "))
		}
	}

	// Check if output directory is empty (all devices may have failed)
	// If empty, remove it to avoid compressing/uploading an empty directory
	entries, _ := os.ReadDir(timestampDir)
	if len(entries) == 0 {
		logger.Warn("Device log output directory is empty (all devices may have failed), removing: %s", timestampDir)
		os.RemoveAll(timestampDir)
		return "", collectionErr
	}

	logger.Info("========================================")
	logger.Info("Device log collection complete!")
	logger.Info("Output directory: %s", timestampDir)
	logger.Info("========================================")
	return timestampDir, collectionErr
}

// ============================================================================
// Database Query Collection Functions
// ============================================================================

// OwnerIDResolution holds the result of an owner ID lookup from accountdb.
type OwnerIDResolution struct {
	LoginName   string
	DisplayName string
	CustomerID  string
	OwnerID     string
}

// buildBashAliasQueryCommand builds a robust command that resolves a bash alias
// definition from common profile files, converts it to a concrete psql command,
// and executes the provided base64-encoded SQL.
func buildBashAliasQueryCommand(alias, encodedSQL string) string {
	return fmt.Sprintf(
		`sudo bash -c 'ALIAS_LINE=$(grep -hE "^[[:space:]]*alias[[:space:]]+%s=" /root/.bashrc /root/.bash_profile /root/.profile /root/.bash_aliases /etc/profile /etc/bashrc /etc/profile.d/* /home/*/.bashrc /home/*/.bash_profile /home/*/.profile /home/*/.bash_aliases 2>/dev/null | tail -n1); if [ -z "$ALIAS_LINE" ]; then echo "alias not found: %s" >&2; exit 127; fi; shopt -s expand_aliases; eval "$ALIAS_LINE"; SQL=$(echo %s | base64 -d); eval "%s -c \"$SQL\" --csv"'`,
		alias, alias, encodedSQL, alias,
	)
}

// buildBashAliasQueryCommandInteractive builds a command that runs in interactive
// bash mode and attempts to execute the alias after sourcing common profile files.
func buildBashAliasQueryCommandInteractive(alias, encodedSQL string) string {
	return fmt.Sprintf(
		`sudo bash -ic 'shopt -s expand_aliases; source /etc/profile 2>/dev/null; source /etc/bashrc 2>/dev/null; source /root/.bash_profile 2>/dev/null; source /root/.bashrc 2>/dev/null; source /root/.bash_aliases 2>/dev/null; SQL=$(echo %s | base64 -d); eval "%s -c \"$SQL\" --csv"'`,
		encodedSQL, alias,
	)
}

// buildBashAliasQueryCommandNonInteractive builds a command that runs in
// non-interactive mode but still sources common profile files and executes alias.
func buildBashAliasQueryCommandNonInteractive(alias, encodedSQL string) string {
	return fmt.Sprintf(
		`sudo bash -c 'shopt -s expand_aliases; source /etc/profile 2>/dev/null; source /etc/bashrc 2>/dev/null; source /root/.bash_profile 2>/dev/null; source /root/.bashrc 2>/dev/null; source /root/.bash_aliases 2>/dev/null; SQL=$(echo %s | base64 -d); eval "%s -c \"$SQL\" --csv"'`,
		encodedSQL, alias,
	)
}

// buildBashAliasCheckCommand checks whether an alias definition exists in common
// profile files on the AWS server without requiring interactive shell behavior.
func buildBashAliasCheckCommand(alias string) string {
	return fmt.Sprintf(
		`sudo bash -c 'ALIAS_LINE=$(grep -hE "^[[:space:]]*alias[[:space:]]+%s=" /root/.bashrc /root/.bash_profile /root/.profile /root/.bash_aliases /etc/profile /etc/bashrc /etc/profile.d/* /home/*/.bashrc /home/*/.bash_profile /home/*/.profile /home/*/.bash_aliases 2>/dev/null | tail -n1); if [ -n "$ALIAS_LINE" ]; then echo "ALIAS_OK"; else echo "ALIAS_NOT_FOUND"; fi'`,
		alias,
	)
}

// executeBashAliasQueryWithFallback executes SQL via bash alias using multiple
// shell strategies and returns the first successful output.
func executeBashAliasQueryWithFallback(awsClient *ssh.Client, alias, sql string, logger *Logger) (string, error) {
	encodedSQL := base64.StdEncoding.EncodeToString([]byte(sql))

	strategies := []struct {
		name    string
		command string
	}{
		{name: "alias-definition parse", command: buildBashAliasQueryCommand(alias, encodedSQL)},
		{name: "non-interactive sourced alias", command: buildBashAliasQueryCommandNonInteractive(alias, encodedSQL)},
		{name: "interactive bash alias", command: buildBashAliasQueryCommandInteractive(alias, encodedSQL)},
	}

	var errorDetails []string
	for _, strategy := range strategies {
		session, err := awsClient.NewSession()
		if err != nil {
			errorDetails = append(errorDetails, fmt.Sprintf("%s: failed to create SSH session: %v", strategy.name, err))
			continue
		}

		output, runErr := session.CombinedOutput(strategy.command)
		session.Close()

		outputStr := strings.TrimSpace(string(output))
		outputStr = sanitizeBashNoise(outputStr)
		if runErr == nil {
			logger.Debug("DB alias query succeeded using strategy: %s", strategy.name)
			return outputStr, nil
		}

		if outputStr == "" {
			outputStr = "(no stdout/stderr)"
		}
		errorDetails = append(errorDetails, fmt.Sprintf("%s: %v: %s", strategy.name, runErr, outputStr))
	}

	return "", fmt.Errorf("all alias execution strategies failed for '%s': %s", alias, strings.Join(errorDetails, " | "))
}

// sanitizeBashNoise removes known shell job-control noise lines that can appear
// when commands run in pseudo-interactive shells without a TTY.
func sanitizeBashNoise(output string) string {
	if output == "" {
		return output
	}

	var cleaned []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "bash:") && (strings.Contains(lower, "terminal process group") || strings.Contains(lower, "job control")) {
			continue
		}
		cleaned = append(cleaned, line)
	}

	return strings.Join(cleaned, "\n")
}

// resolveOwnerIDFromAccountDB queries the accountdb to resolve an owner ID
// from a user's login email. Uses a JOIN across acct_user and iam_app_owner
// to get the login name, display name, customer ID, and owner ID in one query.
func resolveOwnerIDFromAccountDB(awsClient *ssh.Client, loginEmail string, dbc DatabaseCollection, logger *Logger) (*OwnerIDResolution, error) {
	alias := "psqlaccountdb"

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  OWNER ID RESOLUTION - Looking up owner from accountdb")
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("Querying accountdb for login email: %s", loginEmail)

	// Build the join query
	sql := fmt.Sprintf(
		"SELECT au.login_name, au.display_name, au.owner_customer, iao.id AS owner_id "+
			"FROM acct_user au "+
			"JOIN iam_app_owner iao ON iao.customer_id = au.owner_customer::integer "+
			"WHERE au.login_name = '%s'", loginEmail)

	logger.Debug("Executing owner ID resolution query via %s...", alias)

	outputStr, err := executeBashAliasQueryWithFallback(awsClient, alias, sql, logger)

	if err != nil {
		return nil, fmt.Errorf("accountdb query failed: %v: %s", err, outputStr)
	}

	// Parse CSV output
	results, err := parseCSV(outputStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse accountdb results: %v", err)
	}

	if len(results) <= 1 {
		return nil, fmt.Errorf("no account found for login email '%s'", loginEmail)
	}

	// Expect: login_name, display_name, owner_customer, owner_id
	// Be robust to shell noise and duplicate header rows by selecting the first
	// non-header row that has at least 4 columns.
	var row []string
	for i := 1; i < len(results); i++ {
		candidate := results[i]
		if len(candidate) < 4 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(candidate[0]), "login_name") {
			continue
		}
		row = candidate
		break
	}
	if row == nil {
		colCount := 0
		if len(results) > 1 {
			colCount = len(results[1])
		}
		return nil, fmt.Errorf("unexpected result format from accountdb (got %d columns, expected >= 4)", colCount)
	}

	resolution := &OwnerIDResolution{
		LoginName:   strings.TrimSpace(row[0]),
		DisplayName: strings.TrimSpace(row[1]),
		CustomerID:  strings.TrimSpace(row[2]),
		OwnerID:     strings.TrimSpace(row[3]),
	}

	if resolution.OwnerID == "" {
		return nil, fmt.Errorf("owner ID resolved to empty value for login email '%s'", loginEmail)
	}

	// Print resolution results
	logger.Info("%s", strings.Repeat("-", 50))
	logger.Info("  Login ID    : %s", resolution.LoginName)
	logger.Info("  Display Name: %s", resolution.DisplayName)
	logger.Info("  Customer ID : %s", resolution.CustomerID)
	logger.Info("  Owner ID    : %s", resolution.OwnerID)
	logger.Info("%s", strings.Repeat("-", 50))
	logger.Info("Using Owner ID: %s (resolved from env_login_id)", resolution.OwnerID)
	logger.Info("%s", strings.Repeat("=", 70))

	return resolution, nil
}

// queryDeviceInfoFromDB queries the hm_device table in configdb_1 to detect
// devices belonging to a given owner_id. This is used for dynamic device detection.
// It prints the results as a table and returns the detected devices.
func queryDeviceInfoFromDB(awsClient *ssh.Client, ownerID string, dbc DatabaseCollection, logger *Logger) ([]DetectedDevice, error) {
	alias := "psqlconfigdb_1"

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  DEVICE INFO QUERY - Detecting devices from database")
	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("Querying hm_device table for owner_id = %s ...", ownerID)

	// Build and execute the query
	sql := fmt.Sprintf("SELECT serial_number, configured_host_name, device_family, software_version, agent_version, ip_address, is_connected, inlets_capable, sim_type FROM hm_device WHERE owner_id = '%s'", ownerID)

	logger.Debug("Executing device info query via %s...", alias)

	outputStr, err := executeBashAliasQueryWithFallback(awsClient, alias, sql, logger)

	if err != nil {
		return nil, fmt.Errorf("device info query failed: %v: %s", err, outputStr)
	}

	// Parse CSV output
	results, err := parseCSV(outputStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse device info results: %v", err)
	}

	if len(results) <= 1 {
		logger.Info("No devices found in hm_device for owner_id = %s", ownerID)
		return nil, nil
	}

	// Parse into DetectedDevice structs (skip header row and any duplicate header rows)
	var devices []DetectedDevice
	for i := 1; i < len(results); i++ {
		row := results[i]
		if len(row) < 9 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(row[0]), "serial_number") {
			continue
		}
		devices = append(devices, DetectedDevice{
			SerialNumber:       strings.TrimSpace(row[0]),
			ConfiguredHostName: strings.TrimSpace(row[1]),
			DeviceFamily:       strings.TrimSpace(row[2]),
			SoftwareVersion:    strings.TrimSpace(row[3]),
			AgentVersion:       strings.TrimSpace(row[4]),
			IPAddress:          strings.TrimSpace(row[5]),
			IsConnected:        strings.TrimSpace(row[6]),
			InletsCapable:      strings.TrimSpace(row[7]),
			SimType:            strings.TrimSpace(row[8]),
		})
	}

	// Print results as a formatted table
	logger.Info("")
	logger.Info("Detected %d device(s) from database:", len(devices))
	logger.Info("%-18s %-24s %-8s %-18s %-14s %-16s %-6s %-8s %-8s", "SERIAL", "HOST NAME", "FAMILY", "SW VERSION", "AGENT VERSION", "IP ADDRESS", "CONN", "INLETS", "SIM")
	logger.Info("%s %s %s %s %s %s %s %s %s", strings.Repeat("-", 18), strings.Repeat("-", 24), strings.Repeat("-", 8), strings.Repeat("-", 18), strings.Repeat("-", 14), strings.Repeat("-", 16), strings.Repeat("-", 6), strings.Repeat("-", 8), strings.Repeat("-", 8))
	for _, d := range devices {
		logger.Info("%-18s %-24s %-8s %-18s %-14s %-16s %-6s %-8s %-8s", d.SerialNumber, d.ConfiguredHostName, d.DeviceFamily, d.SoftwareVersion, d.AgentVersion, d.IPAddress, d.IsConnected, d.InletsCapable, d.SimType)
	}
	logger.Info("")

	return devices, nil
}

// normalizeDeviceFamily maps the raw hm_device.device_family value from the
// database into the internal device types recognized by the collector
// ("exos" or "voss"). The database stores hardware/product family names
// rather than always the literal type string — e.g. VOSS/Fabric Engine
// switches (like the 5520 series) report as "vsp_series" (VSP = Virtual
// Services Platform), not "voss". Pattern matching is used instead of an
// exact lookup table so other VSP/EXOS family variants are still recognized.
// Returns "" if the family is empty or doesn't match any known pattern.
func normalizeDeviceFamily(family string) string {
	f := strings.ToLower(strings.TrimSpace(family))
	if f == "" {
		return ""
	}
	switch {
	case strings.Contains(f, "vsp"), strings.Contains(f, "voss"), strings.Contains(f, "fabric"):
		return "voss"
	case strings.Contains(f, "exos"):
		return "exos"
	default:
		return ""
	}
}

// buildNetworkDevicesFromDetected converts database-detected devices into NetworkDevice
// slice suitable for device log collection, applying type-based defaults.
// maxDevices limits how many devices to process (0 = no limit).
func buildNetworkDevicesFromDetected(detected []DetectedDevice, maxDevices int, dlc DeviceLogCollection, logger *Logger) []NetworkDevice {
	var devices []NetworkDevice
	for i, d := range detected {
		if maxDevices > 0 && i >= maxDevices {
			logger.Info("Reached maxDevices limit (%d), skipping remaining %d device(s)", maxDevices, len(detected)-maxDevices)
			break
		}

		// Skip devices without IP address
		if d.IPAddress == "" {
			logger.Warn("Skipping device '%s' (serial: %s) — no IP address", d.ConfiguredHostName, d.SerialNumber)
			continue
		}

		// Map device_family to device type. hm_device.device_family stores
		// hardware/product family names, not always the literal "exos"/"voss"
		// strings — e.g. VOSS/Fabric Engine switches report as "vsp_series".
		deviceType := normalizeDeviceFamily(d.DeviceFamily)
		if deviceType == "" {
			deviceType = "exos" // Default to exos if unknown
			logger.Warn("Device '%s' has unknown family '%s', defaulting to EXOS", d.ConfiguredHostName, d.DeviceFamily)
		}

		// Determine device name (use hostname if available, otherwise serial)
		name := d.ConfiguredHostName
		if name == "" {
			name = d.SerialNumber
		}

		// Build NetworkDevice with defaults — actual log/credential defaults are applied in collectDeviceLogsFromDeviceInner
		dev := NetworkDevice{
			Name:      name,
			Type:      deviceType,
			Enabled:   true,
			IPAddress: d.IPAddress,
			// Port, Username, Password left at zero values — defaults applied later
			Diagnostics: DeviceDiagnosticConfig{
				Enabled:     true,
				UseDefaults: true,
			},
			Logs: DeviceLogConfig{
				Enabled: true,
				// Log paths will be populated by defaultNosLogFiles defaults in collectDeviceLogsFromDeviceInner
			},
		}
		devices = append(devices, dev)
		logger.Debug("Built NetworkDevice: name=%s type=%s ip=%s", dev.Name, dev.Type, dev.IPAddress)
	}
	return devices
}

// processDatabaseCollection executes database queries and saves results
func processDatabaseCollection(awsClient *ssh.Client, config Config, baseOutputDir string, timestamp string, logger *Logger) (string, error) {
	dbc := config.DatabaseCollection

	if !dbc.Enabled {
		logger.Debug("Database collection is disabled in config.yaml")
		return "", nil
	}

	// Count enabled databases
	enabledDBs := []DatabaseConfig{}
	for _, db := range dbc.Databases {
		if db.Enabled {
			enabledDBs = append(enabledDBs, db)
		}
	}

	if len(enabledDBs) == 0 {
		logger.Warn("No enabled databases found in databaseCollection.databases")
		return "", nil
	}

	logger.Info("%s", strings.Repeat("=", 70))
	logger.Info("  DATABASE COLLECTION - PostgreSQL query results")
	logger.Info("%s", strings.Repeat("=", 70))

	// Determine timestamp
	if timestamp == "" {
		timestamp = time.Now().Format("20060102_150405")
	}

	// Build output directory
	var dbOutputDir string
	if baseOutputDir != "" {
		if dbc.OutputDir != "" {
			dbOutputDir = filepath.Join(baseOutputDir, dbc.OutputDir)
		} else {
			dbOutputDir = filepath.Join(baseOutputDir, "Database")
		}
	} else {
		if dbc.OutputDir != "" {
			dbOutputDir = dbc.OutputDir
		} else {
			dbOutputDir = "Database"
		}
	}

	// Parse global parameters (support comma-separated values)
	globalParams := make(map[string][]string)
	for key, value := range dbc.Parameters {
		if value != "" {
			// Split by comma and trim whitespace
			parts := strings.Split(value, ",")
			values := []string{}
			for _, part := range parts {
				trimmed := strings.TrimSpace(part)
				if trimmed != "" {
					values = append(values, trimmed)
				}
			}
			if len(values) > 0 {
				globalParams[key] = values
			}
		}
	}

	logger.Info("========================================")
	logger.Info("Database Query Collection")
	logger.Info("Enabled databases: %d of %d", len(enabledDBs), len(dbc.Databases))
	logger.Info("Output directory: %s", dbOutputDir)

	// Log global parameters
	if len(globalParams) > 0 {
		logger.Info("Global Parameters:")
		for key, values := range globalParams {
			if len(values) == 1 {
				logger.Info("  %s: %s", key, values[0])
			} else {
				logger.Info("  %s: %s (%d values)", key, strings.Join(values, ", "), len(values))
			}
		}
	}
	logger.Info("========================================")

	// Create output directory
	if err := os.MkdirAll(dbOutputDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create database output directory: %v", err)
	}

	// Execute queries for each database
	for _, db := range enabledDBs {
		logger.Info("")
		logger.Info("Processing database: %s (alias: %s)", db.Name, db.Alias)

		// Check if alias is available (behavior depends on whether aliases are defined in config)
		if !isAliasAvailable(awsClient, db.Alias, dbc.Aliases, logger) {
			if len(dbc.Aliases) > 0 {
				logger.Warn("  Alias '%s' not available - skipping database %s", db.Alias, db.Name)
			} else {
				logger.Warn("  Bash alias '%s' not available on AWS server - skipping database %s", db.Alias, db.Name)
			}
			continue
		}

		if err := executeDatabaseQueries(awsClient, db, dbc, globalParams, dbOutputDir, timestamp, logger); err != nil {
			logger.Error("Failed to execute queries for database %s: %v", db.Name, err)
		}
	}

	logger.Info("")
	logger.Info("========================================")
	logger.Info("Database query collection complete!")
	logger.Info("Output directory: %s", dbOutputDir)
	logger.Info("========================================")

	return dbOutputDir, nil
}

// isAliasAvailable checks if an alias command is available on the AWS server
func isAliasAvailable(awsClient *ssh.Client, alias string, aliases map[string]string, logger *Logger) bool {
	if len(aliases) > 0 {
		// Config mode: Verify alias exists in config and can be resolved to a full psql command
		_, ok := aliases[alias]
		if !ok {
			logger.Debug("  Alias '%s' not found in config", alias)
			return false
		}

		// Resolve the alias chain to get the full psql command
		// e.g., psqlplatdb -> psqlrds -U postgres -d platform_common_db -> psql -h aurora-... -U postgres -d platform_common_db
		resolved := resolveAliases(alias, aliases)
		logger.Debug("  Alias '%s' resolves to: %s", alias, resolved)

		// Verify the resolved command starts with 'psql' (fully resolved)
		if !strings.HasPrefix(resolved, "psql ") && resolved != "psql" {
			logger.Debug("  Alias '%s' could not be fully resolved (got: %s)", alias, resolved)
			return false
		}

		logger.Debug("  Alias '%s' is available (resolved from config)", alias)
		return true
	} else {
		// Bash alias mode: Test if the alias exists on the AWS server
		logger.Debug("  Testing bash alias '%s' on AWS server...", alias)

		testCmd := buildBashAliasCheckCommand(alias)

		session, err := awsClient.NewSession()
		if err != nil {
			logger.Debug("  Failed to create SSH session for alias test: %v", err)
			return false
		}
		defer session.Close()

		output, err := session.CombinedOutput(testCmd)
		outputStr := strings.TrimSpace(string(output))

		// Check the result (ignore err since command may succeed but bash might print warnings)
		if strings.Contains(outputStr, "ALIAS_OK") {
			logger.Debug("  Bash alias '%s' is available on AWS server", alias)
			return true
		}

		logger.Debug("  Bash alias '%s' not found on AWS server (output: %s)", alias, outputStr)
		return false
	}
}

// executeDatabaseQueries executes all queries for a single database with automatic parameter resolution
func executeDatabaseQueries(awsClient *ssh.Client, db DatabaseConfig, dbc DatabaseCollection, globalParams map[string][]string, outputDir string, timestamp string, logger *Logger) error {
	// Create database-specific output file
	outputFile := filepath.Join(outputDir, fmt.Sprintf("%s_queries_%s.txt", db.Name, timestamp))
	f, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer f.Close()

	// Write header
	header := fmt.Sprintf("# Database Query Results: %s\n", db.Name)
	header += fmt.Sprintf("# Timestamp: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	header += fmt.Sprintf("# Alias: %s\n", db.Alias)
	header += strings.Repeat("=", 80) + "\n\n"
	f.WriteString(header)

	// Track completed queries and their results
	completedQueries := make(map[string]bool)
	queryResults := make(map[string][][]string)

	// Execute queries (with automatic ordering based on parameter availability)
	remainingQueries := append([]DatabaseQuery{}, db.Queries...) // Copy slice
	maxAttempts := len(db.Queries) * 2                           // Prevent infinite loops
	attempts := 0

	for len(remainingQueries) > 0 && attempts < maxAttempts {
		attempts++
		progressMade := false

		for i := 0; i < len(remainingQueries); {
			query := remainingQueries[i]

			// Extract required parameters from SQL
			requiredParams := extractParametersFromSQL(query.SQL)

			// Check if all required parameters are available
			canExecute, missingParams := checkParameterAvailability(requiredParams, globalParams, db.Name)

			if !canExecute {
				logger.Debug("  Query '%s' deferred - missing parameters: %v", query.Name, missingParams)
				i++ // Keep in remaining queue
				continue
			}

			// Execute query
			logger.Info("  Executing query: %s", query.Name)
			resultGroups, err := executeQueryWithGlobalParams(awsClient, db.Alias, query, globalParams, db.Name, dbc, logger)

			if err != nil {
				logger.Error("    Failed: %v", err)
				f.WriteString(fmt.Sprintf("## Query: %s\n", query.Name))
				f.WriteString(fmt.Sprintf("SQL: %s\n", query.SQL))
				f.WriteString("Status: FAILED\n")
				remainingQueries = append(remainingQueries[:i], remainingQueries[i+1:]...)
				continue
			}

			// Flatten groups into a single results slice for parameter extraction and backwards compat
			var allResults [][]string
			totalDataRows := 0
			for _, grp := range resultGroups {
				if len(grp.Rows) > 0 {
					if len(allResults) == 0 {
						allResults = append(allResults, grp.Rows[0]) // header from first group
					}
					if len(grp.Rows) > 1 {
						allResults = append(allResults, grp.Rows[1:]...)
						totalDataRows += len(grp.Rows) - 1
					}
				}
			}

			// Store results
			queryResults[query.Name] = allResults
			completedQueries[query.Name] = true
			progressMade = true

			// Write results to file
			f.WriteString(fmt.Sprintf("## Query: %s\n", query.Name))
			f.WriteString(fmt.Sprintf("SQL: %s\n", query.SQL))
			f.WriteString("Status: SUCCESS\n")
			f.WriteString(fmt.Sprintf("Total rows returned: %d\n", totalDataRows))
			f.WriteString(strings.Repeat("-", 80) + "\n")

			if len(resultGroups) == 1 && resultGroups[0].ParamName == "" {
				// Single execution (no multi-value parameter) - write flat
				grp := resultGroups[0]
				if len(grp.Rows) > 0 {
					for _, row := range grp.Rows {
						f.WriteString(fmt.Sprintf("%s\n", strings.Join(row, " | ")))
					}
				} else {
					f.WriteString("No results found.\n")
				}
			} else {
				// Multi-value execution - write grouped with labels
				// Write header once
				if len(allResults) > 0 {
					f.WriteString(fmt.Sprintf("%s\n", strings.Join(allResults[0], " | ")))
				}
				for _, grp := range resultGroups {
					f.WriteString(fmt.Sprintf("\n  --- %s = %s ---\n", grp.ParamName, grp.ParamValue))
					if len(grp.Rows) > 1 {
						for _, row := range grp.Rows[1:] {
							f.WriteString(fmt.Sprintf("%s\n", strings.Join(row, " | ")))
						}
						f.WriteString(fmt.Sprintf("  (%d row(s))\n", len(grp.Rows)-1))
					} else {
						f.WriteString("  (no rows)\n")
					}
				}
			}
			f.WriteString("\n\n")

			logger.Info("    Query completed: %d row(s) returned", totalDataRows)

			// Extract parameters from results if specified
			if len(query.Parameters) > 0 && len(allResults) > 1 {
				extractedValues := extractParametersFromResults(allResults, query.Parameters)

				// Add extracted parameters to global params with namespace
				for paramName, values := range extractedValues {
					namespacedKey := fmt.Sprintf("%s.%s", db.Name, paramName)
					globalParams[namespacedKey] = values

					// Also add without namespace if not already exists (for convenience)
					if _, exists := globalParams[paramName]; !exists {
						globalParams[paramName] = values
					}

					if len(values) == 1 {
						logger.Info("    Extracted parameter: %s = %s", namespacedKey, values[0])
					} else {
						logger.Info("    Extracted parameter: %s = %s (%d values)", namespacedKey, strings.Join(values, ", "), len(values))
					}
				}
			}

			// Remove from remaining queue
			remainingQueries = append(remainingQueries[:i], remainingQueries[i+1:]...)
		}

		// If no progress was made, we have circular dependencies or missing data
		if !progressMade {
			logger.Warn("  Unable to execute %d remaining queries - missing required parameters", len(remainingQueries))
			for _, query := range remainingQueries {
				requiredParams := extractParametersFromSQL(query.SQL)
				_, missingParams := checkParameterAvailability(requiredParams, globalParams, db.Name)
				logger.Warn("    Query '%s' requires: %v", query.Name, missingParams)
			}
			break
		}
	}

	logger.Info("  Results saved to: %s", filepath.Base(outputFile))
	return nil
}

// extractParametersFromSQL finds all {parameter} placeholders in SQL
func extractParametersFromSQL(sql string) []string {
	var params []string
	re := regexp.MustCompile(`\{([a-zA-Z0-9_.]+)\}`)
	matches := re.FindAllStringSubmatch(sql, -1)
	for _, match := range matches {
		if len(match) > 1 {
			params = append(params, match[1])
		}
	}
	return params
}

// checkParameterAvailability checks if all required parameters are available in globalParams
func checkParameterAvailability(requiredParams []string, globalParams map[string][]string, dbName string) (bool, []string) {
	var missing []string

	for _, param := range requiredParams {
		// Try exact match first
		if values, ok := globalParams[param]; ok && len(values) > 0 {
			continue
		}

		// Try with current database namespace
		namespacedParam := fmt.Sprintf("%s.%s", dbName, param)
		if values, ok := globalParams[namespacedParam]; ok && len(values) > 0 {
			continue
		}

		// Try to find it in any other database namespace
		found := false
		for key, values := range globalParams {
			if strings.HasSuffix(key, "."+param) && len(values) > 0 {
				found = true
				break
			}
		}

		if !found {
			missing = append(missing, param)
		}
	}

	return len(missing) == 0, missing
}

// executeQueryWithGlobalParams executes a query with parameter substitution from globalParams
// Returns results grouped by parameter value so the caller can label output clearly.
func executeQueryWithGlobalParams(awsClient *ssh.Client, alias string, query DatabaseQuery, globalParams map[string][]string, dbName string, dbc DatabaseCollection, logger *Logger) ([]QueryResultGroup, error) {
	// Extract required parameters from SQL
	requiredParams := extractParametersFromSQL(query.SQL)

	// Build parameter values map (resolve from globalParams)
	paramValues := make(map[string][]string)
	for _, param := range requiredParams {
		// Try exact match first
		if values, ok := globalParams[param]; ok && len(values) > 0 {
			paramValues[param] = values
			continue
		}

		// Try with current database namespace
		namespacedParam := fmt.Sprintf("%s.%s", dbName, param)
		if values, ok := globalParams[namespacedParam]; ok && len(values) > 0 {
			paramValues[param] = values
			continue
		}

		// Try to find it in any other database namespace
		for key, values := range globalParams {
			if strings.HasSuffix(key, "."+param) && len(values) > 0 {
				paramValues[param] = values
				break
			}
		}
	}

	// Find parameter with multiple values
	var multiValueParam string
	var multiValues []string
	for param, values := range paramValues {
		if len(values) > 1 {
			multiValueParam = param
			multiValues = values
			break
		}
	}

	// If we have a multi-value parameter, execute query for each value
	if multiValueParam != "" {
		logger.Debug("      Executing query %d times (one per value of '%s')", len(multiValues), multiValueParam)
		var groups []QueryResultGroup

		for _, value := range multiValues {
			// Create parameter map with this single value
			singleParams := make(map[string][]string)
			for k, v := range paramValues {
				if k == multiValueParam {
					singleParams[k] = []string{value}
				} else {
					singleParams[k] = v
				}
			}

			results, err := executeSingleQuery(awsClient, alias, query.SQL, singleParams, dbc, logger)
			if err != nil {
				logger.Warn("      Failed for %s=%s: %v", multiValueParam, value, err)
				// Still add an empty group so the output shows the failure
				groups = append(groups, QueryResultGroup{
					ParamName:  multiValueParam,
					ParamValue: value,
					Rows:       nil,
				})
				continue
			}

			groups = append(groups, QueryResultGroup{
				ParamName:  multiValueParam,
				ParamValue: value,
				Rows:       results,
			})
		}

		return groups, nil
	}

	// Single execution - return as a single group with no param label
	results, err := executeSingleQuery(awsClient, alias, query.SQL, paramValues, dbc, logger)
	if err != nil {
		return nil, err
	}
	return []QueryResultGroup{{Rows: results}}, nil
}

// extractParametersFromResults extracts specified column values from query results
func extractParametersFromResults(results [][]string, paramNames []string) map[string][]string {
	extracted := make(map[string][]string)

	if len(results) < 2 { // Need at least header + 1 data row
		return extracted
	}

	// Find the best header row (first row that looks like a column header set)
	// by checking whether it can satisfy at least one requested parameter.
	headerRowIndex := -1
	bestMatchCount := -1
	for rowIndex, row := range results {
		matchCount := 0
		for _, paramName := range paramNames {
			target := paramName
			if strings.Contains(target, ".") {
				parts := strings.Split(target, ".")
				target = parts[len(parts)-1]
			}
			for _, col := range row {
				if strings.EqualFold(strings.TrimSpace(col), target) || strings.HasSuffix(strings.ToLower(strings.TrimSpace(col)), "."+strings.ToLower(target)) {
					matchCount++
					break
				}
			}
		}

		if matchCount > bestMatchCount {
			bestMatchCount = matchCount
			headerRowIndex = rowIndex
		}
	}

	if headerRowIndex == -1 {
		return extracted
	}

	header := results[headerRowIndex]

	for _, paramName := range paramNames {
		target := paramName
		if strings.Contains(target, ".") {
			parts := strings.Split(target, ".")
			target = parts[len(parts)-1]
		}

		// Find column index
		columnIndex := -1
		for i, col := range header {
			normalizedCol := strings.TrimSpace(col)
			if strings.EqualFold(normalizedCol, target) || strings.HasSuffix(strings.ToLower(normalizedCol), "."+strings.ToLower(target)) {
				columnIndex = i
				break
			}
		}

		if columnIndex == -1 {
			continue // Column not found
		}

		// Extract values from all data rows
		values := []string{}
		for i := headerRowIndex + 1; i < len(results); i++ {
			if columnIndex < len(results[i]) {
				// Skip duplicate header rows that may appear in output
				if len(results[i]) == len(header) {
					isDuplicateHeader := true
					for j := range header {
						if !strings.EqualFold(strings.TrimSpace(results[i][j]), strings.TrimSpace(header[j])) {
							isDuplicateHeader = false
							break
						}
					}
					if isDuplicateHeader {
						continue
					}
				}

				value := strings.TrimSpace(results[i][columnIndex])
				if value != "" {
					values = append(values, value)
				}
			}
		}

		if len(values) > 0 {
			extracted[paramName] = values
		}
	}

	return extracted
}

// executeQueryWithParams executes a SQL query with parameter substitution (deprecated - keeping for compatibility)
func executeQueryWithParams(awsClient *ssh.Client, alias string, query DatabaseQuery, paramValues map[string][]string, dbc DatabaseCollection, logger *Logger) ([][]string, error) {
	// Find parameter with multiple values (dependency case)
	var multiValueParam string
	var multiValues []string
	for param, values := range paramValues {
		if len(values) > 1 {
			multiValueParam = param
			multiValues = values
			break
		}
	}

	// If we have a multi-value parameter, execute query for each value
	if multiValueParam != "" {
		logger.Debug("      Executing query %d times for each value of '%s'", len(multiValues), multiValueParam)
		var allResults [][]string
		var headerAdded bool

		for _, value := range multiValues {
			// Create parameter map with this single value
			singleParams := make(map[string][]string)
			for k, v := range paramValues {
				if k == multiValueParam {
					singleParams[k] = []string{value}
				} else {
					singleParams[k] = v
				}
			}

			results, err := executeSingleQuery(awsClient, alias, query.SQL, singleParams, dbc, logger)
			if err != nil {
				logger.Warn("      Failed for %s=%s: %v", multiValueParam, value, err)
				continue
			}

			if len(results) > 0 {
				if !headerAdded {
					allResults = append(allResults, results[0]) // Add header
					headerAdded = true
				}
				if len(results) > 1 {
					allResults = append(allResults, results[1:]...) // Add data rows
				}
			}
		}

		return allResults, nil
	}

	// Single execution
	return executeSingleQuery(awsClient, alias, query.SQL, paramValues, dbc, logger)
}

// executeSingleQuery executes a single SQL query via psql alias on the AWS server
func executeSingleQuery(awsClient *ssh.Client, alias string, sqlTemplate string, paramValues map[string][]string, dbc DatabaseCollection, logger *Logger) ([][]string, error) {
	// Substitute parameters in SQL
	sql := sqlTemplate
	for param, values := range paramValues {
		if len(values) > 0 {
			placeholder := fmt.Sprintf("{%s}", param)
			sql = strings.ReplaceAll(sql, placeholder, values[0])
		}
	}

	// Check for unsubstituted parameters
	if strings.Contains(sql, "{") && strings.Contains(sql, "}") {
		return nil, fmt.Errorf("query contains unsubstituted parameters: %s", sql)
	}

	// SECURITY: Validate SQL is SELECT-only (log collector should not modify data)
	if err := validateSQLIsSelectOnly(sql); err != nil {
		return nil, fmt.Errorf("SQL validation failed: %v", err)
	}

	// Determine whether to use config-defined aliases or bash aliases from AWS server
	// If aliases are defined in config.yaml, resolve them (legacy/custom behavior)
	// If aliases are NOT defined, use bash aliases directly from AWS server (portable across environments)
	var psqlCommand string
	var fullCommand string

	if len(dbc.Aliases) > 0 {
		// Config-defined aliases mode: Resolve alias chain from config
		_, ok := dbc.Aliases[alias]
		if !ok {
			return nil, fmt.Errorf("alias '%s' not found in config", alias)
		}

		// Resolve the alias chain from config to get the full psql command
		// e.g., psqlplatdb -> psqlrds -U postgres -d platform_common_db
		//                  -> psql -h aurora-dl2.cluster-... -U postgres -d platform_common_db
		resolvedCmd := resolveAliases(alias, dbc.Aliases)
		logger.Debug("      [Config Mode] Alias '%s' resolved to: %s", alias, resolvedCmd)

		// Escape single quotes in SQL for shell (inside single-quoted wrapper)
		escapedSQL := strings.ReplaceAll(sql, `'`, `'\''`)

		// Build the psql command with resolved connection string
		psqlCommand = fmt.Sprintf(`%s -c "%s" --csv`, resolvedCmd, escapedSQL)

		// Wrap in sudo su - -c to run as root (psql binary is in root's PATH)
		fullCommand = fmt.Sprintf(`sudo su - -c '%s'`, psqlCommand)
	} else {
		// Bash alias mode: Use bash aliases from AWS server (portable, environment-agnostic)
		logger.Debug("      [Bash Alias Mode] Using bash alias '%s' from AWS server", alias)

		psqlCommand = fmt.Sprintf(`%s -c "<query>" --csv`, alias)
	}

	logger.Debug("      Executing on AWS server (as root): %s", psqlCommand)

	// Execute command on AWS server via SSH
	session, err := awsClient.NewSession()
	if err != nil {
		logger.Debug("      Failed to create SSH session: %v", err)
		return nil, fmt.Errorf("failed to create SSH session: %v", err)
	}
	defer session.Close()

	var outputStr string
	if len(dbc.Aliases) > 0 {
		output, err := session.CombinedOutput(fullCommand)
		outputStr = string(output)
		outputStr = sanitizeBashNoise(outputStr)
		if err != nil {
			logger.Debug("      Command failed: %v", err)
			logger.Debug("      Output: %s", outputStr)
			return nil, fmt.Errorf("%v: %s", err, outputStr)
		}
	} else {
		session.Close()
		outputStr, err = executeBashAliasQueryWithFallback(awsClient, alias, sql, logger)
		if err != nil {
			logger.Debug("      Command failed: %v", err)
			logger.Debug("      Output: %s", outputStr)
			return nil, fmt.Errorf("%v: %s", err, outputStr)
		}
	}

	logger.Debug("      Query executed successfully, parsing results...")

	// Parse CSV output
	results, err := parseCSV(outputStr)
	if err != nil {
		logger.Debug("      CSV parsing failed: %v", err)
		logger.Debug("      Raw output: %s", outputStr)
		return nil, fmt.Errorf("failed to parse query results: %v", err)
	}

	logger.Debug("      Parsed %d rows", len(results))
	return results, nil
}

// validateSQLIsSelectOnly ensures the SQL query is a safe SELECT statement
func validateSQLIsSelectOnly(sql string) error {
	// Remove comments and normalize whitespace
	cleanSQL := removeCommentsAndNormalize(sql)

	if cleanSQL == "" {
		return fmt.Errorf("empty SQL query")
	}

	// Check if query starts with SELECT (case-insensitive)
	if !strings.HasPrefix(strings.ToUpper(cleanSQL), "SELECT") {
		return fmt.Errorf("only SELECT queries are allowed (query starts with: %s)", strings.ToUpper(cleanSQL[:min(20, len(cleanSQL))]))
	}

	// Check for forbidden keywords that could modify data
	forbiddenKeywords := []string{
		"INSERT", "UPDATE", "DELETE", "DROP", "CREATE", "ALTER",
		"TRUNCATE", "REPLACE", "MERGE", "GRANT", "REVOKE",
		"EXECUTE", "EXEC", "CALL", "DO",
	}

	for _, keyword := range forbiddenKeywords {
		if containsKeyword(cleanSQL, keyword) {
			return fmt.Errorf("forbidden keyword '%s' detected - only SELECT queries allowed", keyword)
		}
	}

	return nil
}

// removeCommentsAndNormalize removes SQL comments and normalizes whitespace
func removeCommentsAndNormalize(sql string) string {
	// Remove single-line comments (--)
	lines := strings.Split(sql, "\n")
	var cleaned []string
	for _, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	result := strings.Join(cleaned, " ")

	// Remove multi-line comments (/* */)
	for {
		start := strings.Index(result, "/*")
		if start == -1 {
			break
		}
		end := strings.Index(result[start:], "*/")
		if end == -1 {
			break
		}
		result = result[:start] + " " + result[start+end+2:]
	}

	// Normalize whitespace
	return strings.TrimSpace(strings.Join(strings.Fields(result), " "))
}

// containsKeyword checks if SQL contains a keyword (word boundary aware)
func containsKeyword(sql string, keyword string) bool {
	upperSQL := strings.ToUpper(sql)
	upperKeyword := strings.ToUpper(keyword)

	// Simple word boundary check
	idx := strings.Index(upperSQL, upperKeyword)
	for idx >= 0 {
		// Check if it's a whole word (not part of another word)
		start := idx
		end := idx + len(upperKeyword)

		// Check character before (should be non-alphanumeric or start of string)
		if start > 0 {
			prevChar := upperSQL[start-1]
			if (prevChar >= 'A' && prevChar <= 'Z') || (prevChar >= '0' && prevChar <= '9') || prevChar == '_' {
				idx = strings.Index(upperSQL[end:], upperKeyword)
				if idx >= 0 {
					idx += end
				}
				continue
			}
		}

		// Check character after (should be non-alphanumeric or end of string)
		if end < len(upperSQL) {
			nextChar := upperSQL[end]
			if (nextChar >= 'A' && nextChar <= 'Z') || (nextChar >= '0' && nextChar <= '9') || nextChar == '_' {
				idx = strings.Index(upperSQL[end:], upperKeyword)
				if idx >= 0 {
					idx += end
				}
				continue
			}
		}

		return true // Found as whole word
	}

	return false
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// resolveAliases recursively resolves alias definitions
func resolveAliases(command string, aliases map[string]string) string {
	// Find the first word (potential alias)
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return command
	}

	firstWord := parts[0]
	if replacement, ok := aliases[firstWord]; ok {
		// Replace the alias with its definition and keep the rest
		resolved := replacement
		if len(parts) > 1 {
			resolved += " " + strings.Join(parts[1:], " ")
		}
		// Recursively resolve in case the replacement contains more aliases
		return resolveAliases(resolved, aliases)
	}

	return command
}

// parseCSV parses CSV output from psql
func parseCSV(csvData string) ([][]string, error) {
	var results [][]string
	lines := strings.Split(csvData, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Simple CSV parsing (handles basic cases)
		fields := strings.Split(line, ",")
		for i, field := range fields {
			fields[i] = strings.TrimSpace(field)
		}
		results = append(results, fields)
	}

	// Some environments/alias wrappers can emit the CSV header row more than once.
	// Keep the first row as header and drop any duplicate header rows afterwards.
	if len(results) > 1 {
		header := results[0]
		deduped := make([][]string, 0, len(results))
		deduped = append(deduped, header)

		for i := 1; i < len(results); i++ {
			row := results[i]
			if len(row) == len(header) {
				isDuplicateHeader := true
				for j := range header {
					if !strings.EqualFold(strings.TrimSpace(row[j]), strings.TrimSpace(header[j])) {
						isDuplicateHeader = false
						break
					}
				}
				if isDuplicateHeader {
					continue
				}
			}
			deduped = append(deduped, row)
		}

		results = deduped
	}

	return results, nil
}

// extractColumnValues extracts values from a specific column in query results
func extractColumnValues(results [][]string, columnName string) []string {
	if len(results) == 0 {
		return nil
	}

	// Find the actual header row that contains this column name.
	// This avoids failures when shell noise appears before the CSV header.
	headerRowIndex := -1
	columnIndex := -1
	for rowIndex, row := range results {
		for i, col := range row {
			if strings.EqualFold(strings.TrimSpace(col), columnName) {
				headerRowIndex = rowIndex
				columnIndex = i
				break
			}
		}
		if headerRowIndex != -1 {
			break
		}
	}

	if headerRowIndex == -1 || columnIndex == -1 {
		return nil
	}

	// Extract values from rows after the detected header row.
	header := results[headerRowIndex]
	var values []string
	for i := headerRowIndex + 1; i < len(results); i++ {
		if columnIndex < len(results[i]) {
			if len(results[i]) == len(header) {
				isDuplicateHeader := true
				for j := range header {
					if !strings.EqualFold(strings.TrimSpace(results[i][j]), strings.TrimSpace(header[j])) {
						isDuplicateHeader = false
						break
					}
				}
				if isDuplicateHeader {
					continue
				}
			}

			value := strings.TrimSpace(results[i][columnIndex])
			if value != "" {
				values = append(values, value)
			}
		}
	}

	return values
}

func main() {
	// Define command-line flags
	configFile := flag.String("config", "config.yaml", "Path to configuration file")
	username := flag.String("user", "", "SSH username for bastion host")
	password := flag.String("pass", "", "SSH password for bastion host")
	bastionHost := flag.String("bastion", "", "Bastion host address (e.g., 1.2.3.4)")
	bastionPort := flag.Int("port", 0, "SSH port for bastion host")
	keyPath := flag.String("key", "", "Path to SSH key on bastion")
	awsHost := flag.String("aws", "", "AWS host to connect to")
	awsUsername := flag.String("awsuser", "", "Username for AWS host")
	logPattern := flag.String("logs", "", "Log file pattern to search for")
	outputDir := flag.String("outdir", "", "Local directory to save log files")
	interactive := flag.Bool("interactive", false, "Run in interactive mode")
	listOnly := flag.Bool("list", false, "Only list log files")

	// Define flags with default values (will be updated from config)
	autoRetry := flag.Bool("auto-retry", false, "Automatically retry failed download chunks")
	numChunks := flag.Int("num-chunks", 3, "Number of chunks for parallel downloads (max 6 for optimal performance and stability)")
	logLevel := flag.String("log-level", "INFO", "Set log level (DEBUG, INFO, WARN, ERROR)")

	// Download method flags
	useNativeSCP := flag.Bool("native-scp", false, "Use native SCP for downloads (10x faster, default)")
	useSFTP := flag.Bool("sftp", false, "Use parallel SFTP for downloads instead of native SCP")

	// Operation mode flags (mutually exclusive - only one should be used)
	modeAll := flag.Bool("all", false, "Collect logs + system info + app versions + device logs + database queries (if enabled in config)")
	modeLogs := flag.Bool("logs-only", false, "Collect only logs without system info or app versions")
	modeSysInfo := flag.Bool("sys-info", false, "Collect only general system info (kubectl commands, system stats)")
	modeVersion := flag.Bool("version", false, "Collect only application version information")
	modeDeviceLogs := flag.Bool("device-logs", false, "Collect only network device logs and diagnostics")
	modeDatabase := flag.Bool("database", false, "Collect only database query results")
	modeTemporal := flag.Bool("temporal", false, "Collect Temporal workflow + schedule data (nothing else), then run log analysis on it (status validation, flow report, HTML) if logAnalysis.enabled in config.yaml. Combine with --all to force collecting ALL activities for every workflow (ignores workflowActivitySets matching); without --all, activities follow workflowActivitySets configured in config.yaml")

	// Log collection configuration flags
	logFileName := flag.String("log-name", "", "Name for the log collection (without extension)")
	userID := flag.String("user-id", "", "User ID for log collection (defaults to bastion username)")
	timeDuration := flag.String("time-duration", "", "Collect logs from last X time (e.g., '15m', '30m', '1h', '2h'). Use '0' or 'disabled' to force full logs. Leave empty to use config setting")

	// JIRA integration flag
	jiraIssueID := flag.String("jira", "", "JIRA issue ID(s) to attach files to (e.g., XCP-17614, or a comma-separated list like XCP-1234,XCP-2345,NVO-1234). Requires jira config in config.yaml")

	// Standalone log analyzer flag
	analyzeDir := flag.String("analyze", "", "Analyze local log files/directory for errors (no SSH required). Path to directory or file")

	// AI analyzer flags
	analyzeAIDir := flag.String("analyze-ai", "", "AI-powered log analysis (launches GUI AI Analysis page). Path to directory or file")

	// Version display flag
	showVersion := flag.Bool("v", false, "Show build version and exit")

	// GUI mode flags
	guiMode := flag.Bool("gui", false, "Launch web-based GUI control panel")
	guiPort := flag.Int("gui-port", 9090, "Port for GUI web server (default: 9090)")

	// Custom help message
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Operation Modes (mutually exclusive):\n")
		fmt.Fprintf(os.Stderr, "  (no mode)          Use config.yaml settings (default)\n")
		fmt.Fprintf(os.Stderr, "  --all              Collect logs + system info + app versions + device logs (if enabled)\n")
		fmt.Fprintf(os.Stderr, "  --logs-only        Collect only logs\n")
		fmt.Fprintf(os.Stderr, "  --sys-info         Collect only general system info (kubectl commands)\n")
		fmt.Fprintf(os.Stderr, "  --version          Collect only application version information\n")
		fmt.Fprintf(os.Stderr, "  --device-logs      Collect only network device logs and diagnostics\n")
		fmt.Fprintf(os.Stderr, "  --database         Collect only database query results\n")
		fmt.Fprintf(os.Stderr, "  --temporal         Collect only Temporal workflow + schedule data, then analyze it (add --all for ALL activities)\n")
		fmt.Fprintf(os.Stderr, "  --analyze <path>   Analyze local log files/directory (no SSH required)\n")
		fmt.Fprintf(os.Stderr, "  --analyze-ai <path> AI-powered root cause analysis (launches GUI)\n")
		fmt.Fprintf(os.Stderr, "  --gui              Launch web-based GUI control panel\n\n")
		fmt.Fprintf(os.Stderr, "General Options:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	// Handle -v flag: print version and exit immediately
	if *showVersion {
		fmt.Printf("logcollector version %s (build %s) built on %s\n", appVersion, buildNumber, buildDate)
		os.Exit(0)
	}

	// Handle --gui flag: launch web-based GUI and exit
	if *guiMode {
		if err := startGUIServer(*configFile, *guiPort); err != nil {
			fmt.Fprintf(os.Stderr, "GUI server error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Handle --analyze-ai flag: launch GUI directly on AI Analysis page
	if *analyzeAIDir != "" {
		absPath, _ := filepath.Abs(*analyzeAIDir)
		fmt.Printf("AI Analysis mode: %s\n", absPath)
		fmt.Printf("Launching GUI with AI Analysis page...\n")
		os.Setenv("LOGCOLLECTOR_AI_DIR", absPath)
		if err := startGUIServer(*configFile, *guiPort); err != nil {
			fmt.Fprintf(os.Stderr, "GUI server error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Determine operation mode
	modeCount := 0
	// --all combined with --temporal is a modifier ("collect ALL activities"), not a
	// separate mode, so it doesn't count as a conflicting mode in that combination.
	if *modeAll && !*modeTemporal {
		modeCount++
	}
	if *modeLogs {
		modeCount++
	}
	if *modeSysInfo {
		modeCount++
	}
	if *modeVersion {
		modeCount++
	}
	if *modeDeviceLogs {
		modeCount++
	}
	if *modeDatabase {
		modeCount++
	}
	if *analyzeDir != "" {
		modeCount++
	}
	if *modeTemporal {
		modeCount++
	}

	if modeCount > 1 {
		fmt.Fprintln(os.Stderr, "Error: Only one operation mode can be specified at a time")
		fmt.Fprintln(os.Stderr, "Choose one of: --all, --logs-only, --sys-info, --version, --device-logs, --database, --analyze, --temporal")
		os.Exit(1)
	}

	// Validate download method flags (mutually exclusive)
	if *useNativeSCP && *useSFTP {
		fmt.Fprintln(os.Stderr, "Error: Cannot specify both --native-scp and --sftp")
		fmt.Fprintln(os.Stderr, "Choose one download method or leave unspecified to use config.yaml setting")
		os.Exit(1)
	}

	// Determine download method (priority: command-line flag > config > default)
	downloadMethod := "scp" // default to native SCP
	if *useNativeSCP {
		downloadMethod = "scp"
	} else if *useSFTP {
		downloadMethod = "sftp"
	}

	// Set defaults based on mode (or read from config if no mode specified)
	var collectLogs, collectInfo, collectAppVersions, collectDeviceLogs, collectDatabase bool
	var selectedMode string

	if *modeTemporal {
		// --temporal mode: collect only Temporal workflow + schedule data, nothing else.
		// Checked before --all so "--temporal --all" means "collect ALL activities for
		// every workflow" rather than triggering the full --all collection.
		collectLogs = false
		collectInfo = false
		collectAppVersions = false
		collectDeviceLogs = false
		collectDatabase = false
		selectedMode = "temporal"
	} else if *modeAll {
		// --all mode: override config and collect everything
		collectLogs = true
		collectInfo = true
		collectAppVersions = true
		collectDeviceLogs = true // Will respect config.DeviceLogCollection.Enabled after loading
		collectDatabase = true   // Will respect config.DatabaseCollection.Enabled after loading
		selectedMode = "all"
	} else if *modeLogs {
		// --logs-only mode: collect only logs (K8s logs, excludes device logs)
		collectLogs = true
		collectInfo = false
		collectAppVersions = false
		collectDeviceLogs = false
		collectDatabase = false
		selectedMode = "logs-only"
	} else if *modeSysInfo {
		// --sys-info mode: collect only general system info (kubectl commands)
		collectLogs = false
		collectInfo = true
		collectAppVersions = false
		collectDeviceLogs = false
		collectDatabase = false
		selectedMode = "sys-info"
	} else if *modeVersion {
		// --version mode: collect only application version information
		collectLogs = false
		collectInfo = false
		collectAppVersions = true
		collectDeviceLogs = false
		collectDatabase = false
		selectedMode = "version"
	} else if *modeDeviceLogs {
		// --device-logs mode: collect only network device logs
		collectLogs = false
		collectInfo = false
		collectAppVersions = false
		collectDeviceLogs = true
		collectDatabase = false
		selectedMode = "device-logs"
	} else if *modeDatabase {
		// --database mode: collect only database query results
		collectLogs = false
		collectInfo = false
		collectAppVersions = false
		collectDeviceLogs = false
		collectDatabase = true
		selectedMode = "database"
	} else if *analyzeDir != "" {
		// --analyze mode: standalone local log analysis (no SSH required)
		selectedMode = "analyze"
	} else if modeCount == 0 {
		// No mode specified: use config.yaml settings (default behavior)
		selectedMode = "config"
		// Will be set from config.yaml after loading
	}

	// Load configuration from file
	config, err := LoadConfig(*configFile)
	if err != nil {
		// logger is not initialized yet — use fmt.Printf to avoid nil dereference
		fmt.Printf("Warning: Failed to load config file %s: %v\n", *configFile, err)
		config = &Config{} // Use empty config if loading fails
	}
	globalTemporalAnalysisConfig = config.LogCollection.LogAnalysis.TemporalAnalysis
	// globalJiraConfig is (re)assigned below, AFTER config.Jira.Email gets its {username}/
	// {environment} template substitution - assigning it here would capture the literal
	// unsubstituted placeholder and silently break the Credential Manager token lookup.

	// If using config mode, read settings from config.yaml
	if selectedMode == "config" {
		collectLogs = config.LogCollection.Enabled
		collectInfo = config.SystemInfo.Enabled
		collectAppVersions = config.AppVersionCollection.Enabled
		collectDeviceLogs = config.DeviceLogCollection.Enabled
		collectDatabase = config.DatabaseCollection.Enabled
	}

	// In --all mode, respect feature-specific Enabled flags from config
	if selectedMode == "all" {
		collectDeviceLogs = config.DeviceLogCollection.Enabled
		collectDatabase = config.DatabaseCollection.Enabled
	}

	// Initialize archiveTimestamp early so it's available for template replacement
	config.archiveTimestamp = time.Now().Format("20060102_150405")

	// Determine and prepare outputDir BEFORE logger initialization
	// This ensures logger_info.txt is created in the correct directory from the start
	var loggerOutputDir string

	if selectedMode == "device-logs" {
		// For device-logs mode: Device_<timestamp> subdirectory
		deviceLogFolderName := fmt.Sprintf("Device_%s", config.archiveTimestamp)
		if *outputDir != "" {
			loggerOutputDir = filepath.Join(*outputDir, deviceLogFolderName)
		} else {
			baseDir := config.DeviceLogCollection.OutputDir
			if baseDir == "" {
				baseDir = "."
			}
			loggerOutputDir = filepath.Join(baseDir, deviceLogFolderName)
		}
	} else if selectedMode == "database" {
		// For database mode: resolve outputDir from config (e.g., C:\Logs\{timestamp})
		// then place database files inside the Database subdirectory
		if *outputDir == "" {
			if config.Logs.OutputDir != "" {
				*outputDir = config.Logs.OutputDir
			} else {
				*outputDir = "."
			}
		}
		// Apply template replacement to outputDir
		*outputDir = strings.ReplaceAll(*outputDir, "{timestamp}", config.archiveTimestamp)
		*outputDir = strings.ReplaceAll(*outputDir, "{username}", config.Username)
		*outputDir = strings.ReplaceAll(*outputDir, "{environment}", config.Environment)

		loggerOutputDir = *outputDir
	} else if selectedMode == "analyze" {
		// For analyze mode: logger goes to current directory (or --outdir if specified)
		if *outputDir != "" {
			loggerOutputDir = *outputDir
		} else {
			loggerOutputDir = "." // Current directory
		}
	} else {
		// For other modes (--all, --logs, --info, etc.): use outputDir from config or flag
		if *outputDir == "" {
			if config.Logs.OutputDir != "" {
				*outputDir = config.Logs.OutputDir
			} else {
				*outputDir = "." // Default to current directory
			}
		}
		// Apply template replacement to outputDir
		*outputDir = strings.ReplaceAll(*outputDir, "{timestamp}", config.archiveTimestamp)
		*outputDir = strings.ReplaceAll(*outputDir, "{username}", config.Username)
		*outputDir = strings.ReplaceAll(*outputDir, "{environment}", config.Environment)
		loggerOutputDir = *outputDir
	}

	// Create output directory before logger initialization
	if loggerOutputDir != "" && loggerOutputDir != "." {
		if err := os.MkdirAll(loggerOutputDir, 0755); err != nil {
			fmt.Printf("Warning: Could not create output directory %s: %v\n", loggerOutputDir, err)
			loggerOutputDir = "" // Fall back to PWD
		}
	}

	// Initialize the global logger in the correct output directory
	logLevelEnum := ParseLogLevel(*logLevel)
	logger = NewLogger(logLevelEnum, loggerOutputDir)
	defer logger.Close()

	// Log the selected operation mode
	logger.Debug("Selected operation mode: %s", selectedMode)

	// Apply template replacement to JIRA email field
	if config.Jira.Email != "" {
		config.Jira.Email = strings.ReplaceAll(config.Jira.Email, "{username}", config.Username)
		config.Jira.Email = strings.ReplaceAll(config.Jira.Email, "{environment}", config.Environment)
		logger.Debug("JIRA email after template replacement: %s", config.Jira.Email)
	}
	globalJiraConfig = config.Jira

	// Apply config defaults if flags weren't explicitly set
	// We check if the flag values are still at their defaults
	if *autoRetry == false && config.Options.AutoRetry {
		*autoRetry = config.Options.AutoRetry
	}
	if *numChunks == 3 && config.Options.NumChunks > 0 && config.Options.NumChunks != 3 {
		*numChunks = config.Options.NumChunks
	}
	if *logLevel == "INFO" && config.Options.LogLevel != "" && config.Options.LogLevel != "INFO" {
		*logLevel = config.Options.LogLevel
	}

	// Apply download method from config if not explicitly set via flags
	if !*useNativeSCP && !*useSFTP && config.Options.DownloadMethod != "" {
		if config.Options.DownloadMethod == "sftp" {
			downloadMethod = "sftp"
		} else {
			downloadMethod = "scp" // Default to SCP for any other value
		}
	}
	logger.Debug("Download method: %s", downloadMethod)

	// Merge log collection settings
	if *logFileName == "" && config.LogCollection.LogFileName != "" {
		*logFileName = config.LogCollection.LogFileName
	}
	if *userID == "" {
		*userID = config.Username // Use global username as default
	}

	// Default log collection settings if not specified
	if collectLogs && *logFileName == "" {
		*logFileName = "logs_nvo_hac" // Default from shell script
	}
	if collectLogs && *userID == "" {
		*userID = *username // Use bastion username as default
	}

	// Merge command line arguments with config file
	// Command line arguments take precedence over config file values
	if *username == "" {
		*username = config.Username
	}
	if *password == "" {
		*password = config.Bastion.Password
	}
	if *bastionHost == "" {
		*bastionHost = config.Bastion.Host
	}

	// Get bastion password using multi-source retrieval (env var, keychain, config, prompt)
	var decryptedPassword string
	var passwordNeedsSaving bool

	// Only retrieve password if we have both username and bastionHost (indicating SSH operations will be needed)
	if !*interactive && *username != "" && *bastionHost != "" {
		var err error
		decryptedPassword, passwordNeedsSaving, err = getBastionPassword(*username, *bastionHost, *password, logger)
		if err != nil {
			logger.Error("Failed to retrieve bastion password: %v", err)
			return
		}
		// Update password with retrieved value
		*password = decryptedPassword
	} else if *username != "" || *bastionHost != "" {
		// Legacy fallback for interactive mode or partial configuration
		// Decrypt password if it's encrypted
		if *password != "" {
			var err error
			decryptedPassword, err = decryptPassword(*password)
			if err != nil {
				logger.Warn("Failed to decrypt password: %v", err)
				logger.Warn("Please enter password again")
				decryptedPassword = ""
				*password = ""
			}
		}

		// Prompt for password if empty or decryption failed
		if decryptedPassword == "" && !*interactive {
			newPass, err := promptPassword("Enter bastion password: ")
			if err != nil {
				logger.Error("Failed to read password: %v", err)
				return
			}
			decryptedPassword = newPass
			passwordNeedsSaving = true
		}

		// Update password with decrypted value
		*password = decryptedPassword
	}
	if *bastionPort == 0 {
		if config.Bastion.Port != 0 {
			*bastionPort = config.Bastion.Port
		} else {
			*bastionPort = 22 // Default
		}
	}
	if *keyPath == "" {
		if config.AWS.KeyPath != "" {
			*keyPath = config.AWS.KeyPath
		} else {
			*keyPath = "~/.ssh/id_rsa" // Default
		}
	}
	if *awsHost == "" {
		// Replace template placeholders in AWS host
		host := config.AWS.Host
		host = strings.ReplaceAll(host, "{username}", config.Username)
		host = strings.ReplaceAll(host, "{environment}", config.Environment)
		*awsHost = host
	}
	if *awsUsername == "" {
		*awsUsername = config.Username
	}
	if *logPattern == "" {
		if config.Logs.Pattern != "" {

			// Replace template placeholders with actual values
			pattern := config.Logs.Pattern
			pattern = strings.ReplaceAll(pattern, "{username}", config.Username)
			pattern = strings.ReplaceAll(pattern, "{environment}", config.Environment)
			*logPattern = pattern
		} else {
			*logPattern = "/var/log/*.log /var/log/*.gz" // Default
		}
	}
	// outputDir already set and template-replaced during logger initialization
	// No need to set it again here
	// Apply template replacement to tempDir
	logger.Debug("Original config.LogCollection.TempDir: '%s'", config.LogCollection.TempDir)
	if config.LogCollection.TempDir != "" {
		logger.Debug("Before template replacement: TempDir = '%s'", config.LogCollection.TempDir)
		config.LogCollection.TempDir = strings.ReplaceAll(config.LogCollection.TempDir, "{username}", config.Username)
		config.LogCollection.TempDir = strings.ReplaceAll(config.LogCollection.TempDir, "{environment}", config.Environment)
		logger.Debug("After template replacement: TempDir = '%s'", config.LogCollection.TempDir)
	} else {
		logger.Debug("TempDir is empty in config")
	}
	logger.Debug("Final TempDir value passed to collectKubernetesLogs: '%s'", config.LogCollection.TempDir)

	// Interactive mode prompt for missing info
	if *interactive {
		reader := bufio.NewReader(os.Stdin)

		if *username == "" {
			fmt.Print("Enter bastion username: ")
			*username, _ = reader.ReadString('\n')
			*username = strings.TrimSpace(*username)
		}

		if *password == "" {
			fmt.Print("Enter bastion password: ")
			*password, _ = reader.ReadString('\n')
			*password = strings.TrimSpace(*password)
		}

		if *bastionHost == "" {
			fmt.Print("Enter bastion host: ")
			*bastionHost, _ = reader.ReadString('\n')
			*bastionHost = strings.TrimSpace(*bastionHost)
		}

		if *awsHost == "" {
			fmt.Print("Enter AWS host: ")
			*awsHost, _ = reader.ReadString('\n')
			*awsHost = strings.TrimSpace(*awsHost)
		}

		if *awsUsername == "" {
			fmt.Print("Enter AWS username (or leave blank to try multiple options): ")
			*awsUsername, _ = reader.ReadString('\n')
			*awsUsername = strings.TrimSpace(*awsUsername)
		}
	}

	// --analyze mode: standalone local log analysis (no SSH required)
	if selectedMode == "analyze" {
		logger.Info("Running in analyze mode (standalone local log analysis)...")
		logger.Info("")

		// Determine report output directory
		analyzeOutputDir := *outputDir
		if analyzeOutputDir == "" || analyzeOutputDir == "." {
			// Default: place report next to the analyzed path
			info, err := os.Stat(*analyzeDir)
			if err == nil && info.IsDir() {
				analyzeOutputDir = *analyzeDir
			} else {
				analyzeOutputDir = filepath.Dir(*analyzeDir)
			}
		}

		// Ensure output directory exists
		if err := os.MkdirAll(analyzeOutputDir, 0755); err != nil {
			logger.Error("Failed to create output directory %s: %v", analyzeOutputDir, err)
			return
		}

		// Use logAnalysis config from config.yaml (with overridden Enabled=true since user explicitly asked)
		analysisConfig := config.LogCollection.LogAnalysis
		analysisConfig.Enabled = true

		if err := analyzeLocalDirectory(*analyzeDir, analyzeOutputDir, analysisConfig); err != nil {
			logger.Error("Log analysis failed: %v", err)
		}
		return
	}

	// --device-logs mode: collect only network device logs and diagnostics
	// This mode does NOT require bastion/AWS — it connects directly to devices via SSH
	if selectedMode == "device-logs" {
		logger.Info("Running in device-logs mode (network device logs only)...")

		if !config.DeviceLogCollection.Enabled {
			logger.Warn("Device log collection is disabled in config.yaml (deviceLogCollection.enabled: false)")
			logger.Info("Please enable deviceLogCollection in config.yaml and configure your devices")
			return
		}

		// Logger is already created in the correct directory,
		// so we pass config.archiveTimestamp to reuse the same directory
		dlcOutDir, err := processDeviceLogCollection(*config, *outputDir, config.archiveTimestamp, logger)
		if err != nil {
			logger.Error("Device log collection failed: %v", err)
		}
		// No need to move/copy logger_info.txt - it's already in the right place!

		// Attach device log files to JIRA if requested
		if *jiraIssueID != "" && dlcOutDir != "" {
			logger.Info("")
			if !config.Jira.AttachmentEnabled {
				logger.Warn("JIRA attachment feature is disabled in config.yaml (jira.attachmentEnabled: false)")
			} else if config.Jira.Email == "" {
				logger.Warn("JIRA email not configured in config.yaml")
				logger.Info("Please configure your JIRA email in config.yaml to use the attachment feature")
				logger.Info("API token can be provided via: environment variable (JIRA_API_TOKEN), Windows Credential Manager, config.yaml, or interactive prompt")
			} else {
				// Compress entire device logs directory into a single archive for JIRA
				archiveName := filepath.Base(dlcOutDir) + ".tar.gz"
				archivePath := filepath.Join(filepath.Dir(dlcOutDir), archiveName)
				logger.Info("Compressing device logs for JIRA attachment: %s", archiveName)
				if err := compressDirectoryToTarGz(dlcOutDir, archivePath, logger); err != nil {
					logger.Error("Failed to compress device logs directory: %v", err)
				} else {
					logger.Info("Device logs compressed: %s", archivePath)
					attachmentFiles := []string{archivePath}
					if err := attachFilesToMultipleJiraIssues(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
						logger.Error("Failed to attach device log files to JIRA issue %s: %v", *jiraIssueID, err)
					} else {
						// Clean up the compressed archive after successful JIRA upload
						if err := os.Remove(archivePath); err != nil {
							logger.Warn("Failed to delete compressed archive %s: %v", archivePath, err)
						} else {
							logger.Info("Deleted compressed archive after JIRA upload: %s", archivePath)
						}
					}
				}
			}
		}
		return
	}

	// Validate required parameters
	if *username == "" || *password == "" || *bastionHost == "" {
		fmt.Println("Error: Missing required parameters")
		fmt.Println("Required: -user, -pass, -bastion")
		fmt.Println("These can be provided via command-line flags or config file")
		flag.PrintDefaults()
		return
	}

	// Validate AWS host
	if *awsHost == "" {
		fmt.Println("Error: AWS host is required")
		fmt.Println("Use -aws flag or config file to specify the AWS host")
		return
	}

	// Check if any operations are requested before attempting connections
	hasOperations := collectLogs || collectInfo || collectAppVersions || collectDeviceLogs || collectDatabase || *listOnly || selectedMode == "temporal"

	if !hasOperations {
		fmt.Println("No operations requested.")
		fmt.Println("In config mode, enable operations in config.yaml:")
		fmt.Println("  logCollection.enabled: true")
		fmt.Println("  systemInfo.enabled: true")
		fmt.Println("  appVersionCollection.enabled: true")
		fmt.Println("")
		fmt.Println("Or use operation mode flags:")
		fmt.Println("  --all        : Collect logs + system info + app versions + device logs (if enabled)")
		fmt.Println("  --logs-only  : Collect only logs")
		fmt.Println("  --sys-info   : Collect only general system info")
		fmt.Println("  --version    : Collect only app version information")
		fmt.Println("Or use -list=true to list available log files")
		return
	}

	// Print pre-flight summary of what will be collected.
	// --temporal mode doesn't map to any of the collectX booleans (it's not part of the
	// normal log-collection pipeline), so it's exempted from this generic summary/gate —
	// its own block below logs what it's about to do and validates config itself.
	if selectedMode != "temporal" {
		hasWork := printCollectionSummary(selectedMode, collectLogs, collectInfo, collectAppVersions, collectDeviceLogs, collectDatabase, *listOnly, config, logger)
		if !hasWork {
			// No actual work to do - exit without connecting
			return
		}
	}

	// Determine if bastion/AWS connection is needed
	// Device log collection connects directly to switches — no bastion/AWS required
	// UNLESS dynamic device detection is enabled (needs DB query via AWS)
	// or env_login_id is set (needs accountdb query to resolve ownerID)
	// Note: --device-logs standalone mode always uses config.yaml devices (handled before this point)
	needsAWSConnection := collectLogs || collectInfo || collectAppVersions || collectDatabase || *listOnly || selectedMode == "temporal"
	if selectedMode != "device-logs" && collectDeviceLogs && config.LogCollection.DynamicDeviceDetection.Enabled && (config.OwnerID != "" || config.EnvLoginID != "") {
		needsAWSConnection = true
	}
	if config.EnvLoginID != "" {
		needsAWSConnection = true // Need AWS to resolve ownerID from accountdb
	}

	var bastionClient *ssh.Client
	var awsClient *ssh.Client

	if needsAWSConnection {
		// Connect to bastion
		logger.Info("Connecting to bastion host %s", *bastionHost)
		bastionClient, err = sshConnectBastion(*username, *password, *bastionHost, *bastionPort)
		if err != nil {
			// Check if it's an authentication error
			if strings.Contains(err.Error(), "unable to authenticate") || strings.Contains(err.Error(), "permission denied") {
				logger.Error("Authentication failed: %v", err)
				logger.Warn("Please enter password again")

				newPass, promptErr := promptPassword("Enter bastion password: ")
				if promptErr != nil {
					logger.Error("Failed to read password: %v", promptErr)
					return
				}

				*password = newPass
				passwordNeedsSaving = true

				// Retry connection with new password
				logger.Info("Retrying connection to bastion host %s", *bastionHost)
				bastionClient, err = sshConnectBastion(*username, *password, *bastionHost, *bastionPort)
				if err != nil {
					logger.Error("Failed to connect to bastion: %v", err)
					return
				}
			} else {
				logger.Error("Failed to connect to bastion: %v", err)
				return
			}
		}
		defer bastionClient.Close()
		logger.Info("Successfully connected to bastion")

		// Save the freshly-confirmed-working password wherever it's missing/stale. This is what
		// makes a re-prompt a ONE-TIME event: Windows Credential Manager is checked BEFORE
		// config.yaml (see getBastionPassword), so if a stale/wrong password was already sitting
		// in the keychain, only saving to config.yaml here would never fix that - every future
		// run would keep pulling the same stale keychain entry and prompting again forever.
		if passwordNeedsSaving && *password != "" {
			if err := storeBastionPasswordInKeychain(*username, *bastionHost, *password, logger); err != nil {
				logger.Debug("Could not refresh Windows Credential Manager entry: %v", err)
			} else {
				logger.Info("✓ Password refreshed in Windows Credential Manager for %s@%s", *username, *bastionHost)
			}
			if err := saveConfigWithEncryptedPassword(*configFile, config, *password); err != nil {
				logger.Warn("Failed to save encrypted password: %v", err)
			} else {
				// Display encrypted password for user to copy if needed (terminal only, not in logger_info)
				encryptedPass, encErr := encryptPassword(*password)
				if encErr == nil {
					logger.Info("Password encrypted and saved to config.yaml")
					// Display encrypted password in terminal only (not in logger_info.txt for security)
					fmt.Println("[INFO] Encrypted Password (copy the below \"ENC:...\" if needed):")
					fmt.Printf("[INFO] \"%s\"\n", encryptedPass)
				}
			}
		}

		// Connect to AWS host
		logger.Info("Connecting to AWS server %s via bastion...", *awsHost)
		awsClient, err = sshConnectAWSViaBastion(bastionClient, *awsHost, *keyPath, *awsUsername)
		if err != nil {
			logger.Error("Failed to connect to AWS server: %v", err)
			return
		}
		defer awsClient.Close()
		logger.Info("Successfully connected to AWS server: %s", *awsHost)
	} // end needsAWSConnection

	// Pre-declare variables used in download summary (zero values when AWS is skipped)
	var finalArchiveName string
	successCount := 0
	failCount := 0
	retrySuccessCount := 0
	totalFiles := 0
	var downloadedFiles []string
	var downloadSpeeds []string
	var selectedLogFiles []string

	// All code below this point that uses awsClient is wrapped in needsAWSConnection
	if needsAWSConnection {

		// When CLI flags explicitly request system info collection, override config.yaml's enabled flag
		if collectInfo {
			config.SystemInfo.Enabled = true
		}

		// ── Owner ID Resolution ────────────────────────────────────────────
		// If env_login_id is set, query accountdb to resolve the ownerID from the user's login email.
		// The resolved ownerID always overrides any static ownerID in config.yaml.
		// This also propagates the ownerID into databaseCollection.parameters and messageFilter.
		if config.EnvLoginID != "" {
			resolution, err := resolveOwnerIDFromAccountDB(awsClient, config.EnvLoginID, config.DatabaseCollection, logger)
			if err != nil {
				logger.Warn("Owner ID resolution from accountdb failed: %v", err)
				if config.OwnerID == "" {
					logger.Warn("No static ownerID configured either — features requiring ownerID will be limited")
				} else {
					logger.Info("Falling back to static ownerID: %s", config.OwnerID)
				}
			} else {
				// Override config.OwnerID with the resolved value
				config.OwnerID = resolution.OwnerID
				// Re-propagate into databaseCollection.parameters
				if config.DatabaseCollection.Parameters == nil {
					config.DatabaseCollection.Parameters = make(map[string]string)
				}
				config.DatabaseCollection.Parameters["owner_id"] = config.OwnerID
				// Re-propagate into messageFilter ownerID
				for i := range config.LogCollection.MessageFilter.KeyValueFilters {
					kv := &config.LogCollection.MessageFilter.KeyValueFilters[i]
					if strings.EqualFold(kv.Key, "ownerID") {
						kv.Value = config.OwnerID
					}
				}
			}
		}

		// ── Limited Mode Warning ───────────────────────────────────────────
		// If no ownerID is available after resolution attempts, warn about limited functionality.
		if config.OwnerID == "" {
			logger.Warn("%s", strings.Repeat("=", 70))
			logger.Warn("  NO OWNER ID AVAILABLE")
			logger.Warn("%s", strings.Repeat("=", 70))
			logger.Warn("The following features will be skipped:")
			logger.Warn("  - Dynamic device detection (requires ownerID)")
			logger.Warn("  - Database queries using {owner_id} parameter")
			logger.Warn("  - Message filter ownerID matching (other filters still apply)")
			logger.Warn("")
			logger.Warn("To resolve: set env_login_id (XIQ login email) or ownerID in config.yaml")
			logger.Warn("%s", strings.Repeat("=", 70))
		}

		// ── Device Info Query ──────────────────────────────────────────────
		// Query hm_device from configdb_1 only when dynamic device detection is enabled
		// and device log collection is requested. If devices are found, they replace
		// config.yaml static devices for device log collection.
		var detectedDevices []DetectedDevice
		ownerID := config.OwnerID
		if ownerID != "" && collectDeviceLogs && config.LogCollection.DynamicDeviceDetection.Enabled {
			devices, err := queryDeviceInfoFromDB(awsClient, ownerID, config.DatabaseCollection, logger)
			if err != nil {
				logger.Warn("Device info query failed: %v", err)
			} else {
				detectedDevices = devices
			}
		}

		// Apply dynamic device detection: replace config.yaml devices with DB-detected devices
		if config.LogCollection.DynamicDeviceDetection.Enabled && len(detectedDevices) > 0 && collectDeviceLogs {
			maxDevices := config.LogCollection.DynamicDeviceDetection.MaxDevices
			if maxDevices <= 0 {
				maxDevices = 3 // Default
			}
			dynamicDevices := buildNetworkDevicesFromDetected(detectedDevices, maxDevices, config.DeviceLogCollection, logger)
			if len(dynamicDevices) > 0 {
				logger.Info("Dynamic device detection: using %d device(s) from database (replacing config.yaml devices)", len(dynamicDevices))
				config.DeviceLogCollection.Devices = dynamicDevices
				// Ensure defaultNosLogFiles is enabled for dynamic devices (they need default log paths)
				config.DeviceLogCollection.DefaultNosLogFiles.Enabled = true
				// Enable device log collection in case it was disabled
				config.DeviceLogCollection.Enabled = true
				collectDeviceLogs = true
			} else {
				logger.Warn("Dynamic device detection: no usable devices found (all may lack IP addresses)")
				logger.Info("Falling back to config.yaml device list")
			}
		}

		// Handle standalone operation modes

		// --sys-info mode: collect only general system information
		if selectedMode == "sys-info" {
			logger.Info("Running in sys-info mode (general system info only)...")

			// Create a simple temp directory for sys-info collection
			tempDir := "sys_info_temp"
			infoFileName := fmt.Sprintf("sys_info_%s", time.Now().Format("20060102_150405"))

			err = collectSystemInfo(awsClient, config.SystemInfo, config.Environment, config.Username, tempDir, infoFileName)
			if err != nil {
				logger.Error("Failed to collect system information: %v", err)
				return
			}
			logger.Info("System information collection completed successfully!")
			logger.Info("Files are located in: %s/%s/%s/", tempDir, infoFileName, config.SystemInfo.OutputDir)
			return
		}

		// --version mode: collect only application version information
		if selectedMode == "version" {
			logger.Info("Running in version mode (app version collection only)...")
			versionFilePath, err := collectAppVersionsStandalone(awsClient, config, *outputDir)
			if err != nil {
				logger.Error("Failed to collect app versions: %v", err)
				return
			}
			logger.Info("App version collection completed successfully!")

			// Attach to JIRA if requested
			if *jiraIssueID != "" && versionFilePath != "" {
				logger.Info("")
				if !config.Jira.AttachmentEnabled {
					logger.Warn("JIRA attachment feature is disabled in config.yaml (jira.attachmentEnabled: false)")
				} else if config.Jira.Email == "" {
					logger.Warn("JIRA email not configured in config.yaml")
					logger.Info("Please configure your JIRA email in config.yaml to use the attachment feature")
					logger.Info("API token can be provided via: environment variable (JIRA_API_TOKEN), Windows Credential Manager, config.yaml, or interactive prompt")
				} else {
					attachmentFiles := []string{versionFilePath}
					if err := attachFilesToMultipleJiraIssues(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
						logger.Error("Failed to attach files to JIRA issue %s: %v", *jiraIssueID, err)
					}
				}
			}
			return
		}

		// --database mode: collect only database query results
		// This mode requires AWS connection to execute database commands remotely
		if selectedMode == "database" {
			logger.Info("Running in database mode (database query collection only)...")

			if !config.DatabaseCollection.Enabled {
				logger.Warn("Database collection is disabled in config.yaml (databaseCollection.enabled: false)")
				logger.Info("Please enable databaseCollection in config.yaml and configure your databases")
				return
			}

			// Execute database queries on AWS server via SSH
			dbOutDir, err := processDatabaseCollection(awsClient, *config, *outputDir, config.archiveTimestamp, logger)
			if err != nil {
				logger.Error("Database collection failed: %v", err)
				return
			}
			logger.Info("Database query collection completed successfully!")

			// Attach database query files to JIRA if requested
			if *jiraIssueID != "" && dbOutDir != "" {
				logger.Info("")
				if !config.Jira.AttachmentEnabled {
					logger.Warn("JIRA attachment feature is disabled in config.yaml (jira.attachmentEnabled: false)")
				} else if config.Jira.Email == "" {
					logger.Warn("JIRA email not configured in config.yaml")
					logger.Info("Please configure your JIRA email in config.yaml to use the attachment feature")
					logger.Info("API token can be provided via: environment variable (JIRA_API_TOKEN), Windows Credential Manager, config.yaml, or interactive prompt")
				} else {
					// Compress entire database results directory into a single archive for JIRA
					archiveName := filepath.Base(dbOutDir) + ".tar.gz"
					archivePath := filepath.Join(filepath.Dir(dbOutDir), archiveName)
					logger.Info("Compressing database results for JIRA attachment: %s", archiveName)
					if err := compressDirectoryToTarGz(dbOutDir, archivePath, logger); err != nil {
						logger.Error("Failed to compress database results directory: %v", err)
					} else {
						logger.Info("Database results compressed: %s", archivePath)
						attachmentFiles := []string{archivePath}
						if err := attachFilesToMultipleJiraIssues(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
							logger.Error("Failed to attach database files to JIRA issue %s: %v", *jiraIssueID, err)
						} else {
							// Clean up the compressed archive after JIRA upload attempt
							if err := os.Remove(archivePath); err != nil {
								logger.Warn("Failed to delete compressed archive %s: %v", archivePath, err)
							} else {
								logger.Info("Deleted compressed archive after JIRA upload: %s", archivePath)
							}
						}
					}
				}
			}
			return
		}

		// --temporal mode: collect only Temporal workflow + schedule data, nothing else.
		// --temporal --all forces collecting ALL activities for every workflow, ignoring
		// workflowActivitySets matching; without --all, workflowActivitySets from
		// config.yaml is used exactly as in the normal collection pipeline.
		if selectedMode == "temporal" {
			logger.Info("Running in temporal mode (Temporal workflow + schedule data only)...")

			forceAllActivities := *modeAll
			if forceAllActivities {
				logger.Info("--all specified: collecting ALL activities for every workflow (ignoring workflowActivitySets)")
			}

			twConfig := config.LogCollection.TemporalWorkflowCollection
			twConfig.OwnerID = config.OwnerID
			tsConfig := config.LogCollection.TemporalScheduleCollection
			ztfConfig := config.LogCollection.ZtfOnboardWorkflowCollection
			ztfConfig.OwnerID = config.OwnerID

			if !twConfig.Enabled && !tsConfig.Enabled && !ztfConfig.Enabled {
				logger.Warn("temporalWorkflowCollection, temporalScheduleCollection, and ztfOnboardWorkflowCollection are all disabled in config.yaml")
				logger.Info("Please enable at least one of them to use --temporal")
				return
			}

			tempDir := "temporal_temp"
			finalLogFileName := fmt.Sprintf("temporal_%s", config.archiveTimestamp)

			if twConfig.Enabled {
				if err := collectTemporalWorkflowInfo(awsClient, twConfig, forceAllActivities, config.Environment, config.Username, tempDir, finalLogFileName); err != nil {
					logger.Error("Temporal workflow collection failed: %v", err)
				}
			} else {
				logger.Warn("Temporal workflow collection is disabled in config.yaml (temporalWorkflowCollection.enabled: false)")
			}

			if tsConfig.Enabled {
				if err := collectTemporalScheduleInfo(awsClient, tsConfig, config.Environment, config.Username, tempDir, finalLogFileName); err != nil {
					logger.Error("Temporal schedule collection failed: %v", err)
				}
			} else {
				logger.Warn("Temporal schedule collection is disabled in config.yaml (temporalScheduleCollection.enabled: false)")
			}

			if ztfConfig.Enabled {
				if err := collectZtfOnboardWorkflows(awsClient, ztfConfig, twConfig.WorkflowActivitySets, forceAllActivities, tempDir, finalLogFileName); err != nil {
					logger.Error("ZTF onboarding workflow collection failed: %v", err)
				}
			} else {
				logger.Warn("ZTF onboarding workflow collection is disabled in config.yaml (ztfOnboardWorkflowCollection.enabled: false)")
			}

			connParams := &ConnectionParams{
				BastionClient:     bastionClient,
				BastionUsername:   *username,
				BastionPassword:   *password,
				BastionHost:       *bastionHost,
				BastionPort:       *bastionPort,
				AWSHost:           *awsHost,
				KeyPath:           *keyPath,
				PreferredUsername: *awsUsername,
			}

			temporalArchivePath, archErr := archiveAndDownloadRemoteDir(awsClient, tempDir, finalLogFileName, *userID, *outputDir, *autoRetry, *numChunks, downloadMethod, connParams, logger)
			if archErr != nil {
				logger.Error("Failed to download temporal data archive: %v", archErr)
				return
			}
			logger.Info("Temporal data collection completed successfully!")
			logger.Info("Archive saved to: %s", temporalArchivePath)

			// --temporal only downloads; run the same log analytics pass (Temporal status
			// validation, flow report, HTML) the main --all/--logs-only flow runs, so a
			// standalone temporal run doesn't require a separate --analyze pass to get one.
			if config.LogCollection.LogAnalysis.Enabled && strings.HasSuffix(temporalArchivePath, ".tar.gz") {
				logger.Info("")
				if err := analyzeDownloadedLogs(temporalArchivePath, *outputDir, config.LogCollection.LogAnalysis); err != nil {
					logger.Warn("Log analysis failed for %s: %v", filepath.Base(temporalArchivePath), err)
				}
			}

			// Attach to JIRA if requested
			if *jiraIssueID != "" && temporalArchivePath != "" {
				logger.Info("")
				if !config.Jira.AttachmentEnabled {
					logger.Warn("JIRA attachment feature is disabled in config.yaml (jira.attachmentEnabled: false)")
				} else if config.Jira.Email == "" {
					logger.Warn("JIRA email not configured in config.yaml")
					logger.Info("Please configure your JIRA email in config.yaml to use the attachment feature")
					logger.Info("API token can be provided via: environment variable (JIRA_API_TOKEN), Windows Credential Manager, config.yaml, or interactive prompt")
				} else {
					attachmentFiles := []string{temporalArchivePath}
					if err := attachFilesToMultipleJiraIssues(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
						logger.Error("Failed to attach files to JIRA issue %s: %v", *jiraIssueID, err)
					}
				}
			}
			return
		}

		// In config mode, handle standalone operations based on what's enabled
		if selectedMode == "config" {
			// If only general info is enabled (no logs, no app versions)
			if collectInfo && !collectLogs && !collectAppVersions {
				logger.Info("Running in config mode - collecting general system info only...")
				tempDir := "sys_info_temp"
				infoFileName := fmt.Sprintf("sys_info_%s", time.Now().Format("20060102_150405"))

				err = collectSystemInfo(awsClient, config.SystemInfo, config.Environment, config.Username, tempDir, infoFileName)
				if err != nil {
					logger.Error("Failed to collect system information: %v", err)
					return
				}
				logger.Info("System information collection completed successfully!")
				logger.Info("Files are located in: %s/%s/%s/", tempDir, infoFileName, config.SystemInfo.OutputDir)
				return
			}

			// If only app version collection is enabled (no logs, no general info)
			if collectAppVersions && !collectLogs && !collectInfo {
				logger.Info("Running in config mode - collecting app versions only...")
				versionFilePath, err := collectAppVersionsStandalone(awsClient, config, *outputDir)
				if err != nil {
					logger.Error("Failed to collect app versions: %v", err)
					return
				}
				logger.Info("App version collection completed successfully!")

				// Attach to JIRA if requested
				if *jiraIssueID != "" && versionFilePath != "" {
					logger.Info("")
					if !config.Jira.AttachmentEnabled {
						logger.Warn("JIRA attachment feature is disabled in config.yaml (jira.attachmentEnabled: false)")
					} else if config.Jira.Email == "" {
						logger.Warn("JIRA email not configured in config.yaml")
						logger.Info("Please configure your JIRA email in config.yaml to use the attachment feature")
						logger.Info("API token can be provided via: environment variable (JIRA_API_TOKEN), Windows Credential Manager, config.yaml, or interactive prompt")
					} else {
						attachmentFiles := []string{versionFilePath}
						if err := attachFilesToMultipleJiraIssues(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
							logger.Error("Failed to attach files to JIRA issue %s: %v", *jiraIssueID, err)
						}
					}
				}
				return
			}
		}

		// Perform log collection if requested
		if collectLogs {
			if selectedMode == "all" {
				logger.Info("Running in all mode (logs + system info + app versions)...")
			} else if selectedMode == "logs-only" {
				logger.Info("Running in logs-only mode...")
			} else if selectedMode == "config" {
				logger.Info("Running in config mode (using config.yaml settings)...")
			}
			logger.Info("Starting log collection process...")

			// Determine time-based collection settings
			timeBasedEnabled := false
			timeDurationStr := ""

			// Check command-line flag first
			if *timeDuration != "" {
				// Special handling for explicit disable values
				if *timeDuration == "0" || *timeDuration == "disabled" || *timeDuration == "false" {
					timeBasedEnabled = false
					timeDurationStr = ""
					logger.Info("Time-based collection explicitly disabled via command line")
				} else {
					timeBasedEnabled = true
					timeDurationStr = *timeDuration
					logger.Info("Time-based collection enabled via command line: %s", timeDurationStr)
				}
			} else if config.LogCollection.TimeBasedCollection.Enabled {
				// Use config settings if no command-line flag
				timeBasedEnabled = true
				timeDurationStr = config.LogCollection.TimeBasedCollection.Duration
				logger.Info("Time-based collection enabled via config: %s", timeDurationStr)
			}

			// Start timing the entire log collection and archive creation process
			overallStartTime := time.Now()

			// Wire the resolved ownerID into temporal workflow collection so it can
			// filter workflows via: temporal workflow list --query 'OwnerId="<ownerID>"'
			config.LogCollection.TemporalWorkflowCollection.OwnerID = config.OwnerID
			config.LogCollection.ZtfOnboardWorkflowCollection.OwnerID = config.OwnerID

			finalArchiveName, err = collectKubernetesLogs(awsClient, *logFileName, *userID, config.LogCollection.TempDir, config.LogCollection.CustomSources, config.LogCollection.UseTimestamp, config.LogCollection.TimestampFormat, config.Environment, config.Username, collectInfo, config.SystemInfo, timeBasedEnabled, timeDurationStr, config.Options.MaxSSHSessions, config.LogCollection.AutoDeleteTempDir, config.LogCollection.DefaultEP1Logs,
				struct {
					Enabled              bool `yaml:"enabled"`
					FilterDuringDownload bool `yaml:"filterDuringDownload"`
					KeyValueFilters      []struct {
						Key   string `yaml:"key"`
						Value string `yaml:"value"`
					} `yaml:"keyValueFilters"`
					SpecificStrings []string `yaml:"specificStrings"`
				}{
					Enabled:              config.LogCollection.MessageFilter.Enabled,
					FilterDuringDownload: config.LogCollection.MessageFilter.FilterDuringDownload,
					KeyValueFilters:      config.LogCollection.MessageFilter.KeyValueFilters,
					SpecificStrings:      config.LogCollection.MessageFilter.SpecificStrings,
				},
				config.LogCollection.TemporalWorkflowCollection, config.LogCollection.TemporalScheduleCollection, config.LogCollection.ZtfOnboardWorkflowCollection, config.LogCollection.PodFileCollection)
			if err != nil {
				logger.Error("Failed to collect logs: %v", err)
				return
			}

			// Display overall timing
			overallDuration := time.Since(overallStartTime)
			logger.Info("Overall log collection and archiving completed in: %s", overallDuration.Round(time.Millisecond))

			// Extract timestamp from archive name (e.g., app_log_20260217_095735 -> 20260217_095735)
			// and store it in config for consistent naming across all output files
			tsRe := regexp.MustCompile(`(\d{8}_\d{6})`)
			if tsMatch := tsRe.FindString(finalArchiveName); tsMatch != "" {
				config.archiveTimestamp = tsMatch
				logger.Debug("Extracted archive timestamp: %s", tsMatch)
			}

			// Update the log pattern to look for the newly created archive
			archivePattern := fmt.Sprintf("/home/%s/%s.tar.gz", *userID, finalArchiveName)
			*logPattern = archivePattern
			logger.Info("Updated log pattern to: %s", *logPattern)

			// Give the system a moment to complete file operations
			logger.Info("Waiting for file system operations to complete...")
			time.Sleep(2 * time.Second)
		}

		// Collect app versions if enabled and logs were collected
		if collectAppVersions && collectLogs {
			logger.Info("Collecting application version information...")
			_, err = collectAppVersionsStandalone(awsClient, config, *outputDir)
			if err != nil {
				logger.Warn("Failed to collect app versions: %v", err)
				// Don't return - continue with log downloads
			} else {
				logger.Info("App version collection completed successfully!")
			}
		}

		// List log files on AWS server
		logger.Info("Fetching log files...")
		session, err := awsClient.NewSession()
		if err != nil {
			logger.Error("Failed to create session: %v", err)
			return
		}
		defer session.Close()

		var logFiles []string
		output, err := session.Output("ls " + *logPattern + " 2>/dev/null || echo ''")
		if err != nil {
			logger.Debug("No existing log files found matching pattern %s: %v", *logPattern, err)
			// Don't return here - continue to allow log collection or other operations
			logFiles = []string{}
			fmt.Println("No existing log files found. Logs will be collected from Kubernetes.")
		} else {
			logFiles = strings.Split(strings.TrimSpace(string(output)), "\n")
			if len(logFiles) == 0 || (len(logFiles) == 1 && logFiles[0] == "") {
				logFiles = []string{} // Reset to empty if no valid files
				fmt.Println("No log files found matching pattern:", *logPattern)
				fmt.Println("Logs will be collected from Kubernetes.")
			} else {
				fmt.Println("Available log files:")
				fmt.Println("--------------------")
				for i, file := range logFiles {
					fmt.Printf("%d. %s\n", i+1, file)
				}
			}
		}

		if *listOnly {
			return
		}

		// Select log files to download
		if *interactive {
			fmt.Println("Enter log file numbers to download (comma-separated, or 'all' for all files):")
			reader := bufio.NewReader(os.Stdin)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)

			if input == "all" {
				selectedLogFiles = logFiles
			} else {
				selections := strings.Split(input, ",")
				for _, s := range selections {
					s = strings.TrimSpace(s)
					idx, err := strconv.Atoi(s)
					if err != nil || idx < 1 || idx > len(logFiles) {
						logger.Warn("Invalid selection: %s", s)
						continue
					}
					selectedLogFiles = append(selectedLogFiles, logFiles[idx-1])
				}
			}
		} else {
			selectedLogFiles = logFiles
		}

		if len(selectedLogFiles) == 0 {
			logger.Info("No log files selected for download")
		} else {
			// Create output directory if it doesn't exist
			if err := os.MkdirAll(*outputDir, 0755); err != nil {
				fmt.Println("Failed to create output directory:", err)
				return
			}
			// Download selected log files
			totalFiles = len(selectedLogFiles)
			logger.Info("%s", strings.Repeat("=", 70))
			logger.Info("  FILE DOWNLOAD - Downloading %d file(s) to local machine", totalFiles)
			logger.Info("%s", strings.Repeat("=", 70))
			logger.Info("Starting download of %d file(s) to %s", totalFiles, *outputDir)
			fmt.Println(strings.Repeat("-", 50))

			for i, remotePath := range selectedLogFiles {
				filename := remotePath[strings.LastIndex(remotePath, "/")+1:]
				localPath := filepath.Join(*outputDir, filename)
				logger.Info("Starting download %d of %d: %s", i+1, totalFiles, filename)

				// Get initial file size for display
				sftpClient, statErr := sftp.NewClient(awsClient)
				if statErr == nil {
					remoteFileInfo, statErr := sftpClient.Stat(remotePath)
					if statErr == nil {
						remoteFileSize := remoteFileInfo.Size()
						logger.Debug("File size: %d bytes (%.2f MB)", remoteFileSize, float64(remoteFileSize)/(1024*1024))
					}
					sftpClient.Close()
				}

				// Create a temporary file path for download
				tempFilePath := localPath + ".part"
				logger.Debug("Created temporary file: %s", tempFilePath)
				logger.Debug("Added start marker to file\n")
				logger.Debug("Downloading to temporary file: %s", tempFilePath)

				// Create connection parameters for parallel downloads
				connParams := &ConnectionParams{
					BastionClient:     bastionClient,
					BastionUsername:   *username,
					BastionPassword:   *password,
					BastionHost:       *bastionHost,
					BastionPort:       *bastionPort,
					AWSHost:           *awsHost,
					KeyPath:           *keyPath,
					PreferredUsername: *awsUsername,
				}

				// Record download start time for summary
				downloadStartTime := time.Now()
				err := downloadFileFromAWS(awsClient, remotePath, localPath, *autoRetry, *numChunks, connParams, downloadMethod)
				downloadDuration := time.Since(downloadStartTime)

				if err != nil {
					// Check if this is a retry success but with some warning
					if strings.Contains(err.Error(), "retry_success:") {
						// Extract the real message
						message := strings.TrimPrefix(err.Error(), "retry_success:")
						logger.Warn("%s", message)
						retrySuccessCount++
					} else {
						logger.Error("Error: %s", err)
						failCount++
						continue
					}
				}

				// Get file size for display
				fileInfo, err := os.Stat(localPath)
				var fileSize string
				var fileSizeBytes int64
				if err == nil {
					fileSizeBytes = fileInfo.Size()
					if fileSizeBytes == 0 {
						logger.Warn("Warning: Downloaded file has zero bytes!")
					}

					if fileSizeBytes >= 1024*1024*1024 {
						fileSize = fmt.Sprintf("%.2f GB", float64(fileSizeBytes)/(1024*1024*1024))
					} else if fileSizeBytes >= 1024*1024 {
						fileSize = fmt.Sprintf("%.2f MB", float64(fileSizeBytes)/(1024*1024))
					} else if fileSizeBytes >= 1024 {
						fileSize = fmt.Sprintf("%.2f KB", float64(fileSizeBytes)/1024)
					} else {
						fileSize = fmt.Sprintf("%d B", fileSizeBytes)
					}
				} else {
					logger.Warn("Cannot verify file size: %v", err)
					fileSize = "unknown size"
				}

				var durationStr, speedStr string
				if downloadDuration.Hours() >= 1 {
					durationStr = fmt.Sprintf("%.1f hours", downloadDuration.Hours())
				} else if downloadDuration.Minutes() >= 1 {
					durationStr = fmt.Sprintf("%.1f minutes", downloadDuration.Minutes())
				} else {
					durationStr = fmt.Sprintf("%.1f seconds", downloadDuration.Seconds())
				}

				if fileSizeBytes > 0 {
					bytesPerSecond := float64(fileSizeBytes) / downloadDuration.Seconds()
					if bytesPerSecond >= 1024*1024 {
						speedStr = fmt.Sprintf("%.2f MB/s", bytesPerSecond/(1024*1024))
					} else if bytesPerSecond >= 1024 {
						speedStr = fmt.Sprintf("%.2f KB/s", bytesPerSecond/1024)
					} else {
						speedStr = fmt.Sprintf("%.2f B/s", bytesPerSecond)
					}
				} else {
					speedStr = "unknown speed"
				}

				// Store download information for summary instead of logging immediately
				downloadedFiles = append(downloadedFiles, fmt.Sprintf("%s (%s)", filename, fileSize))
				downloadSpeeds = append(downloadSpeeds, fmt.Sprintf("Download completed in %s (avg. %s)", durationStr, speedStr))
				logger.Debug("Successfully saved %s (%s)", filename, fileSize)
				successCount++
			}

			// Delete source archive if log collection was performed and deletion is enabled
			if collectLogs && finalArchiveName != "" && config.LogCollection.DeleteAfterCopy && successCount > 0 {
				archiveToDelete := fmt.Sprintf("/home/%s/%s.tar.gz", *userID, finalArchiveName)
				logger.Info("Post-download cleanup enabled, deleting source archive...")
				if err := deleteArchiveFromAWS(awsClient, archiveToDelete, config.Environment); err != nil {
					logger.Warn("Failed to delete source archive: %v", err)
				}
				// Note: tempDir cleanup is now handled immediately after archive creation in collectKubernetesLogs()
			}
		} // end of selectedLogFiles > 0 else block
	} // end needsAWSConnection (AWS-dependent operations)

	// Collect network device logs if enabled (in --all or config mode)
	var deviceLogOutputDir string
	if collectDeviceLogs && config.DeviceLogCollection.Enabled {
		logger.Info("")
		logger.Info("Starting network device log collection...")
		dlcOutDir, dlcErr := processDeviceLogCollection(*config, *outputDir, config.archiveTimestamp, logger)
		if dlcErr != nil {
			logger.Error("Device log collection failed: %v", dlcErr)
		}
		if dlcOutDir != "" {
			deviceLogOutputDir = dlcOutDir
		}
	}

	// Collect database query results if enabled (in --all mode)
	var databaseOutputDir string
	if collectDatabase && config.DatabaseCollection.Enabled {
		logger.Info("")
		logger.Info("Starting database query collection...")
		dbOutDir, dbErr := processDatabaseCollection(awsClient, *config, *outputDir, config.archiveTimestamp, logger)
		if dbErr != nil {
			logger.Error("Database collection failed: %v", dbErr)
		}
		if dbOutDir != "" {
			databaseOutputDir = dbOutDir
		}
	}

	logger.Info("Download Summary:")
	logger.Info("-----------------")
	logger.Info("Total files: %d", totalFiles)
	if retrySuccessCount > 0 {
		logger.Info("Successful: %d (%d with retries)", successCount, retrySuccessCount)
	} else {
		logger.Info("Successful: %d", successCount)
	}
	if failCount > 0 {
		logger.Info("Failed: %d", failCount)
	}

	// Show individual downloaded files
	if len(downloadedFiles) > 0 {
		logger.Info("Downloaded files:")
		for i, fileInfo := range downloadedFiles {
			logger.Info("Successfully saved %s", fileInfo)
			if i < len(downloadSpeeds) {
				logger.Info("%s", downloadSpeeds[i])
			}
		}
	}

	// Include downloaded file names in the output directory message
	if successCount > 0 {
		if successCount == 1 && len(selectedLogFiles) == 1 {
			// Single file downloaded
			filename := selectedLogFiles[0][strings.LastIndex(selectedLogFiles[0], "/")+1:]
			logger.Info("Output directory: %s (downloaded: %s)", *outputDir, filename)
		} else {
			// Multiple files downloaded - show directory and count
			logger.Info("Output directory: %s (%d files downloaded)", *outputDir, successCount)
		}
	} else {
		logger.Info("Output directory: %s", *outputDir)
	}

	// Run log analytics on downloaded archives if enabled
	if config.LogCollection.LogAnalysis.Enabled && successCount > 0 {
		logger.Info("")
		for _, remotePath := range selectedLogFiles {
			filename := remotePath[strings.LastIndex(remotePath, "/")+1:]
			localPath := filepath.Join(*outputDir, filename)

			// Only analyze .tar.gz archives
			if strings.HasSuffix(localPath, ".tar.gz") {
				if err := analyzeDownloadedLogs(localPath, *outputDir, config.LogCollection.LogAnalysis); err != nil {
					logger.Warn("Log analysis failed for %s: %v", filename, err)
				}
			}
		}
	}

	// Logger is already created in the output directory, no need to copy

	// Run post-download message filtering if enabled
	if config.LogCollection.MessageFilter.Enabled && successCount > 0 {
		logger.Info("")
		for _, remotePath := range selectedLogFiles {
			filename := remotePath[strings.LastIndex(remotePath, "/")+1:]
			localPath := filepath.Join(*outputDir, filename)

			if strings.HasSuffix(localPath, ".tar.gz") {
				if err := filterDownloadedLogs(localPath, *outputDir, config.LogCollection.MessageFilter); err != nil {
					logger.Warn("Message filtering failed for %s: %v", filename, err)
				}
			}
		}
	}

	// Attach files to JIRA issue if requested
	if *jiraIssueID != "" && successCount == 0 {
		// Previously this case fell through silently — the user would specify --jira
		// but see nothing attached and no explanation why.
		logger.Warn("Skipping JIRA attachment for %s: no log files were successfully downloaded (successCount=0)", *jiraIssueID)
	}
	if *jiraIssueID != "" && successCount > 0 {
		logger.Info("")
		logger.Info("%s", strings.Repeat("=", 70))
		logger.Info("  JIRA ATTACHMENT - Uploading files to %s", *jiraIssueID)
		logger.Info("%s", strings.Repeat("=", 70))

		// Check if JIRA attachment is enabled in config
		if !config.Jira.AttachmentEnabled {
			logger.Warn("JIRA attachment feature is disabled in config.yaml (jira.attachmentEnabled: false)")
		} else if config.Jira.Email == "" {
			logger.Warn("JIRA email not configured in config.yaml")
			logger.Info("Please configure your JIRA email in config.yaml to use the attachment feature")
			logger.Info("API token can be provided via: environment variable (JIRA_API_TOKEN), Windows Credential Manager, config.yaml, or interactive prompt")
		} else {
			// Collect all generated files for attachment
			attachmentFiles := []string{}

			// Add downloaded archive files
			for _, remotePath := range selectedLogFiles {
				filename := remotePath[strings.LastIndex(remotePath, "/")+1:]
				localPath := filepath.Join(*outputDir, filename)
				if _, err := os.Stat(localPath); err == nil {
					attachmentFiles = append(attachmentFiles, localPath)
				}
			}

			// Add logger_info file (check both with and without timestamp)
			loggerInfoPath := filepath.Join(*outputDir, fmt.Sprintf("logger_info_%s.txt", config.archiveTimestamp))
			if _, err := os.Stat(loggerInfoPath); err == nil {
				attachmentFiles = append(attachmentFiles, loggerInfoPath)
			} else {
				// Fallback: check for logger_info.txt without timestamp
				loggerInfoPathNoTs := filepath.Join(*outputDir, "logger_info.txt")
				if _, err := os.Stat(loggerInfoPathNoTs); err == nil {
					attachmentFiles = append(attachmentFiles, loggerInfoPathNoTs)
				}
			}

			// Add app versions file - use the actual configured filename
			appVersionFileName := config.AppVersionCollection.OutputFileName
			if appVersionFileName != "" {
				// Apply template replacements to match the actual file name
				appVersionFileName = strings.ReplaceAll(appVersionFileName, "{environment}", config.Environment)
				appVersionFileName = strings.ReplaceAll(appVersionFileName, "{username}", config.Username)
				if config.archiveTimestamp != "" {
					appVersionFileName = strings.ReplaceAll(appVersionFileName, "{timestamp}", config.archiveTimestamp)
				} else {
					appVersionFileName = strings.ReplaceAll(appVersionFileName, "{timestamp}", time.Now().Format("20060102_150405"))
				}
				appVersionFilePath := filepath.Join(*outputDir, appVersionFileName)
				if _, err := os.Stat(appVersionFilePath); err == nil {
					attachmentFiles = append(attachmentFiles, appVersionFilePath)
				} else {
					// Fallback: glob for any version info files
					matches, _ := filepath.Glob(filepath.Join(*outputDir, "*ersion*nfo*.txt"))
					attachmentFiles = append(attachmentFiles, matches...)
				}
			}

			// Add analytics report file
			analyticsReportPath := filepath.Join(*outputDir, fmt.Sprintf("log_analytics_report_%s.txt", config.archiveTimestamp))
			if _, err := os.Stat(analyticsReportPath); err == nil {
				attachmentFiles = append(attachmentFiles, analyticsReportPath)
			}

			// Add device logs as a single compressed archive
			if deviceLogOutputDir != "" {
				archiveName := filepath.Base(deviceLogOutputDir) + ".tar.gz"
				archivePath := filepath.Join(filepath.Dir(deviceLogOutputDir), archiveName)
				logger.Info("Compressing device logs for JIRA attachment: %s", archiveName)
				if err := compressDirectoryToTarGz(deviceLogOutputDir, archivePath, logger); err != nil {
					logger.Warn("Failed to compress device logs directory: %v", err)
					// Fallback: attach individual files
					deviceFiles, _ := filepath.Glob(filepath.Join(deviceLogOutputDir, "*", "*_diagnostics_*.txt"))
					attachmentFiles = append(attachmentFiles, deviceFiles...)
					deviceLogFiles, _ := filepath.Glob(filepath.Join(deviceLogOutputDir, "*", "*.tar.gz"))
					attachmentFiles = append(attachmentFiles, deviceLogFiles...)
					deviceLogFilesPlain, _ := filepath.Glob(filepath.Join(deviceLogOutputDir, "*", "*.log"))
					attachmentFiles = append(attachmentFiles, deviceLogFilesPlain...)
				} else {
					logger.Info("Device logs compressed: %s", archivePath)
					attachmentFiles = append(attachmentFiles, archivePath)
				}
			}

			// Add database query results as a single compressed archive
			if databaseOutputDir != "" {
				archiveName := filepath.Base(databaseOutputDir) + ".tar.gz"
				archivePath := filepath.Join(filepath.Dir(databaseOutputDir), archiveName)
				logger.Info("Compressing database results for JIRA attachment: %s", archiveName)
				if err := compressDirectoryToTarGz(databaseOutputDir, archivePath, logger); err != nil {
					logger.Warn("Failed to compress database results directory: %v", err)
					// Fallback: attach individual files
					dbFiles, _ := filepath.Glob(filepath.Join(databaseOutputDir, "*.txt"))
					attachmentFiles = append(attachmentFiles, dbFiles...)
				} else {
					logger.Info("Database results compressed: %s", archivePath)
					attachmentFiles = append(attachmentFiles, archivePath)
				}
			}

			// Attempt to attach files to JIRA
			if len(attachmentFiles) > 0 {
				if err := attachFilesToMultipleJiraIssues(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
					logger.Error("Failed to attach files to JIRA issue %s: %v", *jiraIssueID, err)
				}
				// Clean up compressed archives after JIRA upload attempt (success or failure)
				if deviceLogOutputDir != "" {
					archiveName := filepath.Base(deviceLogOutputDir) + ".tar.gz"
					archivePath := filepath.Join(filepath.Dir(deviceLogOutputDir), archiveName)
					if _, statErr := os.Stat(archivePath); statErr == nil {
						if err := os.Remove(archivePath); err != nil {
							logger.Warn("Failed to delete compressed archive %s: %v", archivePath, err)
						} else {
							logger.Info("Deleted compressed archive after JIRA upload attempt: %s", archivePath)
						}
					}
				}
				if databaseOutputDir != "" {
					archiveName := filepath.Base(databaseOutputDir) + ".tar.gz"
					archivePath := filepath.Join(filepath.Dir(databaseOutputDir), archiveName)
					if _, statErr := os.Stat(archivePath); statErr == nil {
						if err := os.Remove(archivePath); err != nil {
							logger.Warn("Failed to delete compressed archive %s: %v", archivePath, err)
						} else {
							logger.Info("Deleted compressed archive after JIRA upload attempt: %s", archivePath)
						}
					}
				}
			} else {
				logger.Warn("No files found to attach to JIRA issue %s", *jiraIssueID)
			}
		}
	}

	fmt.Println("\nDownload complete!")
}
