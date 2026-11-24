package postgres

import "testing"

func TestRebind(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`SELECT 1`, `SELECT 1`},
		{`SELECT * FROM t WHERE a=? AND b=?`, `SELECT * FROM t WHERE a=$1 AND b=$2`},
		{`INSERT INTO t VALUES(?,?,?)`, `INSERT INTO t VALUES($1,$2,$3)`},
		// literals, identifiers and comments are left alone
		{`SELECT '?', "a?b", ? -- what?
			FROM t /* ? /* nested ? */ ? */ WHERE x=?`, `SELECT '?', "a?b", $1 -- what?
			FROM t /* ? /* nested ? */ ? */ WHERE x=$2`},
		{`SELECT 'it''s ?', ?`, `SELECT 'it''s ?', $1`},
		{`SELECT E'\'?', ?`, `SELECT E'\'?', $1`},
		{`SELECT "q""?", ?`, `SELECT "q""?", $1`},
		// '??' is an escaped literal '?' (PostgreSQL jsonb operators)
		{`SELECT doc ?? 'k' FROM t WHERE id=?`, `SELECT doc ? 'k' FROM t WHERE id=$1`},
		{`SELECT ???`, `SELECT ?$1`},
		// SQLite numbered parameters, and plain '?' continuing after the largest index
		{`UPDATE a SET x=x+?1, y=y+?2 WHERE id=?3 AND x+?1>=0`, `UPDATE a SET x=x+$1, y=y+$2 WHERE id=$3 AND x+$1>=0`},
		{`SELECT ?2, ?`, `SELECT $2, $3`},
		// dollar quoting
		{`CREATE FUNCTION f() RETURNS int AS $$ SELECT '?' $$; SELECT ?`, `CREATE FUNCTION f() RETURNS int AS $$ SELECT '?' $$; SELECT $1`},
		{`DO $body$ BEGIN PERFORM ?; END $body$; SELECT ?`, `DO $body$ BEGIN PERFORM ?; END $body$; SELECT $1`},
		{`SELECT $1, ?`, `SELECT $1, $1`},
		{`SELECT a$b, ?`, `SELECT a$b, $1`},
		// unterminated tokens extend to the end
		{`SELECT '? `, `SELECT '? `},
		{`SELECT ? -- trailing ?`, `SELECT $1 -- trailing ?`},
	} {
		if got := Rebind(c.in); got != c.want {
			t.Errorf("Rebind(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func BenchmarkRebind(b *testing.B) {
	q := `SELECT a,b,c FROM t WHERE game_id=? AND player_id=? AND status IN ('a','b') ORDER BY x LIMIT ?`
	for i := 0; i < b.N; i++ {
		Rebind(q)
	}
}
