package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Optional modules. Zero values mean "use the module's documented default".

// GameKit feature packages that can be listed in game.modules.
var implementedGameModules = []string{"ops", "experiments", "social", "match", "commerce", "realtime"}

type OpsConfig struct {
	RedeemPerPlayerPerMinute int `yaml:"redeem_per_player_per_minute" json:"redeem_per_player_per_minute"`
	RedeemPerIPPerMinute     int `yaml:"redeem_per_ip_per_minute" json:"redeem_per_ip_per_minute"`
}

type SocialConfig struct {
	MaxFriends           int              `yaml:"max_friends" json:"max_friends"`
	MaxPendingRequests   int              `yaml:"max_pending_requests" json:"max_pending_requests"`
	MaxBlocks            int              `yaml:"max_blocks" json:"max_blocks"`
	MaxInvites           int              `yaml:"max_invites" json:"max_invites"`
	MaxAcceptsPerInviter int              `yaml:"max_accepts_per_inviter" json:"max_accepts_per_inviter"`
	InviteWindow         time.Duration    `yaml:"invite_window" json:"invite_window"`
	InviterGrant         map[string]int64 `yaml:"inviter_grant" json:"inviter_grant"`
	InviteeGrant         map[string]int64 `yaml:"invitee_grant" json:"invitee_grant"`
	RequestsPerMinute    int              `yaml:"requests_per_minute" json:"requests_per_minute"`
}

type MatchConfig struct {
	LeaseTTL         time.Duration `yaml:"lease_ttl" json:"lease_ttl"`
	ClaimTimeout     time.Duration `yaml:"claim_timeout" json:"claim_timeout"`
	MatchInterval    time.Duration `yaml:"match_interval" json:"match_interval"`
	Retention        time.Duration `yaml:"retention" json:"retention"`
	MaxMatchBatch    int           `yaml:"max_match_batch" json:"max_match_batch"`
	TicketsPerMinute int           `yaml:"tickets_per_minute" json:"tickets_per_minute"`
}

type CommerceConfig struct {
	Apple *AppleIAPConfig `yaml:"apple" json:"apple"`
	// Ads maps the ad network name used in callback URLs to its verifier.
	Ads                      map[string]AdNetworkConfig `yaml:"ads" json:"ads"`
	AllowUnboundTransactions bool                       `yaml:"allow_unbound_transactions" json:"allow_unbound_transactions"`
	AdIntentTTL              time.Duration              `yaml:"ad_intent_ttl" json:"ad_intent_ttl"`
}

type AppleIAPConfig struct {
	// RootCAFile is the PEM of Apple Root CA - G3 downloaded and verified by the operator.
	RootCAFile string                  `yaml:"root_ca_file" json:"root_ca_file"`
	Apps       map[string]AppleAppConf `yaml:"apps" json:"apps"` // by game_id
}

type AppleAppConf struct {
	BundleID     string   `yaml:"bundle_id" json:"bundle_id"`
	Environments []string `yaml:"environments" json:"environments"`
}

type AdNetworkConfig struct {
	Kind string `yaml:"kind" json:"kind"` // admob | hmac
	// KeyURL overrides the AdMob verifier key URL (tests only).
	KeyURL string `yaml:"key_url" json:"key_url"`
	// SecretRefs maps game_id to an HMAC secret reference (env:NAME or file:/path); hmac only.
	SecretRefs map[string]string `yaml:"secret_refs" json:"secret_refs"`
	MaxSkew    time.Duration     `yaml:"max_skew" json:"max_skew"`
}

type RealtimeConfig struct {
	MaxConnections  int           `yaml:"max_connections" json:"max_connections"`
	MaxPerPlayer    int           `yaml:"max_per_player" json:"max_per_player"`
	MaxMessageBytes int64         `yaml:"max_message_bytes" json:"max_message_bytes"`
	InboundRate     float64       `yaml:"inbound_rate" json:"inbound_rate"`
	InboundBurst    int           `yaml:"inbound_burst" json:"inbound_burst"`
	SendQueue       int           `yaml:"send_queue" json:"send_queue"`
	MaxPayloadBytes int           `yaml:"max_payload_bytes" json:"max_payload_bytes"`
	ReplayBuffer    int           `yaml:"replay_buffer" json:"replay_buffer"`
	ReplayMaxBytes  int           `yaml:"replay_max_bytes" json:"replay_max_bytes"`
	ReplayMaxAge    time.Duration `yaml:"replay_max_age" json:"replay_max_age"`
	MaxStreams      int           `yaml:"max_streams" json:"max_streams"`
	PingInterval    time.Duration `yaml:"ping_interval" json:"ping_interval"`
	PongTimeout     time.Duration `yaml:"pong_timeout" json:"pong_timeout"`
	AuthTimeout     time.Duration `yaml:"auth_timeout" json:"auth_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout" json:"write_timeout"`
	SessionRecheck  time.Duration `yaml:"session_recheck" json:"session_recheck"`
	MessageTimeout  time.Duration `yaml:"message_timeout" json:"message_timeout"`
	// Bus connects the hubs of several instances: "none" (default; in-process
	// hub, profile lite) or "postgres" (LISTEN/NOTIFY + realtime tables on the
	// main database; required for the realtime module under profile scale).
	Bus string `yaml:"bus" json:"bus"`
	// BusQueue bounds Send/Broadcast calls waiting for the bus publisher.
	BusQueue int `yaml:"bus_queue" json:"bus_queue"`
}

