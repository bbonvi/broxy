package server

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"broxy/config"
	"broxy/router"
)

// mockConn implements net.Conn for testing
type mockConn struct {
	readBuf    *bytes.Buffer
	writeBuf   *bytes.Buffer
	readDelay  time.Duration
	writeDelay time.Duration
	closed     bool
	closedWrite bool
}

func newMockConn(data []byte) *mockConn {
	return &mockConn{
		readBuf:  bytes.NewBuffer(data),
		writeBuf: &bytes.Buffer{},
	}
}

func (m *mockConn) Read(b []byte) (int, error) {
	if m.closed {
		return 0, net.ErrClosed
	}
	if m.readDelay > 0 {
		time.Sleep(m.readDelay)
	}
	return m.readBuf.Read(b)
}

func (m *mockConn) Write(b []byte) (int, error) {
	if m.closed {
		return 0, net.ErrClosed
	}
	if m.writeDelay > 0 {
		time.Sleep(m.writeDelay)
	}
	return m.writeBuf.Write(b)
}

func (m *mockConn) Close() error {
	m.closed = true
	return nil
}

func (m *mockConn) CloseWrite() error {
	m.closedWrite = true
	return nil
}

func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

// slowConn returns timeout errors after delay
type slowConn struct {
	delay time.Duration
}

func (s *slowConn) Read(b []byte) (int, error) {
	time.Sleep(s.delay)
	return 0, &net.OpError{Op: "read", Err: errors.New("i/o timeout")}
}

func (s *slowConn) Write(b []byte) (int, error) {
	return len(b), nil
}

func (s *slowConn) Close() error                       { return nil }
func (s *slowConn) LocalAddr() net.Addr                { return nil }
func (s *slowConn) RemoteAddr() net.Addr               { return nil }
func (s *slowConn) SetDeadline(t time.Time) error      { return nil }
func (s *slowConn) SetReadDeadline(t time.Time) error  { return nil }
func (s *slowConn) SetWriteDeadline(t time.Time) error { return nil }

// timeoutConn immediately returns timeout on read
type timeoutConn struct {
	deadline time.Time
}

func (t *timeoutConn) Read(b []byte) (int, error) {
	if !t.deadline.IsZero() && time.Now().After(t.deadline) {
		return 0, &timeoutError{}
	}
	// Block until deadline
	if !t.deadline.IsZero() {
		time.Sleep(time.Until(t.deadline) + time.Millisecond)
	}
	return 0, &timeoutError{}
}

func (t *timeoutConn) Write(b []byte) (int, error) { return len(b), nil }
func (t *timeoutConn) Close() error                { return nil }
func (t *timeoutConn) LocalAddr() net.Addr         { return nil }
func (t *timeoutConn) RemoteAddr() net.Addr        { return nil }
func (t *timeoutConn) SetDeadline(d time.Time) error {
	t.deadline = d
	return nil
}
func (t *timeoutConn) SetReadDeadline(d time.Time) error {
	t.deadline = d
	return nil
}
func (t *timeoutConn) SetWriteDeadline(d time.Time) error { return nil }

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

