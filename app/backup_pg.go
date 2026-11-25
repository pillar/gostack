package app

// PostgreSQL (profile scale) logical backups: `backup snapshot|verify|restore`
// built on pg_dump/pg_restore. Point-in-time recovery (WAL archiving + base
// backups) is a server-level procedure; see deploy/postgres and
// docs/DISASTER_RECOVERY.md.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pillar/gostack/adapters/postgres"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/migrations"
	"github.com/pillar/gostack/modules/billing"
)

// PGBackupFormat identifies the sidecar manifest written next to a pg_dump archive.
const PGBackupFormat = "gostack-pg-backup/1"

// pgDumpMagic starts every pg_dump custom-format archive.
var pgDumpMagic = []byte("PGDMP")

// PGManifest is the JSON sidecar (FILE.json) of a PostgreSQL snapshot. Row
// counts and ledger sums are taken inside the same exported snapshot that
// pg_dump reads, so a restore must reproduce them exactly.
type PGManifest struct {
	Format         string     `json:"format"`
	CreatedAt      time.Time  `json:"created_at"`
	GoStackVersion string     `json:"gostack_version"`
	ServerVersion  string     `json:"server_version"`
	PgDumpVersion  string     `json:"pg_dump_version"`
	Main           PGDumpPart `json:"main"`
	// Analytics is set when analytics lives in a separate database
	// (database.analytics_dsn_ref); otherwise its schema is inside Main.
	Analytics *PGDumpPart `json:"analytics,omitempty"`
}

// PGDumpPart describes one pg_dump archive and the data it must contain.
type PGDumpPart struct {
	File    string   `json:"file"` // base name, next to the manifest
	Bytes   int64    `json:"bytes"`
	SHA256  string   `json:"sha256"`
	Schemas []string `json:"schemas"`
	// SchemaVersion is the highest applied migration per schema.
	SchemaVersion map[string]int   `json:"schema_version"`
	Tables        map[string]int64 `json:"row_counts"`
	Ledger        map[string]int64 `json:"ledger_sums"`
}

// PGSnapshotInfo is printed by `backup snapshot` on PostgreSQL.
type PGSnapshotInfo struct {
	Path      string     `json:"path"`
	Manifest  string     `json:"manifest"`
	Analytics string     `json:"analytics_path,omitempty"`
	Info      PGManifest `json:"info"`
}

// PGVerifyReport is the result of `backup verify` / `backup restore` on PostgreSQL.
type PGVerifyReport struct {
	OK               bool               `json:"ok"`
	File             string             `json:"file"`
	Bytes            int64              `json:"bytes"`
	SHA256           string             `json:"sha256"`
	Database         string             `json:"database"` // temporary (verify) or target (restore) database name
	Restore          string             `json:"restore"`
	Schema           schemaSummary      `json:"schema"`
	AnalyticsSchema  *schemaSummary     `json:"analytics_schema"`
	Reconcile        []game.Mismatch    `json:"reconcile_mismatches"`         // null when the GameKit tables are absent
	BillingReconcile []billing.Mismatch `json:"billing_reconcile_mismatches"` // null when the billing tables are absent
	ManifestDiffs    []string           `json:"manifest_differences"`
	Problems         []string           `json:"problems"`
}

// ---------------------------------------------------------------- DSN handling

// pgDSN is a parsed libpq connection string (URL or key=value form) so the
// password can be passed to pg_dump/pg_restore through PGPASSWORD instead of argv.
type pgDSN struct {
	u  *url.URL
	kv [][2]string
}

func parsePGDSN(s string) (pgDSN, error) {
	if _, err := pgx.ParseConfig(s); err != nil {
		return pgDSN{}, errors.New("invalid PostgreSQL DSN") // never echo the DSN
	}
	if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
		u, err := url.Parse(s)
		if err != nil {
			return pgDSN{}, errors.New("invalid PostgreSQL DSN")
		}
		return pgDSN{u: u}, nil
	}
	kv, err := splitKV(s)
	return pgDSN{kv: kv}, err
}

