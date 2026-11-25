// Package config loads, layers and validates GoStack application configuration.
//
// Precedence: defaults < config file < environment variables < explicit CLI overrides.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is an immutable snapshot after Load; callers must not mutate it.
type Config struct {
	Profile     string          `yaml:"profile" json:"profile"`
	Environment string          `yaml:"environment" json:"environment"` // development | production
	HTTP        HTTPConfig      `yaml:"http" json:"http"`
	Database    DatabaseConfig  `yaml:"database" json:"database"`
	Cache       CacheConfig     `yaml:"cache" json:"cache"`
	Queue       QueueConfig     `yaml:"queue" json:"queue"`
	Backup      BackupConfig    `yaml:"backup" json:"backup"`
	Log         LogConfig       `yaml:"log" json:"log"`
	Kits        []string        `yaml:"kits" json:"kits"`
	Game        GameConfig      `yaml:"game" json:"game"`
	Auth        AuthConfig      `yaml:"auth" json:"auth"`
	Billing     BillingConfig   `yaml:"billing" json:"billing"`
	Storage     StorageConfig   `yaml:"storage" json:"storage"`
	RateLimit   RateLimitConfig `yaml:"rate_limit" json:"rate_limit"`
	Valkey      ValkeyConfig    `yaml:"valkey" json:"valkey"`
}

// RateLimitConfig describes how request budgets (game auth/score/events, web
// auth, redeem, social, match) are enforced.
//
// Driver local: process-local token buckets; with N instances the effective
// budget is up to N times the configured one. Profile scale refuses to start
// with it until the operator acknowledges that with accept_per_instance: true,
// so a local limiter is never mistaken for a global one (PRD §3).
//
// Driver valkey (profile scale): one budget shared by all instances (token bucket in a
// Valkey script, server clock). Each check is bounded by Timeout; when Valkey
// fails each limiter applies its documented failure policy (docs/adr/0004).
type RateLimitConfig struct {
	Driver            string        `yaml:"driver" json:"driver"` // local | valkey
	AcceptPerInstance bool          `yaml:"accept_per_instance" json:"accept_per_instance"`
	Timeout           time.Duration `yaml:"timeout" json:"timeout"`
}

// ValkeyConfig is the Valkey server used by cache.driver valkey and
// rate_limit.driver valkey (profile scale). A single primary is supported;
// Sentinel, Cluster and TLS are not implemented.
type ValkeyConfig struct {
	Address     string `yaml:"address" json:"address"` // host:port
	Username    string `yaml:"username" json:"username"`
	PasswordRef string `yaml:"password_ref" json:"password_ref"` // env:NAME or file:/path
	DB          int    `yaml:"db" json:"db"`
	// KeyPrefix namespaces every key of this application (required, e.g. "mygame:").
	KeyPrefix   string        `yaml:"key_prefix" json:"key_prefix"`
	DialTimeout time.Duration `yaml:"dial_timeout" json:"dial_timeout"`
	PoolSize    int           `yaml:"pool_size" json:"pool_size"`
}

// UsesValkey reports whether any driver needs the Valkey connection.
func (c Config) UsesValkey() bool {
	return c.Cache.Driver == "valkey" || c.RateLimit.Driver == "valkey"
}

// AuthConfig groups optional end-user authentication modules.
type AuthConfig struct {
	Web WebAuthConfig `yaml:"web" json:"web"`
}

// WebAuthConfig configures password login with cookie sessions (modules/webauth).
// Disabled by default; API keys and GameKit sessions do not depend on it.
type WebAuthConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// SessionTTL is the absolute session lifetime.
	SessionTTL time.Duration `yaml:"session_ttl" json:"session_ttl"`
	// IdleTimeout expires a session that has not been used for this long.
	IdleTimeout time.Duration `yaml:"idle_timeout" json:"idle_timeout"`
	// AllowSignup enables POST /v1/auth/signup.
	AllowSignup bool `yaml:"allow_signup" json:"allow_signup"`
	// AllowedOrigins are extra origins (scheme://host[:port]) accepted for unsafe
	// requests besides the request's own host.
	AllowedOrigins []string `yaml:"allowed_origins" json:"allowed_origins"`
	// Password-check budgets per minute (process-local).
	IPRatePerMinute      int `yaml:"ip_rate_per_minute" json:"ip_rate_per_minute"`
	AccountRatePerMinute int `yaml:"account_rate_per_minute" json:"account_rate_per_minute"`
	// MaxConcurrentHashes bounds simultaneous Argon2id computations (19 MiB each).
	MaxConcurrentHashes int `yaml:"max_concurrent_hashes" json:"max_concurrent_hashes"`
}