// implementedRealtimeBuses are the accepted game.realtime.bus values ("" = none).
var implementedRealtimeBuses = map[string]bool{"": true, "none": true, "postgres": true}

// SettlementConfig controls automatic season settlement (boards with a settlement section).
type SettlementConfig struct {
	// Interval between scheduling passes; 0 disables automatic scheduling (admin/CLI still work).
	Interval time.Duration `yaml:"interval" json:"interval"`
}

type BillingConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

type StorageConfig struct {
	Driver         string        `yaml:"driver" json:"driver"` // "" (disabled) | local | s3
	MaxObjectBytes int64         `yaml:"max_object_bytes" json:"max_object_bytes"`
	Timeout        time.Duration `yaml:"timeout" json:"timeout"`
	Local          LocalStorage  `yaml:"local" json:"local"`
	S3             S3Storage     `yaml:"s3" json:"s3"`
}

type LocalStorage struct {
	Root string `yaml:"root" json:"root"`
	// PublicURL is where presigned URLs point; its path is mounted on the main listener.
	PublicURL        string `yaml:"public_url" json:"public_url"`
	PresignSecretRef string `yaml:"presign_secret_ref" json:"presign_secret_ref"`
}

type S3Storage struct {
	Endpoint           string `yaml:"endpoint" json:"endpoint"`
	Region             string `yaml:"region" json:"region"`
	Bucket             string `yaml:"bucket" json:"bucket"`
	AccessKeyIDRef     string `yaml:"access_key_id_ref" json:"access_key_id_ref"`
	SecretAccessKeyRef string `yaml:"secret_access_key_ref" json:"secret_access_key_ref"`
	PathStyle          bool   `yaml:"path_style" json:"path_style"`
	SignPayload        bool   `yaml:"sign_payload" json:"sign_payload"`
	TempDir            string `yaml:"temp_dir" json:"temp_dir"`
}

// HasGameModule reports whether a GameKit feature package is enabled.
func (c Config) HasGameModule(name string) bool {
	return slices.Contains(c.Kits, "game") && slices.Contains(c.Game.Modules, name)
}