// splitKV tokenizes key=value pairs; values may be single-quoted with backslash escapes.
func splitKV(s string) ([][2]string, error) {
	var out [][2]string
	i := 0
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
			i++
		}
		if i >= len(s) {
			return out, nil
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, errors.New("invalid PostgreSQL DSN")
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		for i < len(s) && s[i] == ' ' {
			i++
		}
		var val strings.Builder
		if i < len(s) && s[i] == '\'' {
			i++
			for ; i < len(s) && s[i] != '\''; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				val.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("invalid PostgreSQL DSN")
			}
			i++
		} else {
			for ; i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				val.WriteByte(s[i])
			}
		}
		out = append(out, [2]string{key, val.String()})
	}
}

func (d pgDSN) password() string {
	if d.u != nil {
		if p := d.u.Query().Get("password"); p != "" {
			return p
		}
		if d.u.User != nil {
			p, _ := d.u.User.Password()
			return p
		}
		return ""
	}
	for _, f := range d.kv {
		if f[0] == "password" {
			return f[1]
		}
	}
	return ""
}

func (d pgDSN) dbName() string {
	if d.u != nil {
		return strings.TrimPrefix(d.u.Path, "/")
	}
	for _, f := range d.kv {
		if f[0] == "dbname" {
			return f[1]
		}
	}
	return ""
}

