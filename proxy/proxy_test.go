package proxy

import (
	"net"
	"strings"
	"testing"
	"time"

	"broxy/config"
)

func TestDefaultDialer_HasTimeout(t *testing.T) {
	if defaultDialer.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", defaultDialer.Timeout)
	}
}

func TestDefaultDialer_HasKeepalive(t *testing.T) {
	if defaultDialer.KeepAlive != 30*time.Second {
		t.Errorf("KeepAlive = %v, want 30s", defaultDialer.KeepAlive)
	}
}

func TestDefaultDialer_UsesGoResolver(t *testing.T) {
	if defaultDialer.Resolver == nil {
		t.Fatal("Resolver is nil, want pure Go resolver configured")
	}
	if !defaultDialer.Resolver.PreferGo {
		t.Fatal("Resolver.PreferGo = false, want true")
	}
}

func TestShouldFallbackToSystemResolver(t *testing.T) {
	if !shouldFallbackToSystemResolver(&net.DNSError{Name: "example.com", Err: "no such host"}) {
		t.Fatal("expected fallback for non-timeout DNS errors")
	}
	if shouldFallbackToSystemResolver(&net.DNSError{Name: "example.com", Err: "i/o timeout", IsTimeout: true}) {
		t.Fatal("did not expect fallback for timeout DNS errors")
	}
	if shouldFallbackToSystemResolver(&net.OpError{Err: net.ErrClosed}) {
		t.Fatal("did not expect fallback for non-DNS errors")
	}
}

func TestDial_Direct(t *testing.T) {
	// Start a TCP server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	// Accept in background
	go func() {
		conn, _ := listener.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	cfg := &config.ProxyConfig{Type: "direct"}
	conn, err := Dial(cfg, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	conn.Close()
}

func TestDial_UnsupportedType(t *testing.T) {
	cfg := &config.ProxyConfig{Type: "invalid"}
	_, err := Dial(cfg, "tcp", "127.0.0.1:1234")
	if err == nil {
		t.Fatal("expected error for unsupported proxy type")
	}
}

func TestDial_DirectTimeout(t *testing.T) {
	// Use an unroutable address that will timeout
	// 10.255.255.1 is typically unroutable and will timeout
	cfg := &config.ProxyConfig{Type: "direct"}

	// Create a custom dialer with short timeout for testing
	origDialer := defaultDialer
	defaultDialer = &net.Dialer{
		Timeout:   100 * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}
	defer func() { defaultDialer = origDialer }()

	start := time.Now()
	_, err := Dial(cfg, "tcp", "10.255.255.1:12345")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}

	// Should complete around 100ms (with some tolerance)
	if elapsed < 50*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, expected ~100ms", elapsed)
	}
}

func TestDialHTTP_ProxyRefuses(t *testing.T) {
	// Start a server that accepts connections but returns an error
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	for i := 0; i < len(portStr); i++ {
		port = port*10 + int(portStr[i]-'0')
	}

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read request, send 403
		buf := make([]byte, 1024)
		conn.Read(buf)
		conn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
	}()

	cfg := &config.ProxyConfig{
		Type: "http",
		Host: host,
		Port: port,
	}

	_, err = Dial(cfg, "tcp", "example.com:443")
	if err == nil {
		t.Fatal("expected error when proxy refuses")
	}
}

func TestDial_HTTPProxyHandshakeTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	for i := 0; i < len(portStr); i++ {
		port = port*10 + int(portStr[i]-'0')
	}

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 1024)
		_, _ = conn.Read(buf)
		time.Sleep(300 * time.Millisecond) // longer than client timeout
	}()

	cfg := &config.ProxyConfig{
		Type: "http",
		Host: host,
		Port: port,
	}

	origDialer := defaultDialer
	defaultDialer = &net.Dialer{
		Timeout:   100 * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}
	defer func() { defaultDialer = origDialer }()

	start := time.Now()
	_, err = Dial(cfg, "tcp", "example.com:443")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("expected timeout net.Error, got %T: %v", err, err)
	}
	if elapsed < 50*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("elapsed = %v, expected timeout around 100ms", elapsed)
	}
}

func TestDialWithPool_Direct(t *testing.T) {
	// Start a TCP server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, _ := listener.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	cfg := &config.ProxyConfig{Type: "direct"}
	pool := NewPool(DefaultPoolConfig())
	defer pool.Close()

	conn, err := DialWithPool(cfg, "tcp", listener.Addr().String(), pool)
	if err != nil {
		t.Fatalf("DialWithPool failed: %v", err)
	}
	conn.Close()
}

func TestDialWithPool_NilPool(t *testing.T) {
	// Start a mock HTTP proxy
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	for i := 0; i < len(portStr); i++ {
		port = port*10 + int(portStr[i]-'0')
	}

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		conn.Read(buf)
		conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	}()

	cfg := &config.ProxyConfig{
		Type: "http",
		Host: host,
		Port: port,
	}

	// nil pool should work (falls back to direct dial)
	conn, err := DialWithPool(cfg, "tcp", "example.com:443", nil)
	if err != nil {
		t.Fatalf("DialWithPool with nil pool failed: %v", err)
	}
	conn.Close()
}

