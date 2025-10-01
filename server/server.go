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
)

type Server struct {
	router     *router.Router
	listen     string
	port       int
	transports map[string]*http.Transport
	mu         sync.RWMutex
}

func New(r *router.Router, listen string, port int) *Server {
	return &Server{
		router:     r,
		listen:     listen,
		port:       port,
		transports: make(map[string]*http.Transport),
	}
}

func (s *Server) getOrCreateTransport(proxyConfig *config.ProxyConfig) (*http.Transport, error) {
	// Create cache key from proxy config
	cacheKey := fmt.Sprintf("%s:%s:%d", proxyConfig.Type, proxyConfig.Host, proxyConfig.Port)
	if proxyConfig.Auth != nil {
		cacheKey += ":" + proxyConfig.Auth.Username
	}

	// Try read lock first
	s.mu.RLock()
	transport, exists := s.transports[cacheKey]
	s.mu.RUnlock()
	if exists {
		return transport, nil
	}

	// Need to create new transport
	s.mu.Lock()
	defer s.mu.Unlock()

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
	proxyConfig := s.router.Route(r.Host)
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
	proxyConfig := s.router.Route(r.Host)
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
