package proxy

import (
	"net"
	"sync"
	"testing"
	"time"
)

func TestNewPool(t *testing.T) {
	cfg := PoolConfig{
		MaxIdlePerHost: 5,
		MaxIdleTotal:   20,
		IdleTimeout:    30 * time.Second,
	}

	p := NewPool(cfg)
	if p == nil {
		t.Fatal("NewPool returned nil")
	}
	defer p.Close()

	if p.config.MaxIdlePerHost != 5 {
		t.Errorf("MaxIdlePerHost = %d, want 5", p.config.MaxIdlePerHost)
	}
	if p.config.MaxIdleTotal != 20 {
		t.Errorf("MaxIdleTotal = %d, want 20", p.config.MaxIdleTotal)
	}
}

func TestPool_GetPut_Basic(t *testing.T) {
	cfg := PoolConfig{
		MaxIdlePerHost: 2,
		MaxIdleTotal:   10,
		IdleTimeout:    30 * time.Second,
	}

	p := NewPool(cfg)
	defer p.Close()

	// Create a listener to accept connections
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()

	// Accept connections in background
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Keep connection open
			go func(c net.Conn) {
				buf := make([]byte, 1)
				c.Read(buf) // Block until closed
			}(conn)
		}
	}()

	// Get a new connection (should dial)
	conn1, err := p.Get("tcp", addr)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	// Put it back
	p.Put(conn1, addr)

	// Get again - should return the same connection (from pool)
	conn2, err := p.Get("tcp", addr)
	if err != nil {
		t.Fatalf("Get (reuse) failed: %v", err)
	}

	// They should be the same underlying connection
	if conn1.LocalAddr().String() != conn2.LocalAddr().String() {
		t.Error("Expected to reuse pooled connection")
	}

	conn2.Close()
}

func TestPool_MaxIdlePerHost(t *testing.T) {
	cfg := PoolConfig{
		MaxIdlePerHost: 2,
		MaxIdleTotal:   10,
		IdleTimeout:    30 * time.Second,
	}

	p := NewPool(cfg)
	defer p.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1)
				c.Read(buf)
			}(conn)
		}
	}()

	// Create 3 connections
	conns := make([]net.Conn, 3)
	for i := 0; i < 3; i++ {
		c, err := p.Get("tcp", addr)
		if err != nil {
			t.Fatalf("Get %d failed: %v", i, err)
		}
		conns[i] = c
	}

	// Put all 3 back
	for _, c := range conns {
		p.Put(c, addr)
	}

	// Only 2 should be pooled (MaxIdlePerHost = 2)
	// Third should have been closed
	p.mu.Lock()
	count := 0
	if list, ok := p.conns[addr]; ok {
		count = list.Len()
	}
	p.mu.Unlock()

	if count != 2 {
		t.Errorf("Pool has %d connections, want 2", count)
	}
}

func TestPool_MaxIdleTotal(t *testing.T) {
	cfg := PoolConfig{
		MaxIdlePerHost: 5,
		MaxIdleTotal:   3,
		IdleTimeout:    30 * time.Second,
	}

	p := NewPool(cfg)
	defer p.Close()

	// Create listeners for different "hosts"
	listeners := make([]net.Listener, 4)
	addrs := make([]string, 4)
	for i := 0; i < 4; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Failed to create listener %d: %v", i, err)
		}
		listeners[i] = ln
		addrs[i] = ln.Addr().String()
		defer ln.Close()

		go func(l net.Listener) {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					buf := make([]byte, 1)
					c.Read(buf)
				}(conn)
			}
		}(ln)
	}

	// Get and put connections to 4 different hosts
	for _, addr := range addrs {
		c, err := p.Get("tcp", addr)
		if err != nil {
			t.Fatalf("Get failed for %s: %v", addr, err)
		}
		p.Put(c, addr)
	}

	// Total idle should be capped at 3
	p.mu.Lock()
	total := p.idleCount
	p.mu.Unlock()

	if total != 3 {
		t.Errorf("Total idle = %d, want 3", total)
	}
}

func TestPool_Close(t *testing.T) {
	cfg := PoolConfig{
		MaxIdlePerHost: 5,
		MaxIdleTotal:   10,
		IdleTimeout:    30 * time.Second,
	}

	p := NewPool(cfg)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1)
				c.Read(buf)
			}(conn)
		}
	}()

	// Get and pool a connection
	c, err := p.Get("tcp", addr)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	p.Put(c, addr)

	// Close the pool
	p.Close()

	// Get should fail after close
	_, err = p.Get("tcp", addr)
	if err != ErrPoolClosed {
		t.Errorf("Get after close: got %v, want ErrPoolClosed", err)
	}

	// Put should not panic after close
	p.Put(c, addr) // Should be no-op
}

func TestPool_Concurrent(t *testing.T) {
	cfg := PoolConfig{
		MaxIdlePerHost: 5,
		MaxIdleTotal:   20,
		IdleTimeout:    30 * time.Second,
	}

	p := NewPool(cfg)
	defer p.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1)
				c.Read(buf)
			}(conn)
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Get("tcp", addr)
			if err != nil {
				return
			}
			time.Sleep(time.Millisecond)
			p.Put(c, addr)
		}()
	}
	wg.Wait()
}

func TestPool_IsAlive(t *testing.T) {
	cfg := PoolConfig{
		MaxIdlePerHost: 2,
		MaxIdleTotal:   10,
		IdleTimeout:    30 * time.Second,
	}

	p := NewPool(cfg)
	defer p.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	addr := ln.Addr().String()

	var serverConn net.Conn
	done := make(chan struct{})
	go func() {
		serverConn, _ = ln.Accept()
		close(done)
	}()

	// Get a connection
	c, err := p.Get("tcp", addr)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	<-done

	// Put it back
	p.Put(c, addr)

	// Close server side - this should make the connection dead
	serverConn.Close()
	ln.Close()

	// Small delay to let the close propagate
	time.Sleep(10 * time.Millisecond)

	// Get should detect dead connection and dial new (which will fail)
	_, err = p.Get("tcp", addr)
	if err == nil {
		t.Error("Expected error when getting dead connection with no server")
	}
}

func TestPool_Sweeper(t *testing.T) {
	cfg := PoolConfig{
		MaxIdlePerHost: 5,
		MaxIdleTotal:   10,
		IdleTimeout:    50 * time.Millisecond, // Very short for testing
	}

	p := NewPool(cfg)
	defer p.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1)
				c.Read(buf)
			}(conn)
		}
	}()

	// Get and pool a connection
	c, err := p.Get("tcp", addr)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	p.Put(c, addr)

	// Verify it's pooled
	p.mu.Lock()
	count := p.idleCount
	p.mu.Unlock()
	if count != 1 {
		t.Fatalf("Expected 1 pooled connection, got %d", count)
	}

	// Wait for sweeper to evict (idle timeout + sweep interval + buffer)
	time.Sleep(200 * time.Millisecond)

	// Should be evicted
	p.mu.Lock()
	count = p.idleCount
	p.mu.Unlock()
	if count != 0 {
		t.Errorf("Expected 0 pooled connections after sweep, got %d", count)
	}
}
