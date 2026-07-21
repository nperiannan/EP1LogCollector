package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ─────────────────────────────────────────────────────────────────────────────
// GUI Server — Web-based configuration and control panel for LogCollector
// Launched via: logcollector --gui [--gui-port 9090]
// ─────────────────────────────────────────────────────────────────────────────

// guiState holds the runtime state shared between HTTP handlers
type guiState struct {
	mu           sync.Mutex
	configPath   string
	config       *Config
	running      bool
	runOutput    []string
	runStatus    string // "idle", "running", "completed", "failed"
	runStartTime time.Time
	runEndTime   time.Time
	runExitCode  int
	cancelFunc   func()
	lastError    string
}

// newGUIState creates a fresh GUI state from the given config file path
func newGUIState(configPath string) *guiState {
	return &guiState{
		configPath: configPath,
		runStatus:  "idle",
		runOutput:  make([]string, 0),
	}
}

// startGUIServer starts the web-based GUI server on the specified port
func startGUIServer(configPath string, port int) error {
	state := newGUIState(configPath)

	// Load initial config
	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Printf("Warning: Could not load %s: %v (starting with empty config)\n", configPath, err)
		cfg = &Config{}
	}
	state.config = cfg

	mux := http.NewServeMux()

	// Static assets
	mux.HandleFunc("/", state.handleIndex)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// API endpoints
	mux.HandleFunc("/api/config", state.handleConfig)
	mux.HandleFunc("/api/config/save", state.handleConfigSave)
	mux.HandleFunc("/api/run", state.handleRun)
	mux.HandleFunc("/api/status", state.handleStatus)
	mux.HandleFunc("/api/output", state.handleOutput)
	mux.HandleFunc("/api/stop", state.handleStop)
	mux.HandleFunc("/api/validate", state.handleValidate)
	mux.HandleFunc("/api/logs/browse", state.handleLogsBrowse)
	mux.HandleFunc("/api/config/raw", state.handleConfigRaw)
	mux.HandleFunc("/api/config/update-section", state.handleUpdateSection)
	mux.HandleFunc("/api/analyze/ai", state.handleAIAnalysis)
	mux.HandleFunc("/api/analyze/files", state.handleAnalyzeFiles)
	mux.HandleFunc("/api/analyze/init", func(w http.ResponseWriter, r *http.Request) {
		aiDir := os.Getenv("LOGCOLLECTOR_AI_DIR")
		writeJSON(w, http.StatusOK, map[string]interface{}{"aiDir": aiDir, "standalone": aiDir != ""})
	})
	mux.HandleFunc("/api/jira/token", state.handleJiraToken)
	mux.HandleFunc("/api/bastion/password", state.handleBastionPassword)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to start GUI server on %s: %v", addr, err)
	}

	url := fmt.Sprintf("http://%s", addr)
	fmt.Printf("\n")
	fmt.Printf("╔══════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║           LogCollector GUI v%s (build %s)           ║\n", appVersion, buildNumber)
	fmt.Printf("╠══════════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║  URL:    %-49s ║\n", url)
	fmt.Printf("║  Config: %-49s ║\n", truncateStr(configPath, 49))
	fmt.Printf("╠══════════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║  Press Ctrl+C to stop the server                           ║\n")
	fmt.Printf("╚══════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")

	// Open browser automatically
	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser(url)
	}()

	server := &http.Server{Handler: mux}
	return server.Serve(listener)
}

// truncateStr truncates a string to max length with "..." suffix
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// openBrowser opens the default browser on the host OS
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// ─────────────────────────────────────────────────────────────────────────────
// API Handlers
// ─────────────────────────────────────────────────────────────────────────────

// handleConfig GET: returns the current config as JSON
func (s *guiState) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-read from disk to get latest
	cfg, err := LoadConfig(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.config = cfg

	// Build a GUI-friendly response
	resp := buildConfigResponse(cfg, s.configPath)
	writeJSON(w, http.StatusOK, resp)
}

// handleConfigSave POST: saves config changes back to config.yaml
func (s *guiState) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON: " + err.Error()})
		return
	}

	// Read the current YAML file content to preserve comments and formatting
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Cannot read config: " + err.Error()})
		return
	}

	content := string(data)

	// Apply each changed field by replacing lines in the YAML
	for key, value := range payload {
		content = replaceYAMLValue(content, key, value)
	}

	// Write back
	if err := os.WriteFile(s.configPath, []byte(content), 0600); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Cannot write config: " + err.Error()})
		return
	}

	// Reload config
	cfg, err := LoadConfig(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"saved":   true,
			"warning": "Config saved but has parse issues: " + err.Error(),
		})
		return
	}
	s.config = cfg

	writeJSON(w, http.StatusOK, map[string]interface{}{"saved": true})
}

// handleRun POST: starts a log collection run
func (s *guiState) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "A collection is already running"})
		return
	}

	var payload struct {
		Mode            string `json:"mode"`            // "all", "logs-only", "sys-info", "version", "device-logs", "database", "config"
		TimeDuration    string `json:"timeDuration"`    // e.g. "15m", "30m", "1h"
		LogLevel        string `json:"logLevel"`        // "DEBUG", "INFO", "WARN", "ERROR"
		OutputDir       string `json:"outputDir"`       // custom output directory
		JiraIssue       string `json:"jiraIssue"`       // JIRA issue ID
		BastionPassword string `json:"bastionPassword"` // bastion SSH password (passed via env var, not CLI)
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON: " + err.Error()})
		return
	}

	s.running = true
	s.runStatus = "running"
	s.runOutput = make([]string, 0, 500)
	s.runStartTime = time.Now()
	s.runEndTime = time.Time{}
	s.runExitCode = -1
	s.lastError = ""
	s.mu.Unlock()

	// Build the command arguments
	args := []string{"--config", s.configPath}

	switch payload.Mode {
	case "all":
		args = append(args, "--all")
	case "logs-only":
		args = append(args, "--logs-only")
	case "sys-info":
		args = append(args, "--sys-info")
	case "version":
		args = append(args, "--version")
	case "device-logs":
		args = append(args, "--device-logs")
	case "database":
		args = append(args, "--database")
		// "config" mode = no mode flag, use config.yaml settings
	}

	if payload.TimeDuration != "" {
		args = append(args, "--time-duration", payload.TimeDuration)
	}
	if payload.LogLevel != "" {
		args = append(args, "--log-level", payload.LogLevel)
	}
	if payload.OutputDir != "" {
		args = append(args, "--outdir", payload.OutputDir)
	}
	if payload.JiraIssue != "" {
		args = append(args, "--jira", payload.JiraIssue)
	}

	// Find our own executable
	exePath, err := os.Executable()
	if err != nil {
		s.mu.Lock()
		s.running = false
		s.runStatus = "failed"
		s.lastError = "Cannot find executable: " + err.Error()
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": s.lastError})
		return
	}

	// Build environment variables for credentials (never pass as CLI args)
	envVars := make([]string, 0)
	if payload.BastionPassword != "" {
		envVars = append(envVars, "BASTION_PASSWORD="+payload.BastionPassword)
	}

	// Start the collection as a subprocess
	go s.runCollection(exePath, args, envVars)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"started": true,
		"mode":    payload.Mode,
		"args":    args,
	})
}

// runCollection executes the logcollector binary and captures output
func (s *guiState) runCollection(exePath string, args []string, envVars []string) {
	cmd := exec.Command(exePath, args...)

	// Set working directory to the config file's directory
	absConfigPath, _ := filepath.Abs(s.configPath)
	cmd.Dir = filepath.Dir(absConfigPath)

	// Inherit current environment and add credential env vars
	cmd.Env = append(os.Environ(), envVars...)

	// Capture stdout and stderr separately
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.finishRun("failed", 1, "Failed to capture stdout: "+err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.finishRun("failed", 1, "Failed to capture stderr: "+err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		s.finishRun("failed", 1, "Failed to start: "+err.Error())
		return
	}

	// Store cancel function
	s.mu.Lock()
	s.cancelFunc = func() {
		_ = cmd.Process.Kill()
	}
	s.mu.Unlock()

	// Stream stdout and stderr in parallel goroutines
	var wg sync.WaitGroup
	readLines := func(r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			s.mu.Lock()
			s.runOutput = append(s.runOutput, line)
			if len(s.runOutput) > 5000 {
				s.runOutput = s.runOutput[len(s.runOutput)-5000:]
			}
			s.mu.Unlock()
		}
	}
	wg.Add(2)
	go readLines(stdout)
	go readLines(stderr)
	wg.Wait()

	exitCode := 0
	status := "completed"
	errMsg := ""
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
		status = "failed"
		errMsg = err.Error()
	}

	s.finishRun(status, exitCode, errMsg)
}

func (s *guiState) finishRun(status string, exitCode int, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.runStatus = status
	s.runEndTime = time.Now()
	s.runExitCode = exitCode
	s.lastError = errMsg
	s.cancelFunc = nil
}

// handleStatus GET: returns current run status
func (s *guiState) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	resp := map[string]interface{}{
		"status":    s.runStatus,
		"running":   s.running,
		"lineCount": len(s.runOutput),
		"exitCode":  s.runExitCode,
		"error":     s.lastError,
	}
	if !s.runStartTime.IsZero() {
		resp["startTime"] = s.runStartTime.Format(time.RFC3339)
	}
	if !s.runEndTime.IsZero() {
		resp["endTime"] = s.runEndTime.Format(time.RFC3339)
		resp["duration"] = s.runEndTime.Sub(s.runStartTime).String()
	} else if s.running {
		resp["elapsed"] = time.Since(s.runStartTime).String()
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleOutput GET: returns run output lines (supports ?from=N for polling)
func (s *guiState) handleOutput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	fromStr := r.URL.Query().Get("from")
	from := 0
	if fromStr != "" {
		fmt.Sscanf(fromStr, "%d", &from)
	}

	var lines []string
	if from < len(s.runOutput) {
		lines = s.runOutput[from:]
	} else {
		lines = []string{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"lines":      lines,
		"totalLines": len(s.runOutput),
		"status":     s.runStatus,
		"running":    s.running,
	})
}

// handleStop POST: stops a running collection
func (s *guiState) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running || s.cancelFunc == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"stopped": false, "reason": "No collection running"})
		return
	}

	s.cancelFunc()
	writeJSON(w, http.StatusOK, map[string]interface{}{"stopped": true})
}

// handleValidate POST: validates the current config without running
func (s *guiState) handleValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	configPath := s.configPath
	s.mu.Unlock()

	issues := validateConfig(configPath)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"valid":  len(issues) == 0,
		"issues": issues,
	})
}

// handleLogsBrowse GET: lists collected log directories
func (s *guiState) handleLogsBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()

	outputDir := cfg.Logs.OutputDir
	if outputDir == "" {
		outputDir = "."
	}
	// Strip template placeholders for browsing
	outputDir = strings.ReplaceAll(outputDir, "{timestamp}", "*")
	outputDir = strings.ReplaceAll(outputDir, "{username}", cfg.Username)
	outputDir = strings.ReplaceAll(outputDir, "{environment}", cfg.Environment)

	baseDir := filepath.Dir(outputDir)
	if baseDir == "" {
		baseDir = "."
	}

	var dirs []map[string]string
	entries, err := os.ReadDir(baseDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				info, _ := e.Info()
				modTime := ""
				if info != nil {
					modTime = info.ModTime().Format("2006-01-02 15:04:05")
				}
				dirs = append(dirs, map[string]string{
					"name":     e.Name(),
					"path":     filepath.Join(baseDir, e.Name()),
					"modified": modTime,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"baseDir": baseDir,
		"dirs":    dirs,
	})
}

// handleConfigRaw GET: returns raw YAML, POST: saves raw YAML
func (s *guiState) handleConfigRaw(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	configPath := s.configPath
	s.mu.Unlock()

	if r.Method == http.MethodGet {
		data, err := os.ReadFile(configPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"content": string(data)})
		return
	}

	if r.Method == http.MethodPost {
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON: " + err.Error()})
			return
		}

		// Validate YAML before saving
		var testCfg Config
		if err := yaml.Unmarshal([]byte(payload.Content), &testCfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"saved": false,
				"error": "Invalid YAML: " + err.Error(),
			})
			return
		}

		if err := os.WriteFile(configPath, []byte(payload.Content), 0600); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		// Reload config
		s.mu.Lock()
		cfg, _ := LoadConfig(configPath)
		if cfg != nil {
			s.config = cfg
		}
		s.mu.Unlock()

		writeJSON(w, http.StatusOK, map[string]interface{}{"saved": true})
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleUpdateSection POST: updates a full config section (devices, commands, etc.)
// by reading the config, patching the section, and writing back the full YAML.
func (s *guiState) handleUpdateSection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	configPath := s.configPath
	s.mu.Unlock()

	var payload struct {
		Section string          `json:"section"` // "devices", "systemInfoCommands"
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON: " + err.Error()})
		return
	}

	// Load current config
	cfg, err := LoadConfig(configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to load config: " + err.Error()})
		return
	}

	switch payload.Section {
	case "devices":
		var devices []NetworkDevice
		if err := json.Unmarshal(payload.Data, &devices); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid device data: " + err.Error()})
			return
		}
		cfg.DeviceLogCollection.Devices = devices

	case "systemInfoCommands":
		var commands []SystemInfoCommand
		if err := json.Unmarshal(payload.Data, &commands); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid command data: " + err.Error()})
			return
		}
		cfg.SystemInfo.Commands = commands

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Unknown section: " + payload.Section})
		return
	}

	// Marshal the full config back to YAML
	yamlData, err := yaml.Marshal(cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to marshal config: " + err.Error()})
		return
	}

	// However, marshaling loses comments. So instead, we read the raw file,
	// and do section-level replacement. For reliability, we'll use the full
	// re-marshal approach but read the original to preserve non-YAML content.
	//
	// Better approach: read raw file, find the section, replace it.
	rawData, err := os.ReadFile(configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to read config: " + err.Error()})
		return
	}

	rawContent := string(rawData)
	var newContent string

	switch payload.Section {
	case "devices":
		newContent = replaceSectionInYAML(rawContent, "devices", cfg.DeviceLogCollection.Devices)
	case "systemInfoCommands":
		newContent = replaceSectionInYAML(rawContent, "commands", cfg.SystemInfo.Commands)
	}

	if newContent == "" {
		// Fallback: write full re-marshaled YAML
		if err := os.WriteFile(configPath, yamlData, 0600); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to write config: " + err.Error()})
			return
		}
	} else {
		if err := os.WriteFile(configPath, []byte(newContent), 0600); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to write config: " + err.Error()})
			return
		}
	}

	// Reload config
	s.mu.Lock()
	newCfg, _ := LoadConfig(configPath)
	if newCfg != nil {
		s.config = newCfg
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{"saved": true})
}

// replaceSectionInYAML finds an array section in raw YAML by key name and replaces it
func replaceSectionInYAML(content string, sectionKey string, data interface{}) string {
	// Marshal the new data
	newYAML, err := yaml.Marshal(data)
	if err != nil {
		return ""
	}

	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines))

	// Find the section key (e.g., "  devices:" or "    commands:")
	sectionPattern := sectionKey + ":"
	inSection := false
	sectionIndent := -1
	sectionFound := false

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if !inSection {
			// Look for the section key
			if strings.HasPrefix(trimmed, sectionPattern) {
				indent := len(line) - len(strings.TrimLeft(line, " \t"))
				sectionIndent = indent
				sectionFound = true

				// Check if it's an inline empty array
				afterColon := strings.TrimSpace(trimmed[len(sectionPattern):])
				if afterColon == "[]" || afterColon == "" {
					// Found the section header
					result = append(result, line)
					inSection = true
					// If inline empty, just skip - we'll append new data
					if afterColon == "[]" {
						// Replace inline empty with expanded form
						result = result[:len(result)-1] // Remove the line we just added
						result = append(result, strings.Repeat(" ", indent)+sectionKey+":")
						// Insert new items
						for _, newLine := range indentYAML(string(newYAML), indent+2) {
							result = append(result, newLine)
						}
					}
					continue
				}
				// Section header with possible comment
				result = append(result, line)
				inSection = true
				continue
			}
			result = append(result, line)
		} else {
			// We're inside the section — skip lines until we find a line at same or lower indent
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				// Check if next non-empty, non-comment line is still in section
				result = append(result, line)
				continue
			}
			lineIndent := len(line) - len(strings.TrimLeft(line, " \t"))
			if lineIndent <= sectionIndent {
				// We've exited the section — insert new data before this line
				for _, newLine := range indentYAML(string(newYAML), sectionIndent+2) {
					result = append(result, newLine)
				}
				inSection = false
				result = append(result, line)
			}
			// else: skip this line (old section content)
		}
	}

	// If section was at end of file
	if inSection {
		for _, newLine := range indentYAML(string(newYAML), sectionIndent+2) {
			result = append(result, newLine)
		}
	}

	if !sectionFound {
		return "" // Section not found, use fallback
	}

	return strings.Join(result, "\n")
}

