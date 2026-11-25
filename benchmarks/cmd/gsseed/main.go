// Command gsseed writes the PRD §23 benchmark dataset (default 100k players,
// 1M leaderboard rows across 10 weekly seasons, one 8 KiB and one 1 KiB save per
// player, a session per player) into a fresh GoStack database, using the
// application's own storage opening path (app.OpenDB: lock, migrations, PRAGMAs)
// and GameKit APIs (CreateGame, PublishConfig, SeasonID), with batched write
// transactions. Output is reproducible for a given -seed.
//
//	gsseed -config bench.yaml                 # seed (refuses a non-empty game)
//	gsseed -config bench.yaml -verify         # read-only economy checks after a run
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"time"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/benchmarks/internal/benchdata"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
	_ "modernc.org/sqlite"
)

func main() {
	cfgPath := flag.String("config", "", "GoStack config file (database.path is seeded)")
	players := flag.Int("players", 100_000, "players")
	seasons := flag.Int("seasons", 10, "weekly seasons per player on board main (players*seasons leaderboard rows)")
	seed := flag.Uint64("seed", benchdata.DefaultSeed, "fixed seed for tokens, scores and payloads")
	batch := flag.Int("batch", 2000, "players per write transaction")
	verify := flag.Bool("verify", false, "read-only: check reward claims vs ledger vs balances and print counts")
	flag.Parse()
	cfg, err := config.Load(*cfgPath, nil, nil)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	if *verify {
		if err := verifyDB(ctx, cfg.Database.Path); err != nil {
			log.Fatal(err)
		}
		return
	}
	start := time.Now()
	db, err := app.OpenDB(ctx, cfg, true)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := game.CreateGame(ctx, db, "default", benchdata.GameID, "benchmark", nil); err != nil {
		log.Fatalf("create game (use a fresh database): %v", err)
	}
	if _, err := game.PublishConfig(ctx, db, benchdata.GameID, []byte(benchdata.ConfigDoc), "gsseed", "benchmark"); err != nil {
		log.Fatal(err)
	}
	now := time.Now()
	nowMs := now.UnixMilli()
	exp := now.Add(365 * 24 * time.Hour).UnixMilli()
	seasonIDs := make([]string, *seasons)
	for s := range seasonIDs {
		seasonIDs[s] = game.SeasonID("weekly", now.AddDate(0, 0, -7*s))
	}
	rng := rand.New(rand.NewPCG(*seed, 1))
	var scoreRows int
	for lo := 0; lo < *players; lo += *batch {
		hi := min(lo+*batch, *players)
		// Precompute outside the transaction: WriteTx may re-run fn on SQLITE_BUSY.
		type prow struct {
			id, tokHash string
			big, small  []byte
			scores      []int64
		}
		rows := make([]prow, 0, hi-lo)
		for i := lo; i < hi; i++ {
			r := prow{id: benchdata.PlayerID(i), tokHash: auth.Hash(benchdata.Token(*seed, i)),
				big: benchdata.Payload(benchdata.BigBytes, *seed, i, 1), small: benchdata.Payload(benchdata.SmallBytes, *seed, i, 1)}
			for range *seasons {
				r.scores = append(r.scores, rng.Int64N(100_000_000))
			}
			rows = append(rows, r)
		}
		err := db.WriteTx(ctx, func(tx *sql.Tx) error {
			stmts := map[string]*sql.Stmt{}
			for name, q := range map[string]string{
				"p":  `INSERT INTO players(game_id,player_id,created_at) VALUES(?,?,?)`,
				"id": `INSERT INTO player_identities(game_id,provider,subject,player_id,secret_hash,created_at,updated_at) VALUES(?,'guest',?,?,?,?,?)`,
				"s":  `INSERT INTO sessions(token_hash,game_id,player_id,created_at,expires_at) VALUES(?,?,?,?,?)`,
				"sv": `INSERT INTO save_slots(game_id,player_id,slot,revision,schema_version,checksum,payload,updated_at) VALUES(?,?,?,1,1,?,?,?)`,
				"sc": `INSERT INTO scores(game_id,board_id,season_id,player_id,score,achieved_at,status,updated_at) VALUES(?,?,?,?,?,?,'accepted',?)`,
			} {
				st, err := tx.PrepareContext(ctx, q)
				if err != nil {
					return err
				}
				defer st.Close()
				stmts[name] = st
			}
			for i, r := range rows {
				g := benchdata.GameID
				if _, err := stmts["p"].ExecContext(ctx, g, r.id, nowMs); err != nil {
					return err
				}
				if _, err := stmts["id"].ExecContext(ctx, g, fmt.Sprintf("guest_bench%07d", lo+i), r.id, r.tokHash, nowMs, nowMs); err != nil {
					return err
				}
				if _, err := stmts["s"].ExecContext(ctx, r.tokHash, g, r.id, nowMs, exp); err != nil {
					return err
				}
				for _, sv := range []struct {
					slot string
					b    []byte
				}{{benchdata.SlotBig, r.big}, {benchdata.SlotSmall, r.small}} {
					if _, err := stmts["sv"].ExecContext(ctx, g, r.id, sv.slot, checksum(sv.b), sv.b, nowMs); err != nil {
						return err
					}
				}
				for s, score := range r.scores {
					if _, err := stmts["sc"].ExecContext(ctx, g, benchdata.Board, seasonIDs[s], r.id, score, nowMs-int64(s)*7*86400_000, nowMs); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			log.Fatalf("batch at %d: %v", lo, err)
		}
		scoreRows += len(rows) * *seasons
		if (hi/(*batch))%10 == 0 || hi == *players {
			log.Printf("seeded %d/%d players (%s)", hi, *players, time.Since(start).Round(time.Second))
		}
	}
	if _, err := db.Write.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Write.ExecContext(ctx, `ANALYZE`); err != nil {
		log.Fatal(err)
	}
	st, _ := os.Stat(cfg.Database.Path)
	out := map[string]any{"players": *players, "score_rows": scoreRows, "seasons": seasonIDs, "seed": *seed,
		"saves_per_player": map[string]int{benchdata.SlotBig: benchdata.BigBytes, benchdata.SlotSmall: benchdata.SmallBytes},
		"db_bytes":         st.Size(), "elapsed_s": time.Since(start).Seconds()}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

// checksum must match kits/game checksumOf (sha256 hex of the payload bytes).
func checksum(b []byte) string { return auth.Hash(string(b)) }

func verifyDB(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	q := func(s string, args ...any) int64 {
		var n int64
		if err := db.QueryRowContext(ctx, s, args...).Scan(&n); err != nil {
			log.Fatalf("%s: %v", s, err)
		}
		return n
	}
	g := benchdata.GameID
	claims := q(`SELECT count(*) FROM reward_claims WHERE game_id=? AND reward_id=?`, g, benchdata.Reward)
	ledger := q(`SELECT count(*) FROM game_ledger WHERE game_id=? AND kind='reward' AND asset='coin'`, g)
	ledgerSum := q(`SELECT coalesce(sum(amount),0) FROM game_ledger WHERE game_id=? AND asset='coin'`, g)
	balSum := q(`SELECT coalesce(sum(balance),0) FROM wallet_balances WHERE game_id=? AND asset='coin'`, g)
	dupSlots := q(`SELECT count(*) FROM (SELECT 1 FROM reward_claims WHERE game_id=? GROUP BY player_id, reward_id, period_key, seq HAVING count(*)>1)`, g)
	out := map[string]any{"reward_claims": claims, "ledger_reward_rows": ledger, "ledger_coin_sum": ledgerSum, "wallet_coin_sum": balSum,
		"duplicate_claim_slots": dupSlots, "ok": claims == ledger && ledgerSum == balSum && claims == ledgerSum && dupSlots == 0}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}
