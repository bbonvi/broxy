package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/http"
	"net/url"
	"sync"
	"time"

	"broxy/config"
	"golang.org/x/net/proxy"
)

// defaultDialer provides timeout and keepalive for all connections
var defaultDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
	// PreferGo avoids libc resolver behavior that can serialize/fallback slowly
	// on some hosts, which appears as synchronized 5-10s stalls.
	Resolver: &net.Resolver{PreferGo: true},
}

var systemDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
}

const (
	dialAddrCacheTTL       = 30 * time.Second
	dialAddrCacheMaxEntries = 2048
)

type dialAddrCacheEntry struct {
	addr      string
	expiresAt time.Time
}

// dialAddrCache stores short-lived hostname -> ip:port mappings for direct mode.
type dialAddrCache struct {
	mu      sync.RWMutex
	entries map[string]dialAddrCacheEntry
}

var directDialCache = &dialAddrCache{
	entries: make(map[string]dialAddrCacheEntry),
}

// connectHandshakeTimeout returns the timeout used for HTTP CONNECT handshake.
func connectHandshakeTimeout() time.Duration {
	if defaultDialer != nil && defaultDialer.Timeout > 0 {
		return defaultDialer.Timeout
	}
	return 30 * time.Second
}

func shouldFallbackToSystemResolver(err error) bool {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return false
	}
	return !dnsErr.Timeout()
}

func directDial(network, addr string) (net.Conn, error) {
	// Fast path for IP literals, which dominate CONNECT benchmarks.
	if addr != "" {
		first := addr[0]
		if (first >= '0' && first <= '9') || first == '[' {
			return dialWithResolverFallback(network, addr)
		}
	}

	host, port, hasPort := splitDialAddress(addr)
	if !hasPort || isLiteralIP(host) {
		return dialWithResolverFallback(network, addr)
	}

	cacheKey := dialCacheKey(network, host, port)
	if cachedAddr, ok := directDialCache.get(cacheKey); ok {
		conn, err := defaultDialer.Dial(network, cachedAddr)
		if err == nil {
			return conn, nil
		}
		directDialCache.delete(cacheKey)
	}

	conn, err := dialWithResolverFallback(network, addr)
	if err != nil {
		return nil, err
	}
	directDialCache.storeFromConn(network, host, port, conn)
	return conn, nil
}

// directDialContext mirrors directDial but respects caller cancellation.
func directDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	// Fast path for IP literals, which dominate CONNECT benchmarks.
	if addr != "" {
		first := addr[0]
		if (first >= '0' && first <= '9') || first == '[' {
			return dialWithResolverFallbackContext(ctx, network, addr)
		}
	}

	host, port, hasPort := splitDialAddress(addr)
	if !hasPort || isLiteralIP(host) {
		return dialWithResolverFallbackContext(ctx, network, addr)
	}

	cacheKey := dialCacheKey(network, host, port)
	if cachedAddr, ok := directDialCache.get(cacheKey); ok {
		conn, err := defaultDialer.DialContext(ctx, network, cachedAddr)
		if err == nil {
			return conn, nil
		}
		directDialCache.delete(cacheKey)
	}

	conn, err := dialWithResolverFallbackContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	directDialCache.storeFromConn(network, host, port, conn)
	return conn, nil
}

type socksFallbackDialer struct{}

func (d socksFallbackDialer) Dial(network, addr string) (net.Conn, error) {
	return directDial(network, addr)
}

// DirectDialContext dials outbound targets using broxy's shared dialer settings.
func DirectDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return directDialContext(ctx, network, addr)
}

func dialWithResolverFallback(network, addr string) (net.Conn, error) {
	conn, err := defaultDialer.Dial(network, addr)
	if err == nil || !shouldFallbackToSystemResolver(err) {
		return conn, err
	}
	return systemDialer.Dial(network, addr)
}

func dialWithResolverFallbackContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := defaultDialer.DialContext(ctx, network, addr)
	if err == nil || !shouldFallbackToSystemResolver(err) {
		return conn, err
	}
	return systemDialer.DialContext(ctx, network, addr)
}

func splitDialAddress(addr string) (host, port string, ok bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", false
	}
	return host, port, true
}

func dialCacheKey(network, host, port string) string {
	return network + "|" + host + "|" + port
}

