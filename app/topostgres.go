package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pillar/gostack/adapters/postgres"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/migrations"
	"github.com/pillar/gostack/modules/billing"
)

// ToPostgresOptions configures MigrateToPostgres.
type ToPostgresOptions struct {
	From          string // SQLite main database file
	AnalyticsFrom string // optional SQLite analytics database file
	DSN           string // PostgreSQL target (main schema)
	AnalyticsDSN  string // analytics target (schema gostack_analytics); default DSN
}

// TableCopy is the per-table verification result.
type TableCopy struct {
	Table        string `json:"table"`
	SourceRows   int64  `json:"source_rows"`
	TargetRows   int64  `json:"target_rows"`
	SourceDigest string `json:"source_digest"`
	TargetDigest string `json:"target_digest"`
	OK           bool   `json:"ok"`
}

// ToPostgresReport is printed as JSON by `migrate to-postgres`.
type ToPostgresReport struct {
	OK               bool               `json:"ok"`
	From             string             `json:"from"`
	SchemaVersion    int                `json:"schema_version"`
	Tables           []TableCopy        `json:"tables"`
	AnalyticsFrom    string             `json:"analytics_from,omitempty"`
	AnalyticsTables  []TableCopy        `json:"analytics_tables,omitempty"`
	GameReconcile    []game.Mismatch    `json:"game_reconcile_mismatches"`
	BillingReconcile []billing.Mismatch `json:"billing_reconcile_mismatches"`
	Committed        bool               `json:"committed"`
	DurationMS       int64              `json:"duration_ms"`
	Problems         []string           `json:"problems"`
}

func migrateToPostgres(ctx context.Context, args []string, env Env) error {
	fs := flag.NewFlagSet("migrate to-postgres", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "SQLite main database file")
	toRef := fs.String("to-dsn-ref", "", "target PostgreSQL DSN secret reference (env:NAME or file:/path)")
	afrom := fs.String("analytics-from", "", "SQLite analytics database file (optional)")
	atoRef := fs.String("analytics-to-dsn-ref", "", "analytics target DSN reference (default: --to-dsn-ref)")
	usage := UsageError{"usage: migrate to-postgres --from APP.DB --to-dsn-ref env:NAME [--analytics-from ANALYTICS.DB] [--analytics-to-dsn-ref env:NAME]"}
	if err := fs.Parse(args); err != nil || *from == "" || !config.IsSecretRef(*toRef) || fs.NArg() != 0 ||
		(*atoRef != "" && !config.IsSecretRef(*atoRef)) {
		return usage
	}
	o := ToPostgresOptions{From: *from, AnalyticsFrom: *afrom}
	var err error
	if o.DSN, err = config.ResolveSecret(*toRef); err != nil {
		return fmt.Errorf("--to-dsn-ref: %w", err)
	}
	o.AnalyticsDSN = o.DSN
	if *atoRef != "" {
		if o.AnalyticsDSN, err = config.ResolveSecret(*atoRef); err != nil {
			return fmt.Errorf("--analytics-to-dsn-ref: %w", err)
		}
	}
	rep, err := MigrateToPostgres(ctx, o)
	PrintJSON(env.Stdout, rep)
	if err != nil {
		return err
	}
	if !rep.OK {
		return CheckFailed{"migration to PostgreSQL failed verification: " + strings.Join(rep.Problems, "; ")}
	}
	return nil
}

