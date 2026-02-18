package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
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
	appVersion  = "1.0.0"   // Semantic version (set via -ldflags)
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

// NewLogger creates a new logger instance with the specified minimum log level
func NewLogger(minLevel LogLevel) *Logger {
	l := &Logger{minLevel: minLevel}
	// Create log file for capturing all output
	f, err := os.OpenFile("logger_info.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Printf("Warning: Could not create logger_info.txt: %v\n", err)
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
	newFile.Write(content)

	// Remove old file
	os.Remove(oldPath)

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
	// Only print if the message level is >= minimum level
	if level < l.minLevel {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	message := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] [%s] %s\n", timestamp, level.String(), message)
	fmt.Print(line)
	// Also write to log file
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

// GeneralInfoCommand represents a command to collect general system information
type GeneralInfoCommand struct {
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
	PagingDisableCommand string          `yaml:"pagingDisableCommand"`
	DiagnosticCommands   []DeviceCommand `yaml:"diagnosticCommands"`
}

// VossDefaultConfig contains default settings for VOSS devices
type VossDefaultConfig struct {
	EnableCommand        string          `yaml:"enableCommand"`
	ConfigCommand        string          `yaml:"configCommand"`
	PagingDisableCommand string          `yaml:"pagingDisableCommand"`
	DiagnosticCommands   []DeviceCommand `yaml:"diagnosticCommands"`
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

// DeviceLogCollection contains configuration for network device log collection
type DeviceLogCollection struct {
	Enabled           bool              `yaml:"enabled"`
	OutputDir         string            `yaml:"outputDir"`
	ParallelDownloads bool              `yaml:"parallelDownloads"`
	GlobalTimeout     int               `yaml:"globalTimeout"`
	CLISettings       DeviceCLISettings `yaml:"cliSettings"`
	ExosDefaults      ExosDefaultConfig `yaml:"exosDefaults"`
	VossDefaults      VossDefaultConfig `yaml:"vossDefaults"`
	Devices           []NetworkDevice   `yaml:"devices"`
}

// Configuration structure for the application
type Config struct {
	Username         string `yaml:"username"`    // Global username for all connections
	Environment      string `yaml:"environment"` // Environment identifier (e.g., dl1r1, g2r1)
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
		Enabled             bool           `yaml:"enabled"`
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
			Enabled           bool   `yaml:"enabled"`           // Enable temporal workflow data collection
			WorkflowIdPrefix  string `yaml:"workflowIdPrefix"`  // Filter by workflow ID prefix
			NumberOfWorkflows int    `yaml:"numberOfWorkflows"` // Number of workflows to collect (1-20)
			Namespace         string `yaml:"namespace"`         // Temporal namespace (default: configuration)
		} `yaml:"temporalWorkflowCollection"`
		TemporalScheduleCollection struct {
			Enabled           bool   `yaml:"enabled"`           // Enable temporal schedule data collection
			NumberOfSchedules int    `yaml:"numberOfSchedules"` // Number of schedules to collect (1-20)
			Namespace         string `yaml:"namespace"`         // Temporal namespace (default: configuration)
		} `yaml:"temporalScheduleCollection"`
		LogAnalysis struct {
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
		} `yaml:"logAnalysis"`
		MessageFilter struct {
			Enabled         bool `yaml:"enabled"` // Enable/disable post-download message filtering
			KeyValueFilters []struct {
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
	// General system information collection
	GeneralInfo struct {
		Enabled   bool                 `yaml:"enabled"`
		OutputDir string               `yaml:"outputDir"`
		Commands  []GeneralInfoCommand `yaml:"commands"`
	} `yaml:"generalInfo"`
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

	// Parse the YAML config
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("error parsing YAML config file: %v", err)
	}

	// Set default for MaxSSHSessions if not configured
	if config.Options.MaxSSHSessions <= 0 {
		config.Options.MaxSSHSessions = 1 // Conservative default
	}

	return &config, nil
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

	// Try to parse the key, print debug info if it fails
	key, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		// Check if it might be a permission error
		if strings.Contains(err.Error(), "cannot decode encrypted private keys") {
			return nil, fmt.Errorf("the key appears to be password-protected, which is not supported: %v", err)
		}

		// Print the first few characters of the key for debugging
		keyPreview := string(keyBytes)
		if len(keyPreview) > 100 {
			keyPreview = keyPreview[:100] + "..."
		}
		return nil, fmt.Errorf("failed to parse private key: %v\nKey begins with: %s", err, keyPreview)
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
func collectKubernetesLogs(awsClient *ssh.Client, logFileName, userID, tempDir string, customSources []PodLogSource, useTimestamp bool, timestampFormat, environment, username string, collectInfo bool, generalInfoConfig struct {
	Enabled   bool                 `yaml:"enabled"`
	OutputDir string               `yaml:"outputDir"`
	Commands  []GeneralInfoCommand `yaml:"commands"`
}, timeBasedEnabled bool, timeDurationStr string, maxSSHSessions int, autoDeleteTempDir bool, temporalConfig struct {
	Enabled           bool   `yaml:"enabled"`
	WorkflowIdPrefix  string `yaml:"workflowIdPrefix"`
	NumberOfWorkflows int    `yaml:"numberOfWorkflows"`
	Namespace         string `yaml:"namespace"`
}, temporalScheduleConfig struct {
	Enabled           bool   `yaml:"enabled"`
	NumberOfSchedules int    `yaml:"numberOfSchedules"`
	Namespace         string `yaml:"namespace"`
}, podFileCollectionConfig struct {
	Enabled     bool                `yaml:"enabled"`
	Collections []PodFileCollection `yaml:"collections"`
}) (string, error) {
	// Start timing the log collection process
	logCollectionStartTime := time.Now()
	logger.Info("Starting Kubernetes log collection...")
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

	// Use default sources if no custom sources provided
	sources := defaultLogSources
	if len(customSources) > 0 {
		sources = customSources
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
			err := collectLogsFromSourceParallel(awsClient, src, logDir, timeBasedEnabled, sinceTime, maxSSHSessions)
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

	// Collect general system information before archiving (if enabled)
	if collectInfo && generalInfoConfig.Enabled {
		logger.Info("Starting general system information collection...")
		err = collectGeneralInfo(awsClient, generalInfoConfig, environment, username, tempDir, finalLogFileName)
		if err != nil {
			logger.Warn("General info collection failed: %v", err)
			// Don't return here - continue with archive creation
		}
	}

	// Collect Temporal workflow information before archiving (if enabled)
	if temporalConfig.Enabled {
		logger.Info("Starting Temporal workflow information collection...")
		err = collectTemporalWorkflowInfo(awsClient, temporalConfig, environment, username, tempDir, finalLogFileName)
		if err != nil {
			logger.Warn("Temporal workflow collection failed: %v", err)
			// Don't return here - continue with archive creation
		}
	}

	// Collect Temporal schedule information before archiving (if enabled)
	if temporalScheduleConfig.Enabled {
		logger.Info("Starting Temporal schedule information collection...")
		err = collectTemporalScheduleInfo(awsClient, temporalScheduleConfig, environment, username, tempDir, finalLogFileName)
		if err != nil {
			logger.Warn("Temporal schedule collection failed: %v", err)
			// Don't return here - continue with archive creation
		}
	}

	// Collect pod files before archiving (if enabled)
	if podFileCollectionConfig.Enabled && len(podFileCollectionConfig.Collections) > 0 {
		logger.Info("Starting pod file collection...")
		err = collectPodFiles(awsClient, podFileCollectionConfig.Collections, tempDir, finalLogFileName)
		if err != nil {
			logger.Warn("Pod file collection failed: %v", err)
			// Don't return here - continue with archive creation
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
func collectLogsFromSourceParallel(awsClient *ssh.Client, source PodLogSource, logDir string, timeBasedEnabled bool, sinceTime string, maxSSHSessions int) error {
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
		return collectTimeBasedLogs(awsClient, source, logDir, podNames, sinceTime, maxSSHSessions)
	} else {
		logger.Debug("Using file-based collection for %s namespace", source.Namespace)
		return collectFileBased(awsClient, source, logDir, podNames, maxSSHSessions)
	}
}

// collectTimeBasedLogs collects logs using kubectl logs with --since parameter
func collectTimeBasedLogs(awsClient *ssh.Client, source PodLogSource, logDir string, podNames []string, sinceTime string, maxSSHSessions int) error {
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

// executeCommandAsRoot executes a command as root user using sudo su -
func executeCommandAsRoot(session *ssh.Session, command string) error {
	// Wrap the command to run as root
	rootCommand := fmt.Sprintf("sudo su - -c '%s'", command)
	logger.Debug("Executing as root: %s", rootCommand)

	output, err := session.CombinedOutput(rootCommand)
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

// collectGeneralInfo executes general system information commands and saves outputs to files on the remote server
func collectGeneralInfo(awsClient *ssh.Client, generalInfoConfig struct {
	Enabled   bool                 `yaml:"enabled"`
	OutputDir string               `yaml:"outputDir"`
	Commands  []GeneralInfoCommand `yaml:"commands"`
}, environment, username, tempDir, finalLogFileName string) error {
	if !generalInfoConfig.Enabled || len(generalInfoConfig.Commands) == 0 {
		logger.Debug("General info collection is disabled or no commands configured")
		return nil
	}

	logger.Info("Starting general system information collection...")

	// Create the general info directory on the remote server inside the log collection directory
	logDir := fmt.Sprintf("%s/%s", tempDir, finalLogFileName)
	generalOutputDir := fmt.Sprintf("%s/%s", logDir, generalInfoConfig.OutputDir)

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
	totalCommands := len(generalInfoConfig.Commands)

	for i, cmd := range generalInfoConfig.Commands {
		logger.Info("Executing command %d/%d: %s", i+1, totalCommands, cmd.Name)

		// Apply template replacement to the command
		command := cmd.Command
		command = strings.ReplaceAll(command, "{environment}", actualNamespace)
		command = strings.ReplaceAll(command, "{username}", username)

		// Create session for this command
		cmdSession, err := awsClient.NewSession()
		if err != nil {
			logger.Error("Failed to create session for command '%s': %v", cmd.Name, err)
			continue
		}

		// Execute the command as root
		logger.Debug("Executing: %s", command)
		output, err := cmdSession.CombinedOutput(fmt.Sprintf("sudo su - -c '%s'", command))
		cmdSession.Close()

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
	Enabled           bool   `yaml:"enabled"`
	WorkflowIdPrefix  string `yaml:"workflowIdPrefix"`
	NumberOfWorkflows int    `yaml:"numberOfWorkflows"`
	Namespace         string `yaml:"namespace"`
}, environment, username, tempDir, finalLogFileName string) error {
	if !temporalConfig.Enabled {
		logger.Debug("Temporal workflow collection is disabled")
		return nil
	}

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
	logger.Info("Discovering Temporal admin pod in 'common' namespace...")
	podSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for pod discovery: %v", err)
	}

	podCmd := "kubectl get pods -n common --no-headers | grep temporal-admintools | grep Running | head -1 | awk '{print \\$1}'"
	podOutput, err := podSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", podCmd))
	podSession.Close()
	if err != nil {
		// Log the output for debugging, then return error
		logger.Debug("Pod discovery command output: %s", strings.TrimSpace(string(podOutput)))
		return fmt.Errorf("failed to discover temporal admin pod: %v", err)
	}

	adminPod := strings.TrimSpace(string(podOutput))
	if adminPod == "" {
		return fmt.Errorf("no running temporal-admintools pod found in 'common' namespace")
	}
	logger.Info("Found Temporal admin pod: %s", adminPod)

	// Step 2: List workflows
	logger.Info("Listing workflows in namespace '%s'...", temporalNamespace)

	// First: get the plain text tabular listing (human-readable, used for parsing workflow IDs)
	listSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for workflow listing: %v", err)
	}

	listCmd := fmt.Sprintf("kubectl exec %s -n common -- temporal workflow list --namespace %s 2>/dev/null",
		adminPod, temporalNamespace)
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
				if temporalConfig.WorkflowIdPrefix != "" {
					return "prefix=" + temporalConfig.WorkflowIdPrefix
				}
				return "none"
			}(),
			strings.Repeat("-", 60),
			string(listOutput)))

	// Also save JSON listing as workflow_list.json (useful for programmatic analysis)
	jsonListSession, jsonErr := awsClient.NewSession()
	if jsonErr == nil {
		jsonListCmd := fmt.Sprintf("kubectl exec %s -n common -- temporal workflow list --namespace %s --output json 2>/dev/null",
			adminPod, temporalNamespace)
		jsonListOutput, jsonExecErr := jsonListSession.CombinedOutput(fmt.Sprintf("sudo su - -c \"%s\"", jsonListCmd))
		jsonListSession.Close()
		if jsonExecErr == nil {
			writeFileToRemote(awsClient, fmt.Sprintf("%s/workflow_list.json", temporalOutputDir), string(jsonListOutput))
			logger.Debug("Saved JSON workflow listing to workflow_list.json")
		} else {
			logger.Debug("JSON workflow listing failed (non-critical): %v", jsonExecErr)
		}
	}

	// Step 3: Extract workflow IDs from the tabular listing
	listOutputStr := string(listOutput)
	logger.Debug("Workflow list output (first 500 chars): %s", func() string {
		if len(listOutputStr) > 500 {
			return listOutputStr[:500] + "..."
		}
		return listOutputStr
	}())
	workflowIDs := extractWorkflowIDs(listOutputStr, temporalConfig.WorkflowIdPrefix, numberOfWorkflows)
	if len(workflowIDs) == 0 {
		logger.Warn("No workflow IDs found matching the criteria")
		writeFileToRemote(awsClient, fmt.Sprintf("%s/no_workflows_found.txt", temporalOutputDir),
			fmt.Sprintf("No workflows found matching criteria.\nPrefix filter: '%s'\nNamespace: %s\nRaw listing output:\n%s\n",
				temporalConfig.WorkflowIdPrefix, temporalNamespace, listOutputStr))
		return nil
	}

	logger.Info("Found %d workflow(s) to collect information for", len(workflowIDs))
	for i, wfID := range workflowIDs {
		logger.Info("  %d. %s", i+1, wfID)
	}

	// Step 4: For each workflow, collect detailed information
	for i, workflowID := range workflowIDs {
		logger.Info("Collecting data for workflow %d/%d: %s", i+1, len(workflowIDs), workflowID)

		// Create a sanitized filename from workflow ID
		safeWfID := sanitizeFilename(workflowID)
		wfOutputFile := fmt.Sprintf("%s/%s.txt", temporalOutputDir, safeWfID)

		var wfContent strings.Builder
		wfContent.WriteString("# Temporal Workflow Details\n")
		wfContent.WriteString(fmt.Sprintf("# Workflow ID: %s\n", workflowID))
		wfContent.WriteString(fmt.Sprintf("# Namespace: %s\n", temporalNamespace))
		wfContent.WriteString(fmt.Sprintf("# Collected: %s\n", time.Now().Format("2006-01-02 15:04:05")))
		wfContent.WriteString(fmt.Sprintf("#%s\n\n", strings.Repeat("-", 60)))

		// 4a: Workflow Input
		logger.Debug("  Collecting workflow input...")
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n")
		wfContent.WriteString("  WORKFLOW INPUT\n")
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n\n")

		inputCmd := fmt.Sprintf(`kubectl exec %s -n common -- bash -c 'temporal workflow show --namespace %s --workflow-id "%s" --output json 2>&1 | jq -r ".events[0].workflowExecutionStartedEventAttributes.input.payloads[0].data" | base64 -d 2>/dev/null | jq . 2>/dev/null || echo "No input data found"'`,
			adminPod, temporalNamespace, workflowID)
		inputOutput := executeTemporalCommand(awsClient, inputCmd)
		wfContent.WriteString(inputOutput + "\n\n")

		// 4b: Workflow Output
		logger.Debug("  Collecting workflow output...")
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n")
		wfContent.WriteString("  WORKFLOW OUTPUT\n")
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n\n")

		outputCmd := fmt.Sprintf(`kubectl exec %s -n common -- bash -c 'temporal workflow show --namespace %s --workflow-id "%s" --output json 2>&1 | jq -r ".events[] | select(.eventType == \"EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED\") | .workflowExecutionCompletedEventAttributes.result.payloads[0].data" | base64 -d 2>/dev/null | jq . 2>/dev/null || echo "No output data found or workflow still running"'`,
			adminPod, temporalNamespace, workflowID)
		outputOutput := executeTemporalCommand(awsClient, outputCmd)
		wfContent.WriteString(outputOutput + "\n\n")

		// 4c: List Activities
		logger.Debug("  Collecting activity list...")
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n")
		wfContent.WriteString("  ACTIVITIES\n")
		wfContent.WriteString("=" + strings.Repeat("=", 79) + "\n\n")

		activitiesCmd := fmt.Sprintf(`kubectl exec %s -n common -- bash -c 'temporal workflow show --namespace %s --workflow-id "%s" --output json 2>&1 | jq -r ".events[] | select(.eventType | contains(\"ACTIVITY\")) | \"\(.eventId)  \(.eventType)  \(.activityTaskScheduledEventAttributes.activityType.name // \"-\")\""'`,
			adminPod, temporalNamespace, workflowID)
		activitiesOutput := executeTemporalCommand(awsClient, activitiesCmd)
		wfContent.WriteString(activitiesOutput + "\n\n")

		// 4d: Extract unique activity names and collect per-activity details
		activityNames := extractActivityNames(activitiesOutput)
		if len(activityNames) > 0 {
			logger.Info("  Found %d unique activities: %v", len(activityNames), activityNames)

			for _, activityName := range activityNames {
				logger.Debug("  Collecting details for activity: %s", activityName)

				// Activity Input
				wfContent.WriteString("-" + strings.Repeat("-", 79) + "\n")
				wfContent.WriteString(fmt.Sprintf("  ACTIVITY INPUT: %s\n", activityName))
				wfContent.WriteString("-" + strings.Repeat("-", 79) + "\n\n")

				actInputCmd := fmt.Sprintf(`kubectl exec %s -n common -- bash -c 'temporal workflow show --namespace %s --workflow-id "%s" --output json 2>&1 | jq -r ".events[] | select(.activityTaskScheduledEventAttributes.activityType.name == \"%s\") | .activityTaskScheduledEventAttributes.input.payloads[0].data" | base64 -d 2>/dev/null | jq . 2>/dev/null || echo "No input data found for activity %s"'`,
					adminPod, temporalNamespace, workflowID, activityName, activityName)
				actInputOutput := executeTemporalCommand(awsClient, actInputCmd)
				wfContent.WriteString(actInputOutput + "\n\n")

				// Activity Output
				wfContent.WriteString("-" + strings.Repeat("-", 79) + "\n")
				wfContent.WriteString(fmt.Sprintf("  ACTIVITY OUTPUT: %s\n", activityName))
				wfContent.WriteString("-" + strings.Repeat("-", 79) + "\n\n")

				actOutputCmd := fmt.Sprintf(`kubectl exec %s -n common -- bash -c 'SCHED_ID=$(temporal workflow show --namespace %s --workflow-id "%s" --output json 2>&1 | jq -r --arg activityName "%s" ".events[] | select(.activityTaskScheduledEventAttributes.activityType.name == \$activityName) | .eventId"); if [ -n "$SCHED_ID" ]; then temporal workflow show --namespace %s --workflow-id "%s" --output json 2>&1 | jq -r --arg sid "$SCHED_ID" ".events[] | select(.activityTaskCompletedEventAttributes.scheduledEventId == \$sid) | .activityTaskCompletedEventAttributes.result.payloads[0].data" | base64 -d 2>/dev/null | jq . 2>/dev/null || echo "No output data found for activity %s"; else echo "Activity %s not found"; fi'`,
					adminPod, temporalNamespace, workflowID, activityName,
					temporalNamespace, workflowID, activityName, activityName)
				actOutputOutput := executeTemporalCommand(awsClient, actOutputCmd)
				wfContent.WriteString(actOutputOutput + "\n\n")

				// Activity Failure
				wfContent.WriteString("-" + strings.Repeat("-", 79) + "\n")
				wfContent.WriteString(fmt.Sprintf("  ACTIVITY FAILURE: %s\n", activityName))
				wfContent.WriteString("-" + strings.Repeat("-", 79) + "\n\n")

				actFailureCmd := fmt.Sprintf(`kubectl exec %s -n common -- bash -c 'SCHED_ID=$(temporal workflow show --namespace %s --workflow-id "%s" --output json 2>&1 | jq -r --arg activityName "%s" ".events[] | select(.activityTaskScheduledEventAttributes.activityType.name == \$activityName) | .eventId"); if [ -n "$SCHED_ID" ]; then temporal workflow show --namespace %s --workflow-id "%s" --output json 2>&1 | jq -r --arg sid "$SCHED_ID" ".events[] | select(.activityTaskFailedEventAttributes.scheduledEventId == \$sid) | .activityTaskFailedEventAttributes.failure" | jq . 2>/dev/null || echo "No failure data found for activity %s"; else echo "Activity %s not found"; fi'`,
					adminPod, temporalNamespace, workflowID, activityName,
					temporalNamespace, workflowID, activityName, activityName)
				actFailureOutput := executeTemporalCommand(awsClient, actFailureCmd)
				wfContent.WriteString(actFailureOutput + "\n\n")
			}
		} else {
			wfContent.WriteString("No activities found for this workflow.\n\n")
		}

		// Write the complete workflow file
		writeFileToRemote(awsClient, wfOutputFile, wfContent.String())
		logger.Info("  Saved workflow data to: %s", wfOutputFile)
	}

	logger.Info("Temporal workflow collection completed: %d workflow(s) collected", len(workflowIDs))
	logger.Info("Temporal data saved to remote directory: %s", temporalOutputDir)
	return nil
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

// extractWorkflowIDs parses workflow listing output and extracts workflow IDs
// Supports both JSON output and plain text tabular output from `temporal workflow list`
func extractWorkflowIDs(listOutput string, prefixFilter string, maxCount int) []string {
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

	// Limit to maxCount
	if len(workflowIDs) > maxCount {
		workflowIDs = workflowIDs[:maxCount]
	}

	return workflowIDs
}

// extractActivityNames parses the activities listing output and returns unique activity names
func extractActivityNames(activitiesOutput string) []string {
	seen := make(map[string]bool)
	var names []string
	lines := strings.Split(strings.TrimSpace(activitiesOutput), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "ERROR") {
			continue
		}

		// Format: "eventId  eventType  activityName"
		// e.g., "5  EVENT_TYPE_ACTIVITY_TASK_SCHEDULED  GetConfigurationFeatures"
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			// The activity name is the last field, but only for SCHEDULED events
			eventType := fields[1]
			if strings.Contains(eventType, "SCHEDULED") {
				activityName := fields[len(fields)-1]
				if activityName != "-" && activityName != "" && !seen[activityName] {
					seen[activityName] = true
					names = append(names, activityName)
				}
			}
		}
	}

	return names
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
}) error {
	if !logAnalysisConfig.Enabled {
		logger.Debug("Log analysis is disabled")
		return nil
	}

	logger.Info("%s", "="+strings.Repeat("=", 69))
	logger.Info("  LOG ANALYTICS - Analyzing downloaded archive for errors & issues")
	logger.Info("%s", "="+strings.Repeat("=", 69))

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
	logger.Info("Found %d file(s) to analyze", len(logFiles))

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
		globalPatternCounts, logAnalysisConfig, totalMatchesFound, archivePath, correlationIDIssues)
	if err != nil {
		return fmt.Errorf("failed to generate analytics report: %v", err)
	}

	logger.Info("Log analytics report generated: %s", reportPath)

	// Print a summary to the console
	printAnalyticsSummary(allSummaries, correlatedIssues, totalMatchesFound)

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
	}, totalMatches int, archivePath string, correlationIDIssues []CorrelationIDIssue) error {

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
	fmt.Fprintln(w, strings.Repeat("=", 80))
	fmt.Fprintln(w)

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
func printAnalyticsSummary(summaries []FileAnalysisSummary, correlatedIssues []CorrelatedIssue, totalMatches int) {
	logger.Info("%s", "="+strings.Repeat("=", 50))
	logger.Info("  LOG ANALYTICS SUMMARY")
	logger.Info("%s", "="+strings.Repeat("=", 50))

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
	Enabled         bool `yaml:"enabled"`
	KeyValueFilters []struct {
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

	logger.Info("%s", "="+strings.Repeat("=", 69))
	logger.Info("  MESSAGE FILTER - Filtering downloaded logs")
	logger.Info("%s", "="+strings.Repeat("=", 69))

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

	// Discover all files
	var logFiles []string
	err = filepath.Walk(extractDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}
		// Only filter text/log files
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
		escapedKey := regexp.QuoteMeta(kv.Key)
		escapedVal := regexp.QuoteMeta(kv.Value)
		fullRe, err := regexp.Compile("(?i)" + escapedKey + `\s*[:=]\s*"?` + escapedVal + `"?`)
		if err != nil {
			logger.Warn("Invalid key-value pattern for key='%s' value='%s', skipping", kv.Key, kv.Value)
			continue
		}
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
				// e.g., "xiq/nvo-edge-abc123/app.log" -> "xiq/nvo-edge/app.log"
				dir := filepath.Dir(relPath)
				file := filepath.Base(relPath)
				podDir := filepath.Base(dir)

				// Strip replica suffix from pod directory name
				if replicaRegex.MatchString(podDir) {
					podDir = replicaRegex.ReplaceAllString(podDir, "")
					dir = filepath.Join(filepath.Dir(dir), podDir)
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
		logger.Debug("Application Version Information:")
		logger.Debug("%s", strings.Repeat("=", 80))
		lines := strings.Split(content, "\n")
		for _, line := range lines {
			if line != "" {
				logger.Debug("%s", line)
			}
		}
		logger.Debug("%s", strings.Repeat("=", 80))
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

// attachFilesToJira uploads files to a JIRA issue using the JIRA REST API
func attachFilesToJira(jiraConfig JiraConfig, issueKey string, filePaths []string, logger *Logger) error {
	// Validate basic configuration
	if jiraConfig.Email == "" {
		return fmt.Errorf("JIRA email not configured")
	}

	if jiraConfig.BaseURL == "" {
		return fmt.Errorf("JIRA baseUrl not configured")
	}

	if issueKey == "" {
		return fmt.Errorf("JIRA issue key is empty")
	}

	// Get API token from multiple sources (env var, keychain, config, prompt)
	apiToken, err := getJIRAApiToken(&jiraConfig, logger)
	if err != nil {
		return fmt.Errorf("failed to retrieve JIRA API token: %v", err)
	}

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

	// Build the API endpoint
	apiURL := fmt.Sprintf("%s/rest/api/3/issue/%s/attachments", jiraConfig.BaseURL, issueKey)

	// Create multipart form data
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	for _, filePath := range existingFiles {
		file, err := os.Open(filePath)
		if err != nil {
			logger.Warn("Failed to open file %s: %v", filePath, err)
			continue
		}

		fileName := filepath.Base(filePath)
		part, err := writer.CreateFormFile("file", fileName)
		if err != nil {
			file.Close()
			return fmt.Errorf("failed to create form file for %s: %w", fileName, err)
		}

		_, err = io.Copy(part, file)
		file.Close()
		if err != nil {
			return fmt.Errorf("failed to copy file %s to form: %w", fileName, err)
		}

		logger.Debug("Added file to upload: %s", fileName)
	}

	err = writer.Close()
	if err != nil {
		return fmt.Errorf("failed to close multipart writer: %w", err)
	}

	// Create HTTP request
	var req *http.Request
	req, err = http.NewRequest("POST", apiURL, body)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Atlassian-Token", "no-check")

	// Set Basic Authentication (use retrieved token)
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", jiraConfig.Email, apiToken)))
	req.Header.Set("Authorization", fmt.Sprintf("Basic %s", auth))

	// Send request with extended timeout for large file uploads
	// 10 minutes should be sufficient for multi-GB archives
	client := &http.Client{
		Timeout: 10 * time.Minute,
	}

	logger.Info("Uploading %d file(s) to JIRA issue %s...", len(existingFiles), issueKey)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, _ := io.ReadAll(resp.Body)

	// Check response status
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		logger.Info("Successfully attached %d file(s) to JIRA issue %s", len(existingFiles), issueKey)
		logger.Debug("JIRA API Response: %s", string(respBody))
		return nil
	}

	return fmt.Errorf("JIRA API request failed with status %d: %s", resp.StatusCode, string(respBody))
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
		commands = append(commands, dlc.ExosDefaults.DiagnosticCommands...)
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
// VOSS requires: en → conf terminal → terminal more disable
func (ds *DeviceSession) initVossSession(dlc DeviceLogCollection) error {
	logger := ds.Logger

	// Step 1: Enter enable mode
	enableCmd := dlc.VossDefaults.EnableCommand
	if enableCmd == "" {
		enableCmd = "en"
	}
	logger.Info("Entering enable mode on '%s'...", ds.Device.Name)
	if _, err := ds.sendCommand(enableCmd, 10*time.Second); err != nil {
		return fmt.Errorf("failed to enter enable mode on %s: %v", ds.Device.Name, err)
	}
	logger.Info("Enable mode active on '%s'", ds.Device.Name)

	// Step 2: Enter config mode
	configCmd := dlc.VossDefaults.ConfigCommand
	if configCmd == "" {
		configCmd = "configure terminal"
	}
	logger.Info("Entering config mode on '%s'...", ds.Device.Name)
	if _, err := ds.sendCommand(configCmd, 10*time.Second); err != nil {
		return fmt.Errorf("failed to enter config mode on %s: %v", ds.Device.Name, err)
	}
	logger.Info("Config mode active on '%s'", ds.Device.Name)

	// Step 3: Disable CLI paging
	pagingCmd := dlc.VossDefaults.PagingDisableCommand
	if pagingCmd == "" {
		pagingCmd = "terminal more disable"
	}
	logger.Info("Disabling CLI paging on '%s'...", ds.Device.Name)
	if _, err := ds.sendCommand(pagingCmd, 10*time.Second); err != nil {
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
		commands = append(commands, dlc.VossDefaults.DiagnosticCommands...)
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
	logger.Info("========================================")
	logger.Info("Starting collection from device: %s (%s)", device.Name, device.IPAddress)
	logger.Info("Device type: %s", device.Type)
	logger.Info("========================================")

	deviceType := strings.ToLower(device.Type)

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

		// Disable paging
		if err := ds.disableExosPaging(dlc.ExosDefaults.PagingDisableCommand); err != nil {
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
			return timestampDir, fmt.Errorf("%d device(s) failed: %s", len(errors), strings.Join(errors, "; "))
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
			return timestampDir, fmt.Errorf("%d device(s) failed: %s", len(errors), strings.Join(errors, "; "))
		}
	}

	logger.Info("========================================")
	logger.Info("Device log collection complete!")
	logger.Info("Output directory: %s", timestampDir)
	logger.Info("========================================")
	return timestampDir, nil
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
	modeAll := flag.Bool("all", false, "Collect logs + system info + app versions + device logs (if enabled in config)")
	modeLogs := flag.Bool("logs-only", false, "Collect only logs without system info or app versions")
	modeSysInfo := flag.Bool("sys-info", false, "Collect only general system info (kubectl commands, system stats)")
	modeVersion := flag.Bool("version", false, "Collect only application version information")
	modeDeviceLogs := flag.Bool("device-logs", false, "Collect only network device logs and diagnostics")

	// Log collection configuration flags
	logFileName := flag.String("log-name", "", "Name for the log collection (without extension)")
	userID := flag.String("user-id", "", "User ID for log collection (defaults to bastion username)")
	timeDuration := flag.String("time-duration", "", "Collect logs from last X time (e.g., '15m', '30m', '1h', '2h'). Use '0' or 'disabled' to force full logs. Leave empty to use config setting")

	// JIRA integration flag
	jiraIssueID := flag.String("jira", "", "JIRA issue ID to attach files (e.g., XCP-17614). Requires jira config in config.yaml")

	// Version display flag
	showVersion := flag.Bool("v", false, "Show build version and exit")

	// Custom help message
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Operation Modes (mutually exclusive):\n")
		fmt.Fprintf(os.Stderr, "  (no mode)          Use config.yaml settings (default)\n")
		fmt.Fprintf(os.Stderr, "  --all              Collect logs + system info + app versions + device logs (if enabled)\n")
		fmt.Fprintf(os.Stderr, "  --logs-only        Collect only logs\n")
		fmt.Fprintf(os.Stderr, "  --sys-info         Collect only general system info (kubectl commands)\n")
		fmt.Fprintf(os.Stderr, "  --version          Collect only application version information\n")
		fmt.Fprintf(os.Stderr, "  --device-logs      Collect only network device logs and diagnostics\n\n")
		fmt.Fprintf(os.Stderr, "General Options:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	// Handle -v flag: print version and exit immediately
	if *showVersion {
		fmt.Printf("logcollector version %s (build %s) built on %s\n", appVersion, buildNumber, buildDate)
		os.Exit(0)
	}

	// Determine operation mode
	modeCount := 0
	if *modeAll {
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

	if modeCount > 1 {
		fmt.Fprintln(os.Stderr, "Error: Only one operation mode can be specified at a time")
		fmt.Fprintln(os.Stderr, "Choose one of: --all, --logs-only, --sys-info, --version, --device-logs")
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
	var collectLogs, collectInfo, collectAppVersions, collectDeviceLogs bool
	var selectedMode string

	if *modeAll {
		// --all mode: override config and collect everything
		collectLogs = true
		collectInfo = true
		collectAppVersions = true
		collectDeviceLogs = true // Will respect config.DeviceLogCollection.Enabled after loading
		selectedMode = "all"
	} else if *modeLogs {
		// --logs-only mode: collect only logs (K8s logs, excludes device logs)
		collectLogs = true
		collectInfo = false
		collectAppVersions = false
		collectDeviceLogs = false
		selectedMode = "logs-only"
	} else if *modeSysInfo {
		// --sys-info mode: collect only general system info (kubectl commands)
		collectLogs = false
		collectInfo = true
		collectAppVersions = false
		collectDeviceLogs = false
		selectedMode = "sys-info"
	} else if *modeVersion {
		// --version mode: collect only application version information
		collectLogs = false
		collectInfo = false
		collectAppVersions = true
		collectDeviceLogs = false
		selectedMode = "version"
	} else if *modeDeviceLogs {
		// --device-logs mode: collect only network device logs
		collectLogs = false
		collectInfo = false
		collectAppVersions = false
		collectDeviceLogs = true
		selectedMode = "device-logs"
	} else if modeCount == 0 {
		// No mode specified: use config.yaml settings (default behavior)
		selectedMode = "config"
		// Will be set from config.yaml after loading
	}

	// Load configuration from file
	config, err := LoadConfig(*configFile)
	if err != nil {
		logger.Warn("Warning: Failed to load config file: %v", err)
		config = &Config{} // Use empty config if loading fails
	}

	// If using config mode, read settings from config.yaml
	if selectedMode == "config" {
		collectLogs = config.LogCollection.Enabled
		collectInfo = config.GeneralInfo.Enabled
		collectAppVersions = config.AppVersionCollection.Enabled
		collectDeviceLogs = config.DeviceLogCollection.Enabled
	}

	// In --all mode, respect DeviceLogCollection.Enabled from config
	if selectedMode == "all" {
		collectDeviceLogs = config.DeviceLogCollection.Enabled
	}

	// Initialize the global logger with the log level setting
	logLevelEnum := ParseLogLevel(*logLevel)
	logger = NewLogger(logLevelEnum)
	defer logger.Close()

	// Initialize archiveTimestamp early so it's available for template replacement in outputDir
	config.archiveTimestamp = time.Now().Format("20060102_150405")

	// Log the selected operation mode
	logger.Debug("Selected operation mode: %s", selectedMode)

	// Apply template replacement to JIRA email field
	if config.Jira.Email != "" {
		config.Jira.Email = strings.ReplaceAll(config.Jira.Email, "{username}", config.Username)
		config.Jira.Email = strings.ReplaceAll(config.Jira.Email, "{environment}", config.Environment)
		logger.Debug("JIRA email after template replacement: %s", config.Jira.Email)
	}

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
	if *outputDir == "" {
		if config.Logs.OutputDir != "" {
			*outputDir = config.Logs.OutputDir
			// Apply template replacement to outputDir
			*outputDir = strings.ReplaceAll(*outputDir, "{timestamp}", config.archiveTimestamp)
			*outputDir = strings.ReplaceAll(*outputDir, "{username}", config.Username)
			*outputDir = strings.ReplaceAll(*outputDir, "{environment}", config.Environment)
		} else {
			*outputDir = "." // Default to current directory
		}
	}
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

	// --device-logs mode: collect only network device logs and diagnostics
	// This mode does NOT require bastion/AWS — it connects directly to devices via SSH
	if selectedMode == "device-logs" {
		logger.Info("Running in device-logs mode (network device logs only)...")

		if !config.DeviceLogCollection.Enabled {
			logger.Warn("Device log collection is disabled in config.yaml (deviceLogCollection.enabled: false)")
			logger.Info("Please enable deviceLogCollection in config.yaml and configure your devices")
			return
		}

		dlcOutDir, err := processDeviceLogCollection(*config, *outputDir, "", logger)
		if err != nil {
			logger.Error("Device log collection failed: %v", err)
		}

		// Move logger_info.txt directly into the Device_timestamp directory
		if dlcOutDir != "" {
			logger.MoveLogFileTo(dlcOutDir)
		}

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
					if err := attachFilesToJira(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
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
	hasOperations := collectLogs || collectInfo || collectAppVersions || collectDeviceLogs || *listOnly

	if !hasOperations {
		fmt.Println("No operations requested.")
		fmt.Println("In config mode, enable operations in config.yaml:")
		fmt.Println("  logCollection.enabled: true")
		fmt.Println("  generalInfo.enabled: true")
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

	// Connect to bastion
	logger.Info("Connecting to bastion host %s", *bastionHost)
	bastionClient, err := sshConnectBastion(*username, *password, *bastionHost, *bastionPort)
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

	// Save encrypted password if needed
	if passwordNeedsSaving && *password != "" {
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
	awsClient, err := sshConnectAWSViaBastion(bastionClient, *awsHost, *keyPath, *awsUsername)
	if err != nil {
		logger.Error("Failed to connect to AWS server: %v", err)
		return
	}
	defer awsClient.Close()
	logger.Info("Successfully connected to AWS server: %s", *awsHost)

	// When CLI flags explicitly request system info collection, override config.yaml's enabled flag
	if collectInfo {
		config.GeneralInfo.Enabled = true
	}

	// Handle standalone operation modes

	// --sys-info mode: collect only general system information
	if selectedMode == "sys-info" {
		logger.Info("Running in sys-info mode (general system info only)...")

		// Create a simple temp directory for sys-info collection
		tempDir := "sys_info_temp"
		infoFileName := fmt.Sprintf("sys_info_%s", time.Now().Format("20060102_150405"))

		err = collectGeneralInfo(awsClient, config.GeneralInfo, config.Environment, config.Username, tempDir, infoFileName)
		if err != nil {
			logger.Error("Failed to collect general system information: %v", err)
			return
		}
		logger.Info("General system information collection completed successfully!")
		logger.Info("Files are located in: %s/%s/%s/", tempDir, infoFileName, config.GeneralInfo.OutputDir)
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
				if err := attachFilesToJira(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
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

			err = collectGeneralInfo(awsClient, config.GeneralInfo, config.Environment, config.Username, tempDir, infoFileName)
			if err != nil {
				logger.Error("Failed to collect general system information: %v", err)
				return
			}
			logger.Info("General system information collection completed successfully!")
			logger.Info("Files are located in: %s/%s/%s/", tempDir, infoFileName, config.GeneralInfo.OutputDir)
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
					if err := attachFilesToJira(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
						logger.Error("Failed to attach files to JIRA issue %s: %v", *jiraIssueID, err)
					}
				}
			}
			return
		}
	}

	// Perform log collection if requested
	var finalArchiveName string
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

		finalArchiveName, err = collectKubernetesLogs(awsClient, *logFileName, *userID, config.LogCollection.TempDir, config.LogCollection.CustomSources, config.LogCollection.UseTimestamp, config.LogCollection.TimestampFormat, config.Environment, config.Username, collectInfo, config.GeneralInfo, timeBasedEnabled, timeDurationStr, config.Options.MaxSSHSessions, config.LogCollection.AutoDeleteTempDir, config.LogCollection.TemporalWorkflowCollection, config.LogCollection.TemporalScheduleCollection, config.LogCollection.PodFileCollection)
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
	var selectedLogFiles []string
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
		fmt.Println("No log files selected for download")
		return
	}
	// Create output directory if it doesn't exist
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fmt.Println("Failed to create output directory:", err)
		return
	}
	// Download selected log files
	totalFiles := len(selectedLogFiles)
	logger.Info("Starting download of %d file(s) to %s", totalFiles, *outputDir)
	fmt.Println(strings.Repeat("-", 50))

	successCount := 0
	failCount := 0
	retrySuccessCount := 0

	// Variables to collect download summary information
	var downloadedFiles []string
	var downloadSpeeds []string

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

			if fileSizeBytes < 1024 {
				fileSize = fmt.Sprintf("%d B", fileSizeBytes)
			} else if fileSizeBytes < 1024*1024 {
				fileSize = fmt.Sprintf("%.2f KB", float64(fileSizeBytes)/1024)
			} else if fileSizeBytes < 1024*1024*1024 {
				fileSize = fmt.Sprintf("%.2f MB", float64(fileSizeBytes)/(1024*1024))
			} else {
				fileSize = fmt.Sprintf("%.2f GB", float64(fileSizeBytes)/(1024*1024*1024))
			}
		} else {
			fileSize = "unknown size"
			logger.Warn("Cannot verify file size: %v", err)
		}

		// Calculate download speed and timing for summary
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

	// Copy logger_info.txt to the output directory so it lives alongside the downloaded files
	if successCount > 0 {
		logger.CopyLogFileTo(*outputDir, config.archiveTimestamp)
	}

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
	if *jiraIssueID != "" && successCount > 0 {
		logger.Info("")

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

			// Add logger_info file
			loggerInfoPath := filepath.Join(*outputDir, fmt.Sprintf("logger_info_%s.txt", config.archiveTimestamp))
			if _, err := os.Stat(loggerInfoPath); err == nil {
				attachmentFiles = append(attachmentFiles, loggerInfoPath)
			}

			// Add app versions file
			appVersionsPattern := fmt.Sprintf("*_app_versions_%s.txt", config.archiveTimestamp)
			matches, _ := filepath.Glob(filepath.Join(*outputDir, appVersionsPattern))
			attachmentFiles = append(attachmentFiles, matches...)

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

			// Attempt to attach files to JIRA
			if len(attachmentFiles) > 0 {
				if err := attachFilesToJira(config.Jira, *jiraIssueID, attachmentFiles, logger); err != nil {
					logger.Error("Failed to attach files to JIRA issue %s: %v", *jiraIssueID, err)
				} else if deviceLogOutputDir != "" {
					// Clean up the compressed device log archive after successful JIRA upload
					archiveName := filepath.Base(deviceLogOutputDir) + ".tar.gz"
					archivePath := filepath.Join(filepath.Dir(deviceLogOutputDir), archiveName)
					if _, statErr := os.Stat(archivePath); statErr == nil {
						if err := os.Remove(archivePath); err != nil {
							logger.Warn("Failed to delete compressed archive %s: %v", archivePath, err)
						} else {
							logger.Info("Deleted compressed archive after JIRA upload: %s", archivePath)
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
