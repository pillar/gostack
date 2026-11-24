package game

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/ratelimit"
)

type loginResponse struct {
	PlayerID    string  `json:"player_id"`
	GuestID     *string `json:"guest_id,omitempty"`
	GuestSecret *string `json:"guest_secret"`
	AccessToken string  `json:"access_token"`
	ExpiresAt   int64   `json:"expires_at"`
	Created     bool    `json:"created"`
}

type platformLoginResponse struct {
	PlayerID    string `json:"player_id"`
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at"`
	Created     bool   `json:"created"`
}

func (k *Kit) newSession(ctx context.Context, tx *sql.Tx, gameID, playerID string) (token string, exp int64, err error) {
	token = auth.NewSecret("gst.")
	now := k.Now()
	exp = now.Add(k.Cfg.SessionTTL).UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(token_hash,game_id,player_id,created_at,expires_at) VALUES(?,?,?,?,?)`,
		auth.Hash(token), gameID, playerID, now.UnixMilli(), exp)
	return token, exp, err
}

func (k *Kit) createPlayer(ctx context.Context, tx *sql.Tx, gameID string) (string, error) {
	id := httpx.RandomID("plr_")
	_, err := tx.ExecContext(ctx, `INSERT INTO players(game_id,player_id,created_at) VALUES(?,?,?)`, gameID, id, k.Now().UnixMilli())
	return id, err
}

// guestLogin registers a new guest or logs an existing guest in (G02).
func (k *Kit) guestLogin(w http.ResponseWriter, r *http.Request) error {
	g := gameFrom(r.Context())
	if err := k.rateLimit(k.authLimit, "auth:"+g.ID+":"+ratelimit.IPKey(k.ClientIP.Of(r))); err != nil {
		return err
	}
	var req struct {
		GuestID     string `json:"guest_id"`
		GuestSecret string `json:"guest_secret"`
		Rotate      bool   `json:"rotate"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	ctx := r.Context()
	if req.GuestID == "" && req.GuestSecret == "" {
		var resp loginResponse
		err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			pid, err := k.createPlayer(ctx, tx, g.ID)
			if err != nil {
				return err
			}
			gid, secret := httpx.RandomID("gst_"), auth.NewSecret("")
			now := k.Now().UnixMilli()
			if _, err := tx.ExecContext(ctx, `INSERT INTO player_identities(game_id,provider,subject,player_id,secret_hash,created_at,updated_at)
				VALUES(?,?,?,?,?,?,?)`, g.ID, "guest", gid, pid, auth.Hash(secret), now, now); err != nil {
				return err
			}
			tok, exp, err := k.newSession(ctx, tx, g.ID, pid)
			resp = loginResponse{PlayerID: pid, GuestID: &gid, GuestSecret: &secret, AccessToken: tok, ExpiresAt: exp, Created: true}
			return err
		})
		if err != nil {
			return mapWriteErr(err)
		}
		httpx.WriteJSON(w, http.StatusCreated, resp)
		return nil
	}
	if req.GuestID == "" || req.GuestSecret == "" {
		return httpx.BadRequest("guest_id and guest_secret must be provided together")
	}
	var pid, stored string
	err := k.DB.Read.QueryRowContext(ctx, `SELECT player_id, secret_hash FROM player_identities WHERE game_id=? AND provider='guest' AND subject=?`,
		g.ID, req.GuestID).Scan(&pid, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.Unauthorized("invalid guest credentials")
	}
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(auth.Hash(req.GuestSecret))) != 1 {
		return httpx.Unauthorized("invalid guest credentials")
	}
	resp := loginResponse{PlayerID: pid, GuestID: &req.GuestID}
	err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		resp.GuestSecret = nil
		if req.Rotate {
			secret := auth.NewSecret("")
			// Compare-and-swap on the old hash so concurrent rotations cannot both win.
			res, err := tx.ExecContext(ctx, `UPDATE player_identities SET secret_hash=?, updated_at=? WHERE game_id=? AND provider='guest' AND subject=? AND secret_hash=?`,
				auth.Hash(secret), k.Now().UnixMilli(), g.ID, req.GuestID, stored)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return httpx.Unauthorized("invalid guest credentials")
			}
			resp.GuestSecret = &secret
		}
		var err error
		resp.AccessToken, resp.ExpiresAt, err = k.newSession(ctx, tx, g.ID, pid)
		return err
	})
	if err != nil {
		return mapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
	return nil
}

type platformRequest struct {
	Platform string `json:"platform"`
	Code     string `json:"code"`
}

func (k *Kit) verifyPlatform(r *http.Request) (string, string, error) {
	var req platformRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return "", "", err
	}
	v, ok := k.Platforms[req.Platform]
	if !ok {
		return "", "", httpx.NotImplemented(fmt.Sprintf("platform %q is not configured", req.Platform))
	}
	if req.Code == "" || len(req.Code) > 256 {
		return "", "", httpx.BadRequest("code required")
	}
	// Network verification happens outside any database transaction.
	subject, err := v.Verify(r.Context(), gameFrom(r.Context()).ID, req.Code)
	if err != nil {
		return "", "", err
	}
	return req.Platform, subject, nil
}

