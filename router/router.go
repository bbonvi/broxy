package router

import (
	"net"
	"strings"

	"broxy/config"
)

type Router struct {
	rules   []config.Rule
	proxies map[string]*config.ProxyConfig
}

func New(cfg *config.Config) *Router {
	proxies := make(map[string]*config.ProxyConfig)
	for i := range cfg.Proxies {
		proxies[cfg.Proxies[i].Name] = &cfg.Proxies[i]
	}

	return &Router{
		rules:   cfg.Rules,
		proxies: proxies,
	}
}

// Route returns the proxy config for the given host
func (r *Router) Route(host string) *config.ProxyConfig {
	// Extract domain/IP from host (remove port if present)
	domain := host
	if idx := strings.Index(host, ":"); idx != -1 {
		domain = host[:idx]
	}

	// Check if domain is an IP address
	ip := net.ParseIP(domain)

	for _, rule := range r.rules {
		switch rule.Match {
		case "ip":
			// Handle IP/CIDR matching
			if ip != nil {
				// Get values to check (either single value or multiple values)
				valuesToCheck := rule.Values
				if len(valuesToCheck) == 0 && rule.Value != "" {
					valuesToCheck = []string{rule.Value}
				}

				for _, val := range valuesToCheck {
					// Check if it's a CIDR range
					if strings.Contains(val, "/") {
						_, ipNet, err := net.ParseCIDR(val)
						if err == nil && ipNet.Contains(ip) {
							return r.proxies[rule.Proxy]
						}
					} else {
						// Exact IP match
						ruleIP := net.ParseIP(val)
						if ruleIP != nil && ruleIP.Equal(ip) {
							return r.proxies[rule.Proxy]
						}
					}
				}
			}
		case "domain":
			// Only match domains, not IPs
			if ip == nil {
				// Get values to check (either single value or multiple values)
				valuesToCheck := rule.Values
				if len(valuesToCheck) == 0 && rule.Value != "" {
					valuesToCheck = []string{rule.Value}
				}

				for _, val := range valuesToCheck {
					if strings.HasSuffix(domain, val) {
						return r.proxies[rule.Proxy]
					}
					// If rule starts with ".", also match the bare domain
					if strings.HasPrefix(val, ".") {
						bareDomain := val[1:] // Remove leading dot
						if domain == bareDomain {
							return r.proxies[rule.Proxy]
						}
					}
				}
			}
		case "default":
			return r.proxies[rule.Proxy]
		}
	}

	return nil
}