func TestCopyConn_NormalCompletion(t *testing.T) {
	data := []byte("hello world")
	src := newMockConn(data)
	dst := newMockConn(nil)

	written, err := copyConn(dst, src, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != int64(len(data)) {
		t.Errorf("written = %d, want %d", written, len(data))
	}
	if !bytes.Equal(dst.writeBuf.Bytes(), data) {
		t.Errorf("copied data = %q, want %q", dst.writeBuf.Bytes(), data)
	}
}

func TestCopyConn_LargeData(t *testing.T) {
	// Test with data larger than buffer size (32KB)
	data := make([]byte, 100*1024) // 100KB
	for i := range data {
		data[i] = byte(i % 256)
	}
	src := newMockConn(data)
	dst := newMockConn(nil)

	written, err := copyConn(dst, src, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != int64(len(data)) {
		t.Errorf("written = %d, want %d", written, len(data))
	}
	if !bytes.Equal(dst.writeBuf.Bytes(), data) {
		t.Errorf("copied data mismatch")
	}
}

func TestCopyConn_IdleTimeout(t *testing.T) {
	src := &timeoutConn{}
	dst := newMockConn(nil)

	start := time.Now()
	_, err := copyConn(dst, src, 50*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}

	// Should complete around 50ms (with some tolerance)
	if elapsed < 40*time.Millisecond || elapsed > 150*time.Millisecond {
		t.Errorf("elapsed = %v, expected ~50ms", elapsed)
	}

	// Error should be a timeout
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Errorf("error should be timeout, got: %v", err)
	}
}

func TestCopyConn_EmptySource(t *testing.T) {
	src := newMockConn(nil) // Empty buffer returns EOF immediately
	dst := newMockConn(nil)

	written, err := copyConn(dst, src, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != 0 {
		t.Errorf("written = %d, want 0", written)
	}
}

func TestCopyConn_WriteError(t *testing.T) {
	data := []byte("hello")
	src := newMockConn(data)
	dst := newMockConn(nil)
	dst.Close() // Close to trigger write error

	_, err := copyConn(dst, src, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected write error")
	}
}

func TestCloseWrite_TCPConn(t *testing.T) {
	mock := newMockConn(nil)
	closeWrite(mock)
	if !mock.closedWrite {
		t.Error("CloseWrite was not called")
	}
}

func TestCloseWrite_NonTCPConn(t *testing.T) {
	// slowConn doesn't implement CloseWrite
	conn := &slowConn{}
	closeWrite(conn) // Should not panic
}

func TestIsExpectedCloseError_Nil(t *testing.T) {
	if !isExpectedCloseError(nil) {
		t.Error("nil should be expected")
	}
}

func TestIsExpectedCloseError_ErrClosed(t *testing.T) {
	if !isExpectedCloseError(net.ErrClosed) {
		t.Error("net.ErrClosed should be expected")
	}
}

func TestIsExpectedCloseError_Timeout(t *testing.T) {
	err := &timeoutError{}
	if !isExpectedCloseError(err) {
		t.Error("timeout error should be expected")
	}
}

func TestIsExpectedCloseError_ConnectionClosed(t *testing.T) {
	err := errors.New("use of closed network connection")
	if !isExpectedCloseError(err) {
		t.Error("closed connection error should be expected")
	}
}

func TestIsExpectedCloseError_OtherError(t *testing.T) {
	err := errors.New("random error")
	if isExpectedCloseError(err) {
		t.Error("random error should not be expected")
	}
}

func TestIsExpectedCloseError_EOF(t *testing.T) {
	// EOF is not explicitly handled by isExpectedCloseError
	// copyConn handles EOF separately, returning nil error
	if isExpectedCloseError(io.EOF) {
		t.Error("io.EOF should not be expected (handled separately in copyConn)")
	}
}

// Integration tests for handleConnect

func TestHandleConnect_HalfClose(t *testing.T) {
	// Start a mock upstream server that half-closes after sending data
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start upstream listener: %v", err)
	}
	defer upstreamListener.Close()

	upstreamAddr := upstreamListener.Addr().String()
	upstreamDone := make(chan struct{})

	go func() {
		defer close(upstreamDone)
		conn, err := upstreamListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Send data then half-close (CloseWrite)
		conn.Write([]byte("hello from upstream"))
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}

		// Keep reading until client closes
		buf := make([]byte, 1024)
		for {
			_, err := conn.Read(buf)
			if err != nil {
				break
			}
		}
	}()

	// Create proxy server
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start proxy listener: %v", err)
	}
	defer proxyListener.Close()

	proxyAddr := proxyListener.Addr().String()

	// Accept one connection and handle it
	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		conn, err := proxyListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Simulate proxy handleConnect behavior
		// Read CONNECT request
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		if n == 0 {
			return
		}

		// Connect to upstream
		destConn, err := net.Dial("tcp", upstreamAddr)
		if err != nil {
			return
		}
		defer destConn.Close()

		// Send 200 OK
		conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))

		// Use our copyConn with proper sync (simulating fixed handleConnect)
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			copyConn(destConn, conn, 5*time.Second)
			closeWrite(destConn)
		}()

		go func() {
			defer wg.Done()
			copyConn(conn, destConn, 5*time.Second)
			closeWrite(conn)
		}()

		wg.Wait()
	}()

	// Connect client to proxy
	clientConn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer clientConn.Close()

	// Send CONNECT request
	clientConn.Write([]byte("CONNECT " + upstreamAddr + " HTTP/1.1\r\nHost: " + upstreamAddr + "\r\n\r\n"))

	// Read all data (may come in one or multiple reads)
	buf := make([]byte, 1024)
	var received []byte
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))

	for {
		n, err := clientConn.Read(buf)
		if n > 0 {
			received = append(received, buf[:n]...)
		}
		// Check if we have everything we need
		response := string(received)
		if strings.Contains(response, "200") && strings.Contains(response, "hello from upstream") {
			break
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("failed to read: %v", err)
		}
	}

	response := string(received)
	if !strings.Contains(response, "200") {
		t.Fatalf("expected 200 response, got: %s", response)
	}
	if !strings.Contains(response, "hello from upstream") {
		t.Fatalf("expected upstream data, got: %s", response)
	}

	// Half-close client side
	if tcpConn, ok := clientConn.(*net.TCPConn); ok {
		tcpConn.CloseWrite()
	}

	// Connection should close cleanly
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = clientConn.Read(buf)
	if err != io.EOF && !isExpectedCloseError(err) {
		t.Errorf("expected EOF or clean close, got: %v", err)
	}

	// Wait for goroutines
	select {
	case <-proxyDone:
	case <-time.After(5 * time.Second):
		t.Error("proxy goroutine did not complete in time")
	}

	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Error("upstream goroutine did not complete in time")
	}
}