// indentYAML indents a multi-line YAML string to the specified level
func indentYAML(yamlStr string, indent int) []string {
	lines := strings.Split(strings.TrimRight(yamlStr, "\n"), "\n")
	prefix := strings.Repeat(" ", indent)
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			result = append(result, "")
		} else {
			result = append(result, prefix+line)
		}
	}
	return result
}

// ─────────────────────────────────────────────────────────────────────────────
// AI-Powered Log Analysis
// ─────────────────────────────────────────────────────────────────────────────

// handleAnalyzeFiles GET: lists log files in a directory for selection
func (s *guiState) handleAnalyzeFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	dir := r.URL.Query().Get("dir")
	if dir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dir parameter required"})
		return
	}

	// Resolve path
	absDir, err := filepath.Abs(dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid path: " + err.Error()})
		return
	}

	var files []map[string]interface{}
	err = filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() {
			// Include dirs for navigation but limit depth
			relPath, _ := filepath.Rel(absDir, path)
			depth := strings.Count(relPath, string(filepath.Separator))
			if depth > 3 {
				return filepath.SkipDir
			}
			return nil
		}

		// Only include text-like files and compressed archives
		ext := strings.ToLower(filepath.Ext(path))
		isLog := ext == ".log" || ext == ".txt" || ext == ".yaml" || ext == ".yml" ||
			ext == ".json" || ext == ".xml" || ext == ".csv" || ext == "" ||
			ext == ".gz" || ext == ".tgz" || ext == ".tar" ||
			strings.Contains(info.Name(), "log") || strings.Contains(info.Name(), "error") ||
			strings.Contains(info.Name(), "analysis") || strings.Contains(info.Name(), "info")

		if isLog && info.Size() > 0 && info.Size() < 50*1024*1024 { // skip >50MB
			relPath, _ := filepath.Rel(absDir, path)
			files = append(files, map[string]interface{}{
				"name":     info.Name(),
				"path":     path,
				"relPath":  relPath,
				"size":     info.Size(),
				"sizeStr":  formatSize(info.Size()),
				"modified": info.ModTime().Format("2006-01-02 15:04:05"),
			})
		}
		return nil
	})

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dir":   absDir,
		"files": files,
		"count": len(files),
	})
}

func formatSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// readGzFile reads and decompresses a .gz file, returning the content as string
func readGzFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip open: %v", err)
	}
	defer gz.Close()

	// Limit decompressed size to 10MB
	limited := io.LimitReader(gz, 10*1024*1024)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("gzip read: %v", err)
	}
	return string(data), nil
}

// extractTarGzContent reads a .tar.gz and returns a map of filename -> content (text files only)
func extractTarGzContent(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip open: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	result := make(map[string]string)
	totalSize := 0
	maxTotal := 10 * 1024 * 1024 // 10MB total limit

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, nil // Return what we have
		}

		// Skip directories and very large files
		if hdr.Typeflag == tar.TypeDir || hdr.Size > 5*1024*1024 {
			continue
		}

		// Only read text-like files
		ext := strings.ToLower(filepath.Ext(hdr.Name))
		name := filepath.Base(hdr.Name)
		isText := ext == ".log" || ext == ".txt" || ext == ".yaml" || ext == ".yml" ||
			ext == ".json" || ext == ".xml" || ext == ".csv" || ext == "" ||
			strings.Contains(name, "log") || strings.Contains(name, "info") ||
			strings.Contains(name, "error") || strings.Contains(name, "analysis")

		if !isText {
			continue
		}

		if totalSize+int(hdr.Size) > maxTotal {
			break // Stop if we'd exceed total limit
		}

		data, err := io.ReadAll(io.LimitReader(tr, 5*1024*1024))
		if err != nil {
			continue
		}

		totalSize += len(data)
		result[hdr.Name] = string(data)
	}

	return result, nil
}

// handleAIAnalysis POST: sends log content to an LLM for root cause analysis
func (s *guiState) handleAIAnalysis(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		Provider  string   `json:"provider"`  // "cli" (local Claude CLI) or "api" (OpenAI-compatible)
		APIKey    string   `json:"apiKey"`    // OpenAI-compatible API key (api provider only)
		APIURL    string   `json:"apiUrl"`    // API endpoint (default: OpenAI)
		Model     string   `json:"model"`     // Model name
		FilePaths []string `json:"filePaths"` // Paths to log files to analyze
		LogText   string   `json:"logText"`   // Or direct log text input
		Context   string   `json:"context"`   // Additional context about the failure
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON: " + err.Error()})
		return
	}

	// Default to the local Claude CLI when no provider is specified and no API key
	// is supplied. This uses the user's existing Claude Code login (no API key).
	if payload.Provider == "" {
		if payload.APIKey != "" {
			payload.Provider = "api"
		} else {
			payload.Provider = "cli"
		}
	}

	if payload.Provider == "api" {
		if payload.APIKey == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "API key is required. Set your OpenAI API key or compatible LLM endpoint key."})
			return
		}
		// Set API defaults
		if payload.APIURL == "" {
			payload.APIURL = "https://api.openai.com/v1/chat/completions"
		}
		if payload.Model == "" {
			payload.Model = "gpt-4o-mini"
		}
	} else {
		// Claude CLI provider
		if payload.Model == "" {
			payload.Model = "sonnet"
		}
	}

	// Gather log content
	var logContent strings.Builder

	// Read from files (supports plain text, .gz, .tar.gz)
	for _, fp := range payload.FilePaths {
		ext := strings.ToLower(filepath.Ext(fp))
		lowerName := strings.ToLower(filepath.Base(fp))

		if ext == ".gz" {
			// Handle .gz and .tar.gz files
			content, err := readGzFile(fp)
			if err != nil {
				logContent.WriteString(fmt.Sprintf("\n=== ERROR reading %s: %v ===\n", filepath.Base(fp), err))
				continue
			}
			if strings.HasSuffix(lowerName, ".tar.gz") || strings.HasSuffix(lowerName, ".tgz") {
				// Extract tar.gz — read each file inside
				extracted, err := extractTarGzContent(fp)
				if err != nil {
					// Fallback: just use raw decompressed content
					logContent.WriteString(fmt.Sprintf("\n=== FILE: %s (gz decompressed, tar extraction failed: %v) ===\n", filepath.Base(fp), err))
					if len(content) > 8192 {
						logContent.WriteString("... [truncated, showing last 8KB] ...\n")
						content = content[len(content)-8192:]
					}
					logContent.WriteString(content)
				} else {
					for name, fileContent := range extracted {
						logContent.WriteString(fmt.Sprintf("\n=== FILE: %s/%s (%d bytes) ===\n", filepath.Base(fp), name, len(fileContent)))
						text := fileContent
						if len(text) > 8192 {
							logContent.WriteString("... [truncated, showing last 8KB] ...\n")
							text = text[len(text)-8192:]
						}
						logContent.WriteString(text)
						logContent.WriteString("\n")
					}
				}
			} else {
				// Plain .gz file
				logContent.WriteString(fmt.Sprintf("\n=== FILE: %s (gz decompressed, %d bytes) ===\n", filepath.Base(fp), len(content)))
				if len(content) > 8192 {
					logContent.WriteString("... [truncated, showing last 8KB] ...\n")
					content = content[len(content)-8192:]
				}
				logContent.WriteString(content)
			}
			logContent.WriteString("\n")
		} else {
			// Plain text file
			data, err := os.ReadFile(fp)
			if err != nil {
				logContent.WriteString(fmt.Sprintf("\n=== ERROR reading %s: %v ===\n", fp, err))
				continue
			}
			logContent.WriteString(fmt.Sprintf("\n=== FILE: %s (%d bytes) ===\n", filepath.Base(fp), len(data)))
			content := string(data)
			if len(content) > 8192 {
				logContent.WriteString("... [truncated, showing last 8KB] ...\n")
				content = content[len(content)-8192:]
			}
			logContent.WriteString(content)
			logContent.WriteString("\n")
		}
	}

	// Or use direct text input
	if payload.LogText != "" {
		logContent.WriteString("\n=== USER-PROVIDED LOG TEXT ===\n")
		logContent.WriteString(payload.LogText)
	}

	if logContent.Len() == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No log content provided. Select files or paste log text."})
		return
	}

	// Truncate total to ~32KB to stay within token limits
	logStr := logContent.String()
	if len(logStr) > 32768 {
		logStr = logStr[len(logStr)-32768:]
	}

	// Build the analysis prompt
	systemPrompt := `You are an expert Site Reliability Engineer and DevOps troubleshooter. 
You analyze application logs, Kubernetes pod logs, network device diagnostics, database query results, and system information to identify root causes of failures.

Your analysis should include:
1. **Summary**: Brief description of what happened
2. **Root Cause**: The most likely root cause based on the evidence
3. **Evidence**: Specific log lines, error messages, or patterns that support your conclusion
4. **Impact**: What was affected (services, users, data)
5. **Recommended Actions**: Steps to fix the issue
6. **Prevention**: How to prevent this from happening again

Be specific — reference exact timestamps, error messages, service names, and pod names from the logs.
If the logs are insufficient for a definitive root cause, state what additional information would help.`

	userPrompt := "Analyze the following logs and identify the root cause of any failures or issues:\n\n"
	if payload.Context != "" {
		userPrompt += "**Additional Context from User**: " + payload.Context + "\n\n"
	}
	userPrompt += logStr

	// ── Claude CLI provider: run the locally-installed `claude` CLI in print mode.
	// Uses the user's existing Claude Code login — no API key required.
	if payload.Provider == "cli" {
		analysis, tokens, costUSD, err := runClaudeCLI(systemPrompt, userPrompt, payload.Model)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"analysis":   analysis,
			"model":      "claude (" + payload.Model + ")",
			"filesRead":  len(payload.FilePaths),
			"logSize":    len(logStr),
			"tokensUsed": tokens,
			"costUSD":    costUSD,
		})
		return
	}

	// Call the LLM API
	reqBody := map[string]interface{}{
		"model": payload.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": 0.3,
		"max_tokens":  4096,
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to build request: " + err.Error()})
		return
	}

	httpReq, err := http.NewRequest("POST", payload.APIURL, strings.NewReader(string(reqJSON)))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to create request: " + err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+payload.APIKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "API call failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to read API response: " + err.Error()})
		return
	}

	if resp.StatusCode != 200 {
		writeJSON(w, resp.StatusCode, map[string]string{
			"error": fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(body)),
		})
		return
	}

	// Parse OpenAI-compatible response
	var apiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to parse API response: " + err.Error()})
		return
	}

	if len(apiResp.Choices) == 0 {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "API returned no analysis"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"analysis":   apiResp.Choices[0].Message.Content,
		"model":      payload.Model,
		"filesRead":  len(payload.FilePaths),
		"logSize":    len(logStr),
		"tokensUsed": apiResp.Usage.TotalTokens,
	})
}

// handleJiraToken POST: securely stores the JIRA API token in the OS keychain
// (Windows Credential Manager). The token is keyed by the resolved JIRA email
// and is never written to config.yaml in plaintext.
func (s *guiState) handleJiraToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		Token string `json:"token"`
		Email string `json:"email"` // optional; falls back to the configured email
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON: " + err.Error()})
		return
	}

	token := strings.TrimSpace(payload.Token)
	if token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "API token is required"})
		return
	}

	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()

	// Resolve the email used as the keychain key, matching the runtime's
	// {username}/{environment} substitution so the token can be found later.
	email := strings.TrimSpace(payload.Email)
	if email == "" && cfg != nil {
		email = cfg.Jira.Email
	}
	if cfg != nil {
		email = strings.ReplaceAll(email, "{username}", cfg.Username)
		email = strings.ReplaceAll(email, "{environment}", cfg.Environment)
	}
	if strings.TrimSpace(email) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Set the JIRA email first — the token is stored per-email."})
		return
	}

	if err := storeJIRATokenInKeychain(email, token, nil); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "Failed to store token securely: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"saved": true, "email": email})
}

// handleBastionPassword POST: securely stores the bastion SSH password in the OS
// keychain (Windows Credential Manager), keyed by <username>@<bastionHost> to match
// the runtime retrieval in getBastionPassword. Never written to config.yaml.
func (s *guiState) handleBastionPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		Password string `json:"password"`
		Host     string `json:"host"`     // optional; falls back to configured bastion host
		Username string `json:"username"` // optional; falls back to configured username
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON: " + err.Error()})
		return
	}

	if payload.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Bastion password is required"})
		return
	}

	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()

	username := strings.TrimSpace(payload.Username)
	if username == "" && cfg != nil {
		username = cfg.Username
	}
	host := strings.TrimSpace(payload.Host)
	if host == "" && cfg != nil {
		host = cfg.Bastion.Host
	}
	if username == "" || host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Set the username and bastion host first — the password is stored per host."})
		return
	}

	if err := storeBastionPasswordInKeychain(username, host, payload.Password, nil); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "Failed to store password securely: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"saved": true, "target": username + "@" + host})
}

