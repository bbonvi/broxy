package router

import (
	"net/netip"
	"strings"

	"broxy/config"
)

type matchKind uint8

const (
	matchDomain matchKind = iota
	matchIP
	matchDefault
)

type compiledRule struct {
	match          matchKind
	proxy          *config.ProxyConfig
	domainSuffixes []string
	bareDomains    []string
	exactIPs       []netip.Addr
	cidrPrefixes   []netip.Prefix
}

type Router struct {
	rules []compiledRule
}

// New builds a Router with precompiled matchers to avoid per-request parsing.
func New(cfg *config.Config) *Router {
	proxies := make(map[string]*config.ProxyConfig, len(cfg.Proxies))
	for i := range cfg.Proxies {
		proxies[cfg.Proxies[i].Name] = &cfg.Proxies[i]
	}

	compiled := make([]compiledRule, 0, len(cfg.Rules))
	for i := range cfg.Rules {
		rule := cfg.Rules[i]
		cr := compiledRule{
			proxy: proxies[rule.Proxy],
		}

		switch rule.Match {
		case "domain":
			cr.match = matchDomain
			for _, value := range ruleValues(rule) {
				cr.domainSuffixes = append(cr.domainSuffixes, value)
				if strings.HasPrefix(value, ".") && len(value) > 1 {
					cr.bareDomains = append(cr.bareDomains, value[1:])
				}
			}
		case "ip":
			cr.match = matchIP
			for _, value := range ruleValues(rule) {
				if strings.Contains(value, "/") {
					if prefix, err := netip.ParsePrefix(value); err == nil {
						cr.cidrPrefixes = append(cr.cidrPrefixes, prefix)
					}
					continue
				}

				if ip, err := netip.ParseAddr(value); err == nil {
					cr.exactIPs = append(cr.exactIPs, ip)
				}
			}
		default:
			cr.match = matchDefault
		}

		compiled = append(compiled, cr)
	}

	return &Router{
		rules: compiled,
	}
}

// Route returns the proxy config for the given host
func (r *Router) Route(host string) *config.ProxyConfig {
	domain := extractHost(host)
	ip, isIP := parseLiteralIP(domain)
	for _, rule := range r.rules {
		switch rule.match {
		case matchIP:
			if !isIP {
				continue
			}
			for _, exactIP := range rule.exactIPs {
				if exactIP == ip {
					return rule.proxy
				}
			}
			for _, prefix := range rule.cidrPrefixes {
				if prefix.Contains(ip) {
					return rule.proxy
				}
			}
		case matchDomain:
			if isIP {
				continue
			}
			for _, suffix := range rule.domainSuffixes {
				if strings.HasSuffix(domain, suffix) {
					return rule.proxy
				}
			}
			for _, bareDomain := range rule.bareDomains {
				if domain == bareDomain {
					return rule.proxy
				}
			}
		case matchDefault:
			return rule.proxy
		}
	}

	return nil
}

func ruleValues(rule config.Rule) []string {
	if len(rule.Values) > 0 {
		return rule.Values
	}
	if rule.Value == "" {
		return nil
	}
	return []string{rule.Value}
}

// extractHost strips the port while keeping IPv6 literals intact.
func extractHost(host string) string {
	if host == "" {
		return ""
	}

	// [IPv6]:port
	if host[0] == '[' {
		if end := strings.IndexByte(host, ']'); end > 1 {
			return host[1:end]
		}
	}

	// host:port
	firstColon := strings.IndexByte(host, ':')
	if firstColon != -1 {
		lastColon := strings.LastIndexByte(host, ':')
		if firstColon == lastColon {
			return host[:firstColon]
		}
	}

	return host
}

// parseLiteralIP avoids costly parse attempts for obvious hostnames.
func parseLiteralIP(host string) (netip.Addr, bool) {
	if host == "" {
		return netip.Addr{}, false
	}

	hasColon := false
	for i := 0; i < len(host); i++ {
		ch := host[i]
		switch {
		case ch >= '0' && ch <= '9':
		case ch == '.':
		case ch == ':':
			hasColon = true
		case hasColon && ((ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') || ch == '%'):
		default:
			return netip.Addr{}, false
		}
	}

	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip, true
}
