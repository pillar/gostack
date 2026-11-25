package config_test

import (
	"testing"
	"time"

	"github.com/pillar/gostack/core/config"
)

func TestKitSettingsValidated(t *testing.T) {
	ok := config.Defaults()
	ok.Kits = []string{"agent", "web"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("defaults with agent+web rejected: %v", err)
	}
	for name, mut := range map[string]func(*config.Config){
		"step timeout >= lease":   func(c *config.Config) { c.Agent.StepTimeout = c.Queue.Lease },
		"no running slots":        func(c *config.Config) { c.Agent.MaxRunningPerTenant = 0 },
		"default > max timeout":   func(c *config.Config) { c.Agent.DefaultTimeout = 48 * time.Hour },
		"default > max steps":     func(c *config.Config) { c.Agent.DefaultMaxSteps = c.Agent.MaxSteps + 1 },
		"inline output too large": func(c *config.Config) { c.Agent.InlineOutputBytes = 1 << 20 },
		"webhook timeout >= lease": func(c *config.Config) {
			c.Web.WebhookTimeout = c.Queue.Lease
		},
		"webhook attempts": func(c *config.Config) { c.Web.WebhookMaxAttempts = 0 },
		"private webhooks in production": func(c *config.Config) {
			c.Environment = "production"
			c.Web.WebhookAllowPrivateNetworks = true
		},
	} {
		c := config.Defaults()
		c.Kits = []string{"agent", "web"}
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Settings of kits that are not enabled are not checked.
	c := config.Defaults()
	c.Agent.MaxRunningPerTenant = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled kit settings validated: %v", err)
	}
}