// claudeCLIPath locates the `claude` executable. It checks PATH first, then a
// couple of well-known install locations so the GUI works even if it was
// launched with a stripped-down PATH.
func claudeCLIPath() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "claude.exe"),
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude.exe"),
		filepath.Join(home, ".claude", "local", "claude"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// runClaudeCLI invokes the locally-installed `claude` CLI in non-interactive
// print mode (--print) and returns the analysis text. It uses the user's
// existing Claude Code login (OAuth/subscription) — no API key required.
// The system prompt is passed as an argument; the (potentially large) user
// prompt is piped via stdin to avoid command-line length limits.
func runClaudeCLI(systemPrompt, userPrompt, model string) (analysis string, tokens int, costUSD float64, err error) {
	bin := claudeCLIPath()
	if bin == "" {
		return "", 0, 0, fmt.Errorf("the 'claude' CLI was not found. Install Claude Code (https://claude.com/claude-code) and sign in, or switch the AI provider to API")
	}
	if model == "" {
		model = "sonnet"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	args := []string{
		"--print",
		"--output-format", "json",
		"--model", model,
		"--system-prompt", systemPrompt,
		"--permission-mode", "plan", // read-only: the model can never modify anything
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(userPrompt)
	// Run in a neutral directory so project CLAUDE.md / hooks don't leak into the analysis.
	cmd.Dir = os.TempDir()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if runErr := cmd.Run(); runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", 0, 0, fmt.Errorf("claude CLI timed out after 180s")
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = runErr.Error()
		}
		return "", 0, 0, fmt.Errorf("claude CLI failed: %s", detail)
	}

	var cliResp struct {
		IsError   bool    `json:"is_error"`
		Result    string  `json:"result"`
		TotalCost float64 `json:"total_cost_usd"`
		Usage     struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if jerr := json.Unmarshal(stdout.Bytes(), &cliResp); jerr != nil {
		raw := strings.TrimSpace(stdout.String())
		if len(raw) > 500 {
			raw = raw[:500] + "…"
		}
		return "", 0, 0, fmt.Errorf("failed to parse claude CLI output: %v (output: %s)", jerr, raw)
	}
	if cliResp.IsError {
		return "", 0, 0, fmt.Errorf("claude CLI returned an error: %s", cliResp.Result)
	}

	tokens = cliResp.Usage.InputTokens + cliResp.Usage.OutputTokens
	return cliResp.Result, tokens, cliResp.TotalCost, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Config helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildConfigResponse builds a GUI-friendly config representation
func buildConfigResponse(cfg *Config, configPath string) map[string]interface{} {
	// Build device list for GUI — full detail
	devices := make([]map[string]interface{}, 0)
	for _, d := range cfg.DeviceLogCollection.Devices {
		addCmds := make([]map[string]string, 0)
		for _, c := range d.Diagnostics.AdditionalCommands {
			addCmds = append(addCmds, map[string]string{"name": c.Name, "command": c.Command})
		}
		devices = append(devices, map[string]interface{}{
			"name":      d.Name,
			"type":      d.Type,
			"enabled":   d.Enabled,
			"ipAddress": d.IPAddress,
			"port":      d.Port,
			"username":  d.Username,
			"password":  d.Password,
			"diagnostics": map[string]interface{}{
				"enabled":            d.Diagnostics.Enabled,
				"useDefaults":        d.Diagnostics.UseDefaults,
				"additionalCommands": addCmds,
			},
			"logs": map[string]interface{}{
				"enabled":       d.Logs.Enabled,
				"fallbackFiles": d.Logs.FallbackFiles,
			},
		})
	}

	// Build database list — full detail with queries
	databases := make([]map[string]interface{}, 0)
	for _, db := range cfg.DatabaseCollection.Databases {
		queries := make([]map[string]interface{}, 0)
		for _, q := range db.Queries {
			queries = append(queries, map[string]interface{}{
				"name":       q.Name,
				"sql":        q.SQL,
				"parameters": q.Parameters,
			})
		}
		databases = append(databases, map[string]interface{}{
			"name":    db.Name,
			"alias":   db.Alias,
			"enabled": db.Enabled,
			"queries": queries,
		})
	}

	// Build system info commands
	sysCommands := make([]map[string]string, 0)
	for _, c := range cfg.SystemInfo.Commands {
		sysCommands = append(sysCommands, map[string]string{"name": c.Name, "command": c.Command})
	}

	// Build app version namespaces
	appVersionNS := make([]map[string]interface{}, 0)
	for _, ns := range cfg.AppVersionCollection.Namespaces {
		appVersionNS = append(appVersionNS, map[string]interface{}{
			"namespace":   ns.Namespace,
			"description": ns.Description,
			"podPrefixes": ns.PodPrefixes,
		})
	}

	// Build message filter key-value pairs
	kvFilters := make([]map[string]string, 0)
	for _, kv := range cfg.LogCollection.MessageFilter.KeyValueFilters {
		kvFilters = append(kvFilters, map[string]string{"key": kv.Key, "value": kv.Value})
	}

	// Build log analysis error patterns and groups
	errorGroups := make([]map[string]interface{}, 0)
	for _, g := range cfg.LogCollection.LogAnalysis.ErrorGroups {
		errorGroups = append(errorGroups, map[string]interface{}{
			"name":     g.Name,
			"patterns": g.Patterns,
			"severity": g.Severity,
		})
	}

	correlationKeys := make([]map[string]string, 0)
	for _, ck := range cfg.LogCollection.LogAnalysis.CorrelationKeys {
		correlationKeys = append(correlationKeys, map[string]string{"pattern": ck.Pattern, "type": ck.Type})
	}

	// Build pod file collections
	podFileColls := make([]map[string]interface{}, 0)
	for _, pf := range cfg.LogCollection.PodFileCollection.Collections {
		podFileColls = append(podFileColls, map[string]interface{}{
			"namespace":    pf.Namespace,
			"podPrefix":    pf.PodPrefix,
			"logPath":      pf.LogPath,
			"filePatterns": pf.FilePatterns,
			"matchPodName": pf.MatchPodName,
		})
	}

	// Determine whether a JIRA API token is already available (env var or OS
	// keychain) so the GUI can indicate it without ever exposing the value.
	jiraEmail := strings.ReplaceAll(cfg.Jira.Email, "{username}", cfg.Username)
	jiraEmail = strings.ReplaceAll(jiraEmail, "{environment}", cfg.Environment)
	jiraTokenStored := os.Getenv("JIRA_API_TOKEN") != ""
	if !jiraTokenStored && jiraEmail != "" {
		if tok, err := retrieveJIRATokenFromKeychain(jiraEmail, nil); err == nil && tok != "" {
			jiraTokenStored = true
		}
	}

	// Likewise for the bastion SSH password (env var, config file, or OS keychain).
	bastionPasswordStored := os.Getenv("BASTION_PASSWORD") != "" || cfg.Bastion.Password != ""
	if !bastionPasswordStored && cfg.Username != "" && cfg.Bastion.Host != "" {
		if pw, err := retrieveBastionPasswordFromKeychain(cfg.Username, cfg.Bastion.Host, nil); err == nil && pw != "" {
			bastionPasswordStored = true
		}
	}

	return map[string]interface{}{
		"configPath": configPath,
		"essentials": map[string]interface{}{
			"username":    cfg.Username,
			"environment": cfg.Environment,
			"envLoginId":  cfg.EnvLoginID,
			"ownerID":     cfg.OwnerID,
		},
		"bastion": map[string]interface{}{
			"host":           cfg.Bastion.Host,
			"port":           cfg.Bastion.Port,
			"keyPath":        getBastionKeyPath(cfg),
			"passwordStored": bastionPasswordStored,
		},
		"aws": map[string]interface{}{
			"host":    cfg.AWS.Host,
			"keyPath": cfg.AWS.KeyPath,
		},
		"logs": map[string]interface{}{
			"pattern":   cfg.Logs.Pattern,
			"outputDir": cfg.Logs.OutputDir,
		},
		"options": map[string]interface{}{
			"autoRetry":      cfg.Options.AutoRetry,
			"logLevel":       cfg.Options.LogLevel,
			"downloadMethod": cfg.Options.DownloadMethod,
			"numChunks":      cfg.Options.NumChunks,
			"maxSSHSessions": cfg.Options.MaxSSHSessions,
		},
		"logCollection": map[string]interface{}{
			"enabled":           cfg.LogCollection.Enabled,
			"defaultEP1Logs":    cfg.LogCollection.DefaultEP1Logs,
			"logFileName":       cfg.LogCollection.LogFileName,
			"tempDir":           cfg.LogCollection.TempDir,
			"useTimestamp":      cfg.LogCollection.UseTimestamp,
			"deleteAfterCopy":   cfg.LogCollection.DeleteAfterCopy,
			"autoDeleteTempDir": cfg.LogCollection.AutoDeleteTempDir,
			"timestampFormat":   cfg.LogCollection.TimestampFormat,
			"dynamicDeviceDetection": map[string]interface{}{
				"enabled":    cfg.LogCollection.DynamicDeviceDetection.Enabled,
				"maxDevices": cfg.LogCollection.DynamicDeviceDetection.MaxDevices,
			},
			"timeBasedCollection": map[string]interface{}{
				"enabled":  cfg.LogCollection.TimeBasedCollection.Enabled,
				"duration": cfg.LogCollection.TimeBasedCollection.Duration,
			},
			"messageFilter": map[string]interface{}{
				"enabled":              cfg.LogCollection.MessageFilter.Enabled,
				"filterDuringDownload": cfg.LogCollection.MessageFilter.FilterDuringDownload,
				"keyValueFilters":      kvFilters,
				"specificStrings":      cfg.LogCollection.MessageFilter.SpecificStrings,
				"combineReplicas":      cfg.LogCollection.MessageFilter.CombineReplicas,
				"replicaPattern":       cfg.LogCollection.MessageFilter.ReplicaPattern,
				"sortByTimestamp":      cfg.LogCollection.MessageFilter.SortByTimestamp,
			},
			"logAnalysis": map[string]interface{}{
				"enabled":           cfg.LogCollection.LogAnalysis.Enabled,
				"outputFile":        cfg.LogCollection.LogAnalysis.OutputFile,
				"errorPatterns":     cfg.LogCollection.LogAnalysis.ErrorPatterns,
				"excludeKeywords":   cfg.LogCollection.LogAnalysis.ExcludeKeywords,
				"maxMatches":        cfg.LogCollection.LogAnalysis.MaxMatches,
				"contextLines":      cfg.LogCollection.LogAnalysis.ContextLines,
				"correlationKeys":   correlationKeys,
				"timestampPatterns": cfg.LogCollection.LogAnalysis.TimestampPatterns,
				"errorGroups":       errorGroups,
			},
			"temporalWorkflowCollection": map[string]interface{}{
				"enabled":              cfg.LogCollection.TemporalWorkflowCollection.Enabled,
				"workflowIdPrefix":     cfg.LogCollection.TemporalWorkflowCollection.WorkflowIdPrefix,
				"numberOfWorkflows":    cfg.LogCollection.TemporalWorkflowCollection.NumberOfWorkflows,
				"namespace":            cfg.LogCollection.TemporalWorkflowCollection.Namespace,
				"kubeNamespace":        cfg.LogCollection.TemporalWorkflowCollection.KubeNamespace,
				"filterByOwnerID":      cfg.LogCollection.TemporalWorkflowCollection.FilterByOwnerID,
				"workflowActivitySets": cfg.LogCollection.TemporalWorkflowCollection.WorkflowActivitySets,
			},
			"temporalScheduleCollection": map[string]interface{}{
				"enabled":           cfg.LogCollection.TemporalScheduleCollection.Enabled,
				"numberOfSchedules": cfg.LogCollection.TemporalScheduleCollection.NumberOfSchedules,
				"namespace":         cfg.LogCollection.TemporalScheduleCollection.Namespace,
			},
			"podFileCollection": map[string]interface{}{
				"enabled":     cfg.LogCollection.PodFileCollection.Enabled,
				"collections": podFileColls,
			},
		},
		"systemInfo": map[string]interface{}{
			"enabled":        cfg.SystemInfo.Enabled,
			"outputDir":      cfg.SystemInfo.OutputDir,
			"commandTimeout": cfg.SystemInfo.CommandTimeout,
			"commands":       sysCommands,
		},
		"appVersionCollection": map[string]interface{}{
			"enabled":        cfg.AppVersionCollection.Enabled,
			"outputFileName": cfg.AppVersionCollection.OutputFileName,
			"printToLog":     cfg.AppVersionCollection.PrintToLog,
			"namespaces":     appVersionNS,
		},
		"jira": map[string]interface{}{
			"email":             cfg.Jira.Email,
			"attachmentEnabled": cfg.Jira.AttachmentEnabled,
			"baseUrl":           cfg.Jira.BaseURL,
			"tokenStored":       jiraTokenStored,
		},
		"deviceLogCollection": map[string]interface{}{
			"enabled":           cfg.DeviceLogCollection.Enabled,
			"outputDir":         cfg.DeviceLogCollection.OutputDir,
			"parallelDownloads": cfg.DeviceLogCollection.ParallelDownloads,
			"globalTimeout":     cfg.DeviceLogCollection.GlobalTimeout,
			"defaultNosLogFiles": map[string]interface{}{
				"enabled": cfg.DeviceLogCollection.DefaultNosLogFiles.Enabled,
			},
			"cliSettings": map[string]interface{}{
				"commandTimeout": cfg.DeviceLogCollection.CLISettings.CommandTimeout,
				"commandDelay":   cfg.DeviceLogCollection.CLISettings.CommandDelay,
			},
			"devices": devices,
		},
		"databaseCollection": map[string]interface{}{
			"enabled":      cfg.DatabaseCollection.Enabled,
			"outputDir":    cfg.DatabaseCollection.OutputDir,
			"queryTimeout": cfg.DatabaseCollection.QueryTimeout,
			"parameters":   cfg.DatabaseCollection.Parameters,
			"databases":    databases,
		},
	}
}

// getBastionKeyPath extracts the bastion key path from config.yaml raw text
// since the Config struct doesn't have a dedicated field for bastion keyPath
func getBastionKeyPath(cfg *Config) string {
	// This is a display-only field from config.yaml's bastion.keyPath
	return ""
}

// validateConfig checks the config file for common issues
func validateConfig(configPath string) []string {
	var issues []string

	data, err := os.ReadFile(configPath)
	if err != nil {
		return []string{"Cannot read config file: " + err.Error()}
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return []string{"YAML parse error: " + err.Error()}
	}

	// Check essentials
	if cfg.Username == "" {
		issues = append(issues, "CRITICAL: 'username' is empty — SSH connections will fail")
	}
	if cfg.Environment == "" {
		issues = append(issues, "CRITICAL: 'environment' is empty — AWS host cannot be resolved")
	}
	if cfg.Bastion.Host == "" {
		issues = append(issues, "CRITICAL: 'bastion.host' is empty — cannot connect to bastion")
	}
	if cfg.AWS.KeyPath == "" {
		issues = append(issues, "WARNING: 'aws.keyPath' is empty — AWS connections may fail")
	}
	if cfg.Bastion.Port == 0 {
		issues = append(issues, "INFO: 'bastion.port' not set — will default to 22")
	}
	if cfg.Logs.OutputDir == "" {
		issues = append(issues, "INFO: 'logs.outputDir' not set — logs will be saved in current directory")
	}

	// Check for unresolved template variables
	raw := string(data)
	if strings.Contains(raw, "{username}") && cfg.Username == "" {
		issues = append(issues, "WARNING: Template '{username}' used but username is empty")
	}
	if strings.Contains(raw, "{environment}") && cfg.Environment == "" {
		issues = append(issues, "WARNING: Template '{environment}' used but environment is empty")
	}

	// Check enabled sections
	enabledCount := 0
	if cfg.LogCollection.Enabled {
		enabledCount++
	}
	if cfg.SystemInfo.Enabled {
		enabledCount++
	}
	if cfg.AppVersionCollection.Enabled {
		enabledCount++
	}
	if cfg.DeviceLogCollection.Enabled {
		enabledCount++
	}
	if cfg.DatabaseCollection.Enabled {
		enabledCount++
	}
	if enabledCount == 0 {
		issues = append(issues, "WARNING: No collection sections are enabled in config")
	}

	return issues
}

// replaceYAMLValue replaces a specific field value in YAML content
// Supports dotted keys like "bastion.host" and top-level keys like "username"
func replaceYAMLValue(content string, dottedKey string, value interface{}) string {
	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines))

	// Track current indentation path to match nested keys
	indentStack := make([]int, 0)
	pathStack := make([]string, 0)

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Skip comments and empty lines
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			result = append(result, line)
			continue
		}

		// Calculate indentation
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		// Pop stack entries with >= indent level
		for len(indentStack) > 0 && indentStack[len(indentStack)-1] >= indent {
			indentStack = indentStack[:len(indentStack)-1]
			pathStack = pathStack[:len(pathStack)-1]
		}

		// Extract key from this line
		colonIdx := strings.Index(trimmed, ":")
		if colonIdx > 0 {
			lineKey := strings.TrimSpace(trimmed[:colonIdx])
			currentPath := append(pathStack, lineKey)
			fullKey := strings.Join(currentPath, ".")

			if fullKey == dottedKey {
				// Found the target — replace value after colon
				valueStr := formatYAMLValue(value)
				// Preserve inline comment if present
				afterColon := trimmed[colonIdx+1:]
				commentIdx := findInlineComment(afterColon)
				comment := ""
				if commentIdx >= 0 {
					comment = " " + strings.TrimSpace(afterColon[commentIdx:])
				}
				newLine := line[:indent] + lineKey + ": " + valueStr
				if comment != "" {
					newLine += " " + comment
				}
				result = append(result, newLine)
				continue
			}

			// If the value part is empty, this is a parent key (e.g., "bastion:")
			afterColon2 := strings.TrimSpace(trimmed[colonIdx+1:])
			if afterColon2 == "" || strings.HasPrefix(afterColon2, "#") {
				indentStack = append(indentStack, indent)
				pathStack = append(pathStack, lineKey)
			}
		}

		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// findInlineComment finds the position of an inline comment in a YAML value string
func findInlineComment(s string) int {
	inQuote := false
	quoteChar := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inQuote && (c == '"' || c == '\'') {
			inQuote = true
			quoteChar = c
		} else if inQuote && c == quoteChar {
			inQuote = false
		} else if !inQuote && c == '#' {
			return i
		}
	}
	return -1
}

// formatYAMLValue formats a Go value for YAML output
func formatYAMLValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		if val == "" {
			return `""`
		}
		// Quote strings that contain special characters
		if strings.ContainsAny(val, ":{}[]#&*!|>'\",@`\\") || strings.HasPrefix(val, " ") || strings.HasSuffix(val, " ") {
			return fmt.Sprintf(`"%s"`, strings.ReplaceAll(val, `"`, `\"`))
		}
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		if val == float64(int(val)) {
			return fmt.Sprintf("%d", int(val))
		}
		return fmt.Sprintf("%g", val)
	case nil:
		return `""`
	default:
		return fmt.Sprintf("%v", val)
	}
}

// writeJSON writes a JSON response
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// ─────────────────────────────────────────────────────────────────────────────
// HTML/CSS/JS — embedded single-page GUI
// ─────────────────────────────────────────────────────────────────────────────

func (s *guiState) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, guiHTML)
}

const guiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>LogCollector GUI</title>
<style>
:root {
  --bg: #0f172a;
  --bg2: #1e293b;
  --bg3: #334155;
  --fg: #e2e8f0;
  --fg2: #94a3b8;
  --accent: #3b82f6;
  --accent2: #2563eb;
  --success: #22c55e;
  --warn: #f59e0b;
  --error: #ef4444;
  --border: #475569;
  --radius: 8px;
  --font: 'Segoe UI', system-ui, -apple-system, sans-serif;
  --mono: 'Cascadia Code', 'Fira Code', 'Consolas', monospace;
}
* { margin: 0; padding: 0; box-sizing: border-box; }
body {
  font-family: var(--font);
  background: var(--bg);
  color: var(--fg);
  line-height: 1.5;
  min-height: 100vh;
}
a { color: var(--accent); text-decoration: none; }
a:hover { text-decoration: underline; }