func TestHandleConnect_NoGoroutineLeak(t *testing.T) {
	// Start upstream echo server
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start upstream: %v", err)
	}
	defer upstreamListener.Close()

	upstreamAddr := upstreamListener.Addr().String()

	// Handle upstream connections
	go func() {
		for {
			conn, err := upstreamListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c) // Echo
			}(conn)
		}
	}()

	// Start proxy
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	defer proxyListener.Close()

	proxyAddr := proxyListener.Addr().String()

	// Handle proxy connections
	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil {
				return
			}
			go handleTestConnect(conn, upstreamAddr)
		}
	}()

	// Run multiple connections
	const numConnections = 50

	// Give system a moment to stabilize
	time.Sleep(10 * time.Millisecond)
	initialGoroutines := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < numConnections; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runTestConnection(t, proxyAddr, upstreamAddr)
		}()
	}
	wg.Wait()

	// Give goroutines time to clean up
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	finalGoroutines := runtime.NumGoroutine()

	// Allow some variance (test goroutines, gc, etc)
	// But leaked goroutines would show up as 2*numConnections extra
	if finalGoroutines > initialGoroutines+10 {
		t.Errorf("possible goroutine leak: initial=%d, final=%d", initialGoroutines, finalGoroutines)
	}
}

func handleTestConnect(clientConn net.Conn, upstreamAddr string) {
	defer clientConn.Close()

	// Read CONNECT request
	buf := make([]byte, 1024)
	clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := clientConn.Read(buf)
	if err != nil || n == 0 {
		return
	}

	// Connect to upstream
	destConn, err := net.Dial("tcp", upstreamAddr)
	if err != nil {
		return
	}
	defer destConn.Close()

	// Send 200 OK
	clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))

	// Bidirectional copy with proper sync
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		copyConn(destConn, clientConn, 5*time.Second)
		closeWrite(destConn)
	}()

	go func() {
		defer wg.Done()
		copyConn(clientConn, destConn, 5*time.Second)
		closeWrite(clientConn)
	}()

	wg.Wait()
}

func runTestConnection(t *testing.T, proxyAddr, upstreamAddr string) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Logf("dial failed: %v", err)
		return
	}
	defer conn.Close()

	// Send CONNECT
	conn.Write([]byte("CONNECT " + upstreamAddr + " HTTP/1.1\r\nHost: " + upstreamAddr + "\r\n\r\n"))

	// Read response
	buf := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "200") {
		return
	}

	// Send/receive some data
	testData := []byte("test data")
	conn.Write(testData)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = conn.Read(buf)
	if err != nil {
		return
	}

	if !bytes.Equal(buf[:n], testData) {
		t.Logf("echo mismatch: got %q, want %q", buf[:n], testData)
	}
}

// =============================================================================
// Benchmark Infrastructure
// =============================================================================

// echoServer starts a TCP echo server and returns its address and cleanup func
func echoServer(t testing.TB) (string, func()) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start echo server: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
			conn, err := listener.Accept()
			if err != nil {
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	cleanup := func() {
		close(done)
		listener.Close()
	}

	return listener.Addr().String(), cleanup
}

