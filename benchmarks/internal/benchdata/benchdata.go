// Package benchdata holds the identifiers shared by the seeding tool (gsseed)
// and the load generator (gsload) so that both derive the same players and
// session tokens from a fixed seed. The tokens are deterministic and therefore
// NOT secret: use them only against a throwaway benchmark database.
package benchdata

import (
	"fmt"
	"strings"
)

const (
	GameID      = "bench"
	Board       = "main"
	Reward      = "bench_coin"
	SlotBig     = "main"    // 8 KiB save (PRD §23)
	SlotSmall   = "profile" // ~1 KiB save used for indexed point reads (PRD §13 "response ≈1 KiB")
	BigBytes    = 8 << 10
	SmallBytes  = 1 << 10
	DefaultSeed = 20261125
)

// PlayerID is the deterministic id of player i.
func PlayerID(i int) string { return fmt.Sprintf("plr_bench%07d", i) }

// Token is the deterministic session token of player i.
func Token(seed uint64, i int) string { return fmt.Sprintf("gst.bench-%d-%07d", seed, i) }

// Payload returns a JSON object of exactly n bytes whose content depends on (seed, i, rev).
func Payload(n int, seed uint64, i int, rev int64) []byte {
	head := fmt.Sprintf(`{"v":1,"p":%d,"r":%d,"s":%d,"blob":"`, i, rev, seed)
	pad := n - len(head) - 2
	var b strings.Builder
	b.Grow(n)
	b.WriteString(head)
	x := seed ^ uint64(i)*0x9E3779B97F4A7C15 ^ uint64(rev)<<32
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for range pad {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b.WriteByte(alphabet[x%uint64(len(alphabet))])
	}
	b.WriteString(`"}`)
	return []byte(b.String())
}

// ConfigDoc is the GameKit config published for the benchmark game.
const ConfigDoc = `{
  "client": {"welcome": "benchmark", "tuning": {"spawn_rate": 3, "max_level": 120, "motd": "GoStack benchmark configuration document"}},
  "boards": {"main": {"order": "desc", "mode": "best", "period": "weekly", "min_score": 0, "max_score": 100000000}},
  "rewards": {"bench_coin": {"grants": {"coin": 1}, "period": "daily", "per_period": 1000}}
}`