/* Layout */
.app { display: flex; height: 100vh; }
.sidebar {
  width: 240px;
  background: var(--bg2);
  border-right: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  flex-shrink: 0;
}
.sidebar-header {
  padding: 20px 16px;
  border-bottom: 1px solid var(--border);
}
.sidebar-header h1 {
  font-size: 16px;
  font-weight: 700;
  color: var(--accent);
  display: flex;
  align-items: center;
  gap: 8px;
}
.sidebar-header .version { font-size: 11px; color: var(--fg2); font-weight: 400; }
.sidebar nav { flex: 1; padding: 8px; }
.nav-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px;
  border-radius: var(--radius);
  cursor: pointer;
  color: var(--fg2);
  font-size: 13px;
  transition: all 0.15s;
  user-select: none;
}
.nav-item:hover { background: var(--bg3); color: var(--fg); }
.nav-item.active { background: var(--accent2); color: #fff; }
.nav-item .icon { font-size: 16px; width: 20px; text-align: center; }
.nav-section { padding: 8px 12px 4px; font-size: 10px; text-transform: uppercase; letter-spacing: 1px; color: var(--fg2); margin-top: 8px; }

/* Main Content */
.main { flex: 1; overflow-y: auto; }
.main-header {
  position: sticky;
  top: 0;
  z-index: 10;
  background: var(--bg);
  border-bottom: 1px solid var(--border);
  padding: 16px 24px;
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.main-header h2 { font-size: 18px; font-weight: 600; }
.content { padding: 24px; }

/* Cards */
.card {
  background: var(--bg2);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  margin-bottom: 16px;
}
.card-header {
  padding: 14px 20px;
  border-bottom: 1px solid var(--border);
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.card-header h3 { font-size: 14px; font-weight: 600; }
.card-body { padding: 20px; }
.card-body.grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }

/* Forms */
.field { margin-bottom: 14px; }
.field label {
  display: block;
  font-size: 12px;
  color: var(--fg2);
  margin-bottom: 4px;
  font-weight: 500;
}
.field input, .field select {
  width: 100%;
  padding: 8px 12px;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: 6px;
  color: var(--fg);
  font-size: 13px;
  font-family: var(--font);
  outline: none;
  transition: border-color 0.15s;
}
.field input:focus, .field select:focus { border-color: var(--accent); }
.field input[type="checkbox"] { width: auto; margin-right: 8px; }
.field-row { display: flex; gap: 16px; }
.field-row .field { flex: 1; }
.field .hint { font-size: 11px; color: var(--fg2); margin-top: 2px; }

/* Toggle */
.toggle-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 0;
  border-bottom: 1px solid rgba(255,255,255,0.05);
}
.toggle-row:last-child { border-bottom: none; }
.toggle-row .label { font-size: 13px; }
.toggle-row .sublabel { font-size: 11px; color: var(--fg2); }
.toggle {
  position: relative;
  width: 40px;
  height: 22px;
  background: var(--bg3);
  border-radius: 11px;
  cursor: pointer;
  transition: background 0.2s;
}
.toggle.on { background: var(--accent); }
.toggle::after {
  content: '';
  position: absolute;
  top: 3px;
  left: 3px;
  width: 16px;
  height: 16px;
  background: #fff;
  border-radius: 50%;
  transition: transform 0.2s;
}
.toggle.on::after { transform: translateX(18px); }

/* Buttons */
.btn {
  padding: 8px 20px;
  border: none;
  border-radius: 6px;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
  transition: all 0.15s;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-family: var(--font);
}
.btn-primary { background: var(--accent); color: #fff; }
.btn-primary:hover { background: var(--accent2); }
.btn-success { background: var(--success); color: #fff; }
.btn-success:hover { background: #16a34a; }
.btn-danger { background: var(--error); color: #fff; }
.btn-danger:hover { background: #dc2626; }
.btn-outline { background: transparent; border: 1px solid var(--border); color: var(--fg); }
.btn-outline:hover { border-color: var(--accent); color: var(--accent); }
.btn:disabled { opacity: 0.5; cursor: not-allowed; }
.btn-group { display: flex; gap: 8px; }

/* Terminal Output */
.terminal {
  background: #0c0c0c;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  font-family: var(--mono);
  font-size: 12px;
  line-height: 1.6;
  padding: 16px;
  max-height: 500px;
  overflow-y: auto;
  white-space: pre-wrap;
  word-break: break-all;
  color: #cccccc;
}
.terminal .line-info { color: #3b82f6; }
.terminal .line-warn { color: #f59e0b; }
.terminal .line-error { color: #ef4444; }
.terminal .line-debug { color: #64748b; }

/* Status Badge */
.badge {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 3px 10px;
  border-radius: 12px;
  font-size: 11px;
  font-weight: 600;
}
.badge-idle { background: var(--bg3); color: var(--fg2); }
.badge-running { background: rgba(59,130,246,0.2); color: var(--accent); }
.badge-completed { background: rgba(34,197,94,0.2); color: var(--success); }
.badge-failed { background: rgba(239,68,68,0.2); color: var(--error); }

/* Validation */
.validation-item {
  padding: 8px 12px;
  border-radius: 6px;
  margin-bottom: 6px;
  font-size: 12px;
  display: flex;
  align-items: flex-start;
  gap: 8px;
}
.validation-item.critical { background: rgba(239,68,68,0.1); color: var(--error); }
.validation-item.warning { background: rgba(245,158,11,0.1); color: var(--warn); }
.validation-item.info { background: rgba(59,130,246,0.1); color: var(--accent); }
.validation-ok { color: var(--success); font-size: 14px; padding: 12px; text-align: center; }
.ai-file-row:hover { background: var(--bg3); }

/* Toast */
.toast-container { position: fixed; bottom: 20px; right: 20px; z-index: 1000; }
.toast {
  padding: 12px 20px;
  border-radius: var(--radius);
  margin-top: 8px;
  font-size: 13px;
  animation: slideIn 0.3s ease;
  box-shadow: 0 4px 12px rgba(0,0,0,0.3);
}
.toast-success { background: var(--success); color: #fff; }
.toast-error { background: var(--error); color: #fff; }
.toast-info { background: var(--accent); color: #fff; }
@keyframes slideIn { from { transform: translateX(100px); opacity: 0; } to { transform: translateX(0); opacity: 1; } }

/* Loading spinner */
.spinner {
  width: 16px;
  height: 16px;
  border: 2px solid var(--fg2);
  border-top-color: var(--accent);
  border-radius: 50%;
  animation: spin 0.8s linear infinite;
  display: inline-block;
}
@keyframes spin { to { transform: rotate(360deg); } }

/* Responsive */
@media (max-width: 900px) {
  .sidebar { width: 56px; }
  .sidebar .nav-item span, .sidebar-header .version, .sidebar .nav-section { display: none; }
  .sidebar-header h1 span { display: none; }
  .card-body.grid { grid-template-columns: 1fr; }
}
</style>
</head>
<body>
<div class="app">
  <!-- Sidebar -->
  <div class="sidebar">
    <div class="sidebar-header">
      <h1>&#x1F4CB; <span>LogCollector</span></h1>
      <div class="version">GUI Control Panel</div>
    </div>
    <nav>
      <div class="nav-item active" data-page="dashboard" onclick="showPage('dashboard')">
        <span class="icon">&#9776;</span><span>Dashboard</span>
      </div>
      <div class="nav-section">Configuration</div>
      <div class="nav-item" data-page="essentials" onclick="showPage('essentials')">
        <span class="icon">&#9881;</span><span>Essentials</span>
      </div>
      <div class="nav-item" data-page="ssh" onclick="showPage('ssh')">
        <span class="icon">&#128274;</span><span>SSH / Bastion</span>
      </div>
      <div class="nav-item" data-page="collection" onclick="showPage('collection')">
        <span class="icon">&#128230;</span><span>Collection</span>
      </div>
      <div class="nav-item" data-page="devices" onclick="showPage('devices')">
        <span class="icon">&#128187;</span><span>Devices</span>
      </div>
      <div class="nav-item" data-page="database" onclick="showPage('database')">
        <span class="icon">&#128451;</span><span>Database</span>
      </div>
      <div class="nav-item" data-page="jira" onclick="showPage('jira')">
        <span class="icon">&#127915;</span><span>JIRA</span>
      </div>
      <div class="nav-section">Execution</div>
      <div class="nav-item" data-page="run" onclick="showPage('run')">
        <span class="icon">&#9654;</span><span>Run</span>
      </div>
      <div class="nav-item" data-page="output" onclick="showPage('output')">
        <span class="icon">&#128196;</span><span>Output</span>
      </div>
      <div class="nav-section">Advanced</div>
      <div class="nav-item" data-page="aianalysis" onclick="showPage('aianalysis')">
        <span class="icon">&#129302;</span><span>AI Analysis</span>
      </div>
      <div class="nav-item" data-page="rawconfig" onclick="showPage('rawconfig')">
        <span class="icon">&#128221;</span><span>Raw Config</span>
      </div>
    </nav>
  </div>

  <!-- Main -->
  <div class="main">
    <div class="main-header">
      <h2 id="pageTitle">Dashboard</h2>
      <div id="statusBadge" class="badge badge-idle">&#9679; Idle</div>
    </div>
    <div class="content" id="pageContent">
      <!-- Pages rendered by JS -->
    </div>
  </div>
</div>

<div class="toast-container" id="toasts"></div>

<script>
// ═══════════════════════════════════════════════════════════════════════════
// State
// ═══════════════════════════════════════════════════════════════════════════
let config = {};
let currentPage = 'dashboard';
let pollTimer = null;
let outputLineCount = 0;
let isRunning = false;
let lastStatus = 'idle';

// ═══════════════════════════════════════════════════════════════════════════
// Init
// ═══════════════════════════════════════════════════════════════════════════
document.addEventListener('DOMContentLoaded', async () => {
  await loadConfig();
  // Check if launched in standalone AI analysis mode
  try {
    const initR = await fetch('/api/analyze/init');
    const initData = await initR.json();
    if (initData.standalone && initData.aiDir) {
      window._aiInitDir = initData.aiDir;
      showPage('aianalysis');
      startStatusPoll();
      return;
    }
  } catch(e) {}
  showPage('dashboard');
  startStatusPoll();
});

async function loadConfig() {
  try {
    const r = await fetch('/api/config');
    config = await r.json();
  } catch(e) {
    toast('Failed to load config: ' + e.message, 'error');
  }
}

// ═══════════════════════════════════════════════════════════════════════════
// Page Router
// ═══════════════════════════════════════════════════════════════════════════
function showPage(page) {
  currentPage = page;
  document.querySelectorAll('.nav-item').forEach(el => {
    el.classList.toggle('active', el.dataset.page === page);
  });
  const titles = {
    dashboard: 'Dashboard',
    essentials: 'Essential Settings',
    ssh: 'SSH / Bastion Configuration',
    collection: 'Log Collection Settings',
    devices: 'Network Devices',
    database: 'Database Queries',
    jira: 'JIRA Integration',
    run: 'Run Collection',
    output: 'Live Output',
    rawconfig: 'Raw Config Editor',
    aianalysis: 'AI Log Analysis'
  };
  document.getElementById('pageTitle').textContent = titles[page] || page;
  document.getElementById('pageContent').innerHTML = renderPage(page);

  if (page === 'output' || page === 'run') startOutputPoll();
  else stopOutputPoll();
}

function renderPage(page) {
  switch(page) {
    case 'dashboard': return renderDashboard();
    case 'essentials': return renderEssentials();
    case 'ssh': return renderSSH();
    case 'collection': return renderCollection();
    case 'devices': return renderDevices();
    case 'database': return renderDatabase();
    case 'jira': return renderJira();
    case 'run': return renderRun();
    case 'output': return renderOutput();
    case 'rawconfig': return renderRawConfig();
    case 'aianalysis': return renderAIAnalysis();
    default: return '<p>Page not found</p>';
  }
}

// ═══════════════════════════════════════════════════════════════════════════
// Dashboard
// ═══════════════════════════════════════════════════════════════════════════
function renderDashboard() {
  const e = config.essentials || {};
  const sections = [];
  if (config.logCollection?.enabled) sections.push('Kubernetes Logs');
  if (config.systemInfo?.enabled) sections.push('System Info');
  if (config.appVersionCollection?.enabled) sections.push('App Versions');
  if (config.deviceLogCollection?.enabled) sections.push('Device Logs');
  if (config.databaseCollection?.enabled) sections.push('Database');

  return ` + "`" + `
    <div class="card">
      <div class="card-header"><h3>Environment Overview</h3></div>
      <div class="card-body grid">
        <div>
          <div class="field"><label>Username</label><div style="font-size:15px;font-weight:600">${esc(e.username || '—')}</div></div>
          <div class="field"><label>Environment</label><div style="font-size:15px;font-weight:600">${esc(e.environment || '—')}</div></div>
        </div>
        <div>
          <div class="field"><label>Owner ID</label><div style="font-size:13px">${esc(e.ownerID || 'Auto-resolved from login')}</div></div>
          <div class="field"><label>Config File</label><div style="font-size:12px;color:var(--fg2)">${esc(config.configPath || '—')}</div></div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h3>Enabled Sections</h3>
        <button class="btn btn-outline" onclick="validateConfig()">&#9989; Validate Config</button>
      </div>
      <div class="card-body">
        ${sections.length ? sections.map(s => '<span style="display:inline-block;background:rgba(34,197,94,0.15);color:var(--success);padding:4px 12px;border-radius:12px;font-size:12px;margin:3px">' + s + '</span>').join('') : '<span style="color:var(--fg2)">No sections enabled</span>'}
        <div id="validationResults" style="margin-top:16px"></div>
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>Quick Actions</h3></div>
      <div class="card-body">
        <div class="btn-group" style="flex-wrap:wrap">
          <button class="btn btn-primary" onclick="quickRun('all')">&#9654; Collect All</button>
          <button class="btn btn-outline" onclick="quickRun('logs-only')">&#128220; Logs Only</button>
          <button class="btn btn-outline" onclick="quickRun('sys-info')">&#128187; System Info</button>
          <button class="btn btn-outline" onclick="quickRun('device-logs')">&#128268; Device Logs</button>
          <button class="btn btn-outline" onclick="quickRun('database')">&#128451; Database</button>
          <button class="btn btn-outline" onclick="quickRun('config')">&#9881; Config Mode</button>
        </div>
      </div>
    </div>
  ` + "`" + `;
}

// ═══════════════════════════════════════════════════════════════════════════
// Essentials Page
// ═══════════════════════════════════════════════════════════════════════════
function renderEssentials() {
  const e = config.essentials || {};
  return ` + "`" + `
    <div class="card">
      <div class="card-header"><h3>User & Environment</h3>
        <button class="btn btn-primary" onclick="saveEssentials()">&#128190; Save</button>
      </div>
      <div class="card-body">
        <div class="field-row">
          <div class="field">
            <label>Username</label>
            <input id="f_username" value="${esc(e.username)}" placeholder="e.g. nperiannan">
            <div class="hint">Corp username for SSH connections</div>
          </div>
          <div class="field">
            <label>Environment</label>
            <input id="f_environment" value="${esc(e.environment)}" placeholder="e.g. dl2r1, g2r1">
            <div class="hint">Target environment identifier</div>
          </div>
        </div>
        <div class="field-row">
          <div class="field">
            <label>Login Email (env_login_id)</label>
            <input id="f_envLoginId" value="${esc(e.envLoginId)}" placeholder="{username}@extremenetworks.com">
            <div class="hint">XIQ login email — auto-resolves ownerID from accountdb</div>
          </div>
          <div class="field">
            <label>Owner ID</label>
            <input id="f_ownerID" value="${esc(e.ownerID)}" placeholder="Auto-resolved or set manually">
            <div class="hint">Leave empty to auto-resolve from login email</div>
          </div>
        </div>
      </div>
    </div>
    <div class="card">
      <div class="card-header"><h3>Output Settings</h3></div>
      <div class="card-body">
        <div class="field-row">
          <div class="field">
            <label>Output Directory</label>
            <input id="f_outputDir" value="${esc(config.logs?.outputDir)}" placeholder="C:\\Logs\\{timestamp}">
            <div class="hint">Supports {timestamp}, {username}, {environment} placeholders</div>
          </div>
          <div class="field">
            <label>Download Method</label>
            <select id="f_downloadMethod">
              <option value="scp" ${config.options?.downloadMethod === 'scp' ? 'selected' : ''}>SCP (faster)</option>
              <option value="sftp" ${config.options?.downloadMethod === 'sftp' ? 'selected' : ''}>SFTP (parallel)</option>
            </select>
          </div>
        </div>
        <div class="field-row">
          <div class="field">
            <label>Log Level</label>
            <select id="f_logLevel">
              <option value="DEBUG" ${config.options?.logLevel === 'DEBUG' ? 'selected' : ''}>DEBUG</option>
              <option value="INFO" ${config.options?.logLevel === 'INFO' || !config.options?.logLevel ? 'selected' : ''}>INFO</option>
              <option value="WARN" ${config.options?.logLevel === 'WARN' ? 'selected' : ''}>WARN</option>
              <option value="ERROR" ${config.options?.logLevel === 'ERROR' ? 'selected' : ''}>ERROR</option>
            </select>
          </div>
          <div class="field">
            <label>Max SSH Sessions</label>
            <input id="f_maxSSH" type="number" min="1" max="4" value="${config.options?.maxSSHSessions || 2}">
          </div>
        </div>
      </div>
    </div>
  ` + "`" + `;
}

function saveEssentials() {
  saveFields({
    'username': val('f_username'),
    'environment': val('f_environment'),
    'env_login_id': val('f_envLoginId'),
    'ownerID': val('f_ownerID'),
  });
}

// ═══════════════════════════════════════════════════════════════════════════
// SSH Page
// ═══════════════════════════════════════════════════════════════════════════
function renderSSH() {
  const b = config.bastion || {};
  const a = config.aws || {};
  return ` + "`" + `
    <div class="card">
      <div class="card-header"><h3>Bastion Host</h3>
        <button class="btn btn-primary" onclick="saveSSH()">&#128190; Save</button>
      </div>
      <div class="card-body">
        <div class="field-row">
          <div class="field">
            <label>Host</label>
            <input id="f_bastionHost" value="${esc(b.host)}" placeholder="usnc-awsgtwy-02.extremenetworks.com">
          </div>
          <div class="field">
            <label>Port</label>
            <input id="f_bastionPort" type="number" value="${b.port || 22}" min="1" max="65535">
          </div>
        </div>
        <div class="field">
          <label>SSH Key Path (for bastion login)</label>
          <input id="f_bastionKeyPath" value="${esc(b.keyPath)}" placeholder="C:\\Users\\{username}\\.ssh\\id_ed25519_bastion">
          <div class="hint">Path to SSH private key. Leave empty to use password auth (Windows Credential Manager)</div>
        </div>
        <div class="field">
          <label>Bastion Password</label>
          <input id="f_bastionPassword" type="password" autocomplete="off" placeholder="${b.passwordStored ? '•••••• password saved — type a new one to replace' : 'Enter bastion password to save it securely'}">
          <div class="hint" style="margin-top:6px">${b.passwordStored ? '✅ A password is already saved. ' : ''}Stored securely in Windows Credential Manager (keyed by username@host) — never written to config.yaml. Used automatically when no SSH key is set. Leave blank to keep the existing password.</div>
        </div>
      </div>
    </div>
    <div class="card">
      <div class="card-header"><h3>AWS / Cloud Console</h3></div>
      <div class="card-body">
        <div class="field">
          <label>Host</label>
          <input id="f_awsHost" value="${esc(a.host)}" placeholder="{environment}-console.qa.xcloudiq.com">
          <div class="hint">Template: {environment} will be replaced at runtime</div>
        </div>
        <div class="field">
          <label>SSH Key Path (on bastion, for AWS login)</label>
          <input id="f_awsKeyPath" value="${esc(a.keyPath)}" placeholder="~/.ssh/id_ed25519_26Q1">
          <div class="hint">Path to the SSH key on the bastion host that grants access to AWS console</div>
        </div>
      </div>
    </div>
  ` + "`" + `;
}

async function saveSSH() {
  const password = val('f_bastionPassword');

  // Persist the bastion password securely (OS keychain) — only if one was entered.
  if (password) {
    try {
      const r = await fetch('/api/bastion/password', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({ password: password, host: val('f_bastionHost') })
      });
      const data = await r.json();
      if (data.saved) {
        toast('Bastion password stored securely for ' + (data.target || 'bastion'), 'success');
        const pf = document.getElementById('f_bastionPassword');
        if (pf) { pf.value = ''; pf.placeholder = '•••••• password saved — type a new one to replace'; }
      } else {
        toast('Password not saved: ' + (data.error || 'Unknown error'), 'error');
      }
    } catch(e) {
      toast('Password save error: ' + e.message, 'error');
    }
  }

  saveFields({
    'bastion.host': val('f_bastionHost'),
    'bastion.port': parseInt(val('f_bastionPort')) || 22,
    'aws.host': val('f_awsHost'),
    'aws.keyPath': val('f_awsKeyPath'),
  });
}

// ═══════════════════════════════════════════════════════════════════════════
// Collection Page
// ═══════════════════════════════════════════════════════════════════════════
function renderCollection() {
  const lc = config.logCollection || {};
  const si = config.systemInfo || {};
  const av = config.appVersionCollection || {};
  const tw = lc.temporalWorkflowCollection || {};
  const ts = lc.temporalScheduleCollection || {};
  const mf = lc.messageFilter || {};
  const la = lc.logAnalysis || {};
  const pfc = lc.podFileCollection || {};
  return ` + "`" + `
    <div class="card">
      <div class="card-header"><h3>Collection Sections</h3>
        <button class="btn btn-primary" onclick="saveCollection()">&#128190; Save Toggles</button>
      </div>
      <div class="card-body">
        <div class="toggle-row">
          <div><div class="label">Kubernetes Log Collection</div><div class="sublabel">Pod logs, temporal workflows, pod files</div></div>
          <div class="toggle ${lc.enabled ? 'on' : ''}" id="t_logCollection" onclick="toggleEl(this)"></div>
        </div>
        <div class="toggle-row">
          <div><div class="label">Default EP1 Logs</div><div class="sublabel">Built-in log sources + temporal data</div></div>
          <div class="toggle ${lc.defaultEP1Logs ? 'on' : ''}" id="t_defaultEP1" onclick="toggleEl(this)"></div>
        </div>
        <div class="toggle-row">
          <div><div class="label">System Info</div><div class="sublabel">kubectl commands (pods, services, deployments)</div></div>
          <div class="toggle ${si.enabled ? 'on' : ''}" id="t_systemInfo" onclick="toggleEl(this)"></div>
        </div>
        <div class="toggle-row">
          <div><div class="label">App Version Collection</div><div class="sublabel">Running container image versions</div></div>
          <div class="toggle ${av.enabled ? 'on' : ''}" id="t_appVersion" onclick="toggleEl(this)"></div>
        </div>
        <div class="toggle-row">
          <div><div class="label">Device Log Collection</div><div class="sublabel">EXOS/VOSS switch diagnostics and log files</div></div>
          <div class="toggle ${config.deviceLogCollection?.enabled ? 'on' : ''}" id="t_deviceLogs" onclick="toggleEl(this)"></div>
        </div>
        <div class="toggle-row">
          <div><div class="label">Database Collection</div><div class="sublabel">PostgreSQL queries via SSH tunneling</div></div>
          <div class="toggle ${config.databaseCollection?.enabled ? 'on' : ''}" id="t_database" onclick="toggleEl(this)"></div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>Log Collection Options</h3></div>
      <div class="card-body">
        <div class="field-row">
          <div class="field">
            <label>Log File Name</label>
            <input id="f_logFileName" value="${esc(lc.logFileName)}" placeholder="app_ep1_logs">
          </div>
          <div class="field">
            <label>Time-Based Duration</label>
            <input id="f_duration" value="${esc(lc.timeBasedCollection?.duration)}" placeholder='e.g. 15m, 30m, 1h (empty=full)'>
            <div class="hint">Leave empty for full logs</div>
          </div>
        </div>
        <div class="toggle-row">
          <div><div class="label">Dynamic Device Detection</div><div class="sublabel">Auto-detect devices from configdb by ownerID (max: ${lc.dynamicDeviceDetection?.maxDevices || 3})</div></div>
          <div class="toggle ${lc.dynamicDeviceDetection?.enabled ? 'on' : ''}" id="t_dynDevice" onclick="toggleEl(this)"></div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>Temporal Workflow Collection</h3></div>
      <div class="card-body">
        <div class="toggle-row">
          <div><div class="label">Enabled</div><div class="sublabel">Collect workflow execution data</div></div>
          <div class="toggle ${tw.enabled ? 'on' : ''}" id="t_temporal" onclick="toggleEl(this)"></div>
        </div>
        <div class="field-row" style="margin-top:12px">
          <div class="field">
            <label>Workflow ID Prefix</label>
            <input value="${esc(tw.workflowIdPrefix)}" placeholder="(empty = all workflows)" disabled>
            <div class="hint">Filter workflows by ID prefix</div>
          </div>
          <div class="field">
            <label>Number of Workflows</label>
            <input type="number" value="${tw.numberOfWorkflows || 3}" min="1" max="20" disabled>
          </div>
        </div>
        <div class="field-row">
          <div class="field">
            <label>Namespace</label>
            <input value="${esc(tw.namespace || 'configuration')}" disabled>
          </div>
          <div class="field">
            <label>Kube Namespace</label>
            <input value="${esc(tw.kubeNamespace || 'common')}" disabled>
            <div class="hint">Kubernetes namespace hosting the temporal-admintools pod</div>
          </div>
        </div>
        <div class="field-row">
          <div class="field">
            <label>Filter by Owner ID</label>
            <input value="${tw.filterByOwnerID ? 'Enabled' : 'Disabled'}" disabled>
            <div class="hint">Query workflows by resolved ownerID (OwnerId="...")</div>
          </div>
          <div class="field">
            <label>Workflow Activity Sets</label>
            <input value="${esc(Object.keys(tw.workflowActivitySets || {}).join(', ') || '(none)')}" disabled>
            <div class="hint">Per-workflow-type activities to collect, keyed by workflow ID prefix</div>
          </div>
        </div>
        <div class="hint" style="margin-top:4px;color:var(--accent)">&#9998; Edit these fields in <a href="#" onclick="showPage('rawconfig');return false" style="color:var(--accent)">Raw Config</a> &gt; temporalWorkflowCollection</div>
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>Temporal Schedule Collection</h3></div>
      <div class="card-body">
        <div class="toggle-row">
          <div><div class="label">Enabled</div><div class="sublabel">Collect schedule information</div></div>
          <div class="toggle ${ts.enabled ? 'on' : ''}" onclick="toggleEl(this)"></div>
        </div>
        <div class="field-row" style="margin-top:12px">
          <div class="field"><label>Number of Schedules</label><input type="number" value="${ts.numberOfSchedules || 5}" disabled></div>
          <div class="field"><label>Namespace</label><input value="${esc(ts.namespace || 'configuration')}" disabled></div>
        </div>
        <div class="hint" style="margin-top:4px;color:var(--accent)">&#9998; Edit in <a href="#" onclick="showPage('rawconfig');return false" style="color:var(--accent)">Raw Config</a></div>
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>Message Filtering</h3></div>
      <div class="card-body">
        <div class="toggle-row">
          <div><div class="label">Enabled</div><div class="sublabel">Filter logs by key-value pairs and specific strings</div></div>
          <div class="toggle ${mf.enabled ? 'on' : ''}" id="t_msgFilter" onclick="toggleEl(this)"></div>
        </div>
        <div class="toggle-row">
          <div><div class="label">Filter During Download</div><div class="sublabel">Apply grep filters during kubectl logs (faster)</div></div>
          <div class="toggle ${mf.filterDuringDownload ? 'on' : ''}" onclick="toggleEl(this)"></div>
        </div>
        <div class="toggle-row">
          <div><div class="label">Combine Replicas</div><div class="sublabel">Merge replica pod logs into single file per service</div></div>
          <div class="toggle ${mf.combineReplicas ? 'on' : ''}" onclick="toggleEl(this)"></div>
        </div>
        <div style="margin-top:12px"><label style="font-size:12px;color:var(--fg2);font-weight:500">Key-Value Filters</label></div>
        ${renderItemList(mf.keyValueFilters || [], [{label:'Key',key:'key'},{label:'Value',key:'value'}], 'No key-value filters')}
        <div style="margin-top:12px"><label style="font-size:12px;color:var(--fg2);font-weight:500">Specific Strings</label></div>
        <div style="font-size:12px;padding:4px 0">${(mf.specificStrings || []).length ? (mf.specificStrings || []).map(s => '<code style="background:var(--bg);padding:2px 6px;border-radius:4px;margin:2px;display:inline-block">' + esc(s) + '</code>').join('') : '<span style="color:var(--fg2)">None configured</span>'}</div>
        <div class="hint" style="margin-top:8px;color:var(--accent)">&#9998; Edit filter lists in <a href="#" onclick="showPage('rawconfig');return false" style="color:var(--accent)">Raw Config</a> &gt; messageFilter</div>
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>Log Analysis</h3></div>
      <div class="card-body">
        <div class="toggle-row">
          <div><div class="label">Enabled</div><div class="sublabel">Automatic error detection and correlation</div></div>
          <div class="toggle ${la.enabled ? 'on' : ''}" id="t_logAnalysis" onclick="toggleEl(this)"></div>
        </div>
        <div class="field-row" style="margin-top:12px">
          <div class="field"><label>Max Matches</label><input type="number" value="${la.maxMatches || 99}" disabled></div>
          <div class="field"><label>Context Lines</label><input type="number" value="${la.contextLines || 2}" disabled></div>
        </div>
        <div style="margin-top:12px"><label style="font-size:12px;color:var(--fg2);font-weight:500">Error Patterns (${(la.errorPatterns || []).length})</label></div>
        <div style="font-size:11px;padding:4px 0;max-height:80px;overflow-y:auto">${(la.errorPatterns || []).map(p => '<code style="background:var(--bg);padding:1px 5px;border-radius:3px;margin:1px;display:inline-block">' + esc(p) + '</code>').join(' ') || '<span style="color:var(--fg2)">None</span>'}</div>
        <div style="margin-top:8px"><label style="font-size:12px;color:var(--fg2);font-weight:500">Exclude Keywords (${(la.excludeKeywords || []).length})</label></div>
        <div style="font-size:11px;padding:4px 0;max-height:60px;overflow-y:auto">${(la.excludeKeywords || []).map(p => '<code style="background:var(--bg);padding:1px 5px;border-radius:3px;margin:1px;display:inline-block">' + esc(p) + '</code>').join(' ') || '<span style="color:var(--fg2)">None</span>'}</div>
        <div style="margin-top:8px"><label style="font-size:12px;color:var(--fg2);font-weight:500">Error Groups (${(la.errorGroups || []).length})</label></div>
        ${renderItemList(la.errorGroups || [], [{label:'Name',key:'name'},{label:'Severity',key:'severity'},{label:'Patterns',key:'patterns'}], 'No error groups')}
        <div style="margin-top:8px"><label style="font-size:12px;color:var(--fg2);font-weight:500">Correlation Keys (${(la.correlationKeys || []).length})</label></div>
        ${renderItemList(la.correlationKeys || [], [{label:'Type',key:'type'},{label:'Pattern',key:'pattern',mono:true}], 'No correlation keys')}
        <div class="hint" style="margin-top:8px;color:var(--accent)">&#9998; Edit patterns/groups in <a href="#" onclick="showPage('rawconfig');return false" style="color:var(--accent)">Raw Config</a> &gt; logAnalysis</div>
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>Pod File Collection</h3></div>
      <div class="card-body">
        <div class="toggle-row">
          <div><div class="label">Enabled</div><div class="sublabel">Collect files from inside running pods</div></div>
          <div class="toggle ${pfc.enabled ? 'on' : ''}" onclick="toggleEl(this)"></div>
        </div>
        ${renderItemList(pfc.collections || [], [{label:'Namespace',key:'namespace'},{label:'Pod Prefix',key:'podPrefix'},{label:'Log Path',key:'logPath'},{label:'File Patterns',key:'filePatterns'}], 'No pod file collections configured')}
        <div class="hint" style="margin-top:8px;color:var(--accent)">&#9998; Edit in <a href="#" onclick="showPage('rawconfig');return false" style="color:var(--accent)">Raw Config</a> &gt; podFileCollection</div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h3>System Info Commands (${(si.commands || []).length})</h3>
        <button class="btn btn-primary" onclick="saveSysCommands()">&#128190; Save Commands</button>
      </div>
      <div class="card-body">
        <div class="field-row">
          <div class="field"><label>Command Timeout (seconds)</label><input type="number" value="${si.commandTimeout || 180}" disabled></div>
          <div class="field"><label>Output Directory</label><input value="${esc(si.outputDir)}" disabled></div>
        </div>
        <div style="margin-top:12px">
          <label style="font-size:12px;color:var(--fg2);font-weight:500;display:block;margin-bottom:6px">kubectl / System Commands <span style="font-weight:400">(one per line: name: command)</span></label>
          <textarea id="sysCommandsEditor" rows="10" style="width:100%;background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:10px;color:var(--fg);font-family:var(--mono);font-size:11px;line-height:1.6;resize:vertical" spellcheck="false" placeholder="e.g.&#10;kubectl_get_pods: kubectl get pods -n {environment} -o wide&#10;kubectl_top_nodes: kubectl top nodes">${(si.commands || []).map(c => c.name + ': ' + c.command).join('\n')}</textarea>
          <div class="hint">Supports {environment}, {username} placeholders. These are read-only observation commands (kubectl get, describe, top).</div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>App Version Namespaces (${(av.namespaces || []).length})</h3></div>
      <div class="card-body">
        ${renderItemList(av.namespaces || [], [{label:'Namespace',key:'namespace'},{label:'Description',key:'description'},{label:'Pod Prefixes',key:'podPrefixes'}], 'No namespaces configured')}
        <div class="hint" style="margin-top:8px;color:var(--accent)">&#9998; Edit in <a href="#" onclick="showPage('rawconfig');return false" style="color:var(--accent)">Raw Config</a> &gt; appVersionCollection.namespaces</div>
      </div>
    </div>
  ` + "`" + `;
}

async function saveSysCommands() {
  const editor = document.getElementById('sysCommandsEditor');
  if (!editor) return;
  const commands = editor.value.split('\n').filter(l => l.trim() && l.includes(':')).map(l => {
    const [n, ...rest] = l.split(':');
    return { name: n.trim(), command: rest.join(':').trim() };
  });
  try {
    const r = await fetch('/api/config/update-section', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ section: 'systemInfoCommands', data: commands })
    });
    const data = await r.json();
    if (data.saved) { toast('System commands saved (' + commands.length + ' commands)', 'success'); await loadConfig(); }
    else { toast('Save failed: ' + (data.error || 'Unknown'), 'error'); }
  } catch(e) { toast('Error: ' + e.message, 'error'); }
}

function saveCollection() {
  saveFields({
    'logCollection.enabled': isToggleOn('t_logCollection'),
    'logCollection.defaultEP1Logs': isToggleOn('t_defaultEP1'),
    'systemInfo.enabled': isToggleOn('t_systemInfo'),
    'appVersionCollection.enabled': isToggleOn('t_appVersion'),
    'deviceLogCollection.enabled': isToggleOn('t_deviceLogs'),
    'databaseCollection.enabled': isToggleOn('t_database'),
  });
}

// ═══════════════════════════════════════════════════════════════════════════
// Devices Page
// ═══════════════════════════════════════════════════════════════════════════
function renderDevices() {
  const dl = config.deviceLogCollection || {};
  const devices = dl.devices || [];
  const cli = dl.cliSettings || {};
  return ` + "`" + `
    <div class="card">
      <div class="card-header"><h3>Device Collection Settings</h3></div>
      <div class="card-body">
        <div class="toggle-row">
          <div><div class="label">Parallel Downloads</div><div class="sublabel">Download from multiple devices concurrently</div></div>
          <div class="toggle ${dl.parallelDownloads ? 'on' : ''}" onclick="toggleEl(this)"></div>
        </div>
        <div class="toggle-row">
          <div><div class="label">Default NOS Log Files</div><div class="sublabel">Use built-in EXOS/VOSS log file paths</div></div>
          <div class="toggle ${dl.defaultNosLogFiles?.enabled ? 'on' : ''}" onclick="toggleEl(this)"></div>
        </div>
        <div class="field-row" style="margin-top:12px">
          <div class="field"><label>Global Timeout (sec)</label><input type="number" value="${dl.globalTimeout || 600}" disabled></div>
          <div class="field"><label>CLI Command Timeout (sec)</label><input type="number" value="${cli.commandTimeout || 180}" disabled></div>
          <div class="field"><label>Command Delay (sec)</label><input type="number" value="${cli.commandDelay || 1}" disabled></div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h3>Network Devices (${devices.length})</h3>
        <div class="btn-group">
          <button class="btn btn-outline" onclick="addDevice()">+ Add Device</button>
          <button class="btn btn-primary" onclick="saveDevices()">&#128190; Save All Devices</button>
        </div>
      </div>
      <div class="card-body" id="devicesContainer">
        ${devices.length === 0 ? '<div style="color:var(--fg2);text-align:center;padding:20px">No devices configured. Click "Add Device" above.</div>' :
          devices.map((d, i) => renderDeviceCard(d, i)).join('')}
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>Dynamic Device Detection</h3></div>
      <div class="card-body">
        <div class="toggle-row">
          <div><div class="label">Auto-Detect Devices from Database</div><div class="sublabel">Queries configdb_1 (hm_device) to discover devices by ownerID</div></div>
          <div class="toggle ${config.logCollection?.dynamicDeviceDetection?.enabled ? 'on' : ''}" onclick="toggleEl(this)"></div>
        </div>
        <div class="field" style="margin-top:12px">
          <label>Max Devices</label>
          <input type="number" value="${config.logCollection?.dynamicDeviceDetection?.maxDevices || 3}" min="0" max="20" style="width:100px" disabled>
          <div class="hint">0 = no limit</div>
        </div>
      </div>
    </div>
  ` + "`" + `;
}

function renderDeviceCard(d, i) {
  const addCmds = (d.diagnostics?.additionalCommands || []).map(c => c.name + ': ' + c.command).join('\n');
  return ` + "`" + `
    <div class="dev-card" data-idx="${i}" style="border:1px solid var(--border);border-radius:8px;padding:16px;margin-bottom:16px;background:var(--bg)">
      <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:12px">
        <div style="display:flex;align-items:center;gap:8px">
          <input class="dev-name" value="${esc(d.name)}" style="font-weight:600;font-size:14px;background:transparent;border:1px solid var(--border);border-radius:4px;padding:4px 8px;color:var(--fg);width:200px" placeholder="Device name">
          <select class="dev-type" style="background:var(--bg2);border:1px solid var(--border);border-radius:4px;padding:4px 8px;color:var(--fg);font-size:12px">
            <option value="exos" ${d.type === 'exos' ? 'selected' : ''}>EXOS</option>
            <option value="voss" ${d.type === 'voss' ? 'selected' : ''}>VOSS</option>
          </select>
          <div class="toggle ${d.enabled ? 'on' : ''}" onclick="toggleEl(this)" style="flex-shrink:0" title="Enable/Disable"></div>
          <span style="font-size:11px;color:var(--fg2)">Enabled</span>
        </div>
        <button class="btn btn-danger" onclick="removeDevice(${i})" style="padding:4px 12px;font-size:11px">&#128465; Remove</button>
      </div>
      <div class="field-row">
        <div class="field">
          <label>IP Address</label>
          <input class="dev-ip" value="${esc(d.ipAddress)}" placeholder="e.g. 10.0.0.1">
        </div>
        <div class="field">
          <label>SSH Port</label>
          <input class="dev-port" type="number" value="${d.port || 22}" min="1" max="65535" placeholder="22">
        </div>
        <div class="field">
          <label>Username</label>
          <input class="dev-user" value="${esc(d.username)}" placeholder="${d.type === 'voss' ? 'rwa (default)' : 'admin (default)'}">
        </div>
        <div class="field">
          <label>Password</label>
          <input class="dev-pass" type="password" value="${esc(d.password)}" placeholder="${d.type === 'voss' ? 'rwa (default)' : '(empty default)'}">
        </div>
      </div>
      <div class="field-row">
        <div class="field" style="flex:1">
          <div style="display:flex;align-items:center;gap:8px;margin-bottom:4px">
            <label style="margin:0">Diagnostics</label>
            <div class="toggle ${d.diagnostics?.enabled !== false ? 'on' : ''}" onclick="toggleEl(this)" style="transform:scale(0.8)"></div>
            <label style="margin:0;color:var(--fg2)">Use Defaults</label>
            <div class="toggle ${d.diagnostics?.useDefaults !== false ? 'on' : ''}" onclick="toggleEl(this)" style="transform:scale(0.8)"></div>
          </div>
          <label>Additional Show Commands <span style="color:var(--fg2);font-weight:400">(one per line: name: command)</span></label>
          <textarea class="dev-cmds" rows="3" style="width:100%;background:var(--bg2);border:1px solid var(--border);border-radius:6px;padding:8px;color:var(--fg);font-family:var(--mono);font-size:11px;resize:vertical" placeholder="e.g.&#10;show_vlans: show vlan&#10;show_ports: show ports info">${addCmds}</textarea>
          <div class="hint">Built-in defaults include: show version, show switch, show config, etc. Add extras here.</div>
        </div>
      </div>
    </div>
  ` + "`" + `;
}

function addDevice() {
  const container = document.getElementById('devicesContainer');
  if (!container) return;
  const idx = container.querySelectorAll('.dev-card').length;
  const newDevice = { name: 'new-device-' + (idx+1), type: 'exos', enabled: true, ipAddress: '', port: 22, username: '', password: '',
    diagnostics: { enabled: true, useDefaults: true, additionalCommands: [] }, logs: { enabled: false } };
  container.insertAdjacentHTML('beforeend', renderDeviceCard(newDevice, idx));
  toast('Device added — fill in details and click Save', 'info');
}

function removeDevice(idx) {
  const cards = document.querySelectorAll('.dev-card');
  if (cards[idx]) { cards[idx].remove(); toast('Device removed — click Save to apply', 'info'); }
}

function collectDevicesFromDOM() {
  const cards = document.querySelectorAll('.dev-card');
  const devices = [];
  cards.forEach(card => {
    const toggles = card.querySelectorAll('.toggle');
    const cmdsText = card.querySelector('.dev-cmds')?.value || '';
    const additionalCmds = cmdsText.split('\n').filter(l => l.trim() && l.includes(':')).map(l => {
      const [n, ...rest] = l.split(':');
      return { name: n.trim(), command: rest.join(':').trim() };
    });
    devices.push({
      name: card.querySelector('.dev-name')?.value || '',
      type: card.querySelector('.dev-type')?.value || 'exos',
      enabled: toggles[0]?.classList.contains('on') || false,
      ipAddress: card.querySelector('.dev-ip')?.value || '',
      port: parseInt(card.querySelector('.dev-port')?.value) || 22,
      username: card.querySelector('.dev-user')?.value || '',
      password: card.querySelector('.dev-pass')?.value || '',
      diagnostics: {
        enabled: toggles[1]?.classList.contains('on') || false,
        useDefaults: toggles[2]?.classList.contains('on') || false,
        additionalCommands: additionalCmds
      },
      logs: { enabled: false }
    });
  });
  return devices;
}

async function saveDevices() {
  const devices = collectDevicesFromDOM();
  try {
    const r = await fetch('/api/config/update-section', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ section: 'devices', data: devices })
    });
    const data = await r.json();
    if (data.saved) { toast('Devices saved (' + devices.length + ' devices)', 'success'); await loadConfig(); }
    else { toast('Save failed: ' + (data.error || 'Unknown'), 'error'); }
  } catch(e) { toast('Error: ' + e.message, 'error'); }
}

// ═══════════════════════════════════════════════════════════════════════════
// Database Page
// ═══════════════════════════════════════════════════════════════════════════
function renderDatabase() {
  const dc = config.databaseCollection || {};
  const dbs = dc.databases || [];
  const params = dc.parameters || {};
  return ` + "`" + `
    <div class="card">
      <div class="card-header"><h3>Database Collection Settings</h3></div>
      <div class="card-body">
        <div class="field-row">
          <div class="field"><label>Query Timeout (seconds)</label><input type="number" value="${dc.queryTimeout || 60}" disabled></div>
          <div class="field"><label>Output Directory</label><input value="${esc(dc.outputDir)}" disabled></div>
        </div>
        <div style="margin-top:8px"><label style="font-size:12px;color:var(--fg2);font-weight:500">Global Parameters</label></div>
        ${Object.keys(params).length ? '<div style="font-size:12px;padding:4px 0">' + Object.entries(params).map(([k,v]) => '<div style="display:inline-block;background:var(--bg);padding:2px 8px;border-radius:4px;margin:2px"><span style="color:var(--fg2)">' + esc(k) + ':</span> ' + esc(v || '(auto)') + '</div>').join('') + '</div>' : '<div style="color:var(--fg2);font-size:12px">No parameters set</div>'}
      </div>
    </div>

    ${dbs.map(db => ` + "`" + `
    <div class="card">
      <div class="card-header">
        <h3>${esc(db.name)} <span style="color:var(--fg2);font-size:11px;font-weight:400;margin-left:8px">${esc(db.alias)}</span></h3>
        <span class="badge ${db.enabled ? 'badge-completed' : 'badge-failed'}">${db.enabled ? 'Enabled' : 'Disabled'}</span>
      </div>
      <div class="card-body">
        ${(db.queries || []).map(q => ` + "`" + `
          <div style="border-left:3px solid var(--accent);padding:8px 12px;margin-bottom:8px;background:var(--bg);border-radius:0 6px 6px 0">
            <div style="font-size:13px;font-weight:600;margin-bottom:4px">${esc(q.name)}</div>
            <div style="font-family:var(--mono);font-size:11px;color:var(--fg2);word-break:break-all">${esc(q.sql)}</div>
            ${(q.parameters || []).length ? '<div style="margin-top:4px;font-size:11px"><span style="color:var(--fg2)">Exports:</span> ' + q.parameters.map(p => '<code style="background:var(--bg3);padding:1px 4px;border-radius:3px">' + esc(p) + '</code>').join(' ') + '</div>' : ''}
          </div>
        ` + "`" + `).join('')}
      </div>
    </div>
    ` + "`" + `).join('')}

    <div class="hint" style="color:var(--accent)">&#9998; Add/edit databases, queries, SQL, and parameters in <a href="#" onclick="showPage('rawconfig');return false" style="color:var(--accent)">Raw Config</a> &gt; databaseCollection</div>
  ` + "`" + `;
}

// ═══════════════════════════════════════════════════════════════════════════
// JIRA Page
// ═══════════════════════════════════════════════════════════════════════════
function renderJira() {
  const j = config.jira || {};
  return ` + "`" + `
    <div class="card">
      <div class="card-header"><h3>JIRA Integration</h3>
        <button class="btn btn-primary" onclick="saveJira()">&#128190; Save</button>
      </div>
      <div class="card-body">
        <div class="toggle-row" style="margin-bottom:16px">
          <div><div class="label">Attachment Enabled</div><div class="sublabel">Upload collected logs to JIRA tickets</div></div>
          <div class="toggle ${j.attachmentEnabled ? 'on' : ''}" id="t_jiraEnabled" onclick="toggleEl(this)"></div>
        </div>
        <div class="field-row">
          <div class="field">
            <label>Email</label>
            <input id="f_jiraEmail" value="${esc(j.email)}" placeholder="{username}@extremenetworks.com">
          </div>
          <div class="field">
            <label>Base URL</label>
            <input id="f_jiraBaseUrl" value="${esc(j.baseUrl)}" placeholder="https://extremenetworks.atlassian.net">
          </div>
        </div>
        <div class="field">
          <label>API Token</label>
          <input id="f_jiraToken" type="password" autocomplete="off" placeholder="${j.tokenStored ? '•••••• token stored — type a new one to replace' : 'Paste your Atlassian API token'}">
          <div class="hint" style="margin-top:6px">${j.tokenStored ? '✅ A token is already saved. ' : ''}Stored securely in Windows Credential Manager (keyed by email) — never written to config.yaml. Leave blank to keep the existing token. <a href="https://id.atlassian.com/manage-profile/security/api-tokens" target="_blank" style="color:var(--accent)">Generate a token &#8599;</a></div>
        </div>
      </div>
    </div>
  ` + "`" + `;
}

async function saveJira() {
  const email = val('f_jiraEmail');
  const token = val('f_jiraToken');

  // Store the API token securely (OS keychain) — only if one was entered.
  if (token) {
    try {
      const r = await fetch('/api/jira/token', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({ token: token, email: email })
      });
      const data = await r.json();
      if (data.saved) {
        toast('API token stored securely for ' + (data.email || email), 'success');
        const tf = document.getElementById('f_jiraToken');
        if (tf) { tf.value = ''; tf.placeholder = '•••••• token stored — type a new one to replace'; }
      } else {
        toast('Token not saved: ' + (data.error || 'Unknown error'), 'error');
      }
    } catch(e) {
      toast('Token save error: ' + e.message, 'error');
    }
  }

  // Save the non-secret JIRA fields to config.yaml.
  saveFields({
    'jira.email': email,
    'jira.baseUrl': val('f_jiraBaseUrl'),
    'jira.attachmentEnabled': isToggleOn('t_jiraEnabled'),
  });
}

// ═══════════════════════════════════════════════════════════════════════════
// Run Page
// ═══════════════════════════════════════════════════════════════════════════
function renderRun() {
  const e = config.essentials || {};
  const b = config.bastion || {};
  const a = config.aws || {};
  const lc = config.logCollection || {};
  const si = config.systemInfo || {};
  const av = config.appVersionCollection || {};
  const dl = config.deviceLogCollection || {};
  const dc = config.databaseCollection || {};
  const opts = config.options || {};

  // Build enabled sections list
  const sections = [];
  if (lc.enabled) sections.push({name:'K8s Logs', icon:'\u2705'});
  if (si.enabled) sections.push({name:'System Info', icon:'\u2705'});
  if (av.enabled) sections.push({name:'App Versions', icon:'\u2705'});
  if (dl.enabled) sections.push({name:'Device Logs', icon:'\u2705'});
  if (dc.enabled) sections.push({name:'Database', icon:'\u2705'});
  if (sections.length === 0) sections.push({name:'None — enable sections in Collection page', icon:'\u26A0\uFE0F'});

  return ` + "`" + `
    <div class="card">
      <div class="card-header"><h3>Current Config Summary</h3>
        <button class="btn btn-outline" onclick="loadConfig().then(()=>showPage('run'))">\u21BB Refresh</button>
      </div>
      <div class="card-body grid">
        <div>
          <div class="field"><label>Username</label><div style="font-size:14px;font-weight:600">${esc(e.username || '\u2014 not set')}</div></div>
          <div class="field"><label>Environment</label><div style="font-size:14px;font-weight:600">${esc(e.environment || '\u2014 not set')}</div></div>
          <div class="field"><label>Owner ID</label><div style="font-size:12px;color:var(--fg2)">${esc(e.ownerID || 'Auto-resolve from ' + (e.envLoginId || 'login email'))}</div></div>
        </div>
        <div>
          <div class="field"><label>Bastion</label><div style="font-size:12px">${esc(b.host || '\u2014 not set')}:${b.port || 22}</div></div>
          <div class="field"><label>AWS Host</label><div style="font-size:12px">${esc(a.host || '\u2014 not set')}</div></div>
          <div class="field"><label>Output Dir</label><div style="font-size:12px;color:var(--fg2)">${esc(config.logs?.outputDir || 'Current directory')}</div></div>
        </div>
      </div>
      <div style="padding:0 20px 16px">
        <label style="font-size:12px;color:var(--fg2);display:block;margin-bottom:6px">Enabled Sections</label>
        ${sections.map(s => '<span style="display:inline-block;background:rgba(34,197,94,0.15);color:var(--success);padding:3px 10px;border-radius:12px;font-size:11px;margin:2px">' + s.icon + ' ' + s.name + '</span>').join('')}
      </div>
      <div style="padding:0 20px 16px">
        <label style="font-size:12px;color:var(--fg2);display:block;margin-bottom:6px">Sub-Options</label>
        <span style="display:inline-block;padding:3px 10px;border-radius:12px;font-size:11px;margin:2px;background:${lc.defaultEP1Logs ? 'rgba(34,197,94,0.15);color:var(--success)' : 'rgba(239,68,68,0.15);color:var(--error)'}">${lc.defaultEP1Logs ? '\u2705' : '\u274C'} Default EP1 Logs</span>
        <span style="display:inline-block;padding:3px 10px;border-radius:12px;font-size:11px;margin:2px;background:${lc.messageFilter?.enabled ? 'rgba(34,197,94,0.15);color:var(--success)' : 'rgba(100,116,139,0.2);color:var(--fg2)'}">${lc.messageFilter?.enabled ? '\u2705' : '\u2014'} Message Filter</span>
        <span style="display:inline-block;padding:3px 10px;border-radius:12px;font-size:11px;margin:2px;background:${lc.logAnalysis?.enabled ? 'rgba(34,197,94,0.15);color:var(--success)' : 'rgba(100,116,139,0.2);color:var(--fg2)'}">${lc.logAnalysis?.enabled ? '\u2705' : '\u2014'} Log Analysis</span>
        <span style="display:inline-block;padding:3px 10px;border-radius:12px;font-size:11px;margin:2px;background:${lc.temporalWorkflowCollection?.enabled ? 'rgba(34,197,94,0.15);color:var(--success)' : 'rgba(100,116,139,0.2);color:var(--fg2)'}">${lc.temporalWorkflowCollection?.enabled ? '\u2705' : '\u2014'} Temporal</span>
        <span style="display:inline-block;padding:3px 10px;border-radius:12px;font-size:11px;margin:2px;background:${lc.dynamicDeviceDetection?.enabled ? 'rgba(34,197,94,0.15);color:var(--success)' : 'rgba(100,116,139,0.2);color:var(--fg2)'}">${lc.dynamicDeviceDetection?.enabled ? '\u2705' : '\u2014'} Dynamic Device Detection</span>
        <span style="display:inline-block;padding:3px 10px;border-radius:12px;font-size:11px;margin:2px;background:rgba(59,130,246,0.15);color:var(--accent)">\u2699 ${esc(opts.downloadMethod || 'scp')} | ${esc(opts.logLevel || 'INFO')} | ${opts.maxSSHSessions || 1} sessions</span>
      </div>
    </div>

    <div class="card">
      <div class="card-header"><h3>Run Options (Override)</h3></div>
      <div class="card-body">
        <div class="field-row">
          <div class="field">
            <label>Operation Mode</label>
            <select id="f_runMode">
              <option value="config">Config Mode (use config.yaml)</option>
              <option value="all">Collect All</option>
              <option value="logs-only">Logs Only</option>
              <option value="sys-info">System Info Only</option>
              <option value="version">App Versions Only</option>
              <option value="device-logs">Device Logs Only</option>
              <option value="database">Database Only</option>
            </select>
          </div>
          <div class="field">
            <label>Time Duration</label>
            <input id="f_runDuration" value="${esc(lc.timeBasedCollection?.duration)}" placeholder="e.g. 15m, 30m, 1h (empty=full logs)">
            <div class="hint">Overrides config.yaml time-based duration</div>
          </div>
        </div>
        <div class="field-row">
          <div class="field">
            <label>Log Level</label>
            <select id="f_runLogLevel">
              <option value="">Use Config Default (${esc(opts.logLevel || 'INFO')})</option>
              <option value="DEBUG">DEBUG</option>
              <option value="INFO">INFO</option>
              <option value="WARN">WARN</option>
              <option value="ERROR">ERROR</option>
            </select>
          </div>
          <div class="field">
            <label>JIRA Issue (optional)</label>
            <input id="f_runJira" placeholder="e.g. XCP-12345">
            <div class="hint">${config.jira?.attachmentEnabled ? '\u2705 JIRA attachment enabled' : '\u26A0\uFE0F JIRA attachment disabled in config'}</div>
          </div>
        </div>
        <div class="field">
            <label>Output Directory (optional override)</label>
            <input id="f_runOutdir" placeholder="${esc(config.logs?.outputDir || 'Current directory')}">
            <div class="hint">Leave empty to use config.yaml: ${esc(config.logs?.outputDir || '(current directory)')}</div>
          </div>
        </div>
      </div>
    </div>
    <div class="card">
      <div class="card-header"><h3>\u{1F512} Authentication</h3></div>
      <div class="card-body">
        <div class="field-row">
          <div class="field">
            <label>Bastion Password</label>
            <input id="f_runPassword" type="password" placeholder="${config.bastion?.passwordStored ? 'Leave empty to use the saved password' : 'Enter bastion password (or leave empty to use SSH key)'}">
            <div class="hint">Used for this run only (passed via env var, not stored here). ${config.bastion?.passwordStored ? '✅ A saved password exists — leave blank to use it.' : 'To save a password permanently, set it on the SSH / Bastion page (Windows Credential Manager).'}</div>
          </div>
          <div class="field">
            <label>Bastion SSH Key Path</label>
            <input id="f_runKeyPath" value="${esc(b.keyPath)}" placeholder="C:\\Users\\{username}\\.ssh\\id_ed25519_bastion">
            <div class="hint">Path to SSH private key for bastion. Overrides password auth if provided.</div>
          </div>
        </div>
      </div>
    </div>
    <div class="card">
      <div class="card-body" style="display:flex;align-items:center;justify-content:space-between">
        <div class="btn-group">
          <button class="btn btn-success" onclick="startRun()" ${isRunning ? 'disabled' : ''}>\u25B6 Start Collection</button>
          <button class="btn btn-danger" onclick="stopRun()" ${!isRunning ? 'disabled' : ''}>\u25A0 Stop</button>
        </div>
        <div id="runStatusInfo" style="color:var(--fg2);font-size:13px"></div>
      </div>
    </div>
    <div class="card">
      <div class="card-header"><h3>Live Output</h3><span style="font-size:11px;color:var(--fg2)" id="lineCounter"></span></div>
      <div class="terminal" id="runTerminal" style="min-height:200px"></div>
    </div>
  ` + "`" + `;
}

// ═══════════════════════════════════════════════════════════════════════════
// Output Page
// ═══════════════════════════════════════════════════════════════════════════
function renderOutput() {
  return ` + "`" + `
    <div class="card">
      <div class="card-header">
        <h3>Collection Output</h3>
        <div class="btn-group">
          <button class="btn btn-outline" onclick="clearOutput()">&#128465; Clear</button>
          <button class="btn btn-outline" onclick="copyOutput()">&#128203; Copy All</button>
        </div>
      </div>
      <div class="terminal" id="outputTerminal" style="min-height:400px;max-height:70vh"></div>
    </div>
  ` + "`" + `;
}

// ═══════════════════════════════════════════════════════════════════════════
// API Calls
// ═══════════════════════════════════════════════════════════════════════════
async function saveFields(fields) {
  try {
    const r = await fetch('/api/config/save', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(fields)
    });
    const data = await r.json();
    if (data.saved) {
      toast('Configuration saved', 'success');
      await loadConfig();
    } else {
      toast('Save failed: ' + (data.error || 'Unknown'), 'error');
    }
  } catch(e) {
    toast('Save error: ' + e.message, 'error');
  }
}

async function startRun() {
  const payload = {
    mode: val('f_runMode') || 'config',
    timeDuration: val('f_runDuration'),
    logLevel: val('f_runLogLevel'),
    outputDir: val('f_runOutdir'),
    jiraIssue: val('f_runJira'),
    bastionPassword: val('f_runPassword'),
  };
  try {
    const r = await fetch('/api/run', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(payload)
    });
    const data = await r.json();
    if (data.started) {
      toast('Collection started', 'success');
      isRunning = true;
      outputLineCount = 0;
      startOutputPoll();
      updateStatusBadge('running');
    } else {
      toast(data.error || 'Failed to start', 'error');
    }
  } catch(e) {
    toast('Error: ' + e.message, 'error');
  }
}

async function quickRun(mode) {
  const pw = (document.getElementById('f_runPassword') || {}).value || '';
  try {
    const r = await fetch('/api/run', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ mode: mode, bastionPassword: pw })
    });
    const data = await r.json();
    if (data.started) {
      toast('Collection started (' + mode + ')', 'success');
      isRunning = true;
      outputLineCount = 0;
      showPage('run');
      startOutputPoll();
      updateStatusBadge('running');
    } else {
      toast(data.error || 'Failed', 'error');
    }
  } catch(e) {
    toast('Error: ' + e.message, 'error');
  }
}

async function stopRun() {
  try {
    await fetch('/api/stop', { method: 'POST' });
    toast('Stop signal sent', 'info');
  } catch(e) {
    toast('Error: ' + e.message, 'error');
  }
}

async function validateConfig() {
  const el = document.getElementById('validationResults');
  if (!el) return;
  el.innerHTML = '<div class="spinner"></div> Validating...';
  try {
    const r = await fetch('/api/validate', { method: 'POST' });
    const data = await r.json();
    if (data.valid) {
      el.innerHTML = '<div class="validation-ok">&#9989; Configuration is valid — no issues found</div>';
    } else {
      el.innerHTML = (data.issues || []).map(issue => {
        let cls = 'info';
        if (issue.startsWith('CRITICAL')) cls = 'critical';
        else if (issue.startsWith('WARNING')) cls = 'warning';
        return '<div class="validation-item ' + cls + '">' + esc(issue) + '</div>';
      }).join('');
    }
  } catch(e) {
    el.innerHTML = '<div class="validation-item critical">Validation failed: ' + esc(e.message) + '</div>';
  }
}

// ═══════════════════════════════════════════════════════════════════════════
// Output Polling
// ═══════════════════════════════════════════════════════════════════════════
let outputPollTimer = null;

function startOutputPoll() {
  stopOutputPoll();
  pollOutput();
  outputPollTimer = setInterval(pollOutput, 1000);
}

function stopOutputPoll() {
  if (outputPollTimer) { clearInterval(outputPollTimer); outputPollTimer = null; }
}

async function pollOutput() {
  try {
    const r = await fetch('/api/output?from=' + outputLineCount);
    const data = await r.json();

    // Update terminals on current page
    const terminals = ['runTerminal', 'outputTerminal'];
    for (const tid of terminals) {
      const term = document.getElementById(tid);
      if (!term) continue;
      for (const line of (data.lines || [])) {
        const div = document.createElement('div');
        div.textContent = line;
        // Color code by log level
        if (line.includes('[ERROR]')) div.className = 'line-error';
        else if (line.includes('[WARN]')) div.className = 'line-warn';
        else if (line.includes('[DEBUG]')) div.className = 'line-debug';
        else if (line.includes('[INFO]')) div.className = 'line-info';
        term.appendChild(div);
      }
      // Auto-scroll
      if (data.lines?.length) term.scrollTop = term.scrollHeight;
    }

    outputLineCount = data.totalLines || outputLineCount;

    // Update line counter
    const counter = document.getElementById('lineCounter');
    if (counter) counter.textContent = outputLineCount + ' lines';

    // Update run status
    if (data.status && data.status !== 'running') {
      isRunning = false;
      updateStatusBadge(data.status);
      if (data.status === 'completed') toast('Collection completed', 'success');
      else if (data.status === 'failed') toast('Collection failed', 'error');
      // Keep polling a bit longer then stop
      setTimeout(stopOutputPoll, 2000);
    }
  } catch(e) {
    // Silent fail on poll errors
  }
}

function clearOutput() {
  const term = document.getElementById('outputTerminal');
  if (term) term.innerHTML = '';
}

function copyOutput() {
  const term = document.getElementById('outputTerminal');
  if (term) {
    navigator.clipboard.writeText(term.innerText).then(() => toast('Copied to clipboard', 'info'));
  }
}

// ═══════════════════════════════════════════════════════════════════════════
// Status Polling
// ═══════════════════════════════════════════════════════════════════════════
function startStatusPoll() {
  setInterval(async () => {
    try {
      const r = await fetch('/api/status');
      const data = await r.json();
      isRunning = data.running;
      if (data.status !== lastStatus) {
        lastStatus = data.status;
        updateStatusBadge(data.status);
      }
      // Update run page status info if visible
      const info = document.getElementById('runStatusInfo');
      if (info && data.running) {
        info.textContent = 'Running... ' + (data.elapsed || '');
      } else if (info && data.duration) {
        info.textContent = 'Finished in ' + data.duration;
      }
    } catch(e) {}
  }, 3000);
}

function updateStatusBadge(status) {
  const badge = document.getElementById('statusBadge');
  if (!badge) return;
  const labels = { idle: '&#9679; Idle', running: '&#9679; Running', completed: '&#9679; Completed', failed: '&#9679; Failed' };
  badge.className = 'badge badge-' + status;
  badge.innerHTML = labels[status] || status;
}

// ═══════════════════════════════════════════════════════════════════════════
// Utilities
// ═══════════════════════════════════════════════════════════════════════════
function val(id) { const el = document.getElementById(id); return el ? el.value : ''; }
function esc(s) { if (!s) return ''; const d = document.createElement('div'); d.textContent = String(s); return d.innerHTML; }
function toggleEl(el) { el.classList.toggle('on'); }
function isToggleOn(id) { const el = document.getElementById(id); return el ? el.classList.contains('on') : false; }

function toast(msg, type) {
  const container = document.getElementById('toasts');
  const t = document.createElement('div');
  t.className = 'toast toast-' + (type || 'info');
  t.textContent = msg;
  container.appendChild(t);
  setTimeout(() => t.remove(), 4000);
}

// ═══════════════════════════════════════════════════════════════════════════
// Raw Config Editor
// ═══════════════════════════════════════════════════════════════════════════
function renderRawConfig() {
  return ` + "`" + `
    <div class="card">
      <div class="card-header">
        <h3>Full config.yaml Editor</h3>
        <div class="btn-group">
          <button class="btn btn-outline" onclick="loadRawConfig()">&#x21BB; Reload</button>
          <button class="btn btn-primary" onclick="saveRawConfig()">&#128190; Save</button>
        </div>
      </div>
      <div class="card-body">
        <div class="hint" style="margin-bottom:12px">Edit the full config.yaml directly. All options are available here including custom kubectl commands, device-specific CLI commands, database query SQL, error patterns, temporal workflow prefixes, pod file collections, message filter strings, and more. YAML is validated before saving.</div>
        <textarea id="rawConfigEditor" style="width:100%;min-height:600px;background:var(--bg);color:var(--fg);border:1px solid var(--border);border-radius:6px;padding:12px;font-family:var(--mono);font-size:12px;line-height:1.5;resize:vertical;tab-size:2;white-space:pre;overflow-wrap:normal;overflow-x:auto" spellcheck="false"></textarea>
        <div id="rawConfigStatus" style="margin-top:8px;font-size:12px;color:var(--fg2)"></div>
      </div>
    </div>
  ` + "`" + `;
}

async function loadRawConfig() {
  const editor = document.getElementById('rawConfigEditor');
  const status = document.getElementById('rawConfigStatus');
  if (!editor) return;
  try {
    const r = await fetch('/api/config/raw');
    const data = await r.json();
    editor.value = data.content || '';
    if (status) status.textContent = 'Loaded ' + editor.value.length + ' characters';
  } catch(e) {
    toast('Failed to load raw config: ' + e.message, 'error');
  }
}

async function saveRawConfig() {
  const editor = document.getElementById('rawConfigEditor');
  const status = document.getElementById('rawConfigStatus');
  if (!editor) return;
  try {
    const r = await fetch('/api/config/raw', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ content: editor.value })
    });
    const data = await r.json();
    if (data.saved) {
      toast('Config saved successfully', 'success');
      if (status) status.textContent = 'Saved at ' + new Date().toLocaleTimeString();
      await loadConfig();
    } else {
      toast('Save failed: ' + (data.error || 'Unknown error'), 'error');
      if (status) { status.textContent = data.error; status.style.color = 'var(--error)'; }
    }
  } catch(e) {
    toast('Save error: ' + e.message, 'error');
  }
}

// Auto-load raw config when page is shown
const _origShowPage = showPage;
showPage = function(page) {
  _origShowPage(page);
  if (page === 'rawconfig') setTimeout(loadRawConfig, 50);
  if (page === 'aianalysis') setTimeout(initAIPage, 50);
};

// ═══════════════════════════════════════════════════════════════════════════
// AI Log Analysis Page
// ═══════════════════════════════════════════════════════════════════════════
function renderAIAnalysis() {
  // Resolve outputDir - strip {timestamp} to get the parent directory
  let outputDir = config.logs?.outputDir || '.';
  outputDir = outputDir.replace(/\\?\{timestamp\}/g, '').replace(/\\$/, '').replace(/\/$/, '');
  if (!outputDir) outputDir = '.';
  return ` + "`" + `
    <div class="card">
      <div class="card-header"><h3>&#129302; AI-Powered Root Cause Analysis</h3></div>
      <div class="card-body">
        <div class="hint" style="margin-bottom:16px">Use an AI model to analyze collected logs and identify the root cause of failures. Use the local <b>Claude CLI</b> (your existing Claude Code login — no API key) or any OpenAI-compatible API (OpenAI, Azure, Ollama, LM Studio, etc.).</div>

        <div class="field">
          <label>AI Provider</label>
          <select id="ai_provider" onchange="onProviderChange()">
            <option value="cli">Claude CLI &mdash; local, uses your Claude Code login (no API key)</option>
            <option value="api">OpenAI-compatible API &mdash; API key required</option>
          </select>
        </div>

        <!-- Claude CLI settings -->
        <div id="ai_cliSettings">
          <div class="field">
            <label>Claude Model</label>
            <select id="ai_cliModel">
              <option value="sonnet">Sonnet (fast, balanced)</option>
              <option value="opus">Opus (best quality)</option>
              <option value="haiku">Haiku (fastest, cheapest)</option>
            </select>
            <div class="hint">Runs the local <span style="font-family:var(--mono)">claude</span> CLI in print mode. Requires Claude Code installed and signed in (<span style="font-family:var(--mono)">claude</span> on your PATH).</div>
          </div>
        </div>

        <!-- OpenAI-compatible API settings -->
        <div id="ai_apiSettings" style="display:none">
          <div class="field-row">
            <div class="field" style="flex:2">
              <label>API Key</label>
              <input id="ai_apiKey" type="password" placeholder="sk-... (OpenAI) or your LLM API key">
              <div class="hint">Stored in browser session only — never saved to disk or config</div>
            </div>
            <div class="field" style="flex:1">
              <label>Model</label>
              <select id="ai_model">
                <option value="gpt-4o-mini">GPT-4o Mini (fast, cheap)</option>
                <option value="gpt-4o">GPT-4o (best quality)</option>
                <option value="gpt-4.1-mini">GPT-4.1 Mini</option>
                <option value="gpt-4.1">GPT-4.1</option>
                <option value="o4-mini">o4-mini (reasoning)</option>
                <option value="custom">Custom model...</option>
              </select>
            </div>
          </div>
          <div class="field" id="customModelField" style="display:none">
            <label>Custom Model Name</label>
            <input id="ai_customModel" placeholder="e.g. llama3, mistral, claude-3-opus">
          </div>
          <div class="field">
            <label>API Endpoint URL</label>
            <input id="ai_apiUrl" value="https://api.openai.com/v1/chat/completions" placeholder="https://api.openai.com/v1/chat/completions">
            <div class="hint">For Ollama: http://localhost:11434/v1/chat/completions | Azure: https://YOUR.openai.azure.com/openai/deployments/MODEL/chat/completions?api-version=2024-02-01</div>
          </div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-header">
        <h3>Log Source</h3>
      </div>
      <div class="card-body">
        <div class="field">
          <label>Log Directory</label>
          <div style="display:flex;gap:8px">
            <input id="ai_logDir" value="${esc(outputDir.replace(/\\\\/g, '\\\\'))}" placeholder="Path to collected logs directory" style="flex:1">
            <button class="btn btn-outline" onclick="browseLogFiles()">&#128269; Browse</button>
          </div>
        </div>

        <div id="ai_fileList" style="margin-top:12px"></div>

        <div class="field" style="margin-top:16px">
          <label>Or Paste Log Text Directly</label>
          <textarea id="ai_logText" rows="6" style="width:100%;background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:10px;color:var(--fg);font-family:var(--mono);font-size:11px;resize:vertical" placeholder="Paste error logs, stack traces, kubectl output, etc. here..."></textarea>
        </div>

        <div class="field">
          <label>Additional Context (optional)</label>
          <textarea id="ai_context" rows="2" style="width:100%;background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:10px;color:var(--fg);font-size:13px;resize:vertical" placeholder="e.g. 'VLAN deployment failed after conflict resolution' or 'Device went offline after config push'"></textarea>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-body" style="display:flex;align-items:center;justify-content:space-between">
        <div class="btn-group">
          <button class="btn btn-success" onclick="runAIAnalysis()" id="ai_runBtn">&#129302; Analyze with AI</button>
          <button class="btn btn-outline" onclick="clearAIResults()">&#128465; Clear</button>
        </div>
        <div id="ai_status" style="color:var(--fg2);font-size:12px"></div>
      </div>
    </div>

    <div class="card" id="ai_resultsCard" style="display:none">
      <div class="card-header">
        <h3>&#128161; Analysis Results</h3>
        <button class="btn btn-outline" onclick="copyAIResults()" style="font-size:11px">&#128203; Copy</button>
      </div>
      <div class="card-body">
        <div id="ai_results" style="font-size:13px;line-height:1.7"></div>
        <div id="ai_meta" style="margin-top:12px;padding-top:12px;border-top:1px solid var(--border);font-size:11px;color:var(--fg2)"></div>
      </div>
    </div>
  ` + "`" + `;
}

function onProviderChange() {
  const p = document.getElementById('ai_provider')?.value || 'cli';
  const cli = document.getElementById('ai_cliSettings');
  const api = document.getElementById('ai_apiSettings');
  if (cli) cli.style.display = p === 'cli' ? 'block' : 'none';
  if (api) api.style.display = p === 'api' ? 'block' : 'none';
}

function initAIPage() {
  // Set provider field visibility (defaults to Claude CLI)
  onProviderChange();
  const modelSel = document.getElementById('ai_model');
  if (modelSel) {
    modelSel.addEventListener('change', () => {
      const customField = document.getElementById('customModelField');
      if (customField) customField.style.display = modelSel.value === 'custom' ? 'block' : 'none';
    });
  }
  // Load saved API key from sessionStorage (browser session only)
  const savedKey = sessionStorage.getItem('ai_apiKey');
  if (savedKey) {
    const keyField = document.getElementById('ai_apiKey');
    if (keyField) keyField.value = savedKey;
  }
  // If launched via --analyze-ai, pre-fill the directory and auto-browse
  if (window._aiInitDir) {
    const dirInput = document.getElementById('ai_logDir');
    if (dirInput) {
      dirInput.value = window._aiInitDir;
      window._aiInitDir = null; // Only auto-fill once
      setTimeout(browseLogFiles, 200);
    }
  }
}

let selectedFiles = [];

async function browseLogFiles() {
  const dirInput = document.getElementById('ai_logDir');
  const dir = dirInput?.value || '.';
  const container = document.getElementById('ai_fileList');
  if (!container) return;
  container.innerHTML = '<div class="spinner"></div> Scanning directory...';

  try {
    const r = await fetch('/api/analyze/files?dir=' + encodeURIComponent(dir));
    const data = await r.json();
    if (data.error) { container.innerHTML = '<div style="color:var(--error)">' + esc(data.error) + '</div>'; return; }

    const files = data.files || [];
    if (files.length === 0) {
      container.innerHTML = '<div style="color:var(--fg2);padding:8px">No log files found in this directory.</div>';
      return;
    }

    selectedFiles = [];
    container.innerHTML = '<div style="margin-bottom:8px;display:flex;align-items:center;justify-content:space-between">' +
      '<label style="font-size:12px;color:var(--fg2);font-weight:500">Found ' + files.length + ' files — select files for analysis:</label>' +
      '<button class="btn btn-outline" onclick="selectAllFiles()" style="padding:3px 10px;font-size:11px">Select All</button></div>' +
      '<div style="max-height:250px;overflow-y:auto;border:1px solid var(--border);border-radius:6px">' +
      files.map((f, i) => '<label class="ai-file-row" style="display:flex;align-items:center;gap:8px;padding:6px 10px;border-bottom:1px solid rgba(255,255,255,0.03);cursor:pointer;font-size:12px">' +
        '<input type="checkbox" class="ai-file-cb" data-path="' + esc(f.path) + '" onchange="updateFileSelection()" style="flex-shrink:0">' +
        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(f.path) + '">' + esc(f.relPath) + '</span>' +
        '<span style="color:var(--fg2);flex-shrink:0">' + f.sizeStr + '</span>' +
        '</label>').join('') +
      '</div>' +
      '<div id="ai_selectedCount" style="margin-top:4px;font-size:11px;color:var(--fg2)">0 files selected</div>';
  } catch(e) {
    container.innerHTML = '<div style="color:var(--error)">Error: ' + esc(e.message) + '</div>';
  }
}

function selectAllFiles() {
  document.querySelectorAll('.ai-file-cb').forEach(cb => cb.checked = true);
  updateFileSelection();
}

function updateFileSelection() {
  selectedFiles = [];
  document.querySelectorAll('.ai-file-cb:checked').forEach(cb => selectedFiles.push(cb.dataset.path));
  const counter = document.getElementById('ai_selectedCount');
  if (counter) counter.textContent = selectedFiles.length + ' file' + (selectedFiles.length !== 1 ? 's' : '') + ' selected';
}

async function runAIAnalysis() {
  const provider = document.getElementById('ai_provider')?.value || 'cli';
  const logText = document.getElementById('ai_logText')?.value;
  const context = document.getElementById('ai_context')?.value;

  let apiKey = '', apiUrl = '', model = '';
  if (provider === 'cli') {
    model = document.getElementById('ai_cliModel')?.value || 'sonnet';
  } else {
    apiKey = document.getElementById('ai_apiKey')?.value;
    apiUrl = document.getElementById('ai_apiUrl')?.value;
    const modelSel = document.getElementById('ai_model')?.value;
    const customModel = document.getElementById('ai_customModel')?.value;
    model = modelSel === 'custom' ? customModel : modelSel;
    if (!apiKey) { toast('API key is required', 'error'); return; }
    // Save API key to session
    sessionStorage.setItem('ai_apiKey', apiKey);
  }

  if (selectedFiles.length === 0 && !logText) { toast('Select files or paste log text', 'error'); return; }

  const btn = document.getElementById('ai_runBtn');
  const status = document.getElementById('ai_status');
  const resultsCard = document.getElementById('ai_resultsCard');
  const results = document.getElementById('ai_results');
  const meta = document.getElementById('ai_meta');

  if (btn) btn.disabled = true;
  const label = provider === 'cli' ? 'Claude CLI (' + esc(model) + ')' : esc(model);
  if (status) status.innerHTML = '<span class="spinner"></span> Analyzing logs with ' + label + '...';
  if (resultsCard) resultsCard.style.display = 'none';

  try {
    const r = await fetch('/api/analyze/ai', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        provider: provider,
        apiKey: apiKey,
        apiUrl: apiUrl,
        model: model,
        filePaths: selectedFiles,
        logText: logText || '',
        context: context || ''
      })
    });
    const data = await r.json();

    if (data.error) {
      if (status) status.textContent = '';
      toast('Analysis failed: ' + data.error, 'error');
      if (results) results.innerHTML = '<div style="color:var(--error);white-space:pre-wrap">' + esc(data.error) + '</div>';
      if (resultsCard) resultsCard.style.display = 'block';
      return;
    }

    // Render markdown-like analysis
    if (results) results.innerHTML = renderMarkdown(data.analysis || '');
    if (meta) {
      let metaText = 'Model: ' + (data.model || '?') + ' | Files analyzed: ' + (data.filesRead || 0) +
        ' | Log size: ' + formatSizeJS(data.logSize || 0) + ' | Tokens used: ' + (data.tokensUsed || '?');
      if (data.costUSD) metaText += ' | Cost: $' + Number(data.costUSD).toFixed(4);
      meta.textContent = metaText;
    }
    if (resultsCard) resultsCard.style.display = 'block';
    if (status) status.textContent = 'Analysis complete';
    toast('AI analysis complete', 'success');

    // Scroll to results
    resultsCard?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  } catch(e) {
    toast('Error: ' + e.message, 'error');
    if (status) status.textContent = 'Failed';
  } finally {
    if (btn) btn.disabled = false;
  }
}

