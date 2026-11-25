package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// validateSQLite checks the SQLite (profile lite) storage settings.
func (c Config) validateSQLite() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	d := c.Database
	if d.Path == "" || d.AnalyticsPath == "" {
		add("database.path and database.analytics_path are required")
	}
	if d.Path == d.AnalyticsPath {
		add("database.analytics_path must differ from database.path (analytics is isolated from critical writes)")
	}
	if d.WriteConnections != 1 {
		add("database.write_connections must be 1 for sqlite (single writer)")
	}
	switch strings.ToUpper(d.Synchronous) {
	case "FULL", "NORMAL":
	default:
		add("database.synchronous must be FULL or NORMAL")
	}
	if d.BusyTimeout < 0 || d.CacheSizeKiB < 0 || d.MmapSize < 0 || d.WALAutocheckpoint < 0 {
		add("database numeric settings must be non-negative")
	}
	if d.WALAutocheckpoint == 0 && !(c.Backup.Enabled && c.Backup.Driver == "litestream") {
		add("database.wal_autocheckpoint=0 is only allowed when litestream backup manages checkpoints")
	}
	if d.DSNRef != "" || d.AnalyticsDSNRef != "" || d.MaxOpenConnections != 0 {
		add("database.dsn_ref, analytics_dsn_ref and max_open_connections apply to driver postgres only")
	}
	return errs
}

// validatePostgres checks the PostgreSQL (profile scale) storage settings.
func (c Config) validatePostgres() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	d := c.Database
	if !IsSecretRef(d.DSNRef) {
		add("database.dsn_ref (env:NAME or file:/path) is required for driver postgres")
	}
	if d.AnalyticsDSNRef != "" && !IsSecretRef(d.AnalyticsDSNRef) {
		add("database.analytics_dsn_ref must be env:NAME or file:/path")
	}
	if d.MaxOpenConnections < 0 || d.MaxOpenConnections > 500 {
		add("database.max_open_connections must be within [0,500] (0 = default 10)")
	}
	if d.AcceptNormalSyncRisk {
		add("database.accept_normal_sync_risk applies to sqlite only (PostgreSQL durability is configured on the server)")
	}
	return errs
}

// validateProfile rejects driver/profile combinations that are not implemented
// or not safe (PRD §3): lite is single-instance SQLite; scale is PostgreSQL with
// possibly several application instances.
func (c Config) validateProfile() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	switch c.Profile {
	case "lite":
		if c.Database.Driver != "sqlite" {
			add("profile lite requires database.driver sqlite (postgres requires profile scale)")
		}
		if c.Queue.Driver != "sqlite" {
			add("profile lite requires queue.driver sqlite")
		}
		if c.UsesValkey() {
			add("profile lite is single-instance: cache.driver and rate_limit.driver valkey require profile scale")
		}
	case "scale":
		if c.Database.Driver != "postgres" {
			add("profile scale requires database.driver postgres: several instances must never share one SQLite file")
		}
		if c.Queue.Driver != "postgres" {
			add("profile scale requires queue.driver postgres (the jobs table in PostgreSQL)")
		}
		if c.Backup.Enabled {
			add("backup.enabled (litestream) applies to SQLite only; back up PostgreSQL with pg_dump / PITR")
		}
		if c.HasGameModule("realtime") {
			add("game module realtime is not supported under profile scale: its hub is in-process and would split players across instances")
		}
		if c.Storage.Driver == "local" {
			add("storage.driver local is not supported under profile scale: objects on one instance's disk are invisible to the others (use s3)")
		}
		switch {
		case c.RateLimit.Driver == "local" && !c.RateLimit.AcceptPerInstance:
			add("profile scale: rate_limit.driver local is per instance; use rate_limit.driver valkey (global) or set rate_limit.accept_per_instance: true to acknowledge that limits multiply by the instance count")
		case c.RateLimit.Driver == "valkey" && c.RateLimit.AcceptPerInstance:
			add("rate_limit.accept_per_instance applies to driver local only (driver valkey is global)")
		}
	case "distributed":
		// Rejected by the implemented-profile check.
	}
	return errs
}

var keyPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// validateValkey checks the cache and rate limiter settings and the Valkey
// connection they share.
func (c Config) validateValkey() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.Cache.MaxBytes < 1<<20 || c.Cache.MaxValueBytes < 1 || c.Cache.MaxValueBytes > 16<<20 {
		add("cache.max_bytes must be >= 1MiB and cache.max_value_bytes within [1, 16MiB]")
	}
	if c.Cache.Driver == "memory" && c.Cache.MaxValueBytes > c.Cache.MaxBytes/8 {
		add("cache.max_value_bytes must be <= cache.max_bytes/8 for driver memory")
	}
	if c.Cache.Timeout < time.Millisecond || c.Cache.Timeout > 5*time.Second {
		add("cache.timeout must be within [1ms, 5s]")
	}
	if c.RateLimit.Timeout < time.Millisecond || c.RateLimit.Timeout > time.Second {
		add("rate_limit.timeout must be within [1ms, 1s] (it is added to every limited request when Valkey is slow)")
	}
	v := c.Valkey
	if !c.UsesValkey() {
		if v.Address != "" || v.PasswordRef != "" || v.Username != "" || v.KeyPrefix != "" || v.DB != 0 {
			add("valkey.* is set but neither cache.driver nor rate_limit.driver is valkey")
		}
		return errs
	}
	if _, _, err := net.SplitHostPort(v.Address); err != nil {
		add("valkey.address (host:port) is required for cache.driver/rate_limit.driver valkey: %v", err)
	}
	if v.PasswordRef != "" && !IsSecretRef(v.PasswordRef) {
		add("valkey.password_ref must be env:NAME or file:/path")
	}
	if v.Username != "" && v.PasswordRef == "" {
		add("valkey.username requires valkey.password_ref")
	}
	if !keyPrefixPattern.MatchString(v.KeyPrefix) {
		add(`valkey.key_prefix is required ([A-Za-z0-9_.:-]{1,64}, e.g. "mygame:") so applications sharing a server do not collide`)
	}
	if v.DB < 0 || v.DB > 255 {
		add("valkey.db must be within [0,255]")
	}
	if v.DialTimeout < 10*time.Millisecond || v.DialTimeout > 30*time.Second {
		add("valkey.dial_timeout must be within [10ms, 30s]")
	}
	if v.PoolSize < 1 || v.PoolSize > 1024 {
		add("valkey.pool_size must be within [1,1024]")
	}
	return errs
}
