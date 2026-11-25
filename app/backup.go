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
	"os"
	"path/filepath"
	"strings"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/migrations"
)

// schemaSummary condenses migration status for scripts and backup checks.
type schemaSummary struct {
	Known   int   `json:"known"`
	Applied int   `json:"applied"`
	Pending []int `json:"pending_versions"`
	// Unknown are applied versions this binary does not know (database is newer).
	Unknown []int `json:"unknown_versions"`
	// Drifted are known versions whose applied checksum differs.
	Drifted []int `json:"drifted_versions"`
	Drift   bool  `json:"drift"`
}

func summarize(st []store.MigrationStatus, ms []store.Migration) schemaSummary {
	s := schemaSummary{Known: len(ms), Pending: []int{}, Unknown: []int{}, Drifted: []int{}}
	known := map[int]bool{}
	for _, m := range ms {
		known[m.Version] = true
	}
	for _, x := range st {
		switch {
		case x.Applied && !known[x.Version]:
			s.Unknown = append(s.Unknown, x.Version)
		case x.Drift:
			s.Drifted = append(s.Drifted, x.Version)
		case !x.Applied:
			s.Pending = append(s.Pending, x.Version)
		}
		if x.Applied {
			s.Applied++
		}
	}
	s.Drift = len(s.Unknown)+len(s.Drifted) > 0
	return s
}

const backupUsage = "usage: backup snapshot --to FILE | backup verify FILE | backup restore --from FILE --to PATH"

func backupCmd(ctx context.Context, cfg config.Config, cfgErr error, args []string, asJSON bool, env Env) error {
	if len(args) == 0 {
		return UsageError{backupUsage}
	}
	if cfgErr == nil && cfg.Database.Driver == store.Postgres {
		return errors.New("backup snapshot/verify/restore work on SQLite files only; back up PostgreSQL with pg_dump or base backups + WAL archiving (PITR), see docs/adr/0003-postgres-scale.md")
	}
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "target path")
	from := fs.String("from", "", "backup file")
	switch args[0] {
	case "snapshot":
		if err := fs.Parse(args[1:]); err != nil || *to == "" || fs.NArg() != 0 {
			return UsageError{"usage: backup snapshot --to FILE"}
		}
		if cfgErr != nil {
			return fmt.Errorf("invalid configuration:\n%w", cfgErr)
		}
		info, err := Snapshot(ctx, cfg.Database.Path, *to)
		if err != nil {
			return err
		}
		if asJSON {
			PrintJSON(env.Stdout, info)
		} else {
			fmt.Fprintf(env.Stdout, "snapshot %s (%d bytes, sha256 %s)\n", info.Path, info.Bytes, info.SHA256)
		}
		return nil
	case "verify":
		if len(args) != 2 {
			return UsageError{"usage: backup verify FILE"}
		}
		rep, err := VerifyBackup(ctx, args[1])
		if err != nil {
			return err
		}
		printReport(env, rep, asJSON)
		if !rep.OK {
			return CheckFailed{"backup verification failed: " + strings.Join(rep.Problems, "; ")}
		}
		return nil
	case "restore":
		if err := fs.Parse(args[1:]); err != nil || *to == "" || *from == "" || fs.NArg() != 0 {
			return UsageError{"usage: backup restore --from FILE --to PATH"}
		}
		rep, err := RestoreBackup(ctx, *from, *to)
		if err != nil {
			if rep != nil {
				printReport(env, *rep, asJSON)
				if !rep.OK {
					return CheckFailed{err.Error()}
				}
			}
			return err
		}
		if asJSON {
			PrintJSON(env.Stdout, map[string]any{"restored": *to, "verify": rep})
		} else {
			fmt.Fprintf(env.Stdout, "restored %s -> %s (verified). Point database.path at it and start the service.\n", *from, *to)
		}
		return nil
	}
	return UsageError{backupUsage}
}

func printReport(env Env, rep VerifyReport, asJSON bool) {
	if asJSON {
		PrintJSON(env.Stdout, rep)
		return
	}
	fmt.Fprintf(env.Stdout, "file:        %s (%d bytes, sha256 %s)\nintegrity:   %s\nmigrations:  applied=%d known=%d pending=%v unknown=%v drifted=%v\n",
		rep.File, rep.Bytes, rep.SHA256, rep.Integrity, rep.Schema.Applied, rep.Schema.Known, rep.Schema.Pending, rep.Schema.Unknown, rep.Schema.Drifted)
	if rep.Reconcile != nil {
		fmt.Fprintf(env.Stdout, "reconcile:   %d mismatches\n", len(rep.Reconcile))
	} else {
		fmt.Fprintln(env.Stdout, "reconcile:   skipped (no GameKit ledger tables)")
	}
	if rep.OK {
		fmt.Fprintln(env.Stdout, "result:      OK")
	} else {
		fmt.Fprintf(env.Stdout, "result:      FAILED: %s\n", strings.Join(rep.Problems, "; "))
	}
}

// SnapshotInfo describes a written snapshot.
type SnapshotInfo struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Snapshot writes a transactionally consistent copy of the database at src to
// dst using VACUUM INTO on a read-only connection (safe while the server runs;
// it does not take the instance lock). dst must not exist. The copy is written
// to a temporary file and linked into place, so dst is either absent or complete.
func Snapshot(ctx context.Context, src, dst string) (SnapshotInfo, error) {
	if _, err := os.Lstat(dst); err == nil {
		return SnapshotInfo{}, fmt.Errorf("refusing to overwrite existing %s", dst)
	}
	if _, err := os.Stat(src); err != nil {
		return SnapshotInfo{}, fmt.Errorf("database %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return SnapshotInfo{}, err
	}
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return SnapshotInfo{}, err
	}
	defer db.Close()
	tmp, err := tempPath(dst)
	if err != nil {
		return SnapshotInfo{}, err
	}
	defer os.Remove(tmp)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return SnapshotInfo{}, fmt.Errorf("vacuum into: %w", err)
	}
	if err := publish(tmp, dst); err != nil {
		return SnapshotInfo{}, err
	}
	sum, n, err := fileSHA256(dst)
	return SnapshotInfo{Path: dst, Bytes: n, SHA256: sum}, err
}