// MigrateToPostgres copies a stopped SQLite deployment into an empty PostgreSQL
// database (PRD §12). It takes the SQLite instance lock (fails while a server
// runs), requires the source to be fully migrated, applies the PostgreSQL
// migrations to the target, refuses unless every target table is empty, then
// copies all tables in foreign-key order inside ONE PostgreSQL transaction,
// resets identity sequences and, still inside that transaction, compares
// per-table row counts and digests (primary key order, every column) and runs
// the GameKit and billing reconciles. Any mismatch rolls the transaction back,
// leaving the target empty. Analytics (optional) is copied the same way into
// the schema gostack_analytics in a second transaction.
func MigrateToPostgres(ctx context.Context, o ToPostgresOptions) (ToPostgresReport, error) {
	start := time.Now()
	rep := ToPostgresReport{From: o.From, AnalyticsFrom: o.AnalyticsFrom, Tables: []TableCopy{}, Problems: []string{},
		GameReconcile: []game.Mismatch{}, BillingReconcile: []billing.Mismatch{}}
	defer func() { rep.DurationMS = time.Since(start).Milliseconds() }()
	fail := func(format string, a ...any) { rep.Problems = append(rep.Problems, fmt.Sprintf(format, a...)) }

	ver, tables, err := copyDatabase(ctx, o.From, o.DSN, "", migrations.SQLite, "sqlite", fail,
		func(ctx context.Context, tx *sql.Tx) (bool, error) {
			gm, err := game.Reconcile(ctx, tx, "")
			if err != nil {
				return false, fmt.Errorf("game reconcile: %w", err)
			}
			rep.GameReconcile = gm
			if len(gm) > 0 {
				fail("%d wallet balances disagree with the game ledger", len(gm))
			}
			bm, err := billing.ReconcileIn(ctx, tx, "")
			if err != nil {
				return false, fmt.Errorf("billing reconcile: %w", err)
			}
			if bm != nil {
				rep.BillingReconcile = bm
			}
			if len(bm) > 0 {
				fail("%d billing balances disagree with the billing ledger", len(bm))
			}
			return len(gm) == 0 && len(bm) == 0, nil
		})
	rep.SchemaVersion, rep.Tables = ver, tables
	if err != nil || len(rep.Problems) > 0 {
		return rep, err
	}
	rep.Committed = true
	if o.AnalyticsFrom != "" {
		_, at, err := copyDatabase(ctx, o.AnalyticsFrom, o.AnalyticsDSN, AnalyticsSchema, migrations.Analytics, "analytics", fail, nil)
		rep.AnalyticsTables = at
		if err != nil {
			return rep, fmt.Errorf("analytics: %w", err)
		}
	}
	rep.OK = len(rep.Problems) == 0
	return rep, nil
}

type colInfo struct {
	name string
	text bool // TEXT affinity: ordered with COLLATE "C" on PostgreSQL
	pk   int  // 1-based position in the primary key, 0 when not a key column
}

type tableInfo struct {
	name string
	cols []colInfo
	pk   []colInfo
	refs []string
}

// copyDatabase copies one SQLite file into PostgreSQL (schema "" = default).
// verify runs inside the copy transaction after the digests matched.
func copyDatabase(ctx context.Context, from, dsn, schema string, srcFS fs.FS, srcDir string, fail func(string, ...any),
	verify func(context.Context, *sql.Tx) (bool, error)) (int, []TableCopy, error) {
	copies := []TableCopy{}
	if _, err := os.Stat(from); err != nil {
		return 0, copies, fmt.Errorf("source %s: %w", from, err)
	}
	so := sqlite.Options{Path: from, ReadConnections: 1, Synchronous: "FULL", BusyTimeout: 5 * time.Second, WALAutocheckpoint: 1000, WriteQueue: 1}
	src, err := sqlite.Open(so)
	if err != nil {
		if errors.Is(err, sqlite.ErrLocked) {
			return 0, copies, fmt.Errorf("%w; stop the server (and drain its queue) before migrating", err)
		}
		return 0, copies, err
	}
	defer src.Close()
	if err := sqlite.OpenReaders(src, so); err != nil {
		return 0, copies, err
	}
	if err := sqlite.IntegrityCheck(ctx, src); err != nil {
		return 0, copies, fmt.Errorf("source: %w", err)
	}
	sms, err := store.LoadMigrations(srcFS, srcDir)
	if err != nil {
		return 0, copies, err
	}
	sst, err := src.MigrationStatus(ctx, sms)
	if err != nil {
		return 0, copies, err
	}
	if store.Pending(sst) {
		return 0, copies, errors.New("source schema is not exactly this binary's version (pending, drifted or unknown migrations); run `migrate up` on it with this binary first")
	}
	version := sms[len(sms)-1].Version

	dst, err := postgres.Open(ctx, postgres.Options{DSN: dsn, MaxOpenConns: 2, ReadConns: 1, WriteQueue: 4, Schema: schema})
	if err != nil {
		return version, copies, fmt.Errorf("target: %w", err)
	}
	defer dst.Close()
	pfs, pdir := migrations.Main(store.Postgres)
	if schema == AnalyticsSchema {
		pfs, pdir = migrations.AnalyticsFor(store.Postgres)
	}
	pms, err := store.LoadMigrations(pfs, pdir)
	if err != nil {
		return version, copies, err
	}
	if len(pms) != len(sms) || pms[len(pms)-1].Version != version {
		return version, copies, fmt.Errorf("binary bug: %s and %s migrations differ in versions", srcDir, pdir)
	}
	if _, err := dst.Migrate(ctx, pms); err != nil {
		return version, copies, fmt.Errorf("target migrate: %w", err)
	}

	tables, err := sqliteTables(ctx, src.Read)
	if err != nil {
		return version, copies, err
	}
	order, err := fkOrder(tables)
	if err != nil {
		return version, copies, err
	}

	c, err := dst.Write.Conn(ctx)
	if err != nil {
		return version, copies, err
	}
	defer c.Close()
	tx, err := c.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return version, copies, err
	}
	defer tx.Rollback()
	names := make([]string, len(order))
	for i, t := range order {
		names[i] = pgx.Identifier{t.name}.Sanitize()
	}
	if _, err := tx.ExecContext(ctx, `LOCK TABLE `+strings.Join(names, ", ")+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		return version, copies, fmt.Errorf("target: lock tables: %w", err)
	}
	for _, t := range order {
		if err := checkTargetTable(ctx, tx, t); err != nil {
			return version, copies, err
		}
	}
	stx, err := src.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return version, copies, err
	}
	defer stx.Rollback()
	for _, t := range order {
		cp, err := copyTable(ctx, stx, tx, t)
		if err != nil {
			return version, copies, fmt.Errorf("copy %s: %w", t.name, err)
		}
		copies = append(copies, cp)
	}
	if err := resetIdentities(ctx, tx); err != nil {
		return version, copies, err
	}
	for i, t := range order {
		n, d, err := digest(ctx, tx, t, true)
		if err != nil {
			return version, copies, fmt.Errorf("digest %s: %w", t.name, err)
		}
		cp := &copies[i]
		cp.TargetRows, cp.TargetDigest = n, d
		cp.OK = cp.SourceRows == n && cp.SourceDigest == d
		if !cp.OK {
			fail("table %s: source %d rows %s, target %d rows %s", t.name, cp.SourceRows, cp.SourceDigest, n, d)
		}
	}
	clean := true
	for _, cp := range copies {
		clean = clean && cp.OK
	}
	if verify != nil {
		ok, err := verify(ctx, tx)
		if err != nil {
			return version, copies, err
		}
		clean = clean && ok
	}
	if !clean {
		return version, copies, nil // the deferred Rollback leaves the target empty
	}
	return version, copies, tx.Commit()
}

