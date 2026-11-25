package social_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/kits/game/social"
)

// fakePlatform verifies any code "ok:<subject>" as that subject.
type fakePlatform struct{}

func (fakePlatform) Verify(_ context.Context, _, code string) (string, error) {
	if len(code) > 3 && code[:3] == "ok:" {
		return code[3:], nil
	}
	return "", httpx.Unauthorized("bad code")
}

// setupPlatform is setup with a fake "test" platform available for /auth/bind.
func setupPlatform(t *testing.T, tune func(*social.Module)) *fixture {
	f := &fixture{}
	f.e = gametest.New(t, gametest.Options{Config: cfg, Routes: func(mux *http.ServeMux, a *app.App) {
		a.Game.Platforms = map[string]game.PlatformVerifier{"test": fakePlatform{}}
		f.mod = &social.Module{Kit: a.Game, DB: a.DB, Now: func() time.Time {
			return time.Now().Add(time.Duration(f.now.Load()) * time.Millisecond)
		}, RequestsPerMinute: 10000}
		if tune != nil {
			tune(f.mod)
		}
		f.mod.Routes(mux)
	}})
	return f
}

// bind links a fresh verified platform identity to p through the real route.
func (f *fixture) bind(p gametest.Player) {
	f.e.T.Helper()
	sub := "sub-" + p.ID
	f.expect(f.do("POST", "/auth/bind", p, map[string]string{"platform": "test", "code": "ok:" + sub}), 200, "")
}

func grants(m *social.Module) {
	m.InviterGrant = map[string]int64{"gem": 10}
	m.InviteeGrant = map[string]int64{"coin": 5}
}

func TestInviteGuestGrantsDeferredUntilVerified(t *testing.T) {
	f := setupPlatform(t, grants)
	inviter := f.e.Guest("alpha")
	code := f.expect(f.do("POST", "/invites", inviter, nil), 201, "").Body["code"].(string)

	g := f.e.Guest("alpha")
	r := f.expect(f.do("POST", "/invites/"+code+"/accept", g, nil), 200, "")
	if r.Body["attributed"] != true || r.Body["grants_pending"] != true {
		t.Fatalf("%s", r.Raw)
	}
	if balance(f, g, "coin") != 0 || balance(f, inviter, "gem") != 0 {
		t.Fatalf("guest invitee paid: coin=%v gem=%v", balance(f, g, "coin"), balance(f, inviter, "gem"))
	}
	// Still a guest: claim-pending pays nothing.
	f.expect(f.do("POST", "/invites/claim-pending", g, nil), 403, "identity_required")
	// A player without any attribution has nothing pending.
	f.expect(f.do("POST", "/invites/claim-pending", inviter, nil), 404, "")

	f.bind(g)
	r = f.expect(f.do("POST", "/invites/claim-pending", g, nil), 200, "")
	if r.Body["paid"] != true {
		t.Fatalf("%s", r.Raw)
	}
	f.expect(f.do("POST", "/invites/claim-pending", g, nil), 409, "already_paid")
	if balance(f, g, "coin") != 5 || balance(f, inviter, "gem") != 10 {
		t.Fatalf("after bind: coin=%v gem=%v", balance(f, g, "coin"), balance(f, inviter, "gem"))
	}

	// An invitee that is already verified at accept time is paid immediately.
	v := f.e.Guest("alpha")
	f.bind(v)
	r = f.expect(f.do("POST", "/invites/"+code+"/accept", v, nil), 200, "")
	if r.Body["grants_pending"] != false || balance(f, v, "coin") != 5 || balance(f, inviter, "gem") != 20 {
		t.Fatalf("%s coin=%v gem=%v", r.Raw, balance(f, v, "coin"), balance(f, inviter, "gem"))
	}
	f.expect(f.do("POST", "/invites/claim-pending", v, nil), 409, "already_paid")

	// Concurrent claims of a pending attribution pay exactly once.
	c := f.e.Guest("alpha")
	f.expect(f.do("POST", "/invites/"+code+"/accept", c, nil), 200, "")
	f.bind(c)
	var wg sync.WaitGroup
	var ok, conflict atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch f.do("POST", "/invites/claim-pending", c, nil).Status {
			case 200:
				ok.Add(1)
			case 409:
				conflict.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || conflict.Load() != 7 {
		t.Fatalf("ok=%d conflict=%d", ok.Load(), conflict.Load())
	}
	if balance(f, c, "coin") != 5 || balance(f, inviter, "gem") != 30 {
		t.Fatalf("after race: coin=%v gem=%v", balance(f, c, "coin"), balance(f, inviter, "gem"))
	}
	if mm, err := game.Reconcile(t.Context(), f.e.App.DB.Read, "alpha"); err != nil || len(mm) != 0 {
		t.Fatalf("reconcile %v %v", mm, err)
	}
}