func TestDialWithPool_HTTPProxy(t *testing.T) {
	// Start a mock HTTP proxy that tracks connections
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	for i := 0; i < len(portStr); i++ {
		port = port*10 + int(portStr[i]-'0')
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				c.Read(buf)
				c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
			}(conn)
		}
	}()

	cfg := &config.ProxyConfig{
		Type: "http",
		Host: host,
		Port: port,
	}

	pool := NewPool(DefaultPoolConfig())
	defer pool.Close()

	conn, err := DialWithPool(cfg, "tcp", "example.com:443", pool)
	if err != nil {
		t.Fatalf("DialWithPool failed: %v", err)
	}
	conn.Close()
}

func TestDialWithPool_HTTPProxyHandshakeTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	for i := 0; i < len(portStr); i++ {
		port = port*10 + int(portStr[i]-'0')
	}

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 1024)
		_, _ = conn.Read(buf)
		time.Sleep(300 * time.Millisecond) // longer than client timeout
	}()

	cfg := &config.ProxyConfig{
		Type: "http",
		Host: host,
		Port: port,
	}

	pool := NewPool(DefaultPoolConfig())
	defer pool.Close()

	origDialer := defaultDialer
	defaultDialer = &net.Dialer{
		Timeout:   100 * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}
	defer func() { defaultDialer = origDialer }()

	start := time.Now()
	_, err = DialWithPool(cfg, "tcp", "example.com:443", pool)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("expected timeout net.Error, got %T: %v", err, err)
	}
	if elapsed < 50*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("elapsed = %v, expected timeout around 100ms", elapsed)
	}
}

func TestDirectDial_CachesHostnameResolution(t *testing.T) {
	directDialCache.clear()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err == nil && conn != nil {
			conn.Close()
		}
	}()

	_, port, _ := net.SplitHostPort(listener.Addr().String())
	addr := net.JoinHostPort("localhost", port)

	conn, err := directDial("tcp", addr)
	if err != nil {
		t.Fatalf("directDial failed: %v", err)
	}
	conn.Close()

	cacheKey := dialCacheKey("tcp", "localhost", port)
	cachedAddr, ok := directDialCache.get(cacheKey)
	if !ok {
		t.Fatal("expected dial cache entry for localhost")
	}

	cachedHost, cachedPort, err := net.SplitHostPort(cachedAddr)
	if err != nil {
		t.Fatalf("cached addr parse failed: %v", err)
	}
	if cachedPort != port {
		t.Fatalf("cached port = %s, want %s", cachedPort, port)
	}
	if !isLiteralIP(cachedHost) {
		t.Fatalf("cached host = %s, want literal IP", cachedHost)
	}
}

func TestDirectDial_UsesCachedAddress(t *testing.T) {
	directDialCache.clear()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err == nil && conn != nil {
			conn.Close()
		}
	}()

	host, port, _ := net.SplitHostPort(listener.Addr().String())
	cacheHost := "cache-hit.invalid"
	cacheKey := dialCacheKey("tcp", cacheHost, port)
	directDialCache.put(cacheKey, net.JoinHostPort(host, port))

	conn, err := directDial("tcp", net.JoinHostPort(cacheHost, port))
	if err != nil {
		t.Fatalf("directDial with cached address failed: %v", err)
	}
	conn.Close()
}

func BenchmarkDirectDial(b *testing.B) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()

	done := make(chan struct{})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					return
				}
			}
			conn.Close()
		}
	}()
	defer close(done)

	cfg := &config.ProxyConfig{Type: "direct"}
	ipAddr := listener.Addr().String()
	_, port, _ := net.SplitHostPort(ipAddr)
	hostnameAddr := net.JoinHostPort("localhost", port)

	b.Run("ip", func(b *testing.B) {
		directDialCache.clear()
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			conn, err := Dial(cfg, "tcp", ipAddr)
			if err != nil {
				b.Fatalf("Dial(ip) failed: %v", err)
			}
			conn.Close()
		}
	})

	b.Run("localhost_cached", func(b *testing.B) {
		directDialCache.clear()
		conn, err := Dial(cfg, "tcp", hostnameAddr)
		if err != nil {
			b.Fatalf("warmup Dial(localhost) failed: %v", err)
		}
		conn.Close()

		cacheKey := dialCacheKey("tcp", "localhost", port)
		if _, ok := directDialCache.get(cacheKey); !ok {
			b.Fatalf("expected warmup to populate cache key %s", cacheKey)
		}

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			conn, err := Dial(cfg, "tcp", hostnameAddr)
			if err != nil {
				b.Fatalf("Dial(localhost) failed: %v", err)
			}
			conn.Close()
		}
	})

	b.Run("localhost_cold", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			directDialCache.clear()
			conn, err := Dial(cfg, "tcp", hostnameAddr)
			if err != nil {
				b.Fatalf("Dial(localhost cold) failed: %v", err)
			}
			conn.Close()
		}
	})
}

func TestIsLiteralIP(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{host: "127.0.0.1", want: true},
		{host: "2001:db8::1", want: true},
		{host: "localhost", want: false},
		{host: "service.internal", want: false},
		{host: "not-an-ip::name", want: false},
	}

	for _, tt := range tests {
		t.Run(strings.ReplaceAll(tt.host, ":", "_"), func(t *testing.T) {
			if got := isLiteralIP(tt.host); got != tt.want {
				t.Fatalf("isLiteralIP(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}
