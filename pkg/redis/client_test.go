package redis

import (
	"testing"
	"time"
)

func TestNew_MapsConfigToOptions(t *testing.T) {
	cfg := Config{
		Addr:         "example.com:6379",
		Password:     "secret",
		DB:           3,
		PoolSize:     7,
		DialTimeout:  1 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 3 * time.Second,
	}

	client := New(cfg)
	t.Cleanup(func() { _ = client.Close() })

	opts := client.Options()
	if opts.Addr != cfg.Addr {
		t.Errorf("Addr: expected %q, got %q", cfg.Addr, opts.Addr)
	}
	if opts.Password != cfg.Password {
		t.Errorf("Password: expected %q, got %q", cfg.Password, opts.Password)
	}
	if opts.DB != cfg.DB {
		t.Errorf("DB: expected %d, got %d", cfg.DB, opts.DB)
	}
	if opts.PoolSize != cfg.PoolSize {
		t.Errorf("PoolSize: expected %d, got %d", cfg.PoolSize, opts.PoolSize)
	}
	if opts.DialTimeout != cfg.DialTimeout {
		t.Errorf("DialTimeout: expected %v, got %v", cfg.DialTimeout, opts.DialTimeout)
	}
	if opts.ReadTimeout != cfg.ReadTimeout {
		t.Errorf("ReadTimeout: expected %v, got %v", cfg.ReadTimeout, opts.ReadTimeout)
	}
	if opts.WriteTimeout != cfg.WriteTimeout {
		t.Errorf("WriteTimeout: expected %v, got %v", cfg.WriteTimeout, opts.WriteTimeout)
	}
}
