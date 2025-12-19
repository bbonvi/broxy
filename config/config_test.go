package config

import (
	"testing"
	"time"
)

func TestPoolConfig_ParseIdleTimeout(t *testing.T) {
	tests := []struct {
		name     string
		timeout  string
		want     time.Duration
		wantErr  bool
	}{
		{"empty", "", 0, false},
		{"90s", "90s", 90 * time.Second, false},
		{"5m", "5m", 5 * time.Minute, false},
		{"1h30m", "1h30m", 90 * time.Minute, false},
		{"invalid", "not-a-duration", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &PoolConfig{IdleTimeout: tt.timeout}
			got, err := p.ParseIdleTimeout()
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseIdleTimeout() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseIdleTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfig_Validate_PoolConfig(t *testing.T) {
	baseConfig := func() Config {
		return Config{
			Server: ServerConfig{Listen: "127.0.0.1", Port: 3128},
			Proxies: []ProxyConfig{
				{Name: "direct", Type: "direct"},
			},
			Rules: []Rule{
				{Match: "default", Proxy: "direct"},
			},
		}
	}

	t.Run("valid pool config", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Pool = PoolConfig{
			MaxIdlePerHost: 20,
			MaxIdleTotal:   200,
			IdleTimeout:    "2m",
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() error = %v, want nil", err)
		}
	})

	t.Run("empty pool config uses defaults", func(t *testing.T) {
		cfg := baseConfig()
		// Pool is zero value
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() error = %v, want nil", err)
		}
	})

	t.Run("invalid idle timeout", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Pool = PoolConfig{IdleTimeout: "not-a-duration"}
		if err := cfg.Validate(); err == nil {
			t.Error("Validate() expected error for invalid idle_timeout")
		}
	})
}
