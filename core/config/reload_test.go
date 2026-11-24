package config_test

import (
	"slices"
	"testing"
	"time"

	"github.com/pillar/gostack/core/config"
)

func TestDiffClassifiesHotAndRestartKeys(t *testing.T) {
	a := config.Defaults()
	b := a
	b.Log.Level = "debug"
	b.HTTP.Listen = "127.0.0.1:9999"
	b.HTTP.RequestTimeout = 3 * time.Second
	b.Game.Platforms = map[string]config.PlatformConfig{"wechat": {}}
	got := config.Diff(a, b)
	want := map[string]bool{"log.level": true, "http.listen": false, "http.request_timeout": false, "game.platforms": false}
	if len(got) != len(want) {
		t.Fatalf("diff = %+v", got)
	}
	for _, c := range got {
		hot, ok := want[c.Key]
		if !ok || hot != c.Hot {
			t.Fatalf("unexpected change %+v", c)
		}
		if c.Key == "http.request_timeout" && (c.Old != `"10s"` || c.New != `"3s"`) {
			t.Fatalf("duration rendering: %+v", c)
		}
	}
}

// WithHot must copy exactly the advertised hot keys: a key listed as hot but not
// copied would be silently ignored, and a copied restart key would be applied
// to a running process that cannot honour it.
func TestWithHotMatchesHotKeys(t *testing.T) {
	a := config.Defaults()
	b := a
	b.Log.Level, b.Log.Format = "debug", "text"
	b.HTTP.MaxConcurrent, b.HTTP.Listen = 7, "127.0.0.1:1"
	b.Game.AuthRatePerMinute, b.Game.ScoreRatePerMinute, b.Game.SaveHistory = 3, 4, 1
	b.Auth.Web.IPRatePerMinute, b.Auth.Web.AccountRatePerMinute, b.Auth.Web.Enabled = 5, 6, true
	b.Database.ReadConnections = 9
	applied := config.WithHot(a, b)
	var changed []string
	for _, c := range config.Diff(a, applied) {
		changed = append(changed, c.Key)
	}
	if !slices.Equal(changed, config.HotKeys()) {
		t.Fatalf("WithHot changed %v, HotKeys %v", changed, config.HotKeys())
	}
	for _, c := range config.Diff(applied, b) {
		if c.Hot {
			t.Fatalf("hot key %s not applied", c.Key)
		}
	}
}

func TestSnapshotAtomicSwap(t *testing.T) {
	s := config.NewSnapshot(config.Defaults())
	c := s.Load()
	c.Log.Level = "error"
	if s.Load().Log.Level != "info" {
		t.Fatal("Load returned shared value")
	}
	s.Store(c)
	if s.Load().Log.Level != "error" {
		t.Fatal("store not visible")
	}
}

func TestWebAuthValidation(t *testing.T) {
	for name, mut := range map[string]func(*config.WebAuthConfig){
		"short ttl":        func(w *config.WebAuthConfig) { w.SessionTTL = time.Minute },
		"idle beyond ttl":  func(w *config.WebAuthConfig) { w.IdleTimeout = w.SessionTTL + time.Hour },
		"zero ip rate":     func(w *config.WebAuthConfig) { w.IPRatePerMinute = 0 },
		"too many hashes":  func(w *config.WebAuthConfig) { w.MaxConcurrentHashes = 65 },
		"origin with path": func(w *config.WebAuthConfig) { w.AllowedOrigins = []string{"https://a.example/x"} },
		"origin bad":       func(w *config.WebAuthConfig) { w.AllowedOrigins = []string{"a.example"} },
	} {
		c := config.Defaults()
		c.Auth.Web.Enabled = true
		mut(&c.Auth.Web)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := config.Defaults()
	c.Auth.Web.Enabled = true
	c.Auth.Web.AllowedOrigins = []string{"https://app.example.com", "http://localhost:5173"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid web auth rejected: %v", err)
	}
	c.Environment = "production"
	if err := c.Validate(); err == nil {
		t.Fatal("production accepted http origin")
	}
	c = config.Defaults()
	c.HTTP.MaxConcurrent = config.MaxConcurrentCeiling + 1
	if err := c.Validate(); err == nil {
		t.Fatal("max_concurrent above ceiling accepted")
	}
}
