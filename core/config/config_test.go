package config_test

import (
	"os"
	"path/filepath"
	"testing"

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
		"profile scale":      func(c *config.Config) { c.Profile = "scale" },
		"postgres":           func(c *config.Config) { c.Database.Driver = "postgres" },
		"valkey":             func(c *config.Config) { c.Cache.Driver = "valkey" },
		"two writers":        func(c *config.Config) { c.Database.WriteConnections = 2 },
		"shared analytics":   func(c *config.Config) { c.Database.AnalyticsPath = c.Database.Path },
		"checkpoint off":     func(c *config.Config) { c.Database.WALAutocheckpoint = 0 },
		"agent kit":          func(c *config.Config) { c.Kits = []string{"agent"} },
		"prod public listen": func(c *config.Config) { c.Environment = "production"; c.HTTP.Listen = "0.0.0.0:80" },
		"prod normal sync":   func(c *config.Config) { c.Environment = "production"; c.Database.Synchronous = "NORMAL" },
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