type HTTPConfig struct {
	Listen          string        `yaml:"listen" json:"listen"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes" json:"max_body_bytes"`
	MaxHeaderBytes  int           `yaml:"max_header_bytes" json:"max_header_bytes"`
	RequestTimeout  time.Duration `yaml:"request_timeout" json:"request_timeout"`
	MaxConcurrent   int           `yaml:"max_concurrent" json:"max_concurrent"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" json:"shutdown_timeout"`
	// TrustedProxies lists CIDRs whose CF-Connecting-IP / X-Forwarded-For headers are trusted.
	TrustedProxies []string `yaml:"trusted_proxies" json:"trusted_proxies"`
	// InternalListen serves /metrics and pprof; empty disables them.
	InternalListen string `yaml:"internal_listen" json:"internal_listen"`
}

type DatabaseConfig struct {
	Driver           string        `yaml:"driver" json:"driver"`
	Path             string        `yaml:"path" json:"path"`
	AnalyticsPath    string        `yaml:"analytics_path" json:"analytics_path"`
	ReadConnections  int           `yaml:"read_connections" json:"read_connections"`
	WriteConnections int           `yaml:"write_connections" json:"write_connections"`
	Synchronous      string        `yaml:"synchronous" json:"synchronous"`
	BusyTimeout      time.Duration `yaml:"busy_timeout" json:"busy_timeout"`
	CacheSizeKiB     int           `yaml:"cache_size_kib" json:"cache_size_kib"`
	MmapSize         int64         `yaml:"mmap_size" json:"mmap_size"`
	// WALAutocheckpoint in pages; 0 disables (only valid when Litestream manages checkpoints).
	WALAutocheckpoint int `yaml:"wal_autocheckpoint" json:"wal_autocheckpoint"`
	// WriteQueue bounds how many callers may wait for the single write connection.
	WriteQueue int `yaml:"write_queue" json:"write_queue"`
	// DSNRef is the PostgreSQL connection string as a secret reference
	// (env:NAME or file:/path); profile scale only.
	DSNRef string `yaml:"dsn_ref" json:"dsn_ref"`
	// AnalyticsDSNRef is the PostgreSQL connection for analytics (profile
	// scale). Analytics tables live in the schema gostack_analytics; empty
	// means the main database (same server, separate schema and pool).
	AnalyticsDSNRef string `yaml:"analytics_dsn_ref" json:"analytics_dsn_ref"`
	// MaxOpenConnections bounds the PostgreSQL write pool per instance
	// (profile scale; read_connections bounds the read pool).
	MaxOpenConnections int `yaml:"max_open_connections" json:"max_open_connections"`
	// AcceptNormalSyncRisk must be true to run synchronous=NORMAL in production
	// (committed transactions may be lost on power failure).
	AcceptNormalSyncRisk bool `yaml:"accept_normal_sync_risk" json:"accept_normal_sync_risk"`
}

// CacheConfig selects App.Cache: memory (bounded in-process LRU, max_bytes) or
// valkey (shared; requires valkey.address). A cache never holds business facts.
type CacheConfig struct {
	Driver   string `yaml:"driver" json:"driver"`
	MaxBytes int64  `yaml:"max_bytes" json:"max_bytes"`
	// MaxValueBytes bounds one value (both drivers).
	MaxValueBytes int64 `yaml:"max_value_bytes" json:"max_value_bytes"`
	// Timeout bounds each Valkey cache operation.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
}

type QueueConfig struct {
	Driver       string        `yaml:"driver" json:"driver"`
	PollInterval time.Duration `yaml:"poll_interval" json:"poll_interval"`
	Lease        time.Duration `yaml:"lease" json:"lease"`
	Workers      int           `yaml:"workers" json:"workers"`
}

type BackupConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Driver  string `yaml:"driver" json:"driver"`
}

type LogConfig struct {
	Level  string `yaml:"level" json:"level"`
	Format string `yaml:"format" json:"format"` // json | text
}

