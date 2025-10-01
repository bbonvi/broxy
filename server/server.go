package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"broxy/config"
	"broxy/proxy"
	"broxy/router"

	"github.com/fsnotify/fsnotify"
)

type Server struct {
	router         *router.Router
	routerMu       sync.RWMutex
	listen         string
	port           int
	configFile     string
	transports     map[string]*http.Transport
	transportMu    sync.RWMutex
}

func New(r *router.Router, listen string, port int, configFile string) *Server {
	return &Server{
		router:     r,
		listen:     listen,
		port:       port,
		configFile: configFile,
		transports: make(map[string]*http.Transport),
	}
}

func (s *Server) reloadConfig() error {
	log.Printf("Reloading config from %s", s.configFile)

	cfg, err := config.Load(s.configFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	newRouter := router.New(cfg)

	// Update router with write lock
	s.routerMu.Lock()
	s.router = newRouter
	s.routerMu.Unlock()

	// Clear transport cache (proxy configs might have changed)
	s.transportMu.Lock()
	s.transports = make(map[string]*http.Transport)
	s.transportMu.Unlock()

	log.Printf("Config reloaded successfully")
	return nil
}

func (s *Server) getRouter() *router.Router {
	s.routerMu.RLock()
	defer s.routerMu.RUnlock()
	return s.router
}

func (s *Server) getOrCreateTransport(proxyConfig *config.ProxyConfig) (*http.Transport, error) {
	// Create cache key from proxy config
	cacheKey := fmt.Sprintf("%s:%s:%d", proxyConfig.Type, proxyConfig.Host, proxyConfig.Port)
	if proxyConfig.Auth != nil {
		cacheKey += ":" + proxyConfig.Auth.Username
	}

	// Try read lock first
	s.transportMu.RLock()
	transport, exists := s.transports[cacheKey]
	s.transportMu.RUnlock()
	if exists {
		return transport, nil
	}

	// Need to create new transport
	s.transportMu.Lock()
	defer s.transportMu.Unlock()

	// Double-check after acquiring write lock
	if transport, exists := s.transports[cacheKey]; exists {
		return transport, nil
	}

	// Create new transport based on proxy type
	var newTransport *http.Transport

	switch proxyConfig.Type {
	case "direct":
		newTransport = &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		}
	case "http":
		var proxyURL string
		if proxyConfig.Auth != nil {
			proxyURL = fmt.Sprintf("http://%s:%s@%s:%d",
				url.QueryEscape(proxyConfig.Auth.Username),
				url.QueryEscape(proxyConfig.Auth.Password),
				proxyConfig.Host,
				proxyConfig.Port)
		} else {
			proxyURL = fmt.Sprintf("http://%s:%d", proxyConfig.Host, proxyConfig.Port)
		}
		proxyURLParsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		newTransport = &http.Transport{
			Proxy:               http.ProxyURL(proxyURLParsed),
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		}
	default:
		return nil, fmt.Errorf("unsupported proxy type for HTTP: %s", proxyConfig.Type)
	}

	s.transports[cacheKey] = newTransport
	return newTransport, nil
}

func (s *Server) Start() error {
	addr := fmt.Sprintf("%s:%d", s.listen, s.port)
	log.Printf("Starting proxy server on %s", addr)

	// Set up file watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	defer watcher.Close()

	err = watcher.Add(s.configFile)
	if err != nil {
		return fmt.Errorf("failed to watch config file: %w", err)
	}

	// Start goroutine to handle file changes
	go func() {
		var debounceTimer *time.Timer
		var debounceMu sync.Mutex

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Handle Write, Create, Remove, Rename events (editors often do atomic writes)
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
					// Re-add watch in case file was removed/recreated (atomic write)
					if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
						time.Sleep(100 * time.Millisecond) // Wait for file recreation
						if err := watcher.Add(s.configFile); err != nil {
							log.Printf("Failed to re-add watch: %v", err)
						}
					}

					// Debounce: wait for writes to settle before reloading
					debounceMu.Lock()
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					debounceTimer = time.AfterFunc(200*time.Millisecond, func() {
						if err := s.reloadConfig(); err != nil {
							log.Printf("Error reloading config: %v", err)
						}
					})
					debounceMu.Unlock()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("File watcher error: %v", err)
			}
		}
	}()

	server := &http.Server{
		Addr:    addr,
		Handler: http.HandlerFunc(s.handleRequest),
	}

	return server.ListenAndServe()
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
	} else {
		s.handleHTTP(w, r)
	}
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	// Find the appropriate proxy for this host
	proxyConfig := s.getRouter().Route(r.Host)
	if proxyConfig == nil {
		http.Error(w, "No proxy configured", http.StatusBadGateway)
		return
	}

	// Connect through the configured proxy
	destConn, err := proxy.Dial(proxyConfig, "tcp", r.Host)
	if err != nil {
		log.Printf("Error connecting to %s: %v", r.Host, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer destConn.Close()

	// Hijack the connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Send 200 Connection established
	clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))

	// Bidirectional copy
	go io.Copy(destConn, clientConn)
	io.Copy(clientConn, destConn)
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Find the appropriate proxy for this host
	proxyConfig := s.getRouter().Route(r.Host)
	if proxyConfig == nil {
		http.Error(w, "No proxy configured", http.StatusBadGateway)
		return
	}

	// Handle SOCKS5 separately (not supported for plain HTTP)
	if proxyConfig.Type == "socks5" {
		http.Error(w, "SOCKS5 for plain HTTP not fully supported, use HTTPS", http.StatusNotImplemented)
		return
	}

	// Get or create cached transport
	transport, err := s.getOrCreateTransport(proxyConfig)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	client := &http.Client{
		Transport: transport,
	}

	// Create new request
	proxyReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy headers (excluding proxy-specific headers)
	for key, values := range r.Header {
		if key == "Proxy-Connection" {
			continue
		}
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