// discardServer starts a TCP server that discards all input, returns addr and cleanup
func discardServer(t testing.TB) (string, func()) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start discard server: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
			conn, err := listener.Accept()
			if err != nil {
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(io.Discard, c)
			}(conn)
		}
	}()

	cleanup := func() {
		close(done)
		listener.Close()
	}

	return listener.Addr().String(), cleanup
}

// mockHTTPProxy starts an HTTP CONNECT proxy that forwards to any host
func mockHTTPProxy(t testing.TB) (string, func()) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start mock HTTP proxy: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
			conn, err := listener.Accept()
			if err != nil {
				continue
			}
			go handleMockProxyConn(conn)
		}
	}()

	cleanup := func() {
		close(done)
		listener.Close()
	}

	return listener.Addr().String(), cleanup
}

func handleMockProxyConn(clientConn net.Conn) {
	defer clientConn.Close()

	reader := bufio.NewReader(clientConn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	if req.Method != "CONNECT" {
		clientConn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		return
	}

	// Connect to the target
	targetConn, err := net.DialTimeout("tcp", req.Host, 5*time.Second)
	if err != nil {
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	// Send 200 OK
	clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))

	// Bidirectional copy
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(targetConn, clientConn)
	}()
	go func() {
		defer wg.Done()
		io.Copy(clientConn, targetConn)
	}()
	wg.Wait()
}

// testRouter creates a router that routes all traffic through the given proxy config
func testRouter(proxyConfig *config.ProxyConfig) *router.Router {
	cfg := &config.Config{
		Server: config.ServerConfig{Listen: "127.0.0.1", Port: 3128},
		Proxies: []config.ProxyConfig{
			*proxyConfig,
		},
		Rules: []config.Rule{
			{Match: "default", Proxy: proxyConfig.Name},
		},
	}
	return router.New(cfg)
}

// testServer creates a test Server with the given router
func testServer(t testing.TB, r *router.Router) (*Server, string, func()) {
	// Create server - we need a valid config file path but won't use file watching in tests
	s := New(r, "127.0.0.1", 0, "/dev/null")

	// Start listener manually for testing
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test server: %v", err)
	}

	done := make(chan struct{})
	go func() {
		httpServer := &http.Server{Handler: http.HandlerFunc(s.handleRequest)}
		go httpServer.Serve(listener)
		<-done
		httpServer.Close()
	}()

	cleanup := func() {
		close(done)
		listener.Close()
	}

	return s, listener.Addr().String(), cleanup
}

// =============================================================================
// Benchmarks
// =============================================================================

// BenchmarkCopyConn measures raw copy throughput (core of handleConnect)
func BenchmarkCopyConn(b *testing.B) {
	sizes := []int{1024, 32 * 1024, 256 * 1024, 1024 * 1024}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i % 256)
			}

			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				src := newMockConn(data)
				dst := newMockConn(nil)
				copyConn(dst, src, 10*time.Second)
			}
		})
	}
}

// BenchmarkHandleConnect_Direct measures end-to-end CONNECT with direct connection
func BenchmarkHandleConnect_Direct(b *testing.B) {
	// Start echo server as destination
	destAddr, destCleanup := echoServer(b)
	defer destCleanup()

	// Create server with direct proxy
	directProxy := &config.ProxyConfig{Name: "direct", Type: "direct"}
	r := testRouter(directProxy)
	_, serverAddr, serverCleanup := testServer(b, r)
	defer serverCleanup()

	// Test data
	testData := bytes.Repeat([]byte("benchmark data "), 1024) // ~15KB

	b.ReportAllocs()
	b.SetBytes(int64(len(testData) * 2)) // round trip
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
		if err != nil {
			b.Fatalf("dial failed: %v", err)
		}

		// Send CONNECT
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", destAddr, destAddr)

		// Read response
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			conn.Close()
			b.Fatalf("failed to read CONNECT response: %v", err)
		}
		resp.Body.Close()

		if resp.StatusCode != 200 {
			conn.Close()
			b.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		// Send test data
		_, err = conn.Write(testData)
		if err != nil {
			conn.Close()
			b.Fatalf("write failed: %v", err)
		}

		// Read echo back
		received := make([]byte, len(testData))
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = io.ReadFull(conn, received)
		if err != nil {
			conn.Close()
			b.Fatalf("read failed: %v", err)
		}

		conn.Close()
	}
}

