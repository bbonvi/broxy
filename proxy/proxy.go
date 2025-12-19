package proxy

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"broxy/config"
	"golang.org/x/net/proxy"
)

// defaultDialer provides timeout and keepalive for all connections
var defaultDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

// Dial creates a connection through the specified proxy
func Dial(proxyConfig *config.ProxyConfig, network, addr string) (net.Conn, error) {
	switch proxyConfig.Type {
	case "direct":
		return defaultDialer.Dial(network, addr)
	case "http":
		return dialHTTP(proxyConfig, network, addr)
	case "socks5":
		return dialSOCKS5(proxyConfig, network, addr)
	default:
		return nil, fmt.Errorf("unsupported proxy type: %s", proxyConfig.Type)
	}
}

func dialHTTP(proxyConfig *config.ProxyConfig, network, addr string) (net.Conn, error) {
	proxyAddr := fmt.Sprintf("%s:%d", proxyConfig.Host, proxyConfig.Port)
	conn, err := defaultDialer.Dial(network, proxyAddr)
	if err != nil {
		return nil, err
	}

	// Send CONNECT request to upstream proxy
	req := &http.Request{
		Method: "CONNECT",
		URL: &url.URL{
			Host: addr,
		},
		Host:   addr,
		Header: make(http.Header),
		Proto:  "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}

	// Add authentication if configured
	if proxyConfig.Auth != nil {
		auth := proxyConfig.Auth.Username + ":" + proxyConfig.Auth.Password
		basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
		req.Header.Set("Proxy-Authorization", basicAuth)
	}

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}

	// Read response
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy returned status: %s", resp.Status)
	}

	return conn, nil
}

func dialSOCKS5(proxyConfig *config.ProxyConfig, network, addr string) (net.Conn, error) {
	proxyAddr := fmt.Sprintf("%s:%d", proxyConfig.Host, proxyConfig.Port)

	var auth *proxy.Auth
	if proxyConfig.Auth != nil {
		auth = &proxy.Auth{
			User:     proxyConfig.Auth.Username,
			Password: proxyConfig.Auth.Password,
		}
	}

	dialer, err := proxy.SOCKS5(network, proxyAddr, auth, defaultDialer)
	if err != nil {
		return nil, err
	}

	return dialer.Dial(network, addr)
}
