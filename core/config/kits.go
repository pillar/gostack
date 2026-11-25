package config

import (
	"fmt"
	"slices"
	"time"
)

// AgentConfig configures AgentKit (kits: [agent]); see docs/api/agent.md.
type AgentConfig struct {
	// MaxRunningPerTenant bounds runs in status running per tenant (all workflows).
	MaxRunningPerTenant int `yaml:"max_running_per_tenant" json:"max_running_per_tenant"`
	// DefaultTimeout / MaxTimeout bound a run's deadline (created_at + timeout).
	DefaultTimeout time.Duration `yaml:"default_timeout" json:"default_timeout"`
	MaxTimeout     time.Duration `yaml:"max_timeout" json:"max_timeout"`
	// StepTimeout bounds one step; it must be shorter than queue.lease because
	// a run executes inside a leased job.
	StepTimeout time.Duration `yaml:"step_timeout" json:"step_timeout"`
	// DefaultMaxSteps / MaxSteps bound the step budget of a run.
	DefaultMaxSteps int `yaml:"default_max_steps" json:"default_max_steps"`
	MaxSteps        int `yaml:"max_steps" json:"max_steps"`
	// InlineOutputBytes: step outputs above this size go to object storage
	// (storage.driver required, else the step fails).
	InlineOutputBytes int `yaml:"inline_output_bytes" json:"inline_output_bytes"`
	// CancelPoll is how often a running step checks for a cancel request.
	CancelPoll time.Duration `yaml:"cancel_poll" json:"cancel_poll"`
}

// WebConfig configures WebKit (kits: [web]); see docs/api/web.md.
type WebConfig struct {
	// PublicMaxAge is the Cache-Control max-age of public pages, sitemap and robots.
	PublicMaxAge time.Duration `yaml:"public_max_age" json:"public_max_age"`
	// WebhookTimeout bounds one delivery attempt; WebhookMaxAttempts bounds retries.
	WebhookTimeout     time.Duration `yaml:"webhook_timeout" json:"webhook_timeout"`
	WebhookMaxAttempts int           `yaml:"webhook_max_attempts" json:"webhook_max_attempts"`
	// WebhookAllowPrivateNetworks permits webhook URLs resolving to loopback,
	// private or link-local addresses (SSRF guard; off by default).
	WebhookAllowPrivateNetworks bool `yaml:"webhook_allow_private_networks" json:"webhook_allow_private_networks"`
}

func defaultAgent() AgentConfig {
	return AgentConfig{MaxRunningPerTenant: 8, DefaultTimeout: time.Hour, MaxTimeout: 24 * time.Hour,
		StepTimeout: 20 * time.Second, DefaultMaxSteps: 50, MaxSteps: 1000, InlineOutputBytes: 16 << 10,
		CancelPoll: time.Second}
}

func defaultWeb() WebConfig {
	return WebConfig{PublicMaxAge: time.Minute, WebhookTimeout: 10 * time.Second, WebhookMaxAttempts: 8}
}

func (c Config) validateKits() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if slices.Contains(c.Kits, "agent") {
		a := c.Agent
		if a.MaxRunningPerTenant < 1 || a.MaxRunningPerTenant > 10000 {
			add("agent.max_running_per_tenant must be within [1,10000]")
		}
		if a.DefaultTimeout < time.Second || a.MaxTimeout < a.DefaultTimeout || a.MaxTimeout > 30*24*time.Hour {
			add("agent.default_timeout must be >= 1s and <= agent.max_timeout <= 720h")
		}
		if a.StepTimeout < 100*time.Millisecond || a.StepTimeout >= c.Queue.Lease {
			add("agent.step_timeout must be >= 100ms and shorter than queue.lease (%s)", c.Queue.Lease)
		}
		if a.DefaultMaxSteps < 1 || a.MaxSteps < a.DefaultMaxSteps || a.MaxSteps > 100000 {
			add("agent.default_max_steps must be >= 1 and <= agent.max_steps <= 100000")
		}
		if a.InlineOutputBytes < 1024 || a.InlineOutputBytes > 60<<10 {
			add("agent.inline_output_bytes must be within [1KiB, 60KiB]")
		}
		if a.CancelPoll < 10*time.Millisecond || a.CancelPoll > time.Minute {
			add("agent.cancel_poll must be within [10ms, 1m]")
		}
	}
	if slices.Contains(c.Kits, "web") {
		w := c.Web
		if w.PublicMaxAge < 0 || w.PublicMaxAge > 24*time.Hour {
			add("web.public_max_age must be within [0, 24h]")
		}
		if w.WebhookTimeout < time.Second || w.WebhookTimeout >= c.Queue.Lease {
			add("web.webhook_timeout must be >= 1s and shorter than queue.lease (%s)", c.Queue.Lease)
		}
		if w.WebhookMaxAttempts < 1 || w.WebhookMaxAttempts > 50 {
			add("web.webhook_max_attempts must be within [1,50]")
		}
		if c.Environment == "production" && w.WebhookAllowPrivateNetworks {
			add("production: web.webhook_allow_private_networks enables SSRF to internal addresses; not allowed")
		}
	}
	return errs
}