// BenchmarkHandleConnect_HTTPProxy measures CONNECT through an upstream HTTP proxy
func BenchmarkHandleConnect_HTTPProxy(b *testing.B) {
	// Start echo server as destination
	destAddr, destCleanup := echoServer(b)
	defer destCleanup()

	// Start mock upstream HTTP proxy
	proxyAddr, proxyCleanup := mockHTTPProxy(b)
	defer proxyCleanup()

	// Parse proxy address
	host, portStr, _ := net.SplitHostPort(proxyAddr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// Create server with HTTP proxy
	httpProxy := &config.ProxyConfig{Name: "http", Type: "http", Host: host, Port: port}
	r := testRouter(httpProxy)
	_, serverAddr, serverCleanup := testServer(b, r)
	defer serverCleanup()

	// Test data
	testData := bytes.Repeat([]byte("benchmark data "), 1024) // ~15KB

	b.ReportAllocs()
	b.SetBytes(int64(len(testData) * 2)) // round trip
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		conn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
		if err != nil {
			b.Fatalf("dial failed: %v", err)
		}

		// Send CONNECT
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", destAddr, destAddr)

		// Read response
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			conn.Close()
			b.Fatalf("failed to read CONNECT response: %v", err)
		}
		resp.Body.Close()

		if resp.StatusCode != 200 {
			conn.Close()
			b.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		// Send test data
		_, err = conn.Write(testData)
		if err != nil {
			conn.Close()
			b.Fatalf("write failed: %v", err)
		}

		// Read echo back
		received := make([]byte, len(testData))
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = io.ReadFull(conn, received)
		if err != nil {
			conn.Close()
			b.Fatalf("read failed: %v", err)
		}

		conn.Close()
	}
}

// BenchmarkDial_Direct measures direct dial latency
func BenchmarkDial_Direct(b *testing.B) {
	// Start discard server as destination
	destAddr, destCleanup := discardServer(b)
	defer destCleanup()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		conn, err := net.Dial("tcp", destAddr)
		if err != nil {
			b.Fatalf("dial failed: %v", err)
		}
		conn.Close()
	}
}

// BenchmarkDial_HTTPProxy measures dial latency through HTTP proxy
func BenchmarkDial_HTTPProxy(b *testing.B) {
	// Start discard server as destination
	destAddr, destCleanup := discardServer(b)
	defer destCleanup()

	// Start mock upstream HTTP proxy
	proxyAddr, proxyCleanup := mockHTTPProxy(b)
	defer proxyCleanup()

	// Parse proxy address
	host, portStr, _ := net.SplitHostPort(proxyAddr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	proxyConfig := &config.ProxyConfig{Name: "http", Type: "http", Host: host, Port: port}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		conn, err := dialHTTPBench(proxyConfig, "tcp", destAddr)
		if err != nil {
			b.Fatalf("dial failed: %v", err)
		}
		conn.Close()
	}
}

// dialHTTPBench is a copy of proxy.dialHTTP for benchmarking without import cycle
func dialHTTPBench(proxyConfig *config.ProxyConfig, network, addr string) (net.Conn, error) {
	proxyAddr := fmt.Sprintf("%s:%d", proxyConfig.Host, proxyConfig.Port)
	conn, err := net.Dial(network, proxyAddr)
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequest("CONNECT", "", nil)
	req.Host = addr
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy returned status: %s", resp.Status)
	}

	return conn, nil
}

// BenchmarkHandleHTTP_Direct measures plain HTTP proxying
func BenchmarkHandleHTTP_Direct(b *testing.B) {
	// Start a simple HTTP server as destination
	destMux := http.NewServeMux()
	destMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	destServer := &http.Server{Handler: destMux}
	destListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start dest server: %v", err)
	}
	go destServer.Serve(destListener)
	defer destServer.Close()

	destAddr := destListener.Addr().String()

	// Create broxy server with direct proxy
	directProxy := &config.ProxyConfig{Name: "direct", Type: "direct"}
	r := testRouter(directProxy)
	_, serverAddr, serverCleanup := testServer(b, r)
	defer serverCleanup()

	// Create HTTP client that uses our proxy
	proxyURL := fmt.Sprintf("http://%s", serverAddr)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) {
				return url.Parse(proxyURL)
			},
		},
	}

	targetURL := fmt.Sprintf("http://%s/", destAddr)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		resp, err := client.Get(targetURL)
		if err != nil {
			b.Fatalf("GET failed: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