func (c Config) validateModules() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if len(c.Game.Modules) > 0 && !slices.Contains(c.Kits, "game") {
		add("game.modules requires kits: [game]")
	}
	seen := map[string]bool{}
	for _, m := range c.Game.Modules {
		if !slices.Contains(implementedGameModules, m) {
			add("game.modules: %q is not implemented (implemented: %s)", m, strings.Join(implementedGameModules, ", "))
		}
		if seen[m] {
			add("game.modules: %q listed twice", m)
		}
		seen[m] = true
	}
	nonNeg := func(name string, v ...int64) {
		for _, x := range v {
			if x < 0 {
				add("%s settings must be non-negative", name)
				return
			}
		}
	}
	o := c.Game.Ops
	nonNeg("game.ops", int64(o.RedeemPerPlayerPerMinute), int64(o.RedeemPerIPPerMinute))
	s := c.Game.Social
	nonNeg("game.social", int64(s.MaxFriends), int64(s.MaxPendingRequests), int64(s.MaxBlocks), int64(s.MaxInvites),
		int64(s.MaxAcceptsPerInviter), int64(s.InviteWindow), int64(s.RequestsPerMinute))
	for _, grant := range []map[string]int64{s.InviterGrant, s.InviteeGrant} {
		for asset, n := range grant {
			if asset == "" || n <= 0 {
				add("game.social invite grants must map asset ids to positive integers")
			}
		}
	}
	m := c.Game.Match
	nonNeg("game.match", int64(m.LeaseTTL), int64(m.ClaimTimeout), int64(m.MatchInterval), int64(m.Retention),
		int64(m.MaxMatchBatch), int64(m.TicketsPerMinute))
	if c.Game.Settlement.Interval < 0 || (c.Game.Settlement.Interval > 0 && c.Game.Settlement.Interval < time.Minute) {
		add("game.settlement.interval must be 0 (disabled) or >= 1m")
	}
	r := c.Game.Realtime
	nonNeg("game.realtime", int64(r.MaxConnections), int64(r.MaxPerPlayer), r.MaxMessageBytes, int64(r.InboundBurst),
		int64(r.SendQueue), int64(r.MaxPayloadBytes), int64(r.ReplayBuffer), int64(r.ReplayMaxBytes), int64(r.ReplayMaxAge),
		int64(r.MaxStreams), int64(r.PingInterval), int64(r.PongTimeout), int64(r.AuthTimeout), int64(r.WriteTimeout),
		int64(r.SessionRecheck), int64(r.MessageTimeout), int64(r.BusQueue))
	if r.InboundRate < 0 {
		add("game.realtime.inbound_rate must be non-negative")
	}
	switch {
	case !implementedRealtimeBuses[r.Bus]:
		add("game.realtime.bus %q is not implemented (implemented: none, postgres)", r.Bus)
	case r.Bus == "postgres" && c.Database.Driver != "postgres":
		add("game.realtime.bus postgres requires database.driver postgres (profile scale)")
	}
	cm := c.Game.Commerce
	if a := cm.Apple; a != nil {
		if a.RootCAFile == "" || len(a.Apps) == 0 {
			add("game.commerce.apple: root_ca_file and at least one app are required")
		}
		for g, app := range a.Apps {
			if app.BundleID == "" || len(app.Environments) == 0 {
				add("game.commerce.apple.apps.%s: bundle_id and environments are required", g)
			}
			for _, e := range app.Environments {
				if e != "Production" && e != "Sandbox" && e != "Xcode" && e != "LocalTesting" {
					add("game.commerce.apple.apps.%s: unknown environment %q", g, e)
				}
			}
		}
	}
	for name, n := range cm.Ads {
		switch n.Kind {
		case "admob":
			if len(n.SecretRefs) > 0 {
				add("game.commerce.ads.%s: admob verifies with Google's public keys; secret_refs is not used", name)
			}
		case "hmac":
			if len(n.SecretRefs) == 0 {
				add("game.commerce.ads.%s: hmac requires secret_refs", name)
			}
			for g, ref := range n.SecretRefs {
				if !IsSecretRef(ref) {
					add("game.commerce.ads.%s.secret_refs.%s must be env:NAME or file:/path", name, g)
				}
			}
		default:
			add("game.commerce.ads.%s: kind %q is not implemented (implemented: admob, hmac)", name, n.Kind)
		}
	}
	if cm.AdIntentTTL < 0 {
		add("game.commerce.ad_intent_ttl must be non-negative")
	}
	if c.Environment == "production" && cm.AllowUnboundTransactions {
		add("production: game.commerce.allow_unbound_transactions weakens cross-player replay protection; not allowed")
	}

	st := c.Storage
	switch st.Driver {
	case "":
	case "local":
		if st.Local.Root == "" || st.Local.PublicURL == "" || !IsSecretRef(st.Local.PresignSecretRef) {
			add("storage.local: root, public_url and presign_secret_ref (env:NAME or file:/path) are required")
		}
		if u, err := url.Parse(st.Local.PublicURL); err != nil || !strings.HasPrefix(u.Path, "/") || u.Path == "/" {
			add("storage.local.public_url must be an absolute URL or path below / (e.g. /v1/files)")
		}
	case "s3":
		s3 := st.S3
		if s3.Endpoint == "" || s3.Region == "" || s3.Bucket == "" || !IsSecretRef(s3.AccessKeyIDRef) || !IsSecretRef(s3.SecretAccessKeyRef) {
			add("storage.s3: endpoint, region, bucket, access_key_id_ref and secret_access_key_ref are required")
		}
		if c.Environment == "production" && strings.HasPrefix(s3.Endpoint, "http://") {
			add("production: storage.s3.endpoint must use https")
		}
	default:
		add("storage.driver %q is not implemented (implemented: local, s3)", st.Driver)
	}
	if st.MaxObjectBytes < 0 || st.Timeout < 0 {
		add("storage.max_object_bytes and storage.timeout must be non-negative")
	}
	return errs
}
