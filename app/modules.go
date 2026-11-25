package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game/commerce"
	"github.com/pillar/gostack/kits/game/experiments"
	"github.com/pillar/gostack/kits/game/match"
	"github.com/pillar/gostack/kits/game/ops"
	"github.com/pillar/gostack/kits/game/realtime"
	"github.com/pillar/gostack/kits/game/social"
	"github.com/pillar/gostack/modules/billing"
	"github.com/pillar/gostack/modules/storage"
)

// Modules holds the optional modules enabled by configuration (nil when disabled).
type Modules struct {
	Ops         *ops.Module
	Experiments *experiments.Module
	Social      *social.Module
	Match       *match.Module
	Commerce    *commerce.Module
	Realtime    *realtime.Module
	Billing     *billing.Service
	// Storage is the configured object store; LocalStorage is also set for the
	// local driver (its presign handler is mounted on the main listener).
	Storage      storage.Storage
	LocalStorage *storage.Local
}

// maintainer is implemented by modules with periodic cleanup.
type maintainer interface{ Maintain(context.Context) error }

func (a *App) initModules(cfg config.Config) error {
	m := &a.Mod
	if a.Game != nil {
		g := cfg.Game
		if cfg.HasGameModule("ops") {
			m.Ops = &ops.Module{Kit: a.Game, DB: a.DB,
				RedeemPerPlayerPerMinute: g.Ops.RedeemPerPlayerPerMinute, RedeemPerIPPerMinute: g.Ops.RedeemPerIPPerMinute}
		}
		if cfg.HasGameModule("experiments") {
			m.Experiments = &experiments.Module{Kit: a.Game, DB: a.DB}
		}
		if cfg.HasGameModule("social") {
			s := g.Social
			m.Social = &social.Module{Kit: a.Game, DB: a.DB, MaxFriends: s.MaxFriends, MaxPendingRequests: s.MaxPendingRequests,
				MaxBlocks: s.MaxBlocks, MaxInvites: s.MaxInvites, MaxAcceptsPerInviter: s.MaxAcceptsPerInviter,
				InviteWindow: s.InviteWindow, InviterGrant: s.InviterGrant, InviteeGrant: s.InviteeGrant, RequestsPerMinute: s.RequestsPerMinute}
		}
		if cfg.HasGameModule("match") {
			mc := g.Match
			m.Match = &match.Module{Kit: a.Game, DB: a.DB, Log: a.Log, LeaseTTL: mc.LeaseTTL, ClaimTimeout: mc.ClaimTimeout,
				MatchInterval: mc.MatchInterval, Retention: mc.Retention, MaxMatchBatch: mc.MaxMatchBatch, TicketsPerMinute: mc.TicketsPerMinute}
		}
		if cfg.HasGameModule("commerce") {
			c, err := newCommerce(a, g.Commerce)
			if err != nil {
				return err
			}
			m.Commerce = c
		}
		if cfg.HasGameModule("realtime") {
			r := g.Realtime
			m.Realtime = &realtime.Module{Kit: a.Game, DB: a.DB, Metrics: a.Metrics, Log: a.Log, Options: realtime.Options{
				MaxConnections: r.MaxConnections, MaxPerPlayer: r.MaxPerPlayer, MaxMessageBytes: r.MaxMessageBytes,
				InboundRate: r.InboundRate, InboundBurst: r.InboundBurst, SendQueue: r.SendQueue, MaxPayloadBytes: r.MaxPayloadBytes,
				ReplayBuffer: r.ReplayBuffer, ReplayMaxBytes: r.ReplayMaxBytes, ReplayMaxAge: r.ReplayMaxAge, MaxStreams: r.MaxStreams,
				PingInterval: r.PingInterval, PongTimeout: r.PongTimeout, AuthTimeout: r.AuthTimeout, WriteTimeout: r.WriteTimeout,
				SessionRecheck: r.SessionRecheck, MessageTimeout: r.MessageTimeout}}
		}
	}
	if cfg.Billing.Enabled {
		m.Billing = &billing.Service{DB: a.DB}
	}
	switch st := cfg.Storage; st.Driver {
	case "local":
		secret, err := config.ResolveSecret(st.Local.PresignSecretRef)
		if err != nil {
			return fmt.Errorf("storage.local.presign_secret_ref: %w", err)
		}
		l, err := storage.NewLocal(storage.LocalConfig{Root: st.Local.Root, PublicURL: st.Local.PublicURL, Secret: []byte(secret),
			MaxObjectBytes: st.MaxObjectBytes, Timeout: st.Timeout})
		if err != nil {
			return err
		}
		m.Storage, m.LocalStorage = l, l
	case "s3":
		id, err := config.ResolveSecret(st.S3.AccessKeyIDRef)
		if err != nil {
			return fmt.Errorf("storage.s3.access_key_id_ref: %w", err)
		}
		key, err := config.ResolveSecret(st.S3.SecretAccessKeyRef)
		if err != nil {
			return fmt.Errorf("storage.s3.secret_access_key_ref: %w", err)
		}
		s3, err := storage.NewS3(storage.S3Config{Endpoint: st.S3.Endpoint, Region: st.S3.Region, Bucket: st.S3.Bucket,
			AccessKeyID: id, SecretAccessKey: key, PathStyle: st.S3.PathStyle, SignPayload: st.S3.SignPayload,
			MaxObjectBytes: st.MaxObjectBytes, Timeout: st.Timeout, TempDir: st.S3.TempDir})
		if err != nil {
			return err
		}
		m.Storage = s3
	}
	return nil
}