// sqliteTables lists application tables with columns, primary keys and FK targets.
func sqliteTables(ctx context.Context, q *sql.DB) ([]tableInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'schema_migrations' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	var out []tableInfo
	for _, n := range names {
		t := tableInfo{name: n}
		cr, err := q.QueryContext(ctx, `SELECT name, type, pk FROM pragma_table_info(?) ORDER BY cid`, n)
		if err != nil {
			return nil, err
		}
		for cr.Next() {
			var c colInfo
			var typ string
			if err := cr.Scan(&c.name, &typ, &c.pk); err != nil {
				cr.Close()
				return nil, err
			}
			c.text = strings.EqualFold(typ, "TEXT")
			t.cols = append(t.cols, c)
		}
		cr.Close()
		for _, c := range t.cols {
			if c.pk > 0 {
				t.pk = append(t.pk, c)
			}
		}
		slices.SortFunc(t.pk, func(a, b colInfo) int { return a.pk - b.pk })
		if len(t.pk) == 0 {
			return nil, fmt.Errorf("table %s has no primary key; cannot verify a deterministic digest", n)
		}
		fr, err := q.QueryContext(ctx, `SELECT DISTINCT "table" FROM pragma_foreign_key_list(?)`, n)
		if err != nil {
			return nil, err
		}
		for fr.Next() {
			var r string
			if err := fr.Scan(&r); err != nil {
				fr.Close()
				return nil, err
			}
			if r != n {
				t.refs = append(t.refs, r)
			}
		}
		fr.Close()
		out = append(out, t)
	}
	return out, nil
}

