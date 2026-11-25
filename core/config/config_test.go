package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pillar/gostack/core/config"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestPrecedence(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gostack.yaml")
	os.WriteFile(p, []byte("http:\n  listen: 127.0.0.1:9000\ndatabase:\n  path: data/x.db\n  read_connections: 3\n"), 0o600)
	c, err := config.Load(p, env(map[string]string{"GOSTACK_DATABASE_READ_CONNECTIONS": "4", "GOSTACK_HTTP_LISTEN": "127.0.0.1:9100"}),
		map[string]string{"http.listen": "127.0.0.1:9200"})
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP.Listen != "127.0.0.1:9200" || c.Database.ReadConnections != 4 || c.Database.Path != filepath.Join(dir, "data/x.db") || c.HTTP.MaxBodyBytes != 1<<20 {
		t.Fatalf("precedence wrong: %+v", c)
	}
}

func TestRejectsUnknownKeysAndUnimplemented(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("htp:\n  listen: x\n"), 0o600)
	if _, err := config.Load(p, env(nil), nil); err == nil {
		t.Fatal("unknown key accepted")
	}
	for name, mut := range map[string]func(*config.Config){
		"profile distributed": func(c *config.Config) { c.Profile = "distributed" },
		"scale on sqlite":     func(c *config.Config) { c.Profile = "scale" },
		"postgres under lite": func(c *config.Config) { c.Database.Driver = "postgres"; c.Database.DSNRef = "env:X" },
		"valkey":              func(c *config.Config) { c.Cache.Driver = "valkey" },
		"two writers":         func(c *config.Config) { c.Database.WriteConnections = 2 },
		"shared analytics":    func(c *config.Config) { c.Database.AnalyticsPath = c.Database.Path },
		"checkpoint off":      func(c *config.Config) { c.Database.WALAutocheckpoint = 0 },
		"agent kit":           func(c *config.Config) { c.Kits = []string{"agent"} },
		"prod public listen":  func(c *config.Config) { c.Environment = "production"; c.HTTP.Listen = "0.0.0.0:80" },
		"prod normal sync":    func(c *config.Config) { c.Environment = "production"; c.Database.Synchronous = "NORMAL" },
		"plain secret": func(c *config.Config) {
			c.Game.Platforms = map[string]config.PlatformConfig{"wechat": {Apps: map[string]config.PlatformApp{"g": {AppID: "a", AppSecretRef: "literal"}}}}
		},
	} {
		c := config.Defaults()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := config.Defaults()
	c.Environment, c.Database.Synchronous, c.Database.AcceptNormalSyncRisk = "production", "NORMAL", true
	if err := c.Validate(); err != nil {
		t.Fatalf("explicit NORMAL acceptance rejected: %v", err)
	}
	c = config.Defaults()
	c.Backup.Enabled, c.Database.WALAutocheckpoint = true, 0
	if err := c.Validate(); err != nil {
		t.Fatalf("litestream-managed checkpoint rejected: %v", err)
	}
}

func scale() config.Config {
	c := config.Defaults()
	c.Profile = "scale"
	c.Database.Driver = "postgres"
	c.Database.DSNRef = "env:GOSTACK_DSN"
	c.Queue.Driver = "postgres"
	c.RateLimit.AcceptPerInstance = true
	return c
}

func TestScaleProfile(t *testing.T) {
	c := scale()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid scale config rejected: %v", err)
	}
	c.Database.AnalyticsDSNRef, c.Database.MaxOpenConnections = "file:/run/secrets/an", 40
	c.Kits, c.Game.Modules = []string{"game"}, []string{"ops", "match"}
	if err := c.Validate(); err != nil {
		t.Fatalf("scale with analytics dsn and modules rejected: %v", err)
	}
	for name, mut := range map[string]func(*config.Config){
		"no dsn":              func(c *config.Config) { c.Database.DSNRef = "" },
		"literal dsn":         func(c *config.Config) { c.Database.DSNRef = "postgres://u:p@h/db" },
		"literal analytics":   func(c *config.Config) { c.Database.AnalyticsDSNRef = "postgres://h/db" },
		"sqlite queue":        func(c *config.Config) { c.Queue.Driver = "sqlite" },
		"sqlite driver":       func(c *config.Config) { c.Database.Driver = "sqlite" },
		"local rate limits":   func(c *config.Config) { c.RateLimit.AcceptPerInstance = false },
		"realtime":            func(c *config.Config) { c.Kits, c.Game.Modules = []string{"game"}, []string{"realtime"} },
		"litestream":          func(c *config.Config) { c.Backup.Enabled = true },
		"local storage":       func(c *config.Config) { c.Storage.Driver = "local" },
		"sqlite sync risk":    func(c *config.Config) { c.Database.AcceptNormalSyncRisk = true },
		"too many conns":      func(c *config.Config) { c.Database.MaxOpenConnections = 501 },
		"postgres queue lite": func(c *config.Config) { c.Profile = "lite" },
	} {
		c := scale()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Lite rejects PostgreSQL-only settings instead of ignoring them.
	l := config.Defaults()
	l.Database.DSNRef = "env:X"
	if err := l.Validate(); err == nil {
		t.Error("dsn_ref accepted under lite")
	}
	l = config.Defaults()
	l.Queue.Driver = "postgres"
	if err := l.Validate(); err == nil {
		t.Error("postgres queue accepted under lite")
	}
	// Environment overrides reach the new keys.
	got, err := config.Load("", env(map[string]string{"GOSTACK_PROFILE": "scale", "GOSTACK_DATABASE_DRIVER": "postgres",
		"GOSTACK_DATABASE_DSN_REF": "env:D", "GOSTACK_QUEUE_DRIVER": "postgres", "GOSTACK_RATE_LIMIT_ACCEPT_PER_INSTANCE": "true",
		"GOSTACK_DATABASE_MAX_OPEN_CONNECTIONS": "20"}), nil)
	if err != nil || got.Database.MaxOpenConnections != 20 || got.Database.DSNRef != "env:D" {
		t.Fatalf("env scale config: %+v %v", got.Database, err)
	}
}

