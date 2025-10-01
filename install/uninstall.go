package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Uninstall removes broxy service and binary
func Uninstall() error {
	// Check OS support
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("unsupported operating system: %s (only linux and darwin are supported)", runtime.GOOS)
	}

	configPath, err := getConfigPath()
	if err != nil {
		return err
	}

	// Show uninstallation plan
	fmt.Println("Broxy Uninstallation")
	fmt.Println("====================")
	fmt.Printf("Operating System: %s\n", runtime.GOOS)
	fmt.Printf("Binary will be removed from: %s\n", binaryPath)

	if runtime.GOOS == "linux" {
		fmt.Printf("Service file will be removed: /etc/systemd/system/%s.service\n", binaryName)
		fmt.Println("\nNote: Uninstallation requires sudo privileges on Linux")
	} else {
		home, _ := os.UserHomeDir()
		fmt.Printf("Service file will be removed: %s/Library/LaunchAgents/com.%s.plist\n", home, binaryName)
	}

	fmt.Printf("\nConfig directory (%s) will NOT be removed\n", filepath.Dir(configPath))

	// Prompt for confirmation
	fmt.Print("\nProceed with uninstallation? (y/N): ")
	var response string
	fmt.Scanln(&response)
	response = strings.ToLower(strings.TrimSpace(response))

	if response != "y" && response != "yes" {
		fmt.Println("Uninstallation cancelled")
		return nil
	}

	// Uninstall service
	if runtime.GOOS == "linux" {
		if err := uninstallLinuxService(); err != nil {
			fmt.Printf("Warning: %v\n", err)
		}
	} else {
		if err := uninstallMacOSService(); err != nil {
			fmt.Printf("Warning: %v\n", err)
		}
	}

	// Remove binary
	if err := os.Remove(binaryPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove binary (try running with sudo): %w", err)
		}
		fmt.Printf("Binary not found at %s, skipping\n", binaryPath)
	} else {
		fmt.Printf("✓ Removed binary from %s\n", binaryPath)
	}

	fmt.Println("\n✓ Uninstallation completed successfully!")
	fmt.Printf("\nConfig directory preserved at: %s\n", filepath.Dir(configPath))
	fmt.Println("Remove it manually if you no longer need it.")

	return nil
}

// uninstallLinuxService uninstalls systemd service on Linux
func uninstallLinuxService() error {
	servicePath := fmt.Sprintf("/etc/systemd/system/%s.service", binaryName)

	// Check if service exists
	if _, err := os.Stat(servicePath); os.IsNotExist(err) {
		fmt.Printf("Service file not found at %s, skipping\n", servicePath)
		return nil
	}

	// Stop service
	if err := exec.Command("systemctl", "stop", binaryName).Run(); err != nil {
		fmt.Printf("Warning: failed to stop service: %v\n", err)
	}

	// Disable service
	if err := exec.Command("systemctl", "disable", binaryName).Run(); err != nil {
		fmt.Printf("Warning: failed to disable service: %v\n", err)
	}

	// Remove service file
	if err := os.Remove(servicePath); err != nil {
		return fmt.Errorf("failed to remove service file (try running with sudo): %w", err)
	}

	// Reload systemd
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		fmt.Printf("Warning: failed to reload systemd: %v\n", err)
	}

	fmt.Printf("✓ Removed systemd service\n")
	return nil
}

// uninstallMacOSService uninstalls launchd service on macOS
func uninstallMacOSService() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", fmt.Sprintf("com.%s.plist", binaryName))
	serviceName := fmt.Sprintf("com.%s", binaryName)

	// Check if plist exists
	plistExists := true
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Printf("Service file not found at %s\n", plistPath)
		plistExists = false
	}

	// Try to kill the service first
	exec.Command("launchctl", "kill", "SIGTERM", fmt.Sprintf("gui/%s/%s", os.Getenv("UID"), serviceName)).Run()

	// Bootout (modern macOS way to unload)
	if err := exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%s", os.Getenv("UID")), plistPath).Run(); err != nil {
		// Try legacy unload if bootout fails
		if err := exec.Command("launchctl", "unload", plistPath).Run(); err != nil {
			fmt.Printf("Warning: failed to unload service: %v\n", err)
		}
	}

	// Remove from launchd database
	exec.Command("launchctl", "remove", serviceName).Run()

	// Kill any remaining processes
	exec.Command("pkill", "-9", binaryName).Run()

	// Remove plist file if it exists
	if plistExists {
		if err := os.Remove(plistPath); err != nil {
			return fmt.Errorf("failed to remove plist file: %w", err)
		}
	}

	fmt.Printf("✓ Removed launchd service\n")
	return nil
}
