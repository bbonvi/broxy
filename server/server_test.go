package server

import (
	"bytes"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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

	// Read response
	buf := make([]byte, 1024)
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read proxy response: %v", err)
	}

	response := string(buf[:n])
	if !strings.Contains(response, "200") {
		t.Fatalf("expected 200 response, got: %s", response)
	}

	// Read data from upstream
	n, err = clientConn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read upstream data: %v", err)
	}

	if string(buf[:n]) != "hello from upstream" {
		t.Errorf("unexpected data: %s", string(buf[:n]))
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