// tempPath reserves a unique temporary file name next to dst.
func tempPath(dst string) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	return name, os.Remove(name) // VACUUM INTO requires a non-existent or empty target
}

// publish fsyncs tmp and links it to dst without ever replacing an existing
// file (link fails with EEXIST), then fsyncs the directory.
func publish(tmp, dst string) error {
	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if err := os.Link(tmp, dst); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing %s", dst)
		}
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func fileSHA256(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// VerifyReport is the result of `backup verify`.
type VerifyReport struct {
	OK        bool            `json:"ok"`
	File      string          `json:"file"`
	Bytes     int64           `json:"bytes"`
	SHA256    string          `json:"sha256"`
	Integrity string          `json:"integrity"`
	Schema    schemaSummary   `json:"schema"`
	Reconcile []game.Mismatch `json:"reconcile_mismatches"` // null when the GameKit tables are absent
	Problems  []string        `json:"problems"`
}

// VerifyBackup checks a backup without modifying it: it copies the file to a
// private temporary directory, then runs PRAGMA integrity_check, compares the
// schema with this binary's migrations and, when GameKit tables exist,
// reconciles wallet balances against the ledger. Pending migrations are
// acceptable (an older backup is upgraded on start); unknown or drifted ones
// are not.
func VerifyBackup(ctx context.Context, file string) (VerifyReport, error) {
	rep := VerifyReport{File: file, Problems: []string{}}
	sum, n, err := fileSHA256(file)
	if err != nil {
		return rep, err
	}
	rep.SHA256, rep.Bytes = sum, n
	dir, err := os.MkdirTemp("", "gostack-verify-*")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(dir)
	cp := filepath.Join(dir, "verify.db")
	if err := copyFile(file, cp); err != nil {
		return rep, err
	}
	db, err := sql.Open("sqlite", "file:"+cp+"?_pragma=query_only(1)")
	if err != nil {
		return rep, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	fail := func(p string) { rep.Problems = append(rep.Problems, p) }

	var msgs []string
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err == nil {
		for rows.Next() {
			var m string
			if rows.Scan(&m) == nil {
				msgs = append(msgs, m)
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
	}
	switch {
	case err != nil:
		rep.Integrity = "error: " + err.Error()
		fail("not a readable SQLite database")
		return rep, nil
	case len(msgs) == 1 && msgs[0] == "ok":
		rep.Integrity = "ok"
	default:
		rep.Integrity = strings.Join(msgs, "; ")
		fail("integrity_check failed")
	}

	ms, err := store.LoadMigrations(migrations.SQLite, "sqlite")
	if err != nil {
		return rep, err
	}
	tables := map[string]bool{}
	if trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`); err == nil {
		for trows.Next() {
			var t string
			if trows.Scan(&t) == nil {
				tables[t] = true
			}
		}
		trows.Close()
	}
	if !tables["schema_migrations"] {
		fail("no schema_migrations table (not a GoStack database)")
	} else if st, err := store.MigrationStatusOf(ctx, db, store.SQLite, ms); err != nil {
		fail("migration status: " + err.Error())
	} else {
		rep.Schema = summarize(st, ms)
		if len(rep.Schema.Unknown) > 0 {
			fail(fmt.Sprintf("database has migrations unknown to this binary %v (newer version required)", rep.Schema.Unknown))
		}
		if len(rep.Schema.Drifted) > 0 {
			fail(fmt.Sprintf("migration checksum drift %v", rep.Schema.Drifted))
		}
	}
	if tables["game_ledger"] && tables["wallet_balances"] {
		mm, err := game.Reconcile(ctx, db, "")
		if err != nil {
			fail("reconcile: " + err.Error())
		} else {
			rep.Reconcile = mm
			if len(mm) > 0 {
				fail(fmt.Sprintf("%d wallet balances disagree with the ledger", len(mm)))
			}
		}
	}
	rep.OK = len(rep.Problems) == 0
	return rep, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// RestoreBackup verifies from and copies it to the new path to. It never
// overwrites: to (and its -wal/-shm companions) must not exist. The copy is
// checked against the verified checksum before it is linked into place, so a
// failed restore leaves nothing at to and never touches existing data.
func RestoreBackup(ctx context.Context, from, to string) (*VerifyReport, error) {
	for _, p := range []string{to, to + "-wal", to + "-shm"} {
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("refusing to restore over existing %s; choose a new path", p)
		}
	}
	rep, err := VerifyBackup(ctx, from)
	if err != nil {
		return nil, err
	}
	if !rep.OK {
		return &rep, fmt.Errorf("backup %s failed verification; not restored", from)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return &rep, err
	}
	tmp, err := tempPath(to)
	if err != nil {
		return &rep, err
	}
	defer os.Remove(tmp)
	if err := copyFile(from, tmp); err != nil {
		return &rep, err
	}
	if sum, _, err := fileSHA256(tmp); err != nil || sum != rep.SHA256 {
		return &rep, fmt.Errorf("restored copy checksum mismatch (source changed during restore?)")
	}
	if err := publish(tmp, to); err != nil {
		return &rep, err
	}
	return &rep, nil
}