// fkOrder sorts tables so referenced tables come first.
func fkOrder(ts []tableInfo) ([]tableInfo, error) {
	byName := map[string]tableInfo{}
	for _, t := range ts {
		byName[t.name] = t
	}
	var out []tableInfo
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(string) error
	visit = func(n string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("foreign key cycle at %s", n)
		case 2:
			return nil
		}
		state[n] = 1
		t, ok := byName[n]
		if !ok {
			return fmt.Errorf("foreign key to unknown table %s", n)
		}
		for _, r := range t.refs {
			if err := visit(r); err != nil {
				return err
			}
		}
		state[n] = 2
		out = append(out, t)
		return nil
	}
	for _, t := range ts {
		if err := visit(t.name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// checkTargetTable verifies the PostgreSQL table has the same columns and no rows.
func checkTargetTable(ctx context.Context, tx *sql.Tx, t tableInfo) error {
	rows, err := tx.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=? ORDER BY column_name`, t.name)
	if err != nil {
		return err
	}
	var got []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return err
		}
		got = append(got, c)
	}
	rows.Close()
	var want []string
	for _, c := range t.cols {
		want = append(want, c.name)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		return fmt.Errorf("target table %s columns %v differ from source %v", t.name, got, want)
	}
	var any bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgx.Identifier{t.name}.Sanitize()+`)`).Scan(&any); err != nil {
		return err
	}
	if any {
		return fmt.Errorf("target table %s is not empty; migrate only into a freshly created database", t.name)
	}
	return nil
}

func colList(t tableInfo) string {
	q := make([]string, len(t.cols))
	for i, c := range t.cols {
		q[i] = `"` + c.name + `"`
	}
	return strings.Join(q, ",")
}

func orderBy(t tableInfo, pg bool) string {
	q := make([]string, len(t.pk))
	for i, c := range t.pk {
		q[i] = `"` + c.name + `"`
		if pg && c.text {
			q[i] += ` COLLATE "C"`
		}
	}
	return strings.Join(q, ",")
}

// copyTable streams rows from the SQLite snapshot into PostgreSQL with
// multi-row INSERTs and returns the source count and digest.
func copyTable(ctx context.Context, src, dst *sql.Tx, t tableInfo) (TableCopy, error) {
	cp := TableCopy{Table: t.name}
	rows, err := src.QueryContext(ctx, `SELECT `+colList(t)+` FROM "`+t.name+`" ORDER BY `+orderBy(t, false))
	if err != nil {
		return cp, err
	}
	defer rows.Close()
	h := sha256.New()
	nc := len(t.cols)
	batch := max(1, min(500, 30000/nc))
	ph := "(" + strings.TrimSuffix(strings.Repeat("?,", nc), ",") + ")"
	insert := `INSERT INTO ` + pgx.Identifier{t.name}.Sanitize() + `(` + colList(t) + `) VALUES `
	var args []any
	flush := func(n int) error {
		if n == 0 {
			return nil
		}
		q := insert + strings.TrimSuffix(strings.Repeat(ph+",", n), ",")
		_, err := dst.ExecContext(ctx, q, args...)
		args = args[:0]
		return err
	}
	pending := 0
	for rows.Next() {
		vals := make([]any, nc)
		ptrs := make([]any, nc)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return cp, err
		}
		writeRow(h, vals)
		args = append(args, vals...)
		cp.SourceRows++
		pending++
		if pending == batch {
			if err := flush(pending); err != nil {
				return cp, err
			}
			pending = 0
		}
	}
	if err := rows.Err(); err != nil {
		return cp, err
	}
	if err := flush(pending); err != nil {
		return cp, err
	}
	cp.SourceDigest = hex.EncodeToString(h.Sum(nil))
	return cp, nil
}

// digest hashes every row of t in primary-key order (bytewise for text keys).
func digest(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, t tableInfo, pg bool) (int64, string, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+colList(t)+` FROM "`+t.name+`" ORDER BY `+orderBy(t, pg))
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	h := sha256.New()
	var n int64
	vals := make([]any, len(t.cols))
	ptrs := make([]any, len(t.cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return 0, "", err
		}
		writeRow(h, vals)
		n++
	}
	return n, hex.EncodeToString(h.Sum(nil)), rows.Err()
}

// writeRow feeds a canonical, typed encoding of one row into h.
func writeRow(h io.Writer, vals []any) {
	var b []byte
	for _, v := range vals {
		switch x := v.(type) {
		case nil:
			b = append(b, 'n')
		case int64:
			b = append(b, 'i')
			b = strconv.AppendInt(b, x, 10)
		case float64:
			b = append(b, 'f')
			b = strconv.AppendFloat(b, x, 'g', -1, 64)
		case string:
			b = append(b, 's')
			b = strconv.AppendInt(b, int64(len(x)), 10)
			b = append(b, ':')
			b = append(b, x...)
		case []byte:
			b = append(b, 'b')
			b = strconv.AppendInt(b, int64(len(x)), 10)
			b = append(b, ':')
			b = append(b, x...)
		default:
			b = append(b, 'x')
			b = append(b, fmt.Sprintf("%T:%v", x, x)...)
		}
		b = append(b, 0x1f)
	}
	b = append(b, 0x1e)
	h.Write(b)
}

// resetIdentities moves every identity sequence past the copied values.
func resetIdentities(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema=current_schema() AND is_identity='YES'`)
	if err != nil {
		return err
	}
	var pairs [][2]string
	for rows.Next() {
		var t, c string
		if err := rows.Scan(&t, &c); err != nil {
			rows.Close()
			return err
		}
		pairs = append(pairs, [2]string{t, c})
	}
	rows.Close()
	for _, p := range pairs {
		t, c := pgx.Identifier{p[0]}.Sanitize(), pgx.Identifier{p[1]}.Sanitize()
		if _, err := tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence(?, ?), coalesce((SELECT max(`+c+`) FROM `+t+`), 0) + 1, false)`,
			t, p[1]); err != nil {
			return fmt.Errorf("reset identity %s.%s: %w", p[0], p[1], err)
		}
	}
	return nil
}
