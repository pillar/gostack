package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"

	"go.yaml.in/yaml/v3"
)

// MaxConcurrentCeiling is the upper bound for http.max_concurrent.
const MaxConcurrentCeiling = 100_000

// hotKeys are the settings a running process applies on reload (SIGHUP). Every
// other key is restart-required: a reload reports its change but keeps the old
// value. Keys are dotted YAML paths.
//
// The list is deliberately short: each entry must have a consumer that actually
// retunes at runtime (log level filter, HTTP in-flight gate, rate limiters).
// Storage, listeners, timeouts, kits and auth/session settings shape objects
// built at startup and are restart-only.
var hotKeys = []string{
	"log.level",
	"http.max_concurrent",
	"game.auth_rate_per_minute",
	"game.score_rate_per_minute",
	"auth.web.ip_rate_per_minute",
	"auth.web.account_rate_per_minute",
}

// HotKeys returns the hot-reloadable keys, sorted.
func HotKeys() []string {
	out := slices.Clone(hotKeys)
	sort.Strings(out)
	return out
}

// IsHot reports whether a dotted key is applied on reload.
func IsHot(key string) bool { return slices.Contains(hotKeys, key) }

// WithHot returns cur with only the hot keys taken from next. Restart-only
// values in cur are kept so the running snapshot always describes what the
// process actually uses.
func WithHot(cur, next Config) Config {
	out := cur
	out.Log.Level = next.Log.Level
	out.HTTP.MaxConcurrent = next.HTTP.MaxConcurrent
	out.Game.AuthRatePerMinute = next.Game.AuthRatePerMinute
	out.Game.ScoreRatePerMinute = next.Game.ScoreRatePerMinute
	out.Auth.Web.IPRatePerMinute = next.Auth.Web.IPRatePerMinute
	out.Auth.Web.AccountRatePerMinute = next.Auth.Web.AccountRatePerMinute
	return out
}

// Change is one differing key between two configurations. Values are rendered
// as JSON (durations as strings); secrets are never stored in Config (only references), so printing is safe.
type Change struct {
	Key string `json:"key"`
	Old string `json:"old"`
	New string `json:"new"`
	Hot bool   `json:"hot"`
}

// Diff lists the keys whose values differ between a and b, sorted by key.
func Diff(a, b Config) []Change {
	fa, fb := flatten(a), flatten(b)
	keys := map[string]bool{}
	for k := range fa {
		keys[k] = true
	}
	for k := range fb {
		keys[k] = true
	}
	var out []Change
	for k := range keys {
		if fa[k] != fb[k] {
			out = append(out, Change{Key: k, Old: fa[k], New: fb[k], Hot: IsHot(k)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// flatten renders a config as dotted-key → JSON value. Struct fields recurse;
// maps and lists are compared as whole values (they are restart-only).
func flatten(c Config) map[string]string {
	// YAML rather than JSON so durations render as "10s", matching the config file.
	b, err := yaml.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("config: marshal: %v", err)) // plain data; cannot fail
	}
	var m map[string]any
	_ = yaml.Unmarshal(b, &m)
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if obj, ok := v.(map[string]any); ok && !isMapKey(prefix) {
			for k, child := range obj {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, child)
			}
			return
		}
		raw, _ := json.Marshal(v)
		out[prefix] = string(raw)
	}
	walk("", m)
	return out
}

// isMapKey marks config paths that are Go maps (compared as a single value).
func isMapKey(p string) bool { return p == "game.platforms" }

// Snapshot holds the current configuration and is safe for concurrent use.
// Readers always see a complete, validated Config.
type Snapshot struct{ p atomic.Pointer[Config] }

func NewSnapshot(c Config) *Snapshot {
	s := &Snapshot{}
	s.p.Store(&c)
	return s
}

// Load returns the current configuration (a copy; do not mutate shared slices/maps).
func (s *Snapshot) Load() Config { return *s.p.Load() }

// Store replaces the configuration atomically.
func (s *Snapshot) Store(c Config) { s.p.Store(&c) }
