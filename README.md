# Broxy

A simple proxy forwarder written in Go. Routes requests through different upstream proxies based on domain rules.

## Features

- Local HTTP proxy server
- Route requests based on domain matching
- Support for multiple upstream proxy types:
  - HTTP proxies
  - SOCKS5 proxies (with authentication)
  - Direct connections
- YAML-based configuration

## Installation

```bash
go build -o broxy
```

## Configuration

Create a `rules.yaml` file (see `rules.yaml.example` for reference):

```yaml
server:
  listen: 0.0.0.0
  port: 3128

proxies:
  - name: proxy_google
    type: http
    host: proxy1.example.com
    port: 8080

  - name: proxy_reddit
    type: socks5
    host: proxy2.example.com
    port: 1080
    auth:
      username: user
      password: pass

  - name: direct
    type: direct

rules:
  - match: domain
    value: ".google.com"
    proxy: proxy_google

  - match: domain
    value: ".reddit.com"
    proxy: proxy_reddit

  - match: ip
    value: "192.168.1.0/24"
    proxy: proxy_google

  - match: ip
    value: "10.0.0.1"
    proxy: proxy_reddit

  - match: default
    proxy: direct
```

## Usage

```bash
# Run with default config file (~/.config/broxy/rules.yaml)
./broxy run

# Run with custom config file
./broxy run -config /path/to/config.yaml
```

## Running as a Service (macOS)

Create a LaunchAgent plist file at `~/Library/LaunchAgents/com.broxy.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.broxy</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/broxy</string>
        <string>run</string>
        <string>-config</string>
        <string>/Users/YOUR_USERNAME/.config/broxy/rules.yaml</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/Users/YOUR_USERNAME/.config/broxy/broxy.log</string>
    <key>StandardErrorPath</key>
    <string>/Users/YOUR_USERNAME/.config/broxy/broxy.error.log</string>
</dict>
</plist>
```

Then load the service:

```bash
launchctl load ~/Library/LaunchAgents/com.broxy.plist
```

To stop the service:

```bash
launchctl unload ~/Library/LaunchAgents/com.broxy.plist
```

## Configuration Options

### Server
- `listen`: IP address to listen on (default: 0.0.0.0)
- `port`: Port to listen on (default: 3128)

### Proxies
- `name`: Unique identifier for the proxy
- `type`: Proxy type (`http`, `socks5`, or `direct`)
- `host`: Proxy server hostname
- `port`: Proxy server port
- `auth`: (optional) Authentication credentials
  - `username`: Username
  - `password`: Password

### Rules
- `match`: Rule type (`domain`, `ip`, or `default`)
- `value`: Match pattern:
  - For `domain` rules: Domain suffix (e.g., `.google.com` matches `google.com` and `www.google.com`)
  - For `ip` rules: Exact IP (e.g., `1.2.3.4`) or CIDR range (e.g., `192.168.0.0/16`)
- `proxy`: Name of the proxy to use

## How It Works

1. Broxy starts a local HTTP proxy server
2. When a request comes in, the router checks the domain against configured rules
3. The request is forwarded through the matching upstream proxy
4. If no specific rule matches, the default rule is used

## Limitations

- SOCKS5 proxies only work for HTTPS (CONNECT method); plain HTTP through SOCKS5 is not supported
