package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"

	"broxy/proxy"
	"broxy/router"
)

type Server struct {
	router *router.Router
	listen string
	port   int
}

func New(r *router.Router, listen string, port int) *Server {
	return &Server{
		router: r,
		listen: listen,
		port:   port,
	}
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
	log.Printf("CONNECT %s", r.Host)

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
	log.Printf("%s %s", r.Method, r.URL.String())

	// Find the appropriate proxy for this host
	proxyConfig := s.router.Route(r.Host)
	if proxyConfig == nil {
		http.Error(w, "No proxy configured", http.StatusBadGateway)
		return
	}

	// Create HTTP client with appropriate transport
	var transport *http.Transport

	switch proxyConfig.Type {
	case "direct":
		transport = &http.Transport{}
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		transport = &http.Transport{
			Proxy: http.ProxyURL(proxyURLParsed),
		}
	case "socks5":
		// SOCKS5 for HTTP requests requires special handling
		dialer, err := proxy.Dial(proxyConfig, "tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer dialer.Close()
		// For SOCKS5, we need to use a custom dialer
		http.Error(w, "SOCKS5 for plain HTTP not fully supported, use HTTPS", http.StatusNotImplemented)
		return
	default:
		http.Error(w, "Unsupported proxy type", http.StatusBadGateway)
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