// render returns the connection string, with or without the password.
func (d pgDSN) render(withPassword bool) string {
	if d.u != nil {
		u := *d.u
		q := u.Query()
		if !withPassword {
			q.Del("password")
			if u.User != nil {
				u.User = url.User(u.User.Username())
			}
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	var b strings.Builder
	for _, f := range d.kv {
		if f[0] == "password" && !withPassword {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		v := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(f[1])
		fmt.Fprintf(&b, "%s='%s'", f[0], v)
	}
	return b.String()
}

func (d pgDSN) withDB(name string) pgDSN {
	if d.u != nil {
		u := *d.u
		u.Path = "/" + name
		u.RawPath = ""
		return pgDSN{u: &u}
	}
	kv := make([][2]string, 0, len(d.kv)+1)
	for _, f := range d.kv {
		if f[0] != "dbname" {
			kv = append(kv, f)
		}
	}
	return pgDSN{kv: append(kv, [2]string{"dbname", name})}
}

// toolEnv is the environment for pg_dump/pg_restore: the password (if the DSN
// has one) travels in PGPASSWORD, never in argv.
func (d pgDSN) toolEnv() []string {
	env := os.Environ()
	if p := d.password(); p != "" {
		out := env[:0:0]
		for _, e := range env {
			if !strings.HasPrefix(e, "PGPASSWORD=") {
				out = append(out, e)
			}
		}
		env = append(out, "PGPASSWORD="+p)
	}
	return env
}

func resolvePGDSN(ref string) (pgDSN, error) {
	s, err := config.ResolveSecret(ref)
	if err != nil {
		return pgDSN{}, err
	}
	return parsePGDSN(s)
}

func openPlain(d pgDSN) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(d.render(true))
	if err != nil {
		return nil, errors.New("invalid PostgreSQL DSN")
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(2)
	return db, nil
}

// errToolMissing is an environment problem (exit 1), not a failed check.
type errToolMissing struct{ error }

// runTool runs a PostgreSQL client program and returns its stderr on failure.
func runTool(ctx context.Context, d pgDSN, name string, args ...string) error {
	bin, err := osexec.LookPath(name)
	if err != nil {
		return errToolMissing{fmt.Errorf("%s not found in PATH (install the PostgreSQL client tools, same or newer major version as the server)", name)}
	}
	cmd := osexec.CommandContext(ctx, bin, args...)
	cmd.Env = d.toolEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[:2000] + "..."
		}
		return fmt.Errorf("%s failed: %v: %s", name, err, msg)
	}
	return nil
}

func toolVersion(name string) string {
	out, err := osexec.Command(name, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------- statistics

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func qident(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

// pgPartStats counts every base table in schemas and sums the ledgers.
func pgPartStats(ctx context.Context, q queryer, schemas []string) (PGDumpPart, error) {
	p := PGDumpPart{Schemas: schemas, SchemaVersion: map[string]int{}, Tables: map[string]int64{}, Ledger: map[string]int64{}}
	rows, err := q.QueryContext(ctx, `SELECT table_schema, table_name FROM information_schema.tables
		WHERE table_type='BASE TABLE' AND table_schema = ANY($1) ORDER BY 1, 2`, schemas)
	if err != nil {
		return p, err
	}
	var tabs [][2]string
	for rows.Next() {
		var s, t string
		if err := rows.Scan(&s, &t); err != nil {
			rows.Close()
			return p, err
		}
		tabs = append(tabs, [2]string{s, t})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return p, err
	}
	for _, st := range tabs {
		var n int64
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM `+qident(st[0], st[1])).Scan(&n); err != nil {
			return p, err
		}
		p.Tables[st[0]+"."+st[1]] = n
		switch st[1] {
		case "schema_migrations":
			var v int
			if err := q.QueryRowContext(ctx, `SELECT coalesce(max(version),0) FROM `+qident(st[0], st[1])).Scan(&v); err != nil {
				return p, err
			}
			p.SchemaVersion[st[0]] = v
		case "game_ledger":
			r, err := q.QueryContext(ctx, `SELECT asset, coalesce(sum(amount),0)::bigint FROM `+qident(st[0], st[1])+` GROUP BY asset`)
			if err != nil {
				return p, err
			}
			for r.Next() {
				var a string
				var s int64
				if err := r.Scan(&a, &s); err != nil {
					r.Close()
					return p, err
				}
				p.Ledger[st[0]+".game_ledger."+a] = s
			}
			if err := errors.Join(r.Err(), r.Close()); err != nil {
				return p, err
			}
		case "billing_ledger_entries":
			var a, b int64
			if err := q.QueryRowContext(ctx, `SELECT coalesce(sum(delta_available),0)::bigint, coalesce(sum(delta_reserved),0)::bigint FROM `+qident(st[0], st[1])).Scan(&a, &b); err != nil {
				return p, err
			}
			p.Ledger[st[0]+".billing_ledger_entries.delta_available"] = a
			p.Ledger[st[0]+".billing_ledger_entries.delta_reserved"] = b
		}
	}
	return p, nil
}

func diffStats(label string, want, got PGDumpPart) []string {
	var out []string
	cmp := func(kind string, w, g map[string]int64) {
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			wv, wok := w[k]
			gv, gok := g[k]
			switch {
			case !gok:
				out = append(out, fmt.Sprintf("%s %s %s missing after restore (manifest %d)", label, kind, k, wv))
			case !wok:
				out = append(out, fmt.Sprintf("%s %s %s not in manifest (restored %d)", label, kind, k, gv))
			case wv != gv:
				out = append(out, fmt.Sprintf("%s %s %s: manifest %d, restored %d", label, kind, k, wv, gv))
			}
		}
	}
	cmp("rows", want.Tables, got.Tables)
	cmp("ledger", want.Ledger, got.Ledger)
	for s, v := range want.SchemaVersion {
		if got.SchemaVersion[s] != v {
			out = append(out, fmt.Sprintf("%s schema %s version: manifest %d, restored %d", label, s, v, got.SchemaVersion[s]))
		}
	}
	return out
}

// ---------------------------------------------------------------- snapshot

func pgAnalyticsSeparate(d config.DatabaseConfig) bool {
	return d.AnalyticsDSNRef != "" && d.AnalyticsDSNRef != d.DSNRef
}

func pgPaths(to string) (manifest, analytics string) { return to + ".json", to + ".analytics" }

// SnapshotPostgres writes a pg_dump custom-format archive of the main database
// to `to` (plus to+".analytics" when analytics is a separate database) and the
// manifest to+".json". No target may exist. Counts and ledger sums are read in
// the snapshot that pg_dump exports, so they describe exactly the dumped data.
func SnapshotPostgres(ctx context.Context, d config.DatabaseConfig, to string) (PGSnapshotInfo, error) {
	man, an := pgPaths(to)
	sep := pgAnalyticsSeparate(d)
	targets := []string{to, man}
	if sep {
		targets = append(targets, an)
	}
	for _, p := range targets {
		if _, err := os.Lstat(p); err == nil {
			return PGSnapshotInfo{}, fmt.Errorf("refusing to overwrite existing %s", p)
		}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return PGSnapshotInfo{}, err
	}
	main, err := resolvePGDSN(d.DSNRef)
	if err != nil {
		return PGSnapshotInfo{}, fmt.Errorf("database dsn: %w", err)
	}
	m := PGManifest{Format: PGBackupFormat, CreatedAt: time.Now().UTC(), GoStackVersion: Version, PgDumpVersion: toolVersion("pg_dump")}
	info := PGSnapshotInfo{Path: to, Manifest: man}
	var tmps []string
	defer func() {
		for _, t := range tmps {
			os.Remove(t)
		}
	}()
	// mode: 0 main database incl. the analytics schema, 1 main database
	// without it (analytics is separate), 2 analytics schema only.
	dump := func(dsn pgDSN, dst string, mode int) (PGDumpPart, string, error) {
		db, err := openPlain(dsn)
		if err != nil {
			return PGDumpPart{}, "", err
		}
		defer db.Close()
		conn, err := db.Conn(ctx)
		if err != nil {
			return PGDumpPart{}, "", fmt.Errorf("connect: %w", err)
		}
		defer conn.Close()
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			return PGDumpPart{}, "", err
		}
		defer tx.Rollback()
		var snap, cur, ver string
		if err := tx.QueryRowContext(ctx, `SELECT pg_export_snapshot(), current_schema(), current_setting('server_version')`).Scan(&snap, &cur, &ver); err != nil {
			return PGDumpPart{}, "", err
		}
		schemas := map[int][]string{0: {cur, AnalyticsSchema}, 1: {cur}, 2: {AnalyticsSchema}}[mode]
		part, err := pgPartStats(ctx, tx, schemas)
		if err != nil {
			return PGDumpPart{}, "", fmt.Errorf("collect statistics: %w", err)
		}
		tmp, err := tempPath(dst)
		if err != nil {
			return PGDumpPart{}, "", err
		}
		tmps = append(tmps, tmp)
		args := []string{"--format=custom", "--snapshot=" + snap, "--file=" + tmp, "--dbname=" + dsn.render(false)}
		switch mode {
		case 1:
			args = append(args, "--exclude-schema="+AnalyticsSchema)
		case 2:
			args = append(args, "--schema="+AnalyticsSchema)
		}
		if err := runTool(ctx, dsn, "pg_dump", args...); err != nil {
			return PGDumpPart{}, "", err
		}
		if err := publish(tmp, dst); err != nil {
			return PGDumpPart{}, "", err
		}
		part.SHA256, part.Bytes, err = fileSHA256(dst)
		part.File = filepath.Base(dst)
		return part, ver, err
	}
	var published []string
	fail := func(err error) (PGSnapshotInfo, error) {
		for _, p := range published {
			os.Remove(p)
		}
		return PGSnapshotInfo{}, err
	}
	mode := 0
	if sep {
		mode = 1
	}
	part, ver, err := dump(main, to, mode)
	if err != nil {
		return fail(err)
	}
	published = append(published, to)
	m.Main, m.ServerVersion = part, ver
	if sep {
		ad, err := resolvePGDSN(d.AnalyticsDSNRef)
		if err != nil {
			return fail(fmt.Errorf("analytics dsn: %w", err))
		}
		ap, _, err := dump(ad, an, 2)
		if err != nil {
			return fail(fmt.Errorf("analytics: %w", err))
		}
		published = append(published, an)
		m.Analytics = &ap
		info.Analytics = an
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp, err := tempPath(man)
	if err != nil {
		return fail(err)
	}
	tmps = append(tmps, tmp)
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fail(err)
	}
	if err := publish(tmp, man); err != nil {
		return fail(err)
	}
	info.Info = m
	return info, nil
}

// ---------------------------------------------------------------- verify / restore

// IsPGDump reports whether file starts with the pg_dump custom-format magic.
func IsPGDump(file string) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, len(pgDumpMagic))
	_, err = io.ReadFull(bufio.NewReader(f), b)
	return err == nil && bytes.Equal(b, pgDumpMagic)
}

// loadPGBackup reads the manifest and checks the archives' checksums.
func loadPGBackup(file string, rep *PGVerifyReport) (PGManifest, bool) {
	var m PGManifest
	sum, n, err := fileSHA256(file)
	if err != nil {
		rep.Problems = append(rep.Problems, "read backup: "+err.Error())
		return m, false
	}
	rep.SHA256, rep.Bytes = sum, n
	if !IsPGDump(file) {
		rep.Problems = append(rep.Problems, "not a pg_dump custom-format archive")
		return m, false
	}
	man, an := pgPaths(file)
	b, err := os.ReadFile(man)
	if err != nil {
		rep.Problems = append(rep.Problems, "manifest "+filepath.Base(man)+" missing or unreadable (a backup is the archive plus its manifest)")
		return m, false
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Format != PGBackupFormat {
		rep.Problems = append(rep.Problems, "manifest is not a "+PGBackupFormat+" document")
		return m, false
	}
	if m.Main.SHA256 != sum || m.Main.Bytes != n {
		rep.Problems = append(rep.Problems, "archive checksum differs from the manifest (truncated or modified)")
		return m, false
	}
	if m.Analytics != nil {
		asum, an2, err := fileSHA256(an)
		if err != nil || asum != m.Analytics.SHA256 || an2 != m.Analytics.Bytes {
			rep.Problems = append(rep.Problems, "analytics archive "+filepath.Base(an)+" missing or differs from the manifest")
			return m, false
		}
	}
	return m, true
}

// restoreInto runs pg_restore of the backup (and analytics archive) into the
// database of dsn in single transactions, then checks it.
func restoreInto(ctx context.Context, file string, m PGManifest, dsn, analyticsDSN pgDSN, rep *PGVerifyReport) error {
	args := func(f string, target pgDSN) []string {
		return []string{"--exit-on-error", "--single-transaction", "--no-owner", "--no-privileges", "--dbname=" + target.render(false), f}
	}
	if err := runTool(ctx, dsn, "pg_restore", args(file, dsn)...); err != nil {
		if errors.As(err, new(errToolMissing)) {
			return err
		}
		rep.Restore = "failed"
		rep.Problems = append(rep.Problems, err.Error())
		return nil
	}
	if m.Analytics != nil {
		_, an := pgPaths(file)
		if err := runTool(ctx, analyticsDSN, "pg_restore", args(an, analyticsDSN)...); err != nil {
			rep.Restore = "failed (analytics)"
			rep.Problems = append(rep.Problems, err.Error())
			return nil
		}
	}
	rep.Restore = "ok"
	return checkRestored(ctx, m, dsn, analyticsDSN, rep)
}

// checkRestored runs migration status, GameKit and billing reconcile and the
// manifest comparison on a restored database.
func checkRestored(ctx context.Context, m PGManifest, dsn, analyticsDSN pgDSN, rep *PGVerifyReport) error {
	fail := func(p string) { rep.Problems = append(rep.Problems, p) }
	db, err := postgres.Open(ctx, postgres.Options{DSN: dsn.render(true), MaxOpenConns: 2, ReadConns: 2})
	if err != nil {
		return err
	}
	defer db.Close()
	fsys, dir := migrations.Main(store.Postgres)
	ms, err := store.LoadMigrations(fsys, dir)
	if err != nil {
		return err
	}
	schemaCheck := func(q *store.DB, ms []store.Migration, label string) *schemaSummary {
		ok, err := store.TableExists(ctx, q.Write, store.Postgres, "schema_migrations")
		if err != nil {
			fail(label + ": " + err.Error())
			return nil
		}
		if !ok {
			fail(label + ": no schema_migrations table (not a GoStack database)")
			return nil
		}
		st, err := q.MigrationStatus(ctx, ms)
		if err != nil {
			fail(label + " migration status: " + err.Error())
			return nil
		}
		s := summarize(st, ms)
		if len(s.Unknown) > 0 {
			fail(fmt.Sprintf("%s has migrations unknown to this binary %v (newer version required)", label, s.Unknown))
		}
		if len(s.Drifted) > 0 {
			fail(fmt.Sprintf("%s migration checksum drift %v", label, s.Drifted))
		}
		return &s
	}
	if s := schemaCheck(db, ms, "database"); s != nil {
		rep.Schema = *s
	}
	has := func(t string) bool {
		ok, _ := store.TableExists(ctx, db.Write, store.Postgres, t)
		return ok
	}
	if has("game_ledger") && has("wallet_balances") {
		mm, err := game.Reconcile(ctx, db.Read, "")
		if err != nil {
			fail("reconcile: " + err.Error())
		} else {
			rep.Reconcile = mm
			if len(mm) > 0 {
				fail(fmt.Sprintf("%d wallet balances disagree with the ledger", len(mm)))
			}
		}
	}
	if has("billing_accounts") && has("billing_ledger_entries") {
		var mm []billing.Mismatch
		err := db.ReadTx(ctx, func(tx *sql.Tx) error {
			var err error
			mm, err = billing.ReconcileIn(ctx, tx, "")
			return err
		})
		if err != nil {
			fail("billing reconcile: " + err.Error())
		} else {
			if mm == nil {
				mm = []billing.Mismatch{}
			}
			rep.BillingReconcile = mm
			if len(mm) > 0 {
				fail(fmt.Sprintf("%d billing balances disagree with the ledger", len(mm)))
			}
		}
	}
	// Analytics schema (same database unless the manifest has a separate part).
	adsn := dsn
	if m.Analytics != nil {
		adsn = analyticsDSN
	}
	plainA, err := openPlain(adsn)
	if err != nil {
		return err
	}
	defer plainA.Close()
	var aExists bool
	if err := plainA.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, AnalyticsSchema).Scan(&aExists); err != nil {
		return err
	}
	if aExists {
		adb, err := postgres.Open(ctx, postgres.Options{DSN: adsn.render(true), Schema: AnalyticsSchema, MaxOpenConns: 1, ReadConns: 1})
		if err != nil {
			return err
		}
		afs, adir := migrations.AnalyticsFor(store.Postgres)
		ams, err := store.LoadMigrations(afs, adir)
		if err == nil {
			rep.AnalyticsSchema = schemaCheck(adb, ams, "analytics schema")
		}
		adb.Close()
		if err != nil {
			return err
		}
	}
	// Manifest comparison.
	plain, err := openPlain(dsn)
	if err != nil {
		return err
	}
	defer plain.Close()
	got, err := pgPartStats(ctx, plain, m.Main.Schemas)
	if err != nil {
		fail("statistics: " + err.Error())
	} else {
		rep.ManifestDiffs = append(rep.ManifestDiffs, diffStats("main", m.Main, got)...)
	}
	if m.Analytics != nil {
		got, err := pgPartStats(ctx, plainA, m.Analytics.Schemas)
		if err != nil {
			fail("analytics statistics: " + err.Error())
		} else {
			rep.ManifestDiffs = append(rep.ManifestDiffs, diffStats("analytics", *m.Analytics, got)...)
		}
	}
	if len(rep.ManifestDiffs) > 0 {
		fail(fmt.Sprintf("%d differences between the restored data and the manifest", len(rep.ManifestDiffs)))
	}
	return nil
}

func randomName(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// VerifyPostgresBackup restores file into a fresh temporary database
// (gs_verify_<random>, C collation) on the server of adminRef, checks it and
// drops the database. The connecting role needs CREATEDB.
func VerifyPostgresBackup(ctx context.Context, adminRef, file string) (rep PGVerifyReport, err error) {
	rep = PGVerifyReport{File: file, ManifestDiffs: []string{}, Problems: []string{}}
	m, ok := loadPGBackup(file, &rep)
	if !ok {
		return rep, nil
	}
	admin, err := resolvePGDSN(adminRef)
	if err != nil {
		return rep, fmt.Errorf("verify server dsn: %w", err)
	}
	adb, err := openPlain(admin)
	if err != nil {
		return rep, err
	}
	defer adb.Close()
	name := randomName("gs_verify_")
	if _, err := adb.ExecContext(ctx, `CREATE DATABASE `+qident(name)+` TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'`); err != nil {
		return rep, fmt.Errorf("create temporary database (role needs CREATEDB; or pass --dsn-ref): %w", err)
	}
	rep.Database = name
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, err := adb.ExecContext(dctx, `DROP DATABASE IF EXISTS `+qident(name)+` WITH (FORCE)`); err != nil {
			rep.Problems = append(rep.Problems, "drop temporary database "+name+": "+err.Error())
			rep.OK = false
		}
	}()
	vd := admin.withDB(name)
	if err := restoreInto(ctx, file, m, vd, vd, &rep); err != nil {
		return rep, err
	}
	rep.OK = len(rep.Problems) == 0
	return rep, nil
}

// checkEmptyTarget refuses databases that hold any user relation (or, when
// schemaOnly is set, already have the analytics schema) or that do not sort
// bytewise.
func checkEmptyTarget(ctx context.Context, d pgDSN, analyticsOnly bool) error {
	db, err := openPlain(d)
	if err != nil {
		return err
	}
	defer db.Close()
	var bytewise bool
	if err := db.QueryRowContext(ctx, `SELECT ('B' < 'a') AND ('Z' < '_') AND ('_' < 'a')`).Scan(&bytewise); err != nil {
		return fmt.Errorf("connect to target: %w", err)
	}
	if !bytewise {
		return postgres.ErrCollation
	}
	var n int
	q := `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'`
	if analyticsOnly {
		q = `SELECT count(*) FROM pg_namespace WHERE nspname='` + AnalyticsSchema + `'`
	}
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		if analyticsOnly {
			return fmt.Errorf("refusing to restore: analytics target already has schema %s", AnalyticsSchema)
		}
		return fmt.Errorf("refusing to restore: target database %q is not empty (%d relations); restore only into a freshly created database", d.dbName(), n)
	}
	return nil
}

// RestorePostgresBackup restores a verified-checksum backup into the empty
// database of toRef (analytics into analyticsRef when the backup has a
// separate analytics archive; default toRef), then checks the result in place.
// pg_restore runs in a single transaction, so a failed restore leaves the
// target empty. Objects are owned by the connecting role (--no-owner).
func RestorePostgresBackup(ctx context.Context, from, toRef, analyticsRef string) (PGVerifyReport, error) {
	rep := PGVerifyReport{File: from, ManifestDiffs: []string{}, Problems: []string{}}
	m, ok := loadPGBackup(from, &rep)
	if !ok {
		return rep, errors.New("backup " + from + " failed verification; not restored")
	}
	to, err := resolvePGDSN(toRef)
	if err != nil {
		return rep, fmt.Errorf("target dsn: %w", err)
	}
	ato := to
	if m.Analytics != nil && analyticsRef != "" {
		if ato, err = resolvePGDSN(analyticsRef); err != nil {
			return rep, fmt.Errorf("analytics target dsn: %w", err)
		}
	}
	if err := checkEmptyTarget(ctx, to, false); err != nil {
		return rep, err
	}
	if m.Analytics != nil && analyticsRef != "" {
		if err := checkEmptyTarget(ctx, ato, true); err != nil {
			return rep, err
		}
	}
	rep.Database = to.dbName()
	if err := restoreInto(ctx, from, m, to, ato, &rep); err != nil {
		return rep, err
	}
	rep.OK = len(rep.Problems) == 0
	if !rep.OK {
		return rep, errors.New("restored database failed verification; do not point the application at it")
	}
	return rep, nil
}

func printPGReport(env Env, rep PGVerifyReport, asJSON bool) {
	if asJSON {
		PrintJSON(env.Stdout, rep)
		return
	}
	fmt.Fprintf(env.Stdout, "file:        %s (%d bytes, sha256 %s)\ndatabase:    %s\nrestore:     %s\nmigrations:  applied=%d known=%d pending=%v unknown=%v drifted=%v\n",
		rep.File, rep.Bytes, rep.SHA256, rep.Database, rep.Restore, rep.Schema.Applied, rep.Schema.Known, rep.Schema.Pending, rep.Schema.Unknown, rep.Schema.Drifted)
	if rep.Reconcile != nil {
		fmt.Fprintf(env.Stdout, "reconcile:   %d mismatches\n", len(rep.Reconcile))
	} else {
		fmt.Fprintln(env.Stdout, "reconcile:   skipped (no GameKit ledger tables)")
	}
	if rep.BillingReconcile != nil {
		fmt.Fprintf(env.Stdout, "billing:     %d mismatches\n", len(rep.BillingReconcile))
	}
	fmt.Fprintf(env.Stdout, "manifest:    %d differences\n", len(rep.ManifestDiffs))
	for _, d := range rep.ManifestDiffs {
		fmt.Fprintf(env.Stdout, "  - %s\n", d)
	}
	if rep.OK {
		fmt.Fprintln(env.Stdout, "result:      OK")
	} else {
		fmt.Fprintf(env.Stdout, "result:      FAILED: %s\n", strings.Join(rep.Problems, "; "))
	}
}
