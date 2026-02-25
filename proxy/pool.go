package proxy

import (
	"container/list"
	"errors"
	"net"
	"sync"
	"time"
)

// ErrPoolClosed is returned when operations are attempted on a closed pool
var ErrPoolClosed = errors.New("connection pool is closed")

// PoolConfig configures the connection pool behavior
type PoolConfig struct {
	MaxIdlePerHost int           // Max idle connections per host
	MaxIdleTotal   int           // Max total idle connections across all hosts
	IdleTimeout    time.Duration // How long idle connections live before eviction
}

// DefaultPoolConfig returns sensible defaults for connection pooling
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxIdlePerHost: 10,
		MaxIdleTotal:   100,
		IdleTimeout:    90 * time.Second,
	}
}

// pooledConn wraps a connection with metadata for pool management
type pooledConn struct {
	conn      net.Conn
	idleSince time.Time
}

// Pool manages reusable TCP connections to upstream proxies
type Pool struct {
	mu        sync.Mutex
	config    PoolConfig
	conns     map[string]*list.List // host -> list of *pooledConn
	idleCount int
	closed    bool
	closedCh  chan struct{}
}

// NewPool creates a new connection pool and starts the background sweeper
func NewPool(cfg PoolConfig) *Pool {
	if cfg.MaxIdlePerHost <= 0 {
		cfg.MaxIdlePerHost = 10
	}
	if cfg.MaxIdleTotal <= 0 {
		cfg.MaxIdleTotal = 100
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 90 * time.Second
	}

	p := &Pool{
		config:   cfg,
		conns:    make(map[string]*list.List),
		closedCh: make(chan struct{}),
	}

	go p.sweeper()
	return p
}

// Get retrieves an idle connection from the pool or dials a new one
func (p *Pool) Get(network, addr string) (net.Conn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}

	// Try to get an idle connection
	if connList, ok := p.conns[addr]; ok {
		for connList.Len() > 0 {
			elem := connList.Front()
			connList.Remove(elem)
			p.idleCount--

			pc := elem.Value.(*pooledConn)

			// Check if connection is alive
			if p.isAlive(pc.conn) {
				p.mu.Unlock()
				return pc.conn, nil
			}
			// Dead connection, close and try next
			pc.conn.Close()
		}
	}
	p.mu.Unlock()

	// No idle connection available, dial new
	return directDial(network, addr)
}

// Put returns a connection to the pool for reuse
func (p *Pool) Put(conn net.Conn, addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		conn.Close()
		return
	}

	// Check total limit
	if p.idleCount >= p.config.MaxIdleTotal {
		conn.Close()
		return
	}

	// Get or create list for this host
	connList, ok := p.conns[addr]
	if !ok {
		connList = list.New()
		p.conns[addr] = connList
	}

	// Check per-host limit
	if connList.Len() >= p.config.MaxIdlePerHost {
		conn.Close()
		return
	}

	// Add to pool
	pc := &pooledConn{
		conn:      conn,
		idleSince: time.Now(),
	}
	connList.PushFront(pc)
	p.idleCount++
}

// Close shuts down the pool and closes all idle connections
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.closedCh)

	// Close all idle connections
	for _, connList := range p.conns {
		for elem := connList.Front(); elem != nil; elem = elem.Next() {
			pc := elem.Value.(*pooledConn)
			pc.conn.Close()
		}
	}
	p.conns = make(map[string]*list.List)
	p.idleCount = 0
	p.mu.Unlock()
}

// isAlive checks if a connection is still usable using a 1ms read timeout probe
func (p *Pool) isAlive(conn net.Conn) bool {
	// Set a very short read deadline
	conn.SetReadDeadline(time.Now().Add(time.Millisecond))
	defer conn.SetReadDeadline(time.Time{}) // Clear deadline

	// Try to read - if we get data or timeout, connection is alive
	// If we get EOF or other error, it's dead
	one := make([]byte, 1)
	_, err := conn.Read(one)

	if err == nil {
		// Got data unexpectedly - this shouldn't happen for a pooled proxy connection
		// but we'll consider it alive. The data is lost though.
		return true
	}

	// Check if it's a timeout (expected for alive connection)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}

	// Any other error means connection is dead
	return false
}

// sweeper runs periodically to evict idle connections that have timed out
func (p *Pool) sweeper() {
	// Sweep at half the idle timeout interval
	interval := p.config.IdleTimeout / 2
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.closedCh:
			return
		case <-ticker.C:
			p.sweep()
		}
	}
}

// sweep removes connections that have been idle too long
func (p *Pool) sweep() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	now := time.Now()
	for addr, connList := range p.conns {
		// Iterate from back (oldest) to front (newest)
		var next *list.Element
		for elem := connList.Back(); elem != nil; elem = next {
			next = elem.Prev()
			pc := elem.Value.(*pooledConn)

			if now.Sub(pc.idleSince) > p.config.IdleTimeout {
				connList.Remove(elem)
				pc.conn.Close()
				p.idleCount--
			} else {
				// Connections are ordered by time, so if this one is fresh,
				// all remaining ones (toward front) are also fresh
				break
			}
		}

		// Clean up empty lists
		if connList.Len() == 0 {
			delete(p.conns, addr)
		}
	}
}

// Len returns the current number of idle connections (for testing)
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idleCount
}
