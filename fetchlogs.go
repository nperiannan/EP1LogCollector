package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
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
		header := fmt.Sprintf("# Fetchlogs Session Log\n# Started: %s\n# Log Level: %s\n%s\n\n",
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

// CopyLogFileTo copies the logger_info.txt to the specified output directory
func (l *Logger) CopyLogFileTo(outputDir string) {
	if l.logFile == nil {
		return
	}
	// Flush current content
	l.logFile.Sync()

	srcPath := l.logFile.Name()
	dstPath := filepath.Join(outputDir, "logger_info.txt")

	// Don't copy if source and destination are the same
	absSrc, _ := filepath.Abs(srcPath)
	absDst, _ := filepath.Abs(dstPath)
	if absSrc == absDst {
		return
	}

	src, err := os.Open(srcPath)
	if err != nil {
		fmt.Printf("Warning: Could not read logger_info.txt for copy: %v\n", err)
		return
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		fmt.Printf("Warning: Could not create logger_info.txt in output dir: %v\n", err)
		return
	}
	defer dst.Close()

	io.Copy(dst, src)
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

// Configuration structure for the application
type Config struct {
	Username    string `yaml:"username"`    // Global username for all connections
	Environment string `yaml:"environment"` // Environment identifier (e.g., dl1r1, g2r1)
	Bastion     struct {
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
		LogAnalysis struct {
			Enabled         bool     `yaml:"enabled"`         // Enable automatic log analysis
			OutputFile      string   `yaml:"outputFile"`      // Output file name for analysis report
			ErrorPatterns   []string `yaml:"errorPatterns"`   // Patterns to search for (case-insensitive)
			ExcludeKeywords []string `yaml:"excludeKeywords"` // Keywords to exclude from matches (case-insensitive)
			MaxMatches      int      `yaml:"maxMatches"`      // Max matches per log file
			ContextLines    int      `yaml:"contextLines"`    // Lines before/after each match
		} `yaml:"logAnalysis"`
		MessageFilter struct {
			Enabled         bool `yaml:"enabled"`         // Enable/disable post-download message filtering
			KeyValueFilters []struct {
				Key   string `yaml:"key"`   // Key to look for in log lines (e.g., ownerID, serial)
				Value string `yaml:"value"` // Value to match (lines with key but different value are excluded)
			} `yaml:"keyValueFilters"` // Keep lines where key is absent OR key has specified value
			SpecificStrings []string `yaml:"specificStrings"` // Only keep lines containing these exact strings
		} `yaml:"messageFilter"`
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
	tempKeyPath := filepath.Join(os.TempDir(), fmt.Sprintf("fetchlogs_temp_key_%d", time.Now().Unix()))
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
		wfContent.WriteString(fmt.Sprintf("# Temporal Workflow Details\n"))
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
	FileName    string   // Name of the log file
	LineNumber  int      // 1-based line number of the match
	MatchedLine string   // The actual line that matched
	Pattern     string   // The pattern that matched
	BeforeLines []string // Context lines before the match
	AfterLines  []string // Context lines after the match
}

// CorrelatedIssue represents a group of related errors found across multiple files
type CorrelatedIssue struct {
	Pattern     string   // Common pattern or keyword
	Files       []string // Files where this pattern was found
	TotalCount  int      // Total occurrences across all files
	Severity    string   // Estimated severity: CRITICAL, HIGH, MEDIUM, LOW
	Description string   // Auto-generated description of the issue
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
}) error {
	if !logAnalysisConfig.Enabled {
		logger.Debug("Log analysis is disabled")
		return nil
	}

	logger.Info("=" + strings.Repeat("=", 69))
	logger.Info("  LOG ANALYTICS - Analyzing downloaded archive for errors & issues")
	logger.Info("=" + strings.Repeat("=", 69))

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
			logAnalysisConfig.ErrorPatterns, logAnalysisConfig.MaxMatches, logAnalysisConfig.ContextLines)

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
		globalPatternCounts, logAnalysisConfig, totalMatchesFound, archivePath)
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
	compiledExcludes []*regexp.Regexp, patternNames []string, maxMatches, contextLines int) FileAnalysisSummary {

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

				match := LogMatch{
					FileName:    displayName,
					LineNumber:  lineIdx + 1, // 1-based
					MatchedLine: line,
					Pattern:     patternName,
					BeforeLines: beforeLines,
					AfterLines:  afterLines,
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
	}, totalMatches int, archivePath string) error {

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
	logger.Info("=" + strings.Repeat("=", 50))
	logger.Info("  LOG ANALYTICS SUMMARY")
	logger.Info("=" + strings.Repeat("=", 50))

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
// Post-Download Message Filter — Filter logs into a separate filter/ directory
// ============================================================================

// filterDownloadedLogs extracts a .tar.gz archive, applies message filters to each log file,
// and writes filtered versions into a filter/ subdirectory preserving the archive's directory structure.
//
// Filter logic:
//   - keyValueFilters: For each filter, if a line contains the key, it's kept only if the value matches.
//     Lines that don't contain the key at all pass through (unaffected).
//   - specificStrings: If specified, lines MUST contain at least one of these strings to be included.
func filterDownloadedLogs(archivePath, outputDir string, filterConfig struct {
	Enabled         bool `yaml:"enabled"`
	KeyValueFilters []struct {
		Key   string `yaml:"key"`
		Value string `yaml:"value"`
	} `yaml:"keyValueFilters"`
	SpecificStrings []string `yaml:"specificStrings"`
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

	logger.Info("=" + strings.Repeat("=", 69))
	logger.Info("  MESSAGE FILTER - Filtering downloaded logs")
	logger.Info("=" + strings.Repeat("=", 69))

	if hasKeyValueFilters {
		for _, kv := range filterConfig.KeyValueFilters {
			logger.Info("  Key-Value Filter: key=%q value=%q (keep lines without key OR with matching value)", kv.Key, kv.Value)
		}
	}
	if hasSpecificStrings {
		logger.Info("  Specific Strings: %s (only keep lines containing these)", strings.Join(filterConfig.SpecificStrings, ", "))
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

	// Create filter output directory: outputDir/filter/archiveName/
	filterBaseDir := filepath.Join(outputDir, "filter", archiveBase)

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

	// Compile key-value filter patterns for efficient matching
	// For each key-value filter, we build a regex that matches: key followed by optional separators and quote-wrapped value
	// Pattern: key\s*[:=]\s*"?value"?  (handles key=value, key: value, key="value", "key":"value", etc.)
	type kvFilter struct {
		keyPattern *regexp.Regexp   // Matches the key anywhere in the line
		fullPattern *regexp.Regexp  // Matches key with the specified value
	}
	var kvFilters []kvFilter
	for _, kv := range filterConfig.KeyValueFilters {
		// Pattern to detect if the key exists in the line (case-insensitive)
		keyRe, err := regexp.Compile("(?i)" + regexp.QuoteMeta(kv.Key))
		if err != nil {
			logger.Warn("Invalid key pattern '%s', skipping", kv.Key)
			continue
		}
		// Pattern to match key with the exact value
		// Handles formats like: key=value, key="value", key: value, key: "value", "key":"value", "key": "value"
		escapedKey := regexp.QuoteMeta(kv.Key)
		escapedVal := regexp.QuoteMeta(kv.Value)
		fullRe, err := regexp.Compile("(?i)" + escapedKey + `\s*[:=]\s*"?` + escapedVal + `"?`)
		if err != nil {
			logger.Warn("Invalid key-value pattern for key='%s' value='%s', skipping", kv.Key, kv.Value)
			continue
		}
		kvFilters = append(kvFilters, kvFilter{keyPattern: keyRe, fullPattern: fullRe})
	}

	filesFiltered := 0
	totalLinesKept := 0
	totalLinesRemoved := 0

	for _, srcPath := range logFiles {
		relPath, _ := filepath.Rel(extractDir, srcPath)
		destPath := filepath.Join(filterBaseDir, relPath)

		// Ensure destination directory exists
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			logger.Warn("Failed to create filter dir for %s: %v", relPath, err)
			continue
		}

		// Read source file
		srcFile, err := os.Open(srcPath)
		if err != nil {
			logger.Warn("Cannot read %s for filtering: %v", relPath, err)
			continue
		}

		scanner := bufio.NewScanner(srcFile)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer

		var filteredLines []string
		linesRead := 0
		linesKept := 0

		for scanner.Scan() {
			line := scanner.Text()
			linesRead++

			keep := true

			// Apply key-value filters: if line contains the key, it must have the matching value
			for _, kv := range kvFilters {
				if kv.keyPattern.MatchString(line) {
					// Key found in this line — check if value matches
					if !kv.fullPattern.MatchString(line) {
						keep = false
						break
					}
				}
				// If key is NOT in the line, the line passes this filter (keep = true)
			}

			// Apply specific string filters: line must contain at least one
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
				filteredLines = append(filteredLines, line)
				linesKept++
			}
		}
		srcFile.Close()

		linesRemoved := linesRead - linesKept
		totalLinesKept += linesKept
		totalLinesRemoved += linesRemoved

		// Only write the file if there are lines to keep
		if linesKept > 0 {
			destFile, err := os.Create(destPath)
			if err != nil {
				logger.Warn("Failed to create filtered file %s: %v", relPath, err)
				continue
			}
			w := bufio.NewWriter(destFile)
			for _, line := range filteredLines {
				fmt.Fprintln(w, line)
			}
			w.Flush()
			destFile.Close()
			filesFiltered++

			if linesRemoved > 0 {
				logger.Debug("  Filtered %s: kept %d/%d lines (removed %d)", relPath, linesKept, linesRead, linesRemoved)
			}
		} else if linesRead > 0 {
			logger.Debug("  Skipped %s: all %d lines filtered out", relPath, linesRead)
		}
	}

	logger.Info("Message filtering complete:")
	logger.Info("  Files with filtered output: %d/%d", filesFiltered, len(logFiles))
	logger.Info("  Total lines kept: %d, removed: %d", totalLinesKept, totalLinesRemoved)
	logger.Info("  Filtered logs saved to: %s", filterBaseDir)

	return nil
}

// collectAppVersionsStandalone collects application version information and saves it locally
func collectAppVersionsStandalone(awsClient *ssh.Client, config *Config) error {
	if !config.AppVersionCollection.Enabled {
		logger.Info("App version collection is disabled in config")
		return nil
	}

	logger.Debug("Starting standalone application version collection...")

	// First, let's check what namespaces are available
	logger.Debug("Checking available namespaces...")
	nsSession, err := awsClient.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session for namespace check: %v", err)
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

	// Replace {timestamp} placeholder with actual timestamp
	timestampStr := time.Now().Format("20060102_150405")
	outputFileName = strings.ReplaceAll(outputFileName, "{timestamp}", timestampStr)

	// Use the output directory from logs config or current directory
	outputDir := config.Logs.OutputDir
	if outputDir == "" {
		outputDir = "."
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
		return fmt.Errorf("failed to create output directory %s: %v", outputDir, err)
	}

	// Write to local file
	if err := ioutil.WriteFile(localFilePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write app versions file %s: %v", localFilePath, err)
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

	return nil
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
	modeAll := flag.Bool("all", false, "Collect logs + system info + app versions (override config.yaml)")
	modeLogs := flag.Bool("logs-only", false, "Collect only logs without system info or app versions")
	modeSysInfo := flag.Bool("sys-info", false, "Collect only general system info (kubectl commands, system stats)")
	modeVersion := flag.Bool("version", false, "Collect only application version information")

	// Log collection configuration flags
	logFileName := flag.String("log-name", "", "Name for the log collection (without extension)")
	userID := flag.String("user-id", "", "User ID for log collection (defaults to bastion username)")
	timeDuration := flag.String("time-duration", "", "Collect logs from last X time (e.g., '15m', '30m', '1h', '2h'). Use '0' or 'disabled' to force full logs. Leave empty to use config setting")

	// Custom help message
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Operation Modes (mutually exclusive):\n")
		fmt.Fprintf(os.Stderr, "  (no mode)          Use config.yaml settings (default)\n")
		fmt.Fprintf(os.Stderr, "  --all              Collect logs + system info + app versions (override config)\n")
		fmt.Fprintf(os.Stderr, "  --logs-only        Collect only logs\n")
		fmt.Fprintf(os.Stderr, "  --sys-info         Collect only general system info (kubectl commands)\n")
		fmt.Fprintf(os.Stderr, "  --version          Collect only application version information\n\n")
		fmt.Fprintf(os.Stderr, "General Options:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

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

	if modeCount > 1 {
		fmt.Fprintln(os.Stderr, "Error: Only one operation mode can be specified at a time")
		fmt.Fprintln(os.Stderr, "Choose one of: --all, --logs-only, --sys-info, --version")
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
	var collectLogs, collectInfo, collectAppVersions bool
	var selectedMode string

	if *modeAll {
		// --all mode: override config and collect everything
		collectLogs = true
		collectInfo = true
		collectAppVersions = true
		selectedMode = "all"
	} else if *modeLogs {
		// --logs-only mode: collect only logs
		collectLogs = true
		collectInfo = false
		collectAppVersions = false
		selectedMode = "logs-only"
	} else if *modeSysInfo {
		// --sys-info mode: collect only general system info (kubectl commands)
		collectLogs = false
		collectInfo = true
		collectAppVersions = false
		selectedMode = "sys-info"
	} else if *modeVersion {
		// --version mode: collect only application version information
		collectLogs = false
		collectInfo = false
		collectAppVersions = true
		selectedMode = "version"
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
	}

	// Initialize the global logger with the log level setting
	logLevelEnum := ParseLogLevel(*logLevel)
	logger = NewLogger(logLevelEnum)
	defer logger.Close()

	// Log the selected operation mode
	logger.Debug("Selected operation mode: %s", selectedMode)

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

	// Decrypt password if it's encrypted
	var decryptedPassword string
	var passwordNeedsSaving bool

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

	if *bastionHost == "" {
		*bastionHost = config.Bastion.Host
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
	hasOperations := collectLogs || collectInfo || collectAppVersions || *listOnly

	if !hasOperations {
		fmt.Println("No operations requested.")
		fmt.Println("In config mode, enable operations in config.yaml:")
		fmt.Println("  logCollection.enabled: true")
		fmt.Println("  generalInfo.enabled: true")
		fmt.Println("  appVersionCollection.enabled: true")
		fmt.Println("")
		fmt.Println("Or use operation mode flags:")
		fmt.Println("  --all        : Collect logs + system info + app versions")
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
			// Display encrypted password for user to copy if needed
			encryptedPass, encErr := encryptPassword(*password)
			if encErr == nil {
				logger.Info("Password encrypted and saved to config.yaml")
				logger.Info("Encrypted Password (copy the below \"ENC:...\" if needed):")
				logger.Info("\"%s\"", encryptedPass)
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
		err = collectAppVersionsStandalone(awsClient, config)
		if err != nil {
			logger.Error("Failed to collect app versions: %v", err)
			return
		}
		logger.Info("App version collection completed successfully!")
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
			err = collectAppVersionsStandalone(awsClient, config)
			if err != nil {
				logger.Error("Failed to collect app versions: %v", err)
				return
			}
			logger.Info("App version collection completed successfully!")
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

		finalArchiveName, err = collectKubernetesLogs(awsClient, *logFileName, *userID, config.LogCollection.TempDir, config.LogCollection.CustomSources, config.LogCollection.UseTimestamp, config.LogCollection.TimestampFormat, config.Environment, config.Username, collectInfo, config.GeneralInfo, timeBasedEnabled, timeDurationStr, config.Options.MaxSSHSessions, config.LogCollection.AutoDeleteTempDir, config.LogCollection.TemporalWorkflowCollection)
		if err != nil {
			logger.Error("Failed to collect logs: %v", err)
			return
		}

		// Display overall timing
		overallDuration := time.Since(overallStartTime)
		logger.Info("Overall log collection and archiving completed in: %s", overallDuration.Round(time.Millisecond))

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
		err = collectAppVersionsStandalone(awsClient, config)
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
		logger.CopyLogFileTo(*outputDir)
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

	fmt.Println("\nDownload complete!")
}
