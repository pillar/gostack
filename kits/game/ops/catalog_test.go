package ops_test

import (
	"context"
	"testing"

	"github.com/pillar/gostack/kits/game"
)

// Player-claimed grants obey the items catalog like reward claims: an item over
// max_stack fails the whole claim (nothing posted), unknown assets are refused.
func TestMailClaimRespectsItemsCatalog(t *testing.T) {
	f := setup(t, nil)
	doc := `{"items":{"coin":{"kind":"currency"},"sword":{"kind":"item","max_stack":1}}}`
	if _, err := game.PublishConfig(context.Background(), f.App.DB, "alpha", []byte(doc), "test", ""); err != nil {
		t.Fatal(err)
	}
	p := f.Guest("alpha")
	send := func(grants map[string]any) string {
		r := f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, obj("player_id", p.ID, "title", "t", "grants", grants)), 201)
		return r.Body["mail_id"].(string)
	}
	first := send(obj("sword", 1, "coin", 5))
	second := send(obj("sword", 1, "coin", 7))
	unknown := send(obj("gem", 1))

	f.Expect(f.Do("POST", pub("alpha", "/mail/"+first+"/claim"), p.Token, nil), 200)
	if c := f.Expect(f.Do("POST", pub("alpha", "/mail/"+second+"/claim"), p.Token, nil), 409).Code(); c != "stack_limit" {
		t.Fatalf("code %q, want stack_limit", c)
	}
	if c := f.Expect(f.Do("POST", pub("alpha", "/mail/"+unknown+"/claim"), p.Token, nil), 409).Code(); c != "unknown_asset" {
		t.Fatalf("code %q, want unknown_asset", c)
	}
	// The failed claim rolled back entirely: coin from the second mail was not posted.
	if got := f.balance("alpha", p, "coin"); got != 5 {
		t.Fatalf("coin %d, want 5", got)
	}
	if got := f.balance("alpha", p, "sword"); got != 1 {
		t.Fatalf("sword %d, want 1", got)
	}
	if mm, err := game.Reconcile(context.Background(), f.App.DB.Read, "alpha"); err != nil || len(mm) != 0 {
		t.Fatalf("reconcile %v %v", mm, err)
	}
}