type GameConfig struct {
	SessionTTL          time.Duration `yaml:"session_ttl" json:"session_ttl"`
	SaveMaxBytes        int           `yaml:"save_max_bytes" json:"save_max_bytes"`
	SaveSlotsPerUser    int           `yaml:"save_slots_per_player" json:"save_slots_per_player"`
	SaveHistory         int           `yaml:"save_history" json:"save_history"`
	AnalyticsBuffer     int           `yaml:"analytics_buffer" json:"analytics_buffer"`
	AnalyticsRetainDays int           `yaml:"analytics_retain_days" json:"analytics_retain_days"`
	// Per-IP request budget for unauthenticated auth endpoints (requests per minute).
	AuthRatePerMinute int `yaml:"auth_rate_per_minute" json:"auth_rate_per_minute"`
	// Per-player budget for score submissions (per minute).
	ScoreRatePerMinute int `yaml:"score_rate_per_minute" json:"score_rate_per_minute"`
	// Platforms maps platform name to verifier configuration.
	Platforms map[string]PlatformConfig `yaml:"platforms" json:"platforms"`
	// Modules enables optional GameKit feature packages (see modules.go).
	Modules    []string         `yaml:"modules" json:"modules"`
	Ops        OpsConfig        `yaml:"ops" json:"ops"`
	Social     SocialConfig     `yaml:"social" json:"social"`
	Match      MatchConfig      `yaml:"match" json:"match"`
	Commerce   CommerceConfig   `yaml:"commerce" json:"commerce"`
	Realtime   RealtimeConfig   `yaml:"realtime" json:"realtime"`
	Settlement SettlementConfig `yaml:"settlement" json:"settlement"`
}

type PlatformConfig struct {
	// Per game_id credentials. AppSecret must be a secret reference (env:NAME or file:/path).
	Apps map[string]PlatformApp `yaml:"apps" json:"apps"`
	// Endpoint override (tests only).
	Endpoint string `yaml:"endpoint" json:"endpoint"`
}

type PlatformApp struct {
	AppID        string `yaml:"app_id" json:"app_id"`
	AppSecretRef string `yaml:"app_secret_ref" json:"app_secret_ref"`
}

// Defaults returns the Lite profile defaults.
func Defaults() Config {
	return Config{
		Profile:     "lite",
		Environment: "development",
		HTTP: HTTPConfig{
			Listen:          "127.0.0.1:8080",
			MaxBodyBytes:    1 << 20,
			MaxHeaderBytes:  32 << 10,
			RequestTimeout:  10 * time.Second,
			MaxConcurrent:   512,
			ShutdownTimeout: 20 * time.Second,
		},
		Database: DatabaseConfig{
			Driver:            "sqlite",
			Path:              "data/app.db",
			AnalyticsPath:     "data/analytics.db",
			ReadConnections:   2,
			WriteConnections:  1,
			Synchronous:       "FULL",
			BusyTimeout:       5 * time.Second,
			CacheSizeKiB:      4096,
			MmapSize:          0,
			WALAutocheckpoint: 1000,
			WriteQueue:        1024,
		},
		Cache:     CacheConfig{Driver: "memory", MaxBytes: 32 << 20, MaxValueBytes: 1 << 20, Timeout: 100 * time.Millisecond},
		RateLimit: RateLimitConfig{Driver: "local", Timeout: 50 * time.Millisecond},
		Valkey:    ValkeyConfig{DialTimeout: time.Second, PoolSize: 16},
		Queue:     QueueConfig{Driver: "sqlite", PollInterval: 500 * time.Millisecond, Lease: 30 * time.Second, Workers: 2},
		Backup:    BackupConfig{Enabled: false, Driver: "litestream"},
		Log:       LogConfig{Level: "info", Format: "json"},
		Game: GameConfig{
			SessionTTL:          24 * time.Hour,
			SaveMaxBytes:        64 << 10,
			SaveSlotsPerUser:    5,
			SaveHistory:         10,
			AnalyticsBuffer:     10000,
			AnalyticsRetainDays: 30,
			AuthRatePerMinute:   30,
			ScoreRatePerMinute:  60,
			Settlement:          SettlementConfig{Interval: 5 * time.Minute},
		},
		Auth: AuthConfig{Web: WebAuthConfig{
			SessionTTL:           7 * 24 * time.Hour,
			IdleTimeout:          24 * time.Hour,
			AllowSignup:          true,
			IPRatePerMinute:      30,
			AccountRatePerMinute: 10,
			MaxConcurrentHashes:  2,
		}},
	}
}

