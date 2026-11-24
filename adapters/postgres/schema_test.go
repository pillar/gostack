package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/postgres"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/migrations"
)

// schema is a dialect-neutral description used to compare the SQLite and
// PostgreSQL migrations: every entry is a canonical string.
type schema struct {
	tables  []string // name
	columns []string // table.column type notnull
	pks     []string // table(cols)
	uniques []string // table(cols) [partial] for UNIQUE constraints and unique indexes
	indexes []string // name: table(cols) [unique] [partial] for explicitly named indexes
	fks     []string // table(cols) -> ref(cols)
	trigs   []string // trigger names
}

func (s *schema) sort() {
	for _, l := range [][]string{s.tables, s.columns, s.pks, s.uniques, s.indexes, s.fks, s.trigs} {
		sort.Strings(l)
	}
}

func canonType(t string) string {
	switch strings.ToLower(t) {
	case "integer", "bigint":
		return "int"
	case "text":
		return "text"
	case "blob", "bytea":
		return "bytes"
	}
	return "?" + t
}

func sqliteSchema(t *testing.T, fsys fs.FS, dir string) schema {
	t.Helper()
	o := sqlite.Options{Path: filepath.Join(t.TempDir(), "s.db"), ReadConnections: 1, Synchronous: "FULL",
		BusyTimeout: time.Second, WALAutocheckpoint: 1000, WriteQueue: 4}
	db, err := sqlite.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ms, err := store.LoadMigrations(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	q := db.Write
	var s schema
	for _, tbl := range strs(t, q, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'schema_migrations' ORDER BY name`) {
		s.tables = append(s.tables, tbl)
		rows, err := q.Query(`SELECT name, type, "notnull", pk FROM pragma_table_info(?) ORDER BY cid`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		pk := map[int]string{}
		for rows.Next() {
			var name, typ string
			var nn, p int
			if err := rows.Scan(&name, &typ, &nn, &p); err != nil {
				t.Fatal(err)
			}
			s.columns = append(s.columns, fmt.Sprintf("%s.%s %s %v", tbl, name, canonType(typ), nn == 1 || p > 0))
			if p > 0 {
				pk[p] = name
			}
		}
		rows.Close()
		var pkc []string
		for i := 1; i <= len(pk); i++ {
			pkc = append(pkc, pk[i])
		}
		s.pks = append(s.pks, fmt.Sprintf("%s(%s)", tbl, strings.Join(pkc, ",")))
		type idx struct {
			name, origin    string
			unique, partial bool
		}
		var idxs []idx
		rows, err = q.Query(`SELECT name, "unique", origin, partial FROM pragma_index_list(?)`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var x idx
			if err := rows.Scan(&x.name, &x.unique, &x.origin, &x.partial); err != nil {
				t.Fatal(err)
			}
			idxs = append(idxs, x)
		}
		rows.Close()
		for _, x := range idxs {
			cols := strings.Join(strs(t, q, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, x.name), ",")
			desc := fmt.Sprintf("%s(%s)%s", tbl, cols, flag(x.partial, " partial"))
			switch {
			case x.origin == "pk":
			case x.origin == "u":
				s.uniques = append(s.uniques, desc)
			default: // CREATE INDEX
				if x.unique {
					s.uniques = append(s.uniques, desc)
				}
				s.indexes = append(s.indexes, fmt.Sprintf("%s: %s%s", x.name, desc, flag(x.unique, " unique")))
			}
		}
		rows, err = q.Query(`SELECT id, "table", "from", "to" FROM pragma_foreign_key_list(?) ORDER BY id, seq`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		fk := map[int][3][]string{}
		var ids []int
		for rows.Next() {
			var id int
			var ref, from string
			var to sql.NullString
			if err := rows.Scan(&id, &ref, &from, &to); err != nil {
				t.Fatal(err)
			}
			e, seen := fk[id]
			if !seen {
				ids = append(ids, id)
			}
			e[0] = []string{ref}
			e[1] = append(e[1], from)
			e[2] = append(e[2], to.String)
			fk[id] = e
		}
		rows.Close()
		for _, id := range ids {
			e := fk[id]
			s.fks = append(s.fks, fmt.Sprintf("%s(%s) -> %s(%s)", tbl, strings.Join(e[1], ","), e[0][0], strings.Join(e[2], ",")))
		}
	}
	s.trigs = strs(t, q, `SELECT name FROM sqlite_master WHERE type='trigger'`)
	s.sort()
	return s
}

func pgSchema(t *testing.T, fsys fs.FS, dir, sch string) schema {
	t.Helper()
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{DSN: pgtest.NewDatabase(t), Schema: sch})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ms, err := store.LoadMigrations(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, ms); err != nil {
		t.Fatal(err)
	}
	q := db.Write
	var s schema
	s.tables = strs(t, q, `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' AND table_name<>'schema_migrations'`)
	rows, err := q.Query(`SELECT table_name, column_name, data_type, is_nullable FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name<>'schema_migrations'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tbl, col, typ, nullable string
		if err := rows.Scan(&tbl, &col, &typ, &nullable); err != nil {
			t.Fatal(err)
		}
		s.columns = append(s.columns, fmt.Sprintf("%s.%s %s %v", tbl, col, canonType(typ), nullable == "NO"))
	}
	rows.Close()
	// Indexes (constraint-backed and explicit) with their columns in key order.
	rows, err = q.Query(`SELECT ic.relname, t.relname, i.indisunique, i.indisprimary, i.indpred IS NOT NULL,
			c.contype IS NOT NULL,
			(SELECT string_agg(a.attname, ',' ORDER BY k.n) FROM unnest(i.indkey) WITH ORDINALITY k(attnum, n)
			   JOIN pg_attribute a ON a.attrelid=t.oid AND a.attnum=k.attnum)
		FROM pg_index i JOIN pg_class ic ON ic.oid=i.indexrelid JOIN pg_class t ON t.oid=i.indrelid
		LEFT JOIN pg_constraint c ON c.conindid=i.indexrelid AND c.contype IN ('p','u')
		WHERE t.relnamespace = current_schema()::regnamespace AND t.relname<>'schema_migrations'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, tbl, cols string
		var unique, primary, partial, constraint bool
		if err := rows.Scan(&name, &tbl, &unique, &primary, &partial, &constraint, &cols); err != nil {
			t.Fatal(err)
		}
		desc := fmt.Sprintf("%s(%s)%s", tbl, cols, flag(partial, " partial"))
		switch {
		case primary:
			s.pks = append(s.pks, fmt.Sprintf("%s(%s)", tbl, cols))
		case constraint:
			s.uniques = append(s.uniques, desc)
		default:
			if unique {
				s.uniques = append(s.uniques, desc)
			}
			s.indexes = append(s.indexes, fmt.Sprintf("%s: %s%s", name, desc, flag(unique, " unique")))
		}
	}
	rows.Close()
	rows, err = q.Query(`SELECT t.relname, r.relname,
			(SELECT string_agg(a.attname, ',' ORDER BY k.n) FROM unnest(c.conkey) WITH ORDINALITY k(attnum, n) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.attnum),
			(SELECT string_agg(a.attname, ',' ORDER BY k.n) FROM unnest(c.confkey) WITH ORDINALITY k(attnum, n) JOIN pg_attribute a ON a.attrelid=c.confrelid AND a.attnum=k.attnum)
		FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_class r ON r.oid=c.confrelid
		WHERE c.contype='f' AND t.relnamespace = current_schema()::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tbl, ref, from, to string
		if err := rows.Scan(&tbl, &ref, &from, &to); err != nil {
			t.Fatal(err)
		}
		s.fks = append(s.fks, fmt.Sprintf("%s(%s) -> %s(%s)", tbl, from, ref, to))
	}
	rows.Close()
	s.trigs = strs(t, q, `SELECT tgname FROM pg_trigger tg JOIN pg_class t ON t.oid=tg.tgrelid
		WHERE NOT tg.tgisinternal AND t.relnamespace = current_schema()::regnamespace`)
	s.sort()
	return s
}

func flag(b bool, s string) string {
	if b {
		return s
	}
	return ""
}

func strs(t *testing.T, q *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := q.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func diff(t *testing.T, what string, sq, pg []string) {
	t.Helper()
	if slices.Equal(sq, pg) {
		return
	}
	var onlyS, onlyP []string
	for _, x := range sq {
		if !slices.Contains(pg, x) {
			onlyS = append(onlyS, x)
		}
	}
	for _, x := range pg {
		if !slices.Contains(sq, x) {
			onlyP = append(onlyP, x)
		}
	}
	t.Errorf("%s differ:\n  only sqlite:   %v\n  only postgres: %v", what, onlyS, onlyP)
}

// The PostgreSQL migrations must define the same tables, columns (type class
// and nullability), primary keys, unique constraints, named indexes, foreign
// keys and append-only triggers as the SQLite ones. PostgreSQL additionally
// blocks TRUNCATE with *_immutable_truncate triggers.
func TestSchemaEquivalence(t *testing.T) {
	pgtest.RequirePostgres(t)
	for _, c := range []struct {
		name               string
		sfs, pfs           fs.FS
		sdir, pdir, schema string
	}{
		{"main", migrations.SQLite, migrations.Postgres, "sqlite", "postgres", ""},
		{"analytics", migrations.Analytics, migrations.PostgresAnalytics, "analytics", "postgres_analytics", "gostack_analytics"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sms, _ := store.LoadMigrations(c.sfs, c.sdir)
			pms, _ := store.LoadMigrations(c.pfs, c.pdir)
			if len(sms) != len(pms) {
				t.Fatalf("%d sqlite vs %d postgres migrations", len(sms), len(pms))
			}
			for i := range sms {
				if sms[i].Version != pms[i].Version || sms[i].Name != pms[i].Name {
					t.Errorf("migration %d: %s vs %s", i, sms[i].Name, pms[i].Name)
				}
			}
			s, p := sqliteSchema(t, c.sfs, c.sdir), pgSchema(t, c.pfs, c.pdir, c.schema)
			if len(s.tables) == 0 {
				t.Fatal("no tables introspected")
			}
			t.Logf("compared %d tables, %d columns, %d primary keys, %d unique, %d indexes, %d foreign keys, %d triggers",
				len(s.tables), len(s.columns), len(s.pks), len(s.uniques), len(s.indexes), len(s.fks), len(s.trigs))
			diff(t, "tables", s.tables, p.tables)
			diff(t, "columns", s.columns, p.columns)
			diff(t, "primary keys", s.pks, p.pks)
			diff(t, "unique constraints", s.uniques, p.uniques)
			diff(t, "indexes", s.indexes, p.indexes)
			diff(t, "foreign keys", s.fks, p.fks)
			var pgTrigs []string
			for _, x := range p.trigs {
				if !strings.HasSuffix(x, "_immutable_truncate") {
					pgTrigs = append(pgTrigs, x)
				}
			}
			diff(t, "triggers", s.trigs, pgTrigs)
		})
	}
}