function clearAIResults() {
  const card = document.getElementById('ai_resultsCard');
  if (card) card.style.display = 'none';
  const status = document.getElementById('ai_status');
  if (status) status.textContent = '';
}

function copyAIResults() {
  const results = document.getElementById('ai_results');
  if (results) navigator.clipboard.writeText(results.innerText).then(() => toast('Analysis copied', 'info'));
}

function formatSizeJS(b) {
  if (b < 1024) return b + ' B';
  if (b < 1024*1024) return (b/1024).toFixed(1) + ' KB';
  return (b/1024/1024).toFixed(1) + ' MB';
}

// Simple markdown renderer for AI output
function renderMarkdown(text) {
  const BT = String.fromCharCode(96);
  const codeRx = new RegExp(BT + '([^' + BT + ']+)' + BT, 'g');
  return text
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/^### (.+)$/gm, '<h4 style="color:var(--accent);margin:16px 0 8px;font-size:14px">$1</h4>')
    .replace(/^## (.+)$/gm, '<h3 style="color:var(--accent);margin:20px 0 8px;font-size:15px">$1</h3>')
    .replace(/^# (.+)$/gm, '<h2 style="color:var(--accent);margin:24px 0 10px;font-size:16px">$1</h2>')
    .replace(/\*\*(.+?)\*\*/g, '<strong style="color:var(--fg)">$1</strong>')
    .replace(/\*(.+?)\*/g, '<em>$1</em>')
    .replace(codeRx, '<code style="background:var(--bg);padding:1px 5px;border-radius:3px;font-family:var(--mono);font-size:11px">$1</code>')
    .replace(/^- (.+)$/gm, '<div style="padding:2px 0 2px 16px;position:relative"><span style="position:absolute;left:4px;color:var(--accent)">\u2022</span>$1</div>')
    .replace(/^\d+\. (.+)$/gm, '<div style="padding:2px 0 2px 20px">$1</div>')
    .replace(/\n\n/g, '<br><br>')
    .replace(/\n/g, '<br>');
}

// Helper: render items as a read-only table
function renderItemList(items, columns, emptyMsg) {
  if (!items || items.length === 0) return '<div style="color:var(--fg2);padding:8px;font-size:12px">' + (emptyMsg || 'No items configured. Edit in Raw Config.') + '</div>';
  let html = '<table style="width:100%;border-collapse:collapse;font-size:12px">';
  html += '<tr style="border-bottom:1px solid var(--border)">';
  for (const col of columns) html += '<th style="text-align:left;padding:6px 8px;color:var(--fg2);font-weight:500">' + col.label + '</th>';
  html += '</tr>';
  for (const item of items) {
    html += '<tr style="border-bottom:1px solid rgba(255,255,255,0.03)">';
    for (const col of columns) {
      let v = item[col.key];
      if (v === undefined || v === null) v = '';
      const display = Array.isArray(v) ? v.join(', ') : String(v);
      const style = col.mono ? 'font-family:var(--mono);font-size:11px;' : '';
      html += '<td style="padding:6px 8px;color:var(--fg);word-break:break-all;' + style + '">' + esc(display) + '</td>';
    }
    html += '</tr>';
  }
  html += '</table>';
  return html;
}
</script>
</body>
</html>`