// Load builds a Config from defaults, an optional YAML file, environment and overrides.
// Unknown YAML keys are rejected so typos do not silently fall back to defaults.
func Load(path string, env func(string) (string, bool), overrides map[string]string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return cfg, fmt.Errorf("open config: %w", err)
		}
		defer f.Close()
		dec := yaml.NewDecoder(f)
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
		// Relative data paths are resolved against the config file directory.
		base := filepath.Dir(path)
		cfg.Database.Path = resolve(base, cfg.Database.Path)
		cfg.Database.AnalyticsPath = resolve(base, cfg.Database.AnalyticsPath)
		cfg.Storage.Local.Root = resolve(base, cfg.Storage.Local.Root)
		if a := cfg.Game.Commerce.Apple; a != nil {
			a.RootCAFile = resolve(base, a.RootCAFile)
		}
	}
	if env == nil {
		env = os.LookupEnv
	}
	for key, set := range fields(&cfg) {
		if v, ok := env("GOSTACK_" + key); ok {
			if err := set(v); err != nil {
				return cfg, fmt.Errorf("env GOSTACK_%s: %w", key, err)
			}
		}
	}
	for key, v := range overrides {
		set, ok := fields(&cfg)[strings.ToUpper(strings.ReplaceAll(key, ".", "_"))]
		if !ok {
			return cfg, fmt.Errorf("unknown override %q", key)
		}
		if err := set(v); err != nil {
			return cfg, fmt.Errorf("override %s: %w", key, err)
		}
	}
	return cfg, cfg.Validate()
}

