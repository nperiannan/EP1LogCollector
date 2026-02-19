//go:build windows
// +build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/term"
)

// Windows Credential Manager Integration
// Secure storage for JIRA API tokens and bastion credentials
// ================================================================

// Windows Credential Manager structures and functions (Windows-only)
const (
	CRED_TYPE_GENERIC          = 1
	CRED_PERSIST_LOCAL_MACHINE = 2
)

type CREDENTIAL struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	advapi32DLL *syscall.LazyDLL
	credWriteW  *syscall.LazyProc
	credReadW   *syscall.LazyProc
	credDeleteW *syscall.LazyProc
	credFree    *syscall.LazyProc
)

func initCredentialManager() {
	if runtime.GOOS == "windows" {
		advapi32DLL = syscall.NewLazyDLL("advapi32.dll")
		credWriteW = advapi32DLL.NewProc("CredWriteW")
		credReadW = advapi32DLL.NewProc("CredReadW")
		credDeleteW = advapi32DLL.NewProc("CredDeleteW")
		credFree = advapi32DLL.NewProc("CredFree")
	}
}

// storeJIRATokenInKeychain stores JIRA API token in Windows Credential Manager
func storeJIRATokenInKeychain(email, token string, logger *Logger) error {
	initCredentialManager()

	targetName := "LogCollector:JIRA:" + email

	targetNamePtr, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return fmt.Errorf("failed to convert target name: %v", err)
	}

	usernamePtr, err := syscall.UTF16PtrFromString(email)
	if err != nil {
		return fmt.Errorf("failed to convert username: %v", err)
	}

	commentPtr, err := syscall.UTF16PtrFromString("JIRA API Token for LogCollector")
	if err != nil {
		return fmt.Errorf("failed to convert comment: %v", err)
	}

	tokenBytes := []byte(token)

	cred := CREDENTIAL{
		Type:               CRED_TYPE_GENERIC,
		TargetName:         targetNamePtr,
		Comment:            commentPtr,
		CredentialBlobSize: uint32(len(tokenBytes)),
		CredentialBlob:     &tokenBytes[0],
		Persist:            CRED_PERSIST_LOCAL_MACHINE,
		UserName:           usernamePtr,
	}

	ret, _, err := credWriteW.Call(
		uintptr(unsafe.Pointer(&cred)),
		0,
	)

	if ret == 0 {
		return fmt.Errorf("failed to store credential: %v", err)
	}

	logger.Debug("JIRA token saved to Windows Credential Manager for %s", email)
	return nil
}

// retrieveJIRATokenFromKeychain retrieves JIRA API token from Windows Credential Manager
func retrieveJIRATokenFromKeychain(email string, logger *Logger) (string, error) {
	initCredentialManager()

	targetName := "LogCollector:JIRA:" + email

	targetNamePtr, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return "", fmt.Errorf("failed to convert target name: %v", err)
	}

	var credPtr *CREDENTIAL
	ret, _, err := credReadW.Call(
		uintptr(unsafe.Pointer(targetNamePtr)),
		CRED_TYPE_GENERIC,
		0,
		uintptr(unsafe.Pointer(&credPtr)),
	)

	if ret == 0 {
		// ERROR_NOT_FOUND = 1168
		if errno, ok := err.(syscall.Errno); ok && errno == 1168 {
			return "", nil // Not found, not an error
		}
		return "", fmt.Errorf("failed to read credential: %v", err)
	}

	defer credFree.Call(uintptr(unsafe.Pointer(credPtr)))

	cred := credPtr

	// Extract token from credential blob using unsafe.Slice
	blob := unsafe.Slice((*byte)(unsafe.Pointer(cred.CredentialBlob)), cred.CredentialBlobSize)
	tokenBytes := make([]byte, cred.CredentialBlobSize)
	copy(tokenBytes, blob)

	logger.Debug("JIRA token retrieved from Windows Credential Manager for %s", email)
	return string(tokenBytes), nil
}

// getJIRAApiToken retrieves JIRA API token using multi-source priority:
// 1. Environment variable (JIRA_API_TOKEN)
// 2. Windows Credential Manager (OS keychain)
// 3. Config file (encrypted or plain)
// 4. Interactive prompt (saves to keychain if available)
func getJIRAApiToken(jiraConfig *JiraConfig, logger *Logger) (string, error) {
	// Priority 1: Environment variable
	if envToken := os.Getenv("JIRA_API_TOKEN"); envToken != "" {
		logger.Debug("Using JIRA API token from environment variable")
		return envToken, nil
	}

	// Priority 2: Windows Credential Manager
	if jiraConfig.Email != "" {
		if token, err := retrieveJIRATokenFromKeychain(jiraConfig.Email, logger); err == nil && token != "" {
			logger.Debug("Using JIRA API token from Windows Credential Manager")
			return token, nil
		}
	}

	// Priority 3: Config file
	if jiraConfig.ApiToken != "" {
		logger.Debug("Using JIRA API token from config.yaml")
		return jiraConfig.ApiToken, nil
	}

	// Priority 4: Interactive prompt
	if jiraConfig.Email == "" {
		return "", fmt.Errorf("JIRA email not configured - cannot retrieve token")
	}

	logger.Info("JIRA API token not found. Please enter your token:")
	logger.Info("Generate at: https://id.atlassian.com/manage-profile/security/api-tokens")

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

	// Try to save to Windows Credential Manager for future use
	if err := storeJIRATokenInKeychain(jiraConfig.Email, token, logger); err != nil {
		logger.Warn("Failed to save token to Windows Credential Manager: %v", err)
		logger.Info("Token will work for this session, but won't be saved for future use")
	} else {
		logger.Info("✓ Token saved to Windows Credential Manager for future use")
	}

	return token, nil
}

