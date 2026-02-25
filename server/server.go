package server

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"broxy/config"
	"broxy/proxy"
	"broxy/router"

	"github.com/fsnotify/fsnotify"
)

// bufferPool reuses 32KB buffers for connection copying
var bufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

type Server struct {
	router      *router.Router
	routerMu    sync.RWMutex
	listen      string
	port        int
	configFile  string
	transports  map[string]*http.Transport
	transportMu sync.RWMutex
	connPool    *proxy.Pool
}

func New(r *router.Router, cfg *config.Config, configFile string) *Server {
	return &Server{
		router:     r,
		listen:     cfg.Server.Listen,
		port:       cfg.Server.Port,
		configFile: configFile,
		transports: make(map[string]*http.Transport),
		connPool:   proxy.NewPool(toPoolConfig(&cfg.Pool)),
	}
}

// toPoolConfig converts config.PoolConfig to proxy.PoolConfig with defaults
func toPoolConfig(cfg *config.PoolConfig) proxy.PoolConfig {
	pc := proxy.DefaultPoolConfig()
	if cfg.MaxIdlePerHost > 0 {
		pc.MaxIdlePerHost = cfg.MaxIdlePerHost
	}
	if cfg.MaxIdleTotal > 0 {
		pc.MaxIdleTotal = cfg.MaxIdleTotal
	}
	if idleTimeout, err := cfg.ParseIdleTimeout(); err == nil && idleTimeout > 0 {
		pc.IdleTimeout = idleTimeout
	}
	return pc
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
	for _, t := range s.transports {
		t.CloseIdleConnections()
	}
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
			DialContext:         proxy.DirectDialContext,
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
			DialContext:         proxy.DirectDialContext,
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

	// Watch the parent directory, not the file itself.
	// Editors perform atomic saves (write temp + rename), which replaces the
	// file inode and silently kills an inode-level watch. A directory watch
	// survives because the directory inode is stable.
	configDir := filepath.Dir(s.configFile)
	configBase := filepath.Base(s.configFile)
	if err := watcher.Add(configDir); err != nil {
		return fmt.Errorf("failed to watch config directory: %w", err)
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
				// Only react to events on the config file itself
				if filepath.Base(event.Name) != configBase {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
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

// Close shuts down the server's connection pool and releases resources
func (s *Server) Close() {
	if s.connPool != nil {
		s.connPool.Close()
	}
	s.transportMu.Lock()
	for _, t := range s.transports {
		t.CloseIdleConnections()
	}
	s.transports = make(map[string]*http.Transport)
	s.transportMu.Unlock()
}

const defaultIdleTimeout = 60 * time.Second

// deadlineRefreshInterval controls how often copyConn extends read/write deadlines.
func deadlineRefreshInterval(idleTimeout time.Duration) time.Duration {
	interval := idleTimeout / 4
	if interval < 10*time.Millisecond {
		return 10 * time.Millisecond
	}
	if interval > time.Second {
		return time.Second
	}
	return interval
}

// copyConn copies data with idle timeout reset per operation
func copyConn(dst, src net.Conn, idleTimeout time.Duration) (int64, error) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	buf := *bufPtr

	refreshEvery := deadlineRefreshInterval(idleTimeout)
	var nextReadDeadlineSet time.Time
	var nextWriteDeadlineSet time.Time

	var written int64
	for {
		now := time.Now()
		if now.After(nextReadDeadlineSet) {
			src.SetReadDeadline(now.Add(idleTimeout))
			nextReadDeadlineSet = now.Add(refreshEvery)
		}

		nr, rerr := src.Read(buf)
		if nr > 0 {
			now = time.Now()
			if now.After(nextWriteDeadlineSet) {
				dst.SetWriteDeadline(now.Add(idleTimeout))
				nextWriteDeadlineSet = now.Add(refreshEvery)
			}
			nw, werr := dst.Write(buf[:nr])
			written += int64(nw)
			if werr != nil {
				return written, werr
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return written, nil
			}
			return written, rerr
		}
	}
}

// closeWrite sends TCP FIN if supported
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// tuneTCPConn applies TCP optimizations to the connection
func tuneTCPConn(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
}

// isExpectedCloseError returns true for normal close/timeout errors
func isExpectedCloseError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "closed")
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

	// Connect through the configured proxy, using pool for HTTP proxies
	destConn, err := proxy.DialWithPool(proxyConfig, "tcp", r.Host, s.connPool)
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

	// Apply TCP optimizations to both connections
	tuneTCPConn(clientConn)
	tuneTCPConn(destConn)

	// Send 200 Connection established
	clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))

	// Bidirectional copy with proper synchronization
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, err := copyConn(destConn, clientConn, defaultIdleTimeout)
		if !isExpectedCloseError(err) {
			log.Printf("Copy client->dest error for %s: %v", r.Host, err)
		}
		closeWrite(destConn)
	}()

	go func() {
		defer wg.Done()
		_, err := copyConn(clientConn, destConn, defaultIdleTimeout)
		if !isExpectedCloseError(err) {
			log.Printf("Copy dest->client error for %s: %v", r.Host, err)
		}
		closeWrite(clientConn)
	}()

	wg.Wait()
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