func valkeyScale() config.Config {
	c := scale()
	c.RateLimit.AcceptPerInstance = false
	c.RateLimit.Driver, c.Cache.Driver = "valkey", "valkey"
	c.Valkey.Address, c.Valkey.KeyPrefix = "10.0.0.5:6379", "mygame:"
	return c
}

func TestValkeyDrivers(t *testing.T) {
	c := valkeyScale()
	if err := c.Validate(); err != nil {
		t.Fatalf("scale with global limiter and valkey cache rejected: %v", err)
	}
	c.Cache.Driver = "memory" // valkey only for the limiter
	c.Valkey.PasswordRef, c.Valkey.Username, c.Valkey.DB = "env:VK", "app", 3
	if err := c.Validate(); err != nil {
		t.Fatalf("limiter-only valkey rejected: %v", err)
	}
	c = valkeyScale()
	c.RateLimit.Driver, c.RateLimit.AcceptPerInstance = "local", true // valkey only for the cache
	if err := c.Validate(); err != nil {
		t.Fatalf("cache-only valkey rejected: %v", err)
	}
	for name, mut := range map[string]func(*config.Config){
		"no address":        func(c *config.Config) { c.Valkey.Address = "" },
		"bad address":       func(c *config.Config) { c.Valkey.Address = "valkey" },
		"no prefix":         func(c *config.Config) { c.Valkey.KeyPrefix = "" },
		"bad prefix":        func(c *config.Config) { c.Valkey.KeyPrefix = "a b" },
		"literal password":  func(c *config.Config) { c.Valkey.PasswordRef = "hunter2" },
		"user no password":  func(c *config.Config) { c.Valkey.Username = "u" },
		"redundant ack":     func(c *config.Config) { c.RateLimit.AcceptPerInstance = true },
		"unknown limiter":   func(c *config.Config) { c.RateLimit.Driver = "redis" },
		"unknown cache":     func(c *config.Config) { c.Cache.Driver = "memcached" },
		"slow limiter":      func(c *config.Config) { c.RateLimit.Timeout = 2 * time.Second },
		"local without ack": func(c *config.Config) { c.RateLimit.Driver = "local" },
		"valkey under lite": func(c *config.Config) {
			*c = config.Defaults()
			c.Cache.Driver = "valkey"
			c.Valkey.Address, c.Valkey.KeyPrefix = "h:1", "p:"
		},
		"limiter under lite": func(c *config.Config) {
			*c = config.Defaults()
			c.RateLimit.Driver = "valkey"
			c.Valkey.Address, c.Valkey.KeyPrefix = "h:1", "p:"
		},
		"orphan valkey block": func(c *config.Config) {
			c.Cache.Driver, c.RateLimit.Driver, c.RateLimit.AcceptPerInstance = "memory", "local", true
		},
	} {
		c := valkeyScale()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	got, err := config.Load("", env(map[string]string{"GOSTACK_PROFILE": "scale", "GOSTACK_DATABASE_DRIVER": "postgres",
		"GOSTACK_DATABASE_DSN_REF": "env:D", "GOSTACK_QUEUE_DRIVER": "postgres", "GOSTACK_RATE_LIMIT_DRIVER": "valkey",
		"GOSTACK_CACHE_DRIVER": "valkey", "GOSTACK_VALKEY_ADDRESS": "127.0.0.1:6379", "GOSTACK_VALKEY_KEY_PREFIX": "g:",
		"GOSTACK_VALKEY_PASSWORD_REF": "env:VK"}), nil)
	if err != nil || got.Valkey.Address != "127.0.0.1:6379" || got.RateLimit.Driver != "valkey" || got.Cache.Driver != "valkey" {
		t.Fatalf("env valkey config: %+v %v", got.Valkey, err)
	}
}