func newCommerce(a *App, c config.CommerceConfig) (*commerce.Module, error) {
	mod := &commerce.Module{Kit: a.Game, DB: a.DB, AllowUnboundTransactions: c.AllowUnboundTransactions,
		AdIntentTTL: c.AdIntentTTL, Outbox: a.Jobs, IAP: map[string]commerce.Verifier{}, Ads: map[string]commerce.AdVerifier{}}
	if c.Apple != nil {
		pem, err := os.ReadFile(c.Apple.RootCAFile)
		if err != nil {
			return nil, fmt.Errorf("game.commerce.apple.root_ca_file: %w", err)
		}
		apps := map[string]commerce.AppleApp{}
		for g, app := range c.Apple.Apps {
			apps[g] = commerce.AppleApp{BundleID: app.BundleID, Environments: app.Environments}
		}
		v, err := commerce.NewAppleVerifier(pem, apps)
		if err != nil {
			return nil, err
		}
		mod.IAP["apple"] = v
	}
	if c.Google != nil {
		v, err := newGoogleVerifier(a, c.Google)
		if err != nil {
			return nil, err
		}
		mod.IAP["google"] = v
	}
	if c.WeChatPay != nil {
		v, err := newWeChatPayVerifier(a, c.WeChatPay)
		if err != nil {
			return nil, err
		}
		mod.IAP["wechatpay"] = v
	}
	for name, n := range c.Ads {
		switch n.Kind {
		case "admob":
			mod.Ads[name] = &commerce.AdMobVerifier{Keys: &commerce.AdMobKeyFetcher{URL: n.KeyURL}}
		case "hmac":
			secrets := map[string][]byte{}
			for g, ref := range n.SecretRefs {
				s, err := config.ResolveSecret(ref)
				if err != nil {
					return nil, fmt.Errorf("game.commerce.ads.%s.secret_refs.%s: %w", name, g, err)
				}
				secrets[g] = []byte(s)
			}
			mod.Ads[name] = &commerce.HMACAdVerifier{Secrets: secrets, MaxSkew: n.MaxSkew}
		}
	}
	return mod, nil
}

// moduleRoutes mounts every enabled module.
func (a *App) moduleRoutes(mux *http.ServeMux) {
	m := a.Mod
	if m.Ops != nil {
		m.Ops.Routes(mux)
	}
	if m.Experiments != nil {
		m.Experiments.Routes(mux)
	}
	if m.Social != nil {
		m.Social.Routes(mux)
	}
	if m.Match != nil {
		m.Match.Routes(mux)
	}
	if m.Commerce != nil {
		m.Commerce.Routes(mux)
	}
	if m.Realtime != nil {
		m.Realtime.Routes(mux)
	}
	if m.Billing != nil {
		m.Billing.Handler(mux, auth.APIKeys{DB: a.DB})
	}
}

// storageRoute returns the path prefix and handler of the local storage
// presign route (nil without local storage). It is not mounted on the main mux:
// Handler serves it outside the generic body/time limits (see storageChain).
func (a *App) storageRoute() (string, http.Handler) {
	if a.Mod.LocalStorage == nil {
		return "", nil
	}
	p := a.Cfg.Storage.Local.PublicURL
	if u, err := url.Parse(p); err == nil {
		p = u.Path
	}
	return strings.TrimSuffix(p, "/") + "/", a.Mod.LocalStorage.Handler()
}

// storageDeadlines replaces the listener's read/write timeouts (sized for
// http.request_timeout) with storage.timeout for object transfers. The
// storage handler bounds size (storage.max_object_bytes) and applies
// storage.timeout to the operation's context itself.
func (a *App) storageDeadlines(next http.Handler) http.Handler {
	d := a.Cfg.Storage.Timeout
	if d <= 0 {
		d = storage.DefaultTimeout
	}
	d += 5 * time.Second // same slack the listener adds to http.request_timeout
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		dl := time.Now().Add(d)
		_ = rc.SetReadDeadline(dl)
		_ = rc.SetWriteDeadline(dl)
		next.ServeHTTP(w, r)
	})
}

// maintainers lists enabled modules with periodic cleanup.
func (a *App) maintainers() map[string]maintainer {
	m := a.Mod
	out := map[string]maintainer{}
	if m.Ops != nil {
		out["ops"] = m.Ops
	}
	if m.Match != nil {
		out["match"] = m.Match
	}
	if m.Commerce != nil {
		out["commerce"] = m.Commerce
	}
	if m.Billing != nil {
		out["billing"] = m.Billing
	}
	return out
}
