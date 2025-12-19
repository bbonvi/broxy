# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# Project overview
A simple proxy forwarder written in Go. Designed to be primarily used as local proxy.

check rules.yaml.example for example usage.

# Architecture

## Core Components
- **config**: YAML-based configuration loading and validation. Supports `value` (single) or `values` (array) for rules. Pool config at startup only.
- **router**: Domain/IP matching engine that routes requests to appropriate upstream proxies
- **server**: HTTP proxy server with CONNECT support and live config reloading via fsnotify
- **proxy**: Connection dialer that handles HTTP, SOCKS5, and direct connections
- **proxy/pool.go**: Connection pool for upstream HTTP proxy reuse. Uses container/list for O(1) operations.

## Request Flow
1. Client connects to local proxy server (default: 0.0.0.0:3128)
2. Server extracts host from request (CONNECT for HTTPS, Host header for HTTP)
3. Router matches host against rules (IP > domain > default priority)
4. Server establishes connection through matched upstream proxy
5. For HTTPS: hijacks connection and bidirectionally copies data
6. For HTTP: uses http.Transport with appropriate proxy configuration

## Key Features
- **Live config reloading**: Server watches config file and reloads on changes (200ms debounce)
- **Transport caching**: HTTP transports are cached per proxy config to reuse connections
- **Thread-safe routing**: Router uses RWMutex for concurrent access during reloads
- **Multiple values per rule**: Rules support both `value: "string"` and `values: ["str1", "str2"]`

## CONNECT Tunnel Behavior
- **Idle timeout**: 60s per-read/write operation; stalled connections terminate automatically
- **Half-close support**: Properly handles TCP FIN (half-close) from either peer
- **Graceful cleanup**: Both copy goroutines synchronize via WaitGroup before returning

## Connection Pool
- **Scope**: Per-Server instance; one pool shared across all requests
- **Targets**: HTTP upstream proxies only (direct/SOCKS5 bypass pool)
- **Health check**: 1ms read timeout probe before reuse
- **Sweeper**: Background goroutine evicts expired connections at IdleTimeout/2 intervals
- **Limits**: MaxIdlePerHost (default 10), MaxIdleTotal (default 100)
- **Configuration**: Startup-only via config.PoolConfig; no live reload to avoid dropping connections
- **CONNECT tunnels**: Cannot be returned to pool (connection becomes dedicated tunnel)

## Routing Logic
- **IP rules**: Match exact IP or CIDR ranges (e.g., `192.168.1.0/24`)
- **Domain rules**: Suffix matching (e.g., `.google.com` matches `google.com` and `www.google.com`)
- **Default rule**: Fallback when no other rules match
- Rules are evaluated in order; first match wins

## Proxy Types
- **direct**: No upstream proxy, direct connection
- **http**: HTTP CONNECT proxy (supports auth)
- **socks5**: SOCKS5 proxy (supports auth, HTTPS only)

## Performance
- **Buffer pool**: sync.Pool with 32KB buffers for io.CopyBuffer; avoids allocations per copy
- **TCP tuning**: SetNoDelay(true), SetKeepAlive(30s) on both ends of CONNECT tunnels
- **Dial timeout**: 30s default via package-level defaultDialer
- **Optimizations**: 10x faster data copy, 2.6x fewer allocations for typical CONNECT operations

# Development

## Build
```bash
go build -o broxy
```

## Run
```bash
# Default config: ~/.config/broxy/rules.yaml
./broxy run

# Custom config
./broxy run -config /path/to/rules.yaml
```

## Testing
Test the proxy manually:
```bash
# In terminal 1: start proxy
./broxy run -config rules.yaml

# In terminal 2: test with curl
curl -x http://localhost:3128 https://example.com
```

## Config Location
Default: `~/.config/broxy/rules.yaml`

# commits
commits should follow conventional commit style.
dont make commit messages too long.
keep them concise and to the point.

# important-instruction-reminders
Do what has been asked; nothing more, nothing less.
NEVER create files unless they're absolutely necessary for achieving your goal.
ALWAYS prefer editing an existing file to creating a new one.
NEVER proactively create documentation files (*.md) or README files. Only create documentation files if explicitly requested by the User.
