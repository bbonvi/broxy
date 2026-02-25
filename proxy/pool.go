package proxy

import (
	"container/list"
	"errors"
	"net"
	"sync"
	"sync/atomic"
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

type poolShard struct {
	mu    sync.Mutex
	conns map[string]*list.List // host -> list of *pooledConn
}

// Pool manages reusable TCP connections to upstream proxies
type Pool struct {
	config    PoolConfig
	shards    []poolShard
	idleCount atomic.Int64
	closed    atomic.Bool
	closedCh  chan struct{}
}

const poolShardCount = 32

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
		shards:   make([]poolShard, poolShardCount),
		closedCh: make(chan struct{}),
	}
	for i := range p.shards {
		p.shards[i].conns = make(map[string]*list.List)
	}

	go p.sweeper()
	return p
}

func hashAddr(addr string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(addr); i++ {
		h ^= uint32(addr[i])
		h *= 16777619
	}
	return h
}

func (p *Pool) shardFor(addr string) *poolShard {
	idx := int(hashAddr(addr) % uint32(len(p.shards)))
	return &p.shards[idx]
}

func (p *Pool) tryIncrementIdleCount() bool {
	maxIdleTotal := int64(p.config.MaxIdleTotal)
	for {
		current := p.idleCount.Load()
		if current >= maxIdleTotal {
			return false
		}
		if p.idleCount.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// Get retrieves an idle connection from the pool or dials a new one
func (p *Pool) Get(network, addr string) (net.Conn, error) {
	shard := p.shardFor(addr)
	for {
		var pc *pooledConn

		shard.mu.Lock()
		if p.closed.Load() {
			shard.mu.Unlock()
			return nil, ErrPoolClosed
		}

		// Pop one idle connection while holding lock, then validate outside lock.
		if connList, ok := shard.conns[addr]; ok && connList.Len() > 0 {
			elem := connList.Front()
			connList.Remove(elem)
			p.idleCount.Add(-1)
			if connList.Len() == 0 {
				delete(shard.conns, addr)
			}
			pc = elem.Value.(*pooledConn)
		}
		shard.mu.Unlock()

		// No idle connection available, dial new.
		if pc == nil {
			return directDial(network, addr)
		}

		// Dead connections are discarded and we retry for another pooled entry.
		if p.isAlive(pc.conn) {
			return pc.conn, nil
		}
		pc.conn.Close()
	}
}

// Put returns a connection to the pool for reuse
func (p *Pool) Put(conn net.Conn, addr string) {
	if p.closed.Load() {
		conn.Close()
		return
	}

	shard := p.shardFor(addr)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if p.closed.Load() {
		conn.Close()
		return
	}

	// Get or create list for this host
	connList, ok := shard.conns[addr]
	if !ok {
		connList = list.New()
		shard.conns[addr] = connList
	}

	// Check per-host limit
	if connList.Len() >= p.config.MaxIdlePerHost {
		conn.Close()
		return
	}

	// Check total limit across all shards
	if !p.tryIncrementIdleCount() {
		conn.Close()
		return
	}
	if p.closed.Load() {
		p.idleCount.Add(-1)
		conn.Close()
		return
	}

	// Add to pool
	pc := &pooledConn{
		conn:      conn,
		idleSince: time.Now(),
	}
	connList.PushFront(pc)
}

// Close shuts down the pool and closes all idle connections
func (p *Pool) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	close(p.closedCh)

	var removed int64
	for i := range p.shards {
		shard := &p.shards[i]
		shard.mu.Lock()
		for _, connList := range shard.conns {
			for elem := connList.Front(); elem != nil; elem = elem.Next() {
				pc := elem.Value.(*pooledConn)
				pc.conn.Close()
				removed++
			}
		}
		shard.conns = make(map[string]*list.List)
		shard.mu.Unlock()
	}
	if removed > 0 {
		p.idleCount.Add(-removed)
	}
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
	if p.closed.Load() {
		return
	}

	now := time.Now()
	for i := range p.shards {
		shard := &p.shards[i]
		shard.mu.Lock()
		for addr, connList := range shard.conns {
			// Iterate from back (oldest) to front (newest)
			var next *list.Element
			for elem := connList.Back(); elem != nil; elem = next {
				next = elem.Prev()
				pc := elem.Value.(*pooledConn)

				if now.Sub(pc.idleSince) > p.config.IdleTimeout {
					connList.Remove(elem)
					pc.conn.Close()
					p.idleCount.Add(-1)
				} else {
					// Connections are ordered by time, so if this one is fresh,
					// all remaining ones (toward front) are also fresh
					break
				}
			}

			// Clean up empty lists
			if connList.Len() == 0 {
				delete(shard.conns, addr)
			}
		}
		shard.mu.Unlock()
	}
}

// Len returns the current number of idle connections (for testing)
func (p *Pool) Len() int {
	return int(p.idleCount.Load())
}

func (p *Pool) hostLen(addr string) int {
	shard := p.shardFor(addr)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if connList, ok := shard.conns[addr]; ok {
		return connList.Len()
	}
	return 0
}
