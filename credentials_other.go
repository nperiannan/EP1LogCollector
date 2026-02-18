//go:build !windows
// +build !windows

package main

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/term"
)

// Credential storage stubs for non-Windows platforms
// On Linux/macOS, credentials fall back to config file or env vars
// ================================================================

// storeJIRATokenInKeychain - stub for non-Windows platforms
func storeJIRATokenInKeychain(email, token string, logger *Logger) error {
	return fmt.Errorf("OS keychain storage not available on this platform (use environment variables or config file)")
}

// retrieveJIRATokenFromKeychain - stub for non-Windows platforms
func retrieveJIRATokenFromKeychain(email string, logger *Logger) (string, error) {
	return "", nil // Not found, not an error
}

// getJIRAApiToken retrieves JIRA API token using multi-source priority (non-Windows):
// 1. Environment variable (JIRA_API_TOKEN)
// 2. Config file (encrypted or plain)
// 3. Interactive prompt
func getJIRAApiToken(jiraConfig *JiraConfig, logger *Logger) (string, error) {
	// Priority 1: Environment variable
	if envToken := os.Getenv("JIRA_API_TOKEN"); envToken != "" {
		logger.Debug("Using JIRA API token from environment variable")
		return envToken, nil
	}

	// Priority 2: Config file
	if jiraConfig.ApiToken != "" {
		logger.Debug("Using JIRA API token from config.yaml")
		return jiraConfig.ApiToken, nil
	}

	// Priority 3: Interactive prompt
	if jiraConfig.Email == "" {
		return "", fmt.Errorf("JIRA email not configured - cannot retrieve token")
	}

	logger.Info("JIRA API token not found. Please enter your token:")
	logger.Info("Generate at: https://id.atlassian.com/manage-profile/security/api-tokens")
	logger.Info("Note: On Linux/macOS, use environment variable JIRA_API_TOKEN or config.yaml for persistence")

	fmt.Print("Enter JIRA API Token (input hidden): ")
	tokenBytes, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println() // newline after hidden input

	if err != nil {
		return "", fmt.Errorf("failed to read token: %v", err)
	}

	token := string(tokenBytes)
	if token == "" {
		return "", fmt.Errorf("no token entered")
	}

	logger.Warn("Token entered but not saved (no keychain support on this platform)")
	logger.Info("To persist token, set JIRA_API_TOKEN environment variable or add to config.yaml")

	return token, nil
}

// storeBastionPasswordInKeychain - stub for non-Windows platforms
func storeBastionPasswordInKeychain(username, bastionHost, password string, logger *Logger) error {
	return fmt.Errorf("OS keychain storage not available on this platform (use environment variables or config file)")
}

// retrieveBastionPasswordFromKeychain - stub for non-Windows platforms
func retrieveBastionPasswordFromKeychain(username, bastionHost string, logger *Logger) (string, error) {
	return "", nil // Not found, not an error
}

// getBastionPassword retrieves bastion password using multi-source priority (non-Windows):
// 1. Environment variable (BASTION_PASSWORD)
// 2. Config file (encrypted or plain)
// 3. Interactive prompt
func getBastionPassword(username, bastionHost, configPassword string, logger *Logger) (string, bool, error) {
	// Return: (password, needsSaving, error)

	// Priority 1: Environment variable
	if envPassword := os.Getenv("BASTION_PASSWORD"); envPassword != "" {
		logger.Debug("Using bastion password from environment variable")
		return envPassword, false, nil
	}

	// Priority 2: Config file (try to decrypt if encrypted)
	if configPassword != "" {
		decryptedPassword, err := decryptPassword(configPassword)
		if err != nil {
			logger.Warn("Failed to decrypt password from config: %v", err)
			logger.Debug("Will prompt for new password")
		} else {
			logger.Debug("Using bastion password from config.yaml")
			return decryptedPassword, false, nil
		}
	}

	// Priority 3: Interactive prompt
	if username == "" || bastionHost == "" {
		return "", false, fmt.Errorf("username and bastion host required for password prompt")
	}

	logger.Info("Bastion password not found. Please enter your password:")
	logger.Info("Note: On Linux/macOS, use environment variable BASTION_PASSWORD or config.yaml for persistence")

	newPass, err := promptPassword("Enter bastion password: ")
	if err != nil {
		return "", false, fmt.Errorf("failed to read password: %v", err)
	}

	if newPass == "" {
		return "", false, fmt.Errorf("no password entered")
	}

	logger.Warn("Password entered but not saved (no keychain support on this platform)")
	logger.Info("To persist password, set BASTION_PASSWORD environment variable or add (encrypted) to config.yaml")

	// Non-Windows: needs saving to config
	return newPass, true, nil
}