// storeBastionPasswordInKeychain stores bastion password in Windows Credential Manager
func storeBastionPasswordInKeychain(username, bastionHost, password string, logger *Logger) error {
	initCredentialManager()

	targetName := fmt.Sprintf("LogCollector:Bastion:%s@%s", username, bastionHost)

	targetNamePtr, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return fmt.Errorf("failed to convert target name: %v", err)
	}

	usernamePtr, err := syscall.UTF16PtrFromString(username)
	if err != nil {
		return fmt.Errorf("failed to convert username: %v", err)
	}

	commentPtr, err := syscall.UTF16PtrFromString("Bastion SSH Password for LogCollector")
	if err != nil {
		return fmt.Errorf("failed to convert comment: %v", err)
	}

	passwordBytes := []byte(password)

	cred := CREDENTIAL{
		Type:               CRED_TYPE_GENERIC,
		TargetName:         targetNamePtr,
		Comment:            commentPtr,
		CredentialBlobSize: uint32(len(passwordBytes)),
		CredentialBlob:     &passwordBytes[0],
		Persist:            CRED_PERSIST_LOCAL_MACHINE,
		UserName:           usernamePtr,
	}

	ret, _, err := credWriteW.Call(
		uintptr(unsafe.Pointer(&cred)),
		0,
	)

	if ret == 0 {
		return fmt.Errorf("failed to store credential: %v", err)
	}

	logger.Debug("Bastion password saved to Windows Credential Manager for %s@%s", username, bastionHost)
	return nil
}

// retrieveBastionPasswordFromKeychain retrieves bastion password from Windows Credential Manager
func retrieveBastionPasswordFromKeychain(username, bastionHost string, logger *Logger) (string, error) {
	initCredentialManager()

	targetName := fmt.Sprintf("LogCollector:Bastion:%s@%s", username, bastionHost)

	targetNamePtr, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return "", fmt.Errorf("failed to convert target name: %v", err)
	}

	var credPtr *CREDENTIAL
	ret, _, err := credReadW.Call(
		uintptr(unsafe.Pointer(targetNamePtr)),
		CRED_TYPE_GENERIC,
		0,
		uintptr(unsafe.Pointer(&credPtr)),
	)

	if ret == 0 {
		// ERROR_NOT_FOUND = 1168
		if errno, ok := err.(syscall.Errno); ok && errno == 1168 {
			return "", nil // Not found, not an error
		}
		return "", fmt.Errorf("failed to read credent ial: %v", err)
	}

	defer credFree.Call(uintptr(unsafe.Pointer(credPtr)))

	cred := credPtr

	// Extract password from credential blob using unsafe.Slice
	blob := unsafe.Slice((*byte)(unsafe.Pointer(cred.CredentialBlob)), cred.CredentialBlobSize)
	passwordBytes := make([]byte, cred.CredentialBlobSize)
	copy(passwordBytes, blob)

	logger.Debug("Bastion password retrieved from Windows Credential Manager for %s@%s", username, bastionHost)
	return string(passwordBytes), nil
}

// getBastionPassword retrieves bastion password using multi-source priority:
// 1. Environment variable (BASTION_PASSWORD)
// 2. Windows Credential Manager (OS keychain)
// 3. Config file (encrypted or plain)
// 4. Interactive prompt (saves to keychain if available)
func getBastionPassword(username, bastionHost, configPassword string, logger *Logger) (string, bool, error) {
	// Return: (password, needsSaving, error)

	// Priority 1: Environment variable
	if envPassword := os.Getenv("BASTION_PASSWORD"); envPassword != "" {
		logger.Debug("Using bastion password from environment variable")
		return envPassword, false, nil
	}

	// Priority 2: Windows Credential Manager
	if username != "" && bastionHost != "" {
		if password, err := retrieveBastionPasswordFromKeychain(username, bastionHost, logger); err == nil && password != "" {
			logger.Debug("Using bastion password from Windows Credential Manager")
			return password, false, nil
		}
	}

	// Priority 3: Config file (try to decrypt if encrypted)
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

	// Priority 4: Interactive prompt
	if username == "" || bastionHost == "" {
		return "", false, fmt.Errorf("username and bastion host required for password prompt")
	}

	logger.Info("Bastion password not found. Please enter your password:")

	newPass, err := promptPassword("Enter bastion password: ")
	if err != nil {
		return "", false, fmt.Errorf("failed to read password: %v", err)
	}

	if newPass == "" {
		return "", false, fmt.Errorf("no password entered")
	}

	// Try to save to Windows Credential Manager for future use
	if err := storeBastionPasswordInKeychain(username, bastionHost, newPass, logger); err != nil {
		logger.Warn("Failed to save password to Windows Credential Manager: %v", err)
		logger.Info("Password will work for this session, but won't be saved for future use")
		return newPass, true, nil // Still needs saving to config
	} else {
		logger.Info("✓ Password saved to Windows Credential Manager for future use")
		return newPass, false, nil // Saved to keychain, no need to save to config
	}
}