func resolve(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// fields exposes the scalar settings that may be overridden from env/CLI.
func fields(c *Config) map[string]func(string) error {
	str := func(p *string) func(string) error { return func(v string) error { *p = v; return nil } }
	integer := func(p *int) func(string) error {
		return func(v string) error { n, err := strconv.Atoi(v); *p = n; return err }
	}
	i64 := func(p *int64) func(string) error {
		return func(v string) error { n, err := strconv.ParseInt(v, 10, 64); *p = n; return err }
	}
	dur := func(p *time.Duration) func(string) error {
		return func(v string) error { d, err := time.ParseDuration(v); *p = d; return err }
	}
	boolean := func(p *bool) func(string) error {
		return func(v string) error { b, err := strconv.ParseBool(v); *p = b; return err }
	}
	return map[string]func(string) error{
		"PROFILE":                          str(&c.Profile),
		"ENVIRONMENT":                      str(&c.Environment),
		"HTTP_LISTEN":                      str(&c.HTTP.Listen),
		"HTTP_INTERNAL_LISTEN":             str(&c.HTTP.InternalListen),
		"HTTP_MAX_BODY_BYTES":              i64(&c.HTTP.MaxBodyBytes),
		"HTTP_REQUEST_TIMEOUT":             dur(&c.HTTP.RequestTimeout),
		"HTTP_MAX_CONCURRENT":              integer(&c.HTTP.MaxConcurrent),
		"DATABASE_DRIVER":                  str(&c.Database.Driver),
		"DATABASE_PATH":                    str(&c.Database.Path),
		"DATABASE_ANALYTICS_PATH":          str(&c.Database.AnalyticsPath),
		"DATABASE_READ_CONNECTIONS":        integer(&c.Database.ReadConnections),
		"DATABASE_SYNCHRONOUS":             str(&c.Database.Synchronous),
		"DATABASE_BUSY_TIMEOUT":            dur(&c.Database.BusyTimeout),
		"DATABASE_WAL_AUTOCHECKPOINT":      integer(&c.Database.WALAutocheckpoint),
		"DATABASE_DSN_REF":                 str(&c.Database.DSNRef),
		"DATABASE_ANALYTICS_DSN_REF":       str(&c.Database.AnalyticsDSNRef),
		"DATABASE_MAX_OPEN_CONNECTIONS":    integer(&c.Database.MaxOpenConnections),
		"QUEUE_DRIVER":                     str(&c.Queue.Driver),
		"RATE_LIMIT_ACCEPT_PER_INSTANCE":   boolean(&c.RateLimit.AcceptPerInstance),
		"RATE_LIMIT_DRIVER":                str(&c.RateLimit.Driver),
		"CACHE_DRIVER":                     str(&c.Cache.Driver),
		"CACHE_MAX_BYTES":                  i64(&c.Cache.MaxBytes),
		"VALKEY_ADDRESS":                   str(&c.Valkey.Address),
		"VALKEY_PASSWORD_REF":              str(&c.Valkey.PasswordRef),
		"VALKEY_KEY_PREFIX":                str(&c.Valkey.KeyPrefix),
		"QUEUE_WORKERS":                    integer(&c.Queue.Workers),
		"BACKUP_ENABLED":                   boolean(&c.Backup.Enabled),
		"LOG_LEVEL":                        str(&c.Log.Level),
		"LOG_FORMAT":                       str(&c.Log.Format),
		"GAME_AUTH_RATE_PER_MINUTE":        integer(&c.Game.AuthRatePerMinute),
		"GAME_SCORE_RATE_PER_MINUTE":       integer(&c.Game.ScoreRatePerMinute),
		"AUTH_WEB_ENABLED":                 boolean(&c.Auth.Web.Enabled),
		"AUTH_WEB_SESSION_TTL":             dur(&c.Auth.Web.SessionTTL),
		"AUTH_WEB_IDLE_TIMEOUT":            dur(&c.Auth.Web.IdleTimeout),
		"AUTH_WEB_ALLOW_SIGNUP":            boolean(&c.Auth.Web.AllowSignup),
		"AUTH_WEB_IP_RATE_PER_MINUTE":      integer(&c.Auth.Web.IPRatePerMinute),
		"AUTH_WEB_ACCOUNT_RATE_PER_MINUTE": integer(&c.Auth.Web.AccountRatePerMinute),
		"AUTH_WEB_MAX_CONCURRENT_HASHES":   integer(&c.Auth.Web.MaxConcurrentHashes),
		"GAME_SETTLEMENT_INTERVAL":         dur(&c.Game.Settlement.Interval),
		"BILLING_ENABLED":                  boolean(&c.Billing.Enabled),
		"STORAGE_DRIVER":                   str(&c.Storage.Driver),
		"STORAGE_LOCAL_ROOT":               str(&c.Storage.Local.Root),
		"STORAGE_S3_ENDPOINT":              str(&c.Storage.S3.Endpoint),
		"STORAGE_S3_BUCKET":                str(&c.Storage.S3.Bucket),
	}
}

// Implemented capability matrix. Anything else is rejected rather than pretended.
var (
	implementedProfiles = map[string]bool{"lite": true, "scale": true}
	implementedDB       = map[string]bool{"sqlite": true, "postgres": true}
	implementedCache    = map[string]bool{"memory": true, "valkey": true}
	implementedLimiter  = map[string]bool{"local": true, "valkey": true}
	implementedQueue    = map[string]bool{"sqlite": true, "postgres": true}
	implementedBackup   = map[string]bool{"litestream": true}
	implementedKits     = map[string]bool{"game": true}
	implementedPlatform = map[string]bool{"wechat": true}
)

// Validate checks value ranges and rejects unimplemented or incompatible combinations.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !implementedProfiles[c.Profile] {
		add("profile %q is not implemented (implemented: lite, scale)", c.Profile)
	}
	if c.Environment != "development" && c.Environment != "production" {
		add("environment must be development or production")
	}
	if _, _, err := net.SplitHostPort(c.HTTP.Listen); err != nil {
		add("http.listen: %v", err)
	}
	if c.HTTP.InternalListen != "" {
		if _, _, err := net.SplitHostPort(c.HTTP.InternalListen); err != nil {
			add("http.internal_listen: %v", err)
		}
	}
	if c.HTTP.MaxBodyBytes < 1024 || c.HTTP.MaxBodyBytes > 64<<20 {
		add("http.max_body_bytes must be within [1KiB, 64MiB]")
	}
	if c.HTTP.RequestTimeout <= 0 || c.HTTP.MaxConcurrent <= 0 || c.HTTP.ShutdownTimeout <= 0 {
		add("http timeouts and max_concurrent must be positive")
	}
	if c.HTTP.MaxConcurrent > MaxConcurrentCeiling {
		add("http.max_concurrent must be <= %d", MaxConcurrentCeiling)
	}
	for _, cidr := range c.HTTP.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			add("http.trusted_proxies: %v", err)
		}
	}
	if !implementedDB[c.Database.Driver] {
		add("database.driver %q is not implemented", c.Database.Driver)
	}
	if c.Database.ReadConnections < 1 || c.Database.ReadConnections > 64 {
		add("database.read_connections must be within [1,64]")
	}
	if c.Database.WriteQueue < 1 {
		add("database.write_queue must be >= 1")
	}
	if c.Database.Driver == "postgres" {
		errs = append(errs, c.validatePostgres()...)
	} else {
		errs = append(errs, c.validateSQLite()...)
	}
	if !implementedCache[c.Cache.Driver] {
		add("cache.driver %q is not implemented (implemented: memory, valkey)", c.Cache.Driver)
	}
	if !implementedLimiter[c.RateLimit.Driver] {
		add("rate_limit.driver %q is not implemented (implemented: local, valkey)", c.RateLimit.Driver)
	}
	errs = append(errs, c.validateValkey()...)
	if !implementedQueue[c.Queue.Driver] {
		add("queue.driver %q is not implemented", c.Queue.Driver)
	}
	if c.Queue.Workers < 0 || c.Queue.Lease < time.Second || c.Queue.PollInterval <= 0 {
		add("queue.workers >= 0, queue.lease >= 1s, queue.poll_interval > 0 required")
	}
	if c.Backup.Enabled && !implementedBackup[c.Backup.Driver] {
		add("backup.driver %q is not implemented", c.Backup.Driver)
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		add("log.format must be json or text")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be debug|info|warn|error")
	}
	for _, k := range c.Kits {
		if !implementedKits[k] {
			add("kit %q is not implemented (implemented: game)", k)
		}
	}
	g := c.Game
	if g.SessionTTL < time.Minute || g.SaveMaxBytes < 1024 || g.SaveSlotsPerUser < 1 || g.SaveHistory < 0 ||
		g.AnalyticsBuffer < 1 || g.AnalyticsRetainDays < 1 || g.AuthRatePerMinute < 1 || g.ScoreRatePerMinute < 1 {
		add("game settings out of range")
	}
	for name, p := range g.Platforms {
		if !implementedPlatform[name] {
			add("game.platforms.%s is not implemented", name)
		}
		for gameID, app := range p.Apps {
			if app.AppID == "" || !IsSecretRef(app.AppSecretRef) {
				add("game.platforms.%s.apps.%s: app_id and app_secret_ref (env:NAME or file:/path) required", name, gameID)
			}
		}
	}
	if w := c.Auth.Web; w.Enabled {
		if w.SessionTTL < 5*time.Minute || w.SessionTTL > 90*24*time.Hour {
			add("auth.web.session_ttl must be within [5m, 2160h]")
		}
		if w.IdleTimeout < time.Minute || w.IdleTimeout > w.SessionTTL {
			add("auth.web.idle_timeout must be within [1m, session_ttl]")
		}
		if w.IPRatePerMinute < 1 || w.AccountRatePerMinute < 1 {
			add("auth.web.ip_rate_per_minute and account_rate_per_minute must be >= 1")
		}
		if w.MaxConcurrentHashes < 1 || w.MaxConcurrentHashes > 64 {
			add("auth.web.max_concurrent_hashes must be within [1,64] (each hash uses 19 MiB)")
		}
		for _, o := range w.AllowedOrigins {
			if !validOrigin(o) {
				add("auth.web.allowed_origins: %q must be scheme://host[:port] without path", o)
			} else if c.Environment == "production" && !strings.HasPrefix(o, "https://") {
				add("production: auth.web.allowed_origins entry %q must use https", o)
			}
		}
	}
	if c.Environment == "production" {
		if !strings.HasPrefix(c.HTTP.Listen, "127.0.0.1:") && !strings.HasPrefix(c.HTTP.Listen, "[::1]:") && len(c.HTTP.TrustedProxies) == 0 {
			add("production: http.listen is public without trusted_proxies; bind loopback or protect the origin explicitly")
		}
		if c.Database.Driver == "sqlite" && strings.ToUpper(c.Database.Synchronous) != "FULL" && !c.Database.AcceptNormalSyncRisk {
			add("production: database.synchronous=NORMAL requires database.accept_normal_sync_risk=true")
		}
	}
	errs = append(errs, c.validateProfile()...)
	errs = append(errs, c.validateModules()...)
	return errors.Join(errs...)
}

func validOrigin(o string) bool {
	u, err := url.Parse(o)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" &&
		o == u.Scheme+"://"+u.Host
}

// IsSecretRef reports whether s is a supported secret reference.
func IsSecretRef(s string) bool {
	return strings.HasPrefix(s, "env:") && len(s) > 4 || strings.HasPrefix(s, "file:") && len(s) > 5
}

// ResolveSecret reads a secret reference. Secret values are never logged.
func ResolveSecret(ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "env:"):
		v, ok := os.LookupEnv(ref[4:])
		if !ok || v == "" {
			return "", fmt.Errorf("secret env %s is not set", ref[4:])
		}
		return v, nil
	case strings.HasPrefix(ref, "file:"):
		b, err := os.ReadFile(ref[5:])
		if err != nil {
			return "", fmt.Errorf("read secret file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("unsupported secret reference")
}
