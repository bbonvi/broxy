package server

import (
	"bytes"
	"errors"
	"io"
	"net"
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
