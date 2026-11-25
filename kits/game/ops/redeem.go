package ops

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"math/big"
	"net/http"
	"strings"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
	"github.com/pillar/gostack/modules/ratelimit"
)

// Generated codes: 12 characters from a 31-symbol alphabet (about 59 bits),
// shown as XXXX-XXXX-XXXX; matching ignores case, dashes and spaces.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

var errInvalidCode = httpx.NewError(http.StatusBadRequest, "invalid_code", "code is invalid, expired or already used")

func newCode() (string, error) {
	var b strings.Builder
	max := big.NewInt(int64(len(codeAlphabet)))
	for i := 0; i < 12; i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(codeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// normalizeCode returns the canonical form, or "" when the input cannot be a code.
func normalizeCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
	if len(s) < 4 || len(s) > 32 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return ""
		}
	}
	return s
}

// hashCode is the stored form; the game id salts it so equal codes differ across games.
func hashCode(gameID, normalized string) string {
	h := sha256.Sum256([]byte(gameID + ":" + normalized))
	return hex.EncodeToString(h[:])
}

type batchReq struct {
	Label          string  `json:"label"`
	Grants         Grants  `json:"grants"`
	ExpiresAt      Instant `json:"expires_at"`
	Count          int     `json:"count"`           // single-use codes to generate
	Shared         bool    `json:"shared"`          // one code for many players
	Code           string  `json:"code"`            // optional custom shared code
	MaxRedemptions int     `json:"max_redemptions"` // shared only: total redemptions
}

