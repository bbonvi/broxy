package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"broxy/config"
	"broxy/router"
	"broxy/server"
)

func getDefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "rules.yaml"
	}
	return filepath.Join(home, ".config", "broxy", "rules.yaml")
}

func main() {
	// Check for subcommands
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		runProxy()
	case "help", "-h", "--help":
		printHelp()
	case "version", "-v", "--version":
		fmt.Println("broxy version 1.0.0")
	default:
		fmt.Printf("Unknown command: %s\n\n", os.Args[1])
		printHelp()
		os.Exit(1)
	}
}

func runProxy() {
	// Parse flags for run command
	runCmd := flag.NewFlagSet("run", flag.ExitOnError)
	configFile := runCmd.String("config", getDefaultConfigPath(), "Path to configuration file")
	runCmd.Parse(os.Args[2:])

	// Load configuration
	cfg, err := config.Load(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create router
	r := router.New(cfg)

	// Create and start server
	srv := server.New(r, cfg.Server.Listen, cfg.Server.Port, *configFile)
	if err := srv.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func printHelp() {
	fmt.Println("Broxy - Simple Proxy Forwarder")
	fmt.Println("\nUsage:")
	fmt.Println("  broxy [command]")
	fmt.Println("\nCommands:")
	fmt.Println("  run         Run the proxy server")
	fmt.Println("  help        Show this help message")
	fmt.Println("  version     Show version information")
	fmt.Println("\nRun Flags:")
	fmt.Println("  -config string")
	fmt.Printf("        Path to configuration file (default: %s)\n", getDefaultConfigPath())
	fmt.Println("\nExamples:")
	fmt.Println("  broxy run                            # Run with default config")
	fmt.Println("  broxy run -config /path/to/rules.yaml  # Run with custom config")
}
