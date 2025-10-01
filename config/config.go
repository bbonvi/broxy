package config

import (
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
	Match string `yaml:"match"` // domain, default
	Value string `yaml:"value,omitempty"`
	Proxy string `yaml:"proxy"`
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

	return &cfg, nil
}
