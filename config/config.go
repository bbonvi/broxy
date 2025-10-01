package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Proxies []ProxyConfig `yaml:"proxies"`
	Rules   []Rule        `yaml:"rules"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
	Port   int    `yaml:"port"`
}

type ProxyConfig struct {
	Name string     `yaml:"name"`
	Type string     `yaml:"type"` // http, socks5, direct
	Host string     `yaml:"host"`
	Port int        `yaml:"port"`
	Auth *ProxyAuth `yaml:"auth,omitempty"`
}

type ProxyAuth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Rule struct {
	Match  string   `yaml:"match"` // domain, default
	Value  string   `yaml:"value,omitempty"`
	Values []string `yaml:"values,omitempty"`
	Proxy  string   `yaml:"proxy"`
}

func Load(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	// Validate server config
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen is required")
	}
	if c.Server.Port == 0 {
		return fmt.Errorf("server.port is required")
	}

	// Build proxy name set for validation
	proxyNames := make(map[string]bool)
	for i, proxy := range c.Proxies {
		// Check for duplicate proxy names
		if proxyNames[proxy.Name] {
			return fmt.Errorf("duplicate proxy name: %s", proxy.Name)
		}
		if proxy.Name == "" {
			return fmt.Errorf("proxy[%d]: name is required", i)
		}
		proxyNames[proxy.Name] = true

		// Validate proxy type
		switch proxy.Type {
		case "http", "socks5":
			if proxy.Host == "" {
				return fmt.Errorf("proxy %s: host is required for type %s", proxy.Name, proxy.Type)
			}
			if proxy.Port == 0 {
				return fmt.Errorf("proxy %s: port is required for type %s", proxy.Name, proxy.Type)
			}
		case "direct":
			// Direct doesn't need host/port
		case "":
			return fmt.Errorf("proxy %s: type is required", proxy.Name)
		default:
			return fmt.Errorf("proxy %s: invalid type %s (must be http, socks5, or direct)", proxy.Name, proxy.Type)
		}

		// Validate auth if present
		if proxy.Auth != nil {
			if proxy.Auth.Username == "" {
				return fmt.Errorf("proxy %s: auth.username is required when auth is specified", proxy.Name)
			}
			if proxy.Auth.Password == "" {
				return fmt.Errorf("proxy %s: auth.password is required when auth is specified", proxy.Name)
			}
		}
	}

	if len(c.Proxies) == 0 {
		return fmt.Errorf("at least one proxy must be defined")
	}

	// Validate rules
	if len(c.Rules) == 0 {
		return fmt.Errorf("at least one rule must be defined")
	}

	for i, rule := range c.Rules {
		// Validate match type
		switch rule.Match {
		case "domain", "ip":
			// Check that either value or values is provided, but not both
			hasValue := rule.Value != ""
			hasValues := len(rule.Values) > 0

			if !hasValue && !hasValues {
				return fmt.Errorf("rule[%d]: either value or values is required for match type %s", i, rule.Match)
			}
			if hasValue && hasValues {
				return fmt.Errorf("rule[%d]: cannot specify both value and values, use one or the other", i)
			}
			if rule.Proxy == "" {
				return fmt.Errorf("rule[%d]: proxy is required", i)
			}
		case "default":
			if rule.Proxy == "" {
				return fmt.Errorf("rule[%d]: proxy is required", i)
			}
		case "":
			return fmt.Errorf("rule[%d]: match is required", i)
		default:
			return fmt.Errorf("rule[%d]: invalid match type %s (must be domain, ip, or default)", i, rule.Match)
		}

		// Validate proxy reference
		if !proxyNames[rule.Proxy] {
			return fmt.Errorf("rule[%d]: proxy %s not found in proxies list", i, rule.Proxy)
		}
	}

	return nil
}