// platformLogin exchanges a platform code for a session; one player per (game, platform, subject).
func (k *Kit) platformLogin(w http.ResponseWriter, r *http.Request) error {
	g := gameFrom(r.Context())
	if err := k.rateLimit(k.authLimit, "auth:"+g.ID+":"+ratelimit.IPKey(k.ClientIP.Of(r))); err != nil {
		return err
	}
	platform, subject, err := k.verifyPlatform(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var resp platformLoginResponse
	err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		resp = platformLoginResponse{}
		err := tx.QueryRowContext(ctx, `SELECT player_id FROM player_identities WHERE game_id=? AND provider=? AND subject=?`, g.ID, platform, subject).Scan(&resp.PlayerID)
		if errors.Is(err, sql.ErrNoRows) {
			if resp.PlayerID, err = k.createPlayer(ctx, tx, g.ID); err != nil {
				return err
			}
			now := k.Now().UnixMilli()
			_, err = tx.ExecContext(ctx, `INSERT INTO player_identities(game_id,provider,subject,player_id,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
				g.ID, platform, subject, resp.PlayerID, now, now)
			resp.Created = true
		}
		if err != nil {
			return err
		}
		resp.AccessToken, resp.ExpiresAt, err = k.newSession(ctx, tx, g.ID, resp.PlayerID)
		return err
	})
	if err != nil {
		return mapWriteErr(err)
	}
	status := http.StatusOK
	if resp.Created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(w, status, resp)
	return nil
}

// bindPlatform links a verified platform identity to the current player. An
// identity owned by another player is a conflict; nothing is merged or overwritten.
func (k *Kit) bindPlatform(w http.ResponseWriter, r *http.Request) error {
	s := sessionFrom(r.Context())
	platform, subject, err := k.verifyPlatform(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT player_id FROM player_identities WHERE game_id=? AND provider=? AND subject=?`, s.GameID, platform, subject).Scan(&owner)
		switch {
		case err == nil && owner == s.PlayerID:
			return nil
		case err == nil:
			k.Log.WarnContext(ctx, "identity bind conflict", "game", s.GameID, "player", s.PlayerID, "platform", platform)
			return httpx.Conflict("identity_conflict", "platform identity is bound to another player")
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM player_identities WHERE game_id=? AND player_id=? AND provider=?`, s.GameID, s.PlayerID, platform).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return httpx.Conflict("identity_conflict", "player already has a different identity on this platform")
		}
		now := k.Now().UnixMilli()
		_, err = tx.ExecContext(ctx, `INSERT INTO player_identities(game_id,provider,subject,player_id,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			s.GameID, platform, subject, s.PlayerID, now, now)
		return err
	})
	if err != nil {
		return mapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"player_id": s.PlayerID, "platform": platform})
	return nil
}

func (k *Kit) logout(w http.ResponseWriter, r *http.Request) error {
	s := sessionFrom(r.Context())
	err := k.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), `DELETE FROM sessions WHERE token_hash=?`, s.TokenHash)
		return err
	})
	if err != nil {
		return mapWriteErr(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (k *Kit) me(w http.ResponseWriter, r *http.Request) error {
	s := sessionFrom(r.Context())
	ctx := r.Context()
	var created int64
	if err := k.DB.Read.QueryRowContext(ctx, `SELECT created_at FROM players WHERE game_id=? AND player_id=?`, s.GameID, s.PlayerID).Scan(&created); err != nil {
		return err
	}
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT DISTINCT provider FROM player_identities WHERE game_id=? AND player_id=? ORDER BY provider`, s.GameID, s.PlayerID)
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return err
		}
		ids = append(ids, p)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"player_id": s.PlayerID, "game_id": s.GameID, "created_at": created, "identities": ids})
	return rows.Err()
}

// PlatformVerifier validates a client-supplied platform code server-side and returns the stable subject.
type PlatformVerifier interface {
	Verify(ctx context.Context, gameID, code string) (subject string, err error)
}

// WeChatVerifier calls jscode2session. The AppSecret never leaves the server.
type WeChatVerifier struct {
	Apps     map[string]config.PlatformApp
	Endpoint string // default https://api.weixin.qq.com/sns/jscode2session
	Client   *http.Client
}

func (v *WeChatVerifier) Verify(ctx context.Context, gameID, code string) (string, error) {
	app, ok := v.Apps[gameID]
	if !ok {
		return "", httpx.NotImplemented("wechat is not configured for this game")
	}
	secret, err := config.ResolveSecret(app.AppSecretRef)
	if err != nil {
		return "", fmt.Errorf("wechat secret: %w", err)
	}
	endpoint := v.Endpoint
	if endpoint == "" {
		endpoint = "https://api.weixin.qq.com/sns/jscode2session"
	}
	q := url.Values{"appid": {app.AppID}, "secret": {secret}, "js_code": {code}, "grant_type": {"authorization_code"}}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", httpx.Overloaded("wechat verification unavailable")
	}
	defer resp.Body.Close()
	var body struct {
		OpenID  string `json:"openid"`
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 16<<10)).Decode(&body); err != nil {
		return "", fmt.Errorf("wechat response: %w", err)
	}
	if body.ErrCode != 0 || body.OpenID == "" {
		return "", httpx.Unauthorized("wechat code rejected")
	}
	return body.OpenID, nil
}
