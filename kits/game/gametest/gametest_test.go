package gametest_test

import (
	"net/http"
	"testing"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/game/gametest"
)

// The extension API resolves game, session and admin tenant for feature packages.
func TestExtensionRoutes(t *testing.T) {
	e := gametest.New(t, gametest.Options{Routes: func(mux *http.ServeMux, a *app.App) {
		a.Game.Handle(mux, "GET /ext/whoami", game.Player, func(w http.ResponseWriter, r *http.Request) error {
			httpx.WriteJSON(w, 200, map[string]string{"game": game.CurrentGame(r.Context()).ID, "player": game.CurrentSession(r.Context()).PlayerID})
			return nil
		})
		a.Game.HandleAdmin(mux, "GET /ext/info", func(w http.ResponseWriter, r *http.Request) error {
			httpx.WriteJSON(w, 200, map[string]string{"game": game.CurrentGame(r.Context()).ID, "key": game.AdminKey(r.Context()).TenantID})
			return nil
		})
	}})
	p := e.Guest("alpha")
	r := e.Expect(e.Do("GET", "/v1/games/alpha/ext/whoami", p.Token, nil), 200)
	if r.Body["player"] != p.ID || r.Body["game"] != "alpha" {
		t.Fatalf("%s", r.Raw)
	}
	e.Expect(e.Do("GET", "/v1/games/beta/ext/whoami", p.Token, nil), 403)
	e.Expect(e.Do("GET", "/v1/games/alpha/ext/whoami", "", nil), 401)
	e.Expect(e.Do("GET", "/v1/admin/games/alpha/ext/info", e.AdminToken, nil), 200)
	e.Expect(e.Do("GET", "/v1/admin/games/alpha/ext/info", p.Token, nil), 401)
	e.Expect(e.Do("GET", "/v1/admin/games/nope/ext/info", e.AdminToken, nil), 404)
}
