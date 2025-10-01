package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	binaryName     = "broxy"
	binaryPath     = "/usr/local/bin/broxy"
	configDir      = ".config/broxy"
	configFileName = "rules.yaml"
)

// getConfigDir returns the full path to the config directory
func getConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, configDir), nil
}

// getConfigPath returns the full path to the config file
func getConfigPath() (string, error) {
	dir, err := getConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

// Install installs broxy as a system service
func Install() error {
	// Check OS support
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("unsupported operating system: %s (only linux and darwin are supported)", runtime.GOOS)
	}

	// Get executable path
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	configPath, err := getConfigPath()
	if err != nil {
		return err
	}

	// Show installation plan
	fmt.Println("Broxy Installation")
	fmt.Println("==================")
	fmt.Printf("Operating System: %s\n", runtime.GOOS)
	fmt.Printf("Binary will be copied to: %s\n", binaryPath)
	fmt.Printf("Config directory: %s\n", filepath.Dir(configPath))
	fmt.Printf("Config file: %s\n", configPath)

	if runtime.GOOS == "linux" {
		fmt.Printf("Service file: /etc/systemd/system/%s.service\n", binaryName)
		fmt.Println("\nNote: Installation requires sudo privileges on Linux")
	} else {
		home, _ := os.UserHomeDir()
		fmt.Printf("Service file: %s/Library/LaunchAgents/com.%s.plist\n", home, binaryName)
	}

	// Prompt for confirmation
	fmt.Print("\nProceed with installation? (y/N): ")
	var response string
	fmt.Scanln(&response)
	response = strings.ToLower(strings.TrimSpace(response))

	if response != "y" && response != "yes" {
		fmt.Println("Installation cancelled")
		return nil
	}

	// Create config directory and file
	if err := createConfigDir(); err != nil {
		return err
	}

	if err := createDefaultConfig(); err != nil {
		return err
	}

	// Copy binary
	if err := copyBinary(exePath, binaryPath); err != nil {
		return err
	}

	// Install service
	if runtime.GOOS == "linux" {
		if err := installLinuxService(); err != nil {
			return err
		}
	} else {
		if err := installMacOSService(); err != nil {
			return err
		}
	}

	fmt.Println("\n✓ Installation completed successfully!")
	fmt.Printf("✓ Config file created at: %s\n", configPath)
	fmt.Println("✓ Service installed and started")
	fmt.Println("\nEdit the config file to configure your proxies and rules.")

	return nil
}

// createConfigDir creates the config directory if it doesn't exist
func createConfigDir() error {
	dir, err := getConfigDir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	return nil
}

// createDefaultConfig creates a default configuration file
func createDefaultConfig() error {
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}

	// Check if config already exists
	if _, err := os.Stat(configPath); err == nil {
		fmt.Printf("Config file already exists at %s, skipping creation\n", configPath)
		return nil
	}

	// Default config content
	defaultConfig := `server:
  listen: 0.0.0.0
  port: 3128

proxies:
  - name: direct
    type: direct

rules:
  - match: default
    proxy: direct
`

	if err := os.WriteFile(configPath, []byte(defaultConfig), 0644); err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}

	return nil
}

// copyBinary copies the executable to the target location
func copyBinary(src, dst string) error {
	// Get absolute paths for comparison
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("failed to get absolute path of source: %w", err)
	}

	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("failed to get absolute path of destination: %w", err)
	}

	// Check if source and destination are the same
	if srcAbs == dstAbs {
		fmt.Printf("Binary already at %s, skipping copy\n", dst)
		return nil
	}

	// Check if destination already exists and is the same as source
	if dstInfo, err := os.Stat(dst); err == nil {
		srcInfo, err := os.Stat(src)
		if err == nil && os.SameFile(srcInfo, dstInfo) {
			fmt.Printf("Binary already at %s, skipping copy\n", dst)
			return nil
		}
	}

	// Read source file
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("failed to read binary: %w", err)
	}

	// Write to destination
	if err := os.WriteFile(dst, data, 0755); err != nil {
		return fmt.Errorf("failed to copy binary (try running with sudo): %w", err)
	}

	return nil
}

// installLinuxService installs systemd service on Linux
func installLinuxService() error {
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}

	serviceContent := fmt.Sprintf(`[Unit]
Description=Broxy Proxy Forwarder
After=network.target

[Service]
ExecStart=%s run -config %s
Restart=always
User=%s

[Install]
WantedBy=multi-user.target
`, binaryPath, configPath, os.Getenv("USER"))

	servicePath := fmt.Sprintf("/etc/systemd/system/%s.service", binaryName)

	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to create service file (try running with sudo): %w", err)
	}

	// Reload systemd
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}

	// Enable service
	if err := exec.Command("systemctl", "enable", binaryName).Run(); err != nil {
		return fmt.Errorf("failed to enable service: %w", err)
	}

	// Start service
	if err := exec.Command("systemctl", "start", binaryName).Run(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	return nil
}

// installMacOSService installs launchd service on macOS
func installMacOSService() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	configPath, err := getConfigPath()
	if err != nil {
		return err
	}

	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>run</string>
        <string>-config</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>%s/.config/broxy/broxy.log</string>
    <key>StandardErrorPath</key>
    <string>%s/.config/broxy/broxy.error.log</string>
</dict>
</plist>
`, binaryName, binaryPath, configPath, home, home)

	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
	}

	plistPath := filepath.Join(launchAgentsDir, fmt.Sprintf("com.%s.plist", binaryName))
	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("failed to create plist file: %w", err)
	}

	// Load service
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		return fmt.Errorf("failed to load service: %w", err)
	}

	return nil
}