func isLiteralIP(host string) bool {
	if host == "" {
		return false
	}

	hasColon := false
	for i := 0; i < len(host); i++ {
		ch := host[i]
		switch {
		case ch >= '0' && ch <= '9':
		case ch == '.':
		case ch == ':':
			hasColon = true
		case hasColon && ((ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') || ch == '%'):
		default:
			return false
		}
	}

	_, err := netip.ParseAddr(host)
	return err == nil
}

func (c *dialAddrCache) get(key string) (string, bool) {
	now := time.Now()

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if !entry.expiresAt.After(now) {
		c.deleteExpired(key, now)
		return "", false
	}
	return entry.addr, true
}

func (c *dialAddrCache) put(key, addr string) {
	if addr == "" {
		return
	}

	now := time.Now()
	expiresAt := now.Add(dialAddrCacheTTL)

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= dialAddrCacheMaxEntries {
		for existingKey, entry := range c.entries {
			if !entry.expiresAt.After(now) {
				delete(c.entries, existingKey)
			}
		}
	}
	if len(c.entries) >= dialAddrCacheMaxEntries {
		for existingKey := range c.entries {
			delete(c.entries, existingKey)
			break
		}
	}

	c.entries[key] = dialAddrCacheEntry{
		addr:      addr,
		expiresAt: expiresAt,
	}
}

func (c *dialAddrCache) delete(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

func (c *dialAddrCache) deleteExpired(key string, now time.Time) {
	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok && !entry.expiresAt.After(now) {
		delete(c.entries, key)
	}
	c.mu.Unlock()
}

func (c *dialAddrCache) storeFromConn(network, host, port string, conn net.Conn) {
	tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || tcpAddr.IP == nil {
		return
	}
	c.put(dialCacheKey(network, host, port), net.JoinHostPort(tcpAddr.IP.String(), port))
}

func (c *dialAddrCache) clear() {
	c.mu.Lock()
	c.entries = make(map[string]dialAddrCacheEntry)
	c.mu.Unlock()
}

func connectHTTPProxy(conn net.Conn, proxyConfig *config.ProxyConfig, addr string) error {
	req := &http.Request{
		Method: "CONNECT",
		URL: &url.URL{
			Host: addr,
		},
		Host:       addr,
		Header:     make(http.Header),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}

	// Add authentication if configured
	if proxyConfig.Auth != nil {
		auth := proxyConfig.Auth.Username + ":" + proxyConfig.Auth.Password
		basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
		req.Header.Set("Proxy-Authorization", basicAuth)
	}

	// Bound both write and read of CONNECT handshake.
	timeout := connectHandshakeTimeout()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	defer conn.SetDeadline(time.Time{})

	if err := req.Write(conn); err != nil {
		return err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy returned status: %s", resp.Status)
	}

	return nil
}

// Dial creates a connection through the specified proxy
func Dial(proxyConfig *config.ProxyConfig, network, addr string) (net.Conn, error) {
	switch proxyConfig.Type {
	case "direct":
		return directDial(network, addr)
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
	conn, err := directDial(network, proxyAddr)
	if err != nil {
		return nil, err
	}

	if err := connectHTTPProxy(conn, proxyConfig, addr); err != nil {
		conn.Close()
		return nil, err
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

	dialer, err := proxy.SOCKS5(network, proxyAddr, auth, socksFallbackDialer{})
	if err != nil {
		return nil, err
	}

	return dialer.Dial(network, addr)
}

// DialWithPool creates a connection through the specified proxy, using the pool
// for HTTP proxy connections. SOCKS5 and direct connections bypass the pool.
// Note: CONNECT tunnel connections cannot be returned to the pool after use.
func DialWithPool(proxyConfig *config.ProxyConfig, network, addr string, pool *Pool) (net.Conn, error) {
	switch proxyConfig.Type {
	case "direct":
		return directDial(network, addr)
	case "http":
		return dialHTTPWithPool(proxyConfig, network, addr, pool)
	case "socks5":
		return dialSOCKS5(proxyConfig, network, addr)
	default:
		return nil, fmt.Errorf("unsupported proxy type: %s", proxyConfig.Type)
	}
}

func dialHTTPWithPool(proxyConfig *config.ProxyConfig, network, addr string, pool *Pool) (net.Conn, error) {
	proxyAddr := fmt.Sprintf("%s:%d", proxyConfig.Host, proxyConfig.Port)

	var conn net.Conn
	var err error
	if pool != nil {
		conn, err = pool.Get(network, proxyAddr)
	} else {
		conn, err = directDial(network, proxyAddr)
	}
	if err != nil {
		return nil, err
	}

	if err := connectHTTPProxy(conn, proxyConfig, addr); err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}
