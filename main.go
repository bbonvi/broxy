package main

import (
	"flag"
	"log"

	"broxy/config"
	"broxy/router"
	"broxy/server"
)

func main() {
	configFile := flag.String("config", "rules.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create router
	r := router.New(cfg)

	// Create and start server
	srv := server.New(r, cfg.Server.Listen, cfg.Server.Port)
	if err := srv.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
