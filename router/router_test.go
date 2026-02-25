package router

import (
	"fmt"
	"testing"

	"broxy/config"
)

func TestRoute_DomainSuffixAndBare(t *testing.T) {
	cfg := &config.Config{
		Proxies: []config.ProxyConfig{
			{Name: "direct", Type: "direct"},
			{Name: "fallback", Type: "direct"},
		},
		Rules: []config.Rule{
			{Match: "domain", Value: ".example.com", Proxy: "direct"},
			{Match: "default", Proxy: "fallback"},
		},
	}
	r := New(cfg)

	tests := []struct {
		name  string
		host  string
		proxy string
	}{
		{name: "suffix", host: "api.example.com:443", proxy: "direct"},
		{name: "bare", host: "example.com:443", proxy: "direct"},
		{name: "default", host: "other.com:443", proxy: "fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.Route(tt.host)
			if got == nil || got.Name != tt.proxy {
				t.Fatalf("Route(%q) = %v, want proxy %q", tt.host, got, tt.proxy)
			}
		})
	}
}

func TestRoute_IPExactAndCIDR(t *testing.T) {
	cfg := &config.Config{
		Proxies: []config.ProxyConfig{
			{Name: "direct", Type: "direct"},
			{Name: "fallback", Type: "direct"},
		},
		Rules: []config.Rule{
			{Match: "ip", Values: []string{"10.0.0.1", "192.168.0.0/16", "2001:db8::1"}, Proxy: "direct"},
			{Match: "default", Proxy: "fallback"},
		},
	}
	r := New(cfg)

	tests := []struct {
		name  string
		host  string
		proxy string
	}{
		{name: "exact ipv4", host: "10.0.0.1:443", proxy: "direct"},
		{name: "cidr ipv4", host: "192.168.10.5:443", proxy: "direct"},
		{name: "exact ipv6", host: "[2001:db8::1]:443", proxy: "direct"},
		{name: "default", host: "8.8.8.8:443", proxy: "fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.Route(tt.host)
			if got == nil || got.Name != tt.proxy {
				t.Fatalf("Route(%q) = %v, want proxy %q", tt.host, got, tt.proxy)
			}
		})
	}
}

func TestRoute_InvalidIPValuesIgnored(t *testing.T) {
	cfg := &config.Config{
		Proxies: []config.ProxyConfig{
			{Name: "direct", Type: "direct"},
			{Name: "fallback", Type: "direct"},
		},
		Rules: []config.Rule{
			{Match: "ip", Values: []string{"not-an-ip", "bad/cidr"}, Proxy: "direct"},
			{Match: "default", Proxy: "fallback"},
		},
	}
	r := New(cfg)

	got := r.Route("10.10.10.10:443")
	if got == nil || got.Name != "fallback" {
		t.Fatalf("Route() = %v, want fallback proxy", got)
	}
}

func TestRoute_RuleOrderPreserved(t *testing.T) {
	cfg := &config.Config{
		Proxies: []config.ProxyConfig{
			{Name: "first", Type: "direct"},
			{Name: "second", Type: "direct"},
		},
		Rules: []config.Rule{
			{Match: "domain", Value: ".example.com", Proxy: "first"},
			{Match: "domain", Value: ".example.com", Proxy: "second"},
			{Match: "default", Proxy: "second"},
		},
	}
	r := New(cfg)

	got := r.Route("api.example.com:443")
	if got == nil || got.Name != "first" {
		t.Fatalf("Route() = %v, want first matching proxy", got)
	}
}

func BenchmarkRoute_DomainMiss(b *testing.B) {
	ruleCounts := []int{1, 50, 200}
	for _, count := range ruleCounts {
		b.Run(fmt.Sprintf("rules=%d", count), func(b *testing.B) {
			r := newDomainBenchRouter(count)
			host := "service.internal.local:443"

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = r.Route(host)
			}
		})
	}
}

func BenchmarkRoute_IPMiss(b *testing.B) {
	ruleCounts := []int{10, 100}
	for _, count := range ruleCounts {
		b.Run(fmt.Sprintf("cidr_rules=%d", count), func(b *testing.B) {
			r := newIPBenchRouter(count)
			host := "192.168.10.10:443"

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = r.Route(host)
			}
		})
	}
}

func newDomainBenchRouter(domainRules int) *Router {
	proxies := []config.ProxyConfig{{Name: "direct", Type: "direct"}}
	rules := make([]config.Rule, 0, domainRules+1)
	for i := 0; i < domainRules; i++ {
		rules = append(rules, config.Rule{
			Match: "domain",
			Value: fmt.Sprintf(".example%d.com", i),
			Proxy: "direct",
		})
	}
	rules = append(rules, config.Rule{Match: "default", Proxy: "direct"})

	return New(&config.Config{Proxies: proxies, Rules: rules})
}

func newIPBenchRouter(cidrRules int) *Router {
	proxies := []config.ProxyConfig{{Name: "direct", Type: "direct"}}
	rules := make([]config.Rule, 0, cidrRules+1)
	for i := 0; i < cidrRules; i++ {
		rules = append(rules, config.Rule{
			Match: "ip",
			Value: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256),
			Proxy: "direct",
		})
	}
	rules = append(rules, config.Rule{Match: "default", Proxy: "direct"})

	return New(&config.Config{Proxies: proxies, Rules: rules})
}