// adminCreateBatch creates N single-use codes or one shared code. Plaintext codes
// are returned only in this response; only their hashes are stored.
func (m *Module) adminCreateBatch(w http.ResponseWriter, r *http.Request) error {
	var req batchReq
	if err := decodeBody(r, 16<<10, &req); err != nil {
		return err
	}
	if err := validGrants(req.Grants, true); err != nil {
		return err
	}
	if len(req.Label) > 100 {
		return httpx.BadRequest("label too long")
	}
	now := m.now().UnixMilli()
	if req.ExpiresAt != 0 && int64(req.ExpiresAt) <= now {
		return httpx.BadRequest("expires_at must be in the future")
	}
	max := 1
	var codes []string
	if req.Shared {
		if req.Count != 0 || req.MaxRedemptions < 1 || req.MaxRedemptions > 100_000_000 {
			return httpx.BadRequest("a shared batch needs max_redemptions >= 1 and no count")
		}
		max = req.MaxRedemptions
		c := req.Code
		if c == "" {
			var err error
			if c, err = newCode(); err != nil {
				return err
			}
		}
		if normalizeCode(c) == "" {
			return httpx.BadRequest("code must be 4-32 letters or digits")
		}
		codes = []string{c}
	} else {
		if req.Code != "" || req.MaxRedemptions != 0 || req.Count < 1 || req.Count > 10_000 {
			return httpx.BadRequest("a single-use batch needs count between 1 and 10000 and no code or max_redemptions")
		}
		for i := 0; i < req.Count; i++ {
			c, err := newCode()
			if err != nil {
				return err
			}
			codes = append(codes, c)
		}
	}
	g := game.CurrentGame(r.Context())
	batch := httpx.RandomID("rb_")
	err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		ctx := r.Context()
		if _, err := tx.ExecContext(ctx, `INSERT INTO ops_redeem_batches(game_id,batch_id,label,shared,max_redemptions,grants,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			g.ID, batch, req.Label, boolInt(req.Shared), max, jsonString(req.Grants), int64(req.ExpiresAt), now); err != nil {
			return err
		}
		st, err := tx.PrepareContext(ctx, `INSERT INTO ops_redeem_codes(game_id,code_hash,batch_id) VALUES(?,?,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, c := range codes {
			if _, err := st.ExecContext(ctx, g.ID, hashCode(g.ID, normalizeCode(c)), batch); err != nil {
				if store.IsUniqueViolation(err) {
					return httpx.Conflict("code_exists", "a code with this value already exists")
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"batch_id": batch, "shared": req.Shared, "max_redemptions": max, "codes": codes})
	return nil
}

func (m *Module) adminListBatches(w http.ResponseWriter, r *http.Request) error {
	g := game.CurrentGame(r.Context())
	rows, err := m.DB.Read.QueryContext(r.Context(), `SELECT b.batch_id,b.label,b.shared,b.max_redemptions,b.grants,b.expires_at,b.disabled,b.created_at,
		count(c.code_hash), coalesce(sum(c.redeemed),0)
		FROM ops_redeem_batches b LEFT JOIN ops_redeem_codes c ON c.game_id=b.game_id AND c.batch_id=b.batch_id
		WHERE b.game_id=? GROUP BY b.game_id, b.batch_id ORDER BY b.created_at DESC, b.batch_id LIMIT 200`, g.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, label, grants string
		var shared, maxR, expires, disabled, created, codes, redeemed int64
		if err := rows.Scan(&id, &label, &shared, &maxR, &grants, &expires, &disabled, &created, &codes, &redeemed); err != nil {
			return err
		}
		out = append(out, map[string]any{"batch_id": id, "label": label, "shared": shared == 1, "max_redemptions": maxR, "grants": parseGrants(grants),
			"expires_at": expires, "disabled": disabled == 1, "created_at": created, "codes": codes, "redeemed": redeemed})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"batches": out})
	return nil
}

// adminDisableBatch invalidates every code of a batch; idempotent.
func (m *Module) adminDisableBatch(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	g := game.CurrentGame(r.Context())
	var n int64
	err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `UPDATE ops_redeem_batches SET disabled=1 WHERE game_id=? AND batch_id=?`, g.ID, id)
		if err == nil {
			n, err = res.RowsAffected()
		}
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	if n == 0 {
		return httpx.NotFound("batch not found")
	}
	return writeNoContent(w)
}

// redeem exchanges a code for its grants. Errors: invalid, unknown, expired,
// disabled and exhausted codes all return the same 400 invalid_code (no
// enumeration oracle); only a player who already redeemed the code themselves
// gets 409 already_redeemed. Attempts are rate limited per player and per IP.
func (m *Module) redeem(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := game.CurrentSession(ctx)
	if err := m.Kit.RateLimit(m.playerLimit, "redeem:p:"+s.GameID+":"+s.PlayerID); err != nil {
		return err
	}
	if err := m.Kit.RateLimit(m.ipLimit, "redeem:ip:"+s.GameID+":"+ratelimit.IPKey(m.Kit.ClientIP.Of(r))); err != nil {
		return err
	}
	body, err := game.ReadBody(r, 1024)
	if err != nil {
		return err
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := game.DecodeStrict(body, &req); err != nil {
		return err
	}
	norm := normalizeCode(req.Code)
	if norm == "" {
		return errInvalidCode
	}
	hash := hashCode(s.GameID, norm)
	return m.idem(w, r, "game:"+s.GameID+":"+s.PlayerID, "redeem", body, func(tx *sql.Tx) (idempotency.Result, error) {
		var batch, grants string
		var redeemed, max, expires, disabled int64
		err := tx.QueryRowContext(ctx, `SELECT c.batch_id,c.redeemed,b.max_redemptions,b.grants,b.expires_at,b.disabled
			FROM ops_redeem_codes c JOIN ops_redeem_batches b ON b.game_id=c.game_id AND b.batch_id=c.batch_id
			WHERE c.game_id=? AND c.code_hash=?`, s.GameID, hash).Scan(&batch, &redeemed, &max, &grants, &expires, &disabled)
		if err == sql.ErrNoRows {
			return idempotency.Result{}, errInvalidCode
		}
		if err != nil {
			return idempotency.Result{}, err
		}
		now := m.now().UnixMilli()
		if disabled == 1 || (expires != 0 && expires <= now) {
			return idempotency.Result{}, errInvalidCode
		}
		var mine int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ops_redeem_log WHERE game_id=? AND code_hash=? AND player_id=?`, s.GameID, hash, s.PlayerID).Scan(&mine); err != nil {
			return idempotency.Result{}, err
		}
		if mine > 0 {
			return idempotency.Result{}, httpx.Conflict("already_redeemed", "you already redeemed this code")
		}
		// The guarded UPDATE is the capacity check; the log PK and ledger source key are further guards.
		res, err := tx.ExecContext(ctx, `UPDATE ops_redeem_codes SET redeemed=redeemed+1 WHERE game_id=? AND code_hash=? AND redeemed<?`, s.GameID, hash, max)
		if err != nil {
			return idempotency.Result{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return idempotency.Result{}, errInvalidCode
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ops_redeem_log(game_id,code_hash,player_id,created_at) VALUES(?,?,?,?)`, s.GameID, hash, s.PlayerID, now); err != nil {
			return idempotency.Result{}, err
		}
		g := parseGrants(grants)
		if err := m.Kit.Grant(ctx, tx, s.GameID, s.PlayerID, "reward", "redeem:"+hash+":"+s.PlayerID, g, now); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Body: map[string]any{"batch_id": batch, "grants": g}}, nil
	})
}
