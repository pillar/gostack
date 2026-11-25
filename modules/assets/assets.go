// Package assets publishes static game builds to object storage and serves the
// current release's manifest (PRD §22 resource distribution, acceptance G-A07).
//
// Publish order: every non-entry file is uploaded under a content-addressed,
// immutable key (games/<game>/assets/<sha256>/<name>), then the versioned
// manifest (games/<game>/manifests/<release>.json), then the entry file
// (games/<game>/releases/<release>/<entry>), and only then one database
// transaction records the release and switches the game's current-release
// pointer. A failure at any earlier step leaves the previous release current.
//
// Publish and prune of one game are serialized by a lease row (assets_state);
// a second concurrent publish fails fast with ErrBusy. Every object the
// publisher stores is recorded in assets_objects, so deduplication and prune
// never list the bucket, and prune never deletes an object referenced by the
// current release or a kept release.
package assets

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/storage"
)

const (
	// ImmutableCacheControl is stored with every published object: keys are
	// content-addressed (assets) or release-scoped (manifest, entry), so a key's
	// body never changes.
	ImmutableCacheControl = "public, max-age=31536000, immutable"
	DefaultEntry          = "index.html"
	MaxFiles              = 10000
	MaxPathLen            = 1024
	MaxLabelLen           = 128
	maxManifestBytes      = 16 << 20
	maxNameLen            = 128
	manifestVersion       = 1
)

var (
	// ErrBusy: another publish or prune of the same game holds the lease.
	ErrBusy = errors.New("assets: another publish or prune of this game is in progress")
	// ErrLeaseLost: the lease expired and was taken over; nothing was switched.
	ErrLeaseLost = errors.New("assets: lease lost (expired and taken over); the current release was not changed")
	// ErrImmutable: an object exists under a content-addressed key with a different body.
	ErrImmutable = errors.New("assets: object exists with a different body; immutable keys are never overwritten")
	// ErrNoRelease: the game has no current release.
	ErrNoRelease = errors.New("assets: no release published")
)

var releaseIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}$`)

// ValidReleaseID reports whether s has the release id syntax (20261125T101530Z-a1b2c3).
func ValidReleaseID(s string) bool { return releaseIDPattern.MatchString(s) }

// Module publishes releases (CLI) and serves them (HTTP, see Routes).
type Module struct {
	Kit     *game.Kit // HTTP routes only
	DB      *store.DB
	Storage storage.Storage
	// PublicBaseURL serves object keys (CDN in front of the bucket); "" = none.
	PublicBaseURL string
	// ManifestMaxAge is the Cache-Control max-age of the manifest and entry
	// responses (default 60s).
	ManifestMaxAge time.Duration
	// EntryURLTTL is the presigned entry URL lifetime without PublicBaseURL (default 10m).
	EntryURLTTL time.Duration
	// LockTTL bounds how long a crashed publish/prune blocks the next one (default 10m);
	// a running one renews its lease after every object.
	LockTTL time.Duration
	Now     func() time.Time

	mu    sync.Mutex
	cache map[string][]byte // manifest key -> rendered API response (immutable per key)
}

// Release is a published (or pruned) release.
type Release struct {
	GameID         string `json:"game_id"`
	ReleaseID      string `json:"release_id"`
	Label          string `json:"label"`
	ManifestKey    string `json:"manifest_key"`
	ManifestSize   int64  `json:"manifest_size"`
	ManifestSHA256 string `json:"manifest_sha256"`
	EntryPath      string `json:"entry_path"`
	EntryKey       string `json:"entry_key"`
	EntrySize      int64  `json:"entry_size"`
	EntrySHA256    string `json:"entry_sha256"`
	FileCount      int    `json:"file_count"`  // all files including the entry
	TotalBytes     int64  `json:"total_bytes"` // all files including the entry
	CreatedAt      int64  `json:"created_at"`  // epoch ms
	CreatedBy      string `json:"created_by"`
	Status         string `json:"status"` // published | pruned
	Current        bool   `json:"current"`
}

// File describes one object in a manifest.
type File struct {
	Path        string `json:"path,omitempty"` // set for the entry; files are keyed by path
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	URL         string `json:"url,omitempty"` // API responses with a public base URL only
}

// Manifest is stored at games/<game>/manifests/<release>.json.
type Manifest struct {
	ManifestVersion int             `json:"manifest_version"`
	GameID          string          `json:"game_id"`
	ReleaseID       string          `json:"release_id"`
	Label           string          `json:"label"`
	CreatedAt       string          `json:"created_at"`         // RFC 3339 UTC
	BaseURL         string          `json:"base_url,omitempty"` // API responses only
	Entry           File            `json:"entry"`
	Files           map[string]File `json:"files"` // by path relative to the build dir
}

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) lockTTL() time.Duration {
	if m.LockTTL > 0 {
		return m.LockTTL
	}
	return 10 * time.Minute
}

// ---- keys ----

func assetKey(gameID, sha, rel string) string {
	return "games/" + gameID + "/assets/" + sha + "/" + safeName(path.Base(rel))
}

func manifestKey(gameID, rid string) string { return "games/" + gameID + "/manifests/" + rid + ".json" }

func entryKey(gameID, rid, entry string) string {
	return "games/" + gameID + "/releases/" + rid + "/" + safeName(path.Base(entry))
}

// safeName maps a file name to a storage key segment, keeping it readable.
func safeName(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	if len(b) > maxNameLen {
		b = b[len(b)-maxNameLen:]
	}
	if s := string(b); s != "" && s != "." && s != ".." {
		return s
	}
	return "file"
}

func newReleaseID(t time.Time) string {
	var r [3]byte
	_, _ = rand.Read(r[:])
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(r[:])
}

func contentType(p string) string {
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// ---- storage helpers ----

func (m *Module) stat(ctx context.Context, key string) (storage.Info, error) {
	if s, ok := m.Storage.(storage.Stater); ok {
		return s.Stat(ctx, key)
	}
	rc, info, err := m.Storage.Get(ctx, key)
	if err == nil {
		rc.Close()
	}
	return info, err
}

func (m *Module) put(ctx context.Context, key string, r io.Reader, size int64, ct string) error {
	if p, ok := m.Storage.(storage.ObjectPutter); ok {
		return p.PutObject(ctx, key, r, size, storage.PutOptions{ContentType: ct, CacheControl: ImmutableCacheControl})
	}
	return m.Storage.Put(ctx, key, r, size, ct)
}

// hashObject streams an object and returns its SHA-256 and length.
func (m *Module) hashObject(ctx context.Context, key string) (string, int64, error) {
	rc, _, err := m.Storage.Get(ctx, key)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// source is a file to store: a build file on disk or generated bytes.
type source struct {
	rel  string // path relative to the build dir ("" for the manifest)
	abs  string
	data []byte
	size int64
	sha  string
	ct   string
	key  string
}

func (s source) open() (io.ReadCloser, error) {
	if s.data != nil {
		return io.NopCloser(bytes.NewReader(s.data)), nil
	}
	return os.Open(s.abs)
}

func (s source) name() string {
	if s.rel != "" {
		return s.rel
	}
	return s.key
}

func bytesSource(key string, b []byte, ct string) source {
	h := sha256.Sum256(b)
	return source{data: b, size: int64(len(b)), sha: hex.EncodeToString(h[:]), ct: ct, key: key}
}

// ensureObject makes sure s is stored under s.key and recorded in
// assets_objects; it reports whether it uploaded. A key recorded as live is
// trusted after a metadata check (Stat: present, same size); an untracked
// object already under the key is hashed and must match.
func (m *Module) ensureObject(ctx context.Context, gameID string, s source) (bool, error) {
	var known string
	err := m.DB.Read.QueryRowContext(ctx, `SELECT sha256 FROM assets_objects WHERE game_id=? AND object_key=? AND state='live'`,
		gameID, s.key).Scan(&known)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	info, serr := m.stat(ctx, s.key)
	switch {
	case serr != nil && !errors.Is(serr, storage.ErrNotFound):
		return false, fmt.Errorf("stat %s: %w", s.key, serr)
	case serr == nil && known != "":
		if known != s.sha || info.Size != s.size {
			return false, fmt.Errorf("%w: %s (recorded sha256 %s, size %d)", ErrImmutable, s.key, known, info.Size)
		}
		return false, nil
	case serr == nil:
		got, n, err := m.hashObject(ctx, s.key)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", s.key, err)
		}
		if got != s.sha || n != s.size {
			return false, fmt.Errorf("%w: %s", ErrImmutable, s.key)
		}
		return false, m.recordObject(ctx, gameID, s)
	}
	// Missing: upload, hashing what is actually sent.
	rc, err := s.open()
	if err != nil {
		return false, err
	}
	defer rc.Close()
	h := sha256.New()
	if err := m.put(ctx, s.key, io.TeeReader(rc, h), s.size, s.ct); err != nil {
		return false, fmt.Errorf("upload %s: %w", s.name(), err)
	}
	if hex.EncodeToString(h.Sum(nil)) != s.sha {
		_ = m.Storage.Delete(context.WithoutCancel(ctx), s.key)
		return false, fmt.Errorf("assets: %s changed during publish; uploaded copy removed", s.name())
	}
	return true, m.recordObject(ctx, gameID, s)
}

func (m *Module) recordObject(ctx context.Context, gameID string, s source) error {
	now := m.now().UnixMilli()
	return m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO assets_objects(game_id, object_key, sha256, size, state, created_at)
			VALUES (?, ?, ?, ?, 'live', ?)
			ON CONFLICT(game_id, object_key) DO UPDATE SET state='live', sha256=excluded.sha256, size=excluded.size`,
			gameID, s.key, s.sha, s.size, now)
		return err
	})
}

// ---- lease ----

func (m *Module) acquire(ctx context.Context, gameID, holder, actor string) error {
	now := m.now().UnixMilli()
	var n int64
	err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO assets_state(game_id, current_release, lock_holder, lock_expires_at, updated_at, updated_by)
			VALUES (?, NULL, ?, ?, ?, ?)
			ON CONFLICT(game_id) DO UPDATE SET lock_holder=excluded.lock_holder, lock_expires_at=excluded.lock_expires_at
			WHERE assets_state.lock_holder IS NULL OR assets_state.lock_expires_at <= ?`,
			gameID, holder, now+m.lockTTL().Milliseconds(), now, actor, now)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBusy
	}
	return nil
}

// lease renews the holder's lease at most every LockTTL/4.
type lease struct {
	m              *Module
	gameID, holder string
	renewed        time.Time
}

func (l *lease) renew(ctx context.Context, force bool) error {
	now := l.m.now()
	if !force && now.Sub(l.renewed) < l.m.lockTTL()/4 {
		return nil
	}
	var n int64
	err := l.m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE assets_state SET lock_expires_at=? WHERE game_id=? AND lock_holder=?`,
			now.Add(l.m.lockTTL()).UnixMilli(), l.gameID, l.holder)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	l.renewed = now
	return nil
}

func (l *lease) release(ctx context.Context) {
	_ = l.m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE assets_state SET lock_holder=NULL, lock_expires_at=0 WHERE game_id=? AND lock_holder=?`,
			l.gameID, l.holder)
		return err
	})
}

func checkHolder(ctx context.Context, tx *sql.Tx, gameID, holder string) error {
	var h sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT lock_holder FROM assets_state WHERE game_id=?`, gameID).Scan(&h); err != nil {
		return err
	}
	if !h.Valid || h.String != holder {
		return ErrLeaseLost
	}
	return nil
}

// ---- publish ----

// PublishOptions describes a build directory to publish.
type PublishOptions struct {
	GameID string
	Dir    string
	Entry  string // path relative to Dir; default index.html
	Label  string // free-form version label
	Actor  string // recorded as created_by
}

// PublishResult reports a completed publish.
type PublishResult struct {
	Release       Release `json:"release"`
	Previous      string  `json:"previous_release_id"`
	Uploaded      int     `json:"uploaded"` // objects uploaded (assets, manifest, entry)
	Reused        int     `json:"reused"`   // assets already stored (deduplicated)
	UploadedBytes int64   `json:"uploaded_bytes"`
}

func (m *Module) checkGame(ctx context.Context, gameID string) error {
	if !game.ValidID(gameID) {
		return fmt.Errorf("assets: invalid game id %q", gameID)
	}
	var n int
	if err := m.DB.Read.QueryRowContext(ctx, `SELECT count(*) FROM games WHERE game_id=?`, gameID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("assets: game %q not found", gameID)
	}
	return nil
}

func validText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// scan hashes every regular file below dir. Symlinks and other special files
// are refused rather than silently skipped.
func scan(dir string) ([]source, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	var out []source
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			return fmt.Errorf("assets: %s is not a regular file (symlinks and special files are not published)", rel)
		}
		if !validText(rel, MaxPathLen) {
			return fmt.Errorf("assets: path %q must be valid UTF-8 without control characters, at most %d bytes", rel, MaxPathLen)
		}
		if len(out) >= MaxFiles {
			return fmt.Errorf("assets: more than %d files", MaxFiles)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out = append(out, source{rel: rel, abs: p, size: n, sha: hex.EncodeToString(h.Sum(nil)), ct: contentType(rel)})
		return nil
	})
	return out, err
}

// Publish uploads o.Dir as a new release and makes it current. On any error
// the previous release stays current; objects already uploaded are recorded
// and reused by the next publish (or removed by Prune).
func (m *Module) Publish(ctx context.Context, o PublishOptions) (PublishResult, error) {
	var res PublishResult
	if err := m.checkGame(ctx, o.GameID); err != nil {
		return res, err
	}
	if !validText(o.Label, MaxLabelLen) {
		return res, fmt.Errorf("assets: version label must be at most %d bytes of printable UTF-8", MaxLabelLen)
	}
	entry := o.Entry
	if entry == "" {
		entry = DefaultEntry
	}
	entry = path.Clean(filepath.ToSlash(entry))
	files, err := scan(o.Dir)
	if err != nil {
		return res, err
	}
	var entrySrc *source
	assets := files[:0:0]
	for i := range files {
		if files[i].rel == entry {
			entrySrc = &files[i]
		} else {
			assets = append(assets, files[i])
		}
	}
	if entrySrc == nil {
		return res, fmt.Errorf("assets: entry %q not found in %s", entry, o.Dir)
	}
	actor := o.Actor
	if actor == "" {
		actor = "cli"
	}
	start := m.now()
	rid := newReleaseID(start)
	l := &lease{m: m, gameID: o.GameID, holder: "publish:" + rid, renewed: start}
	if err := m.acquire(ctx, o.GameID, l.holder, actor); err != nil {
		return res, err
	}
	switched := false
	defer func() {
		if !switched {
			l.release(context.WithoutCancel(ctx))
		}
	}()

	// 1. assets (content-addressed, deduplicated)
	man := Manifest{ManifestVersion: manifestVersion, GameID: o.GameID, ReleaseID: rid, Label: o.Label,
		CreatedAt: start.UTC().Format(time.RFC3339), Files: map[string]File{}}
	total := entrySrc.size
	for i := range assets {
		s := &assets[i]
		s.key = assetKey(o.GameID, s.sha, s.rel)
		up, err := m.ensureObject(ctx, o.GameID, *s)
		if err != nil {
			return res, err
		}
		if up {
			res.Uploaded++
			res.UploadedBytes += s.size
		} else {
			res.Reused++
		}
		if err := l.renew(ctx, false); err != nil {
			return res, err
		}
		man.Files[s.rel] = File{Key: s.key, Size: s.size, SHA256: s.sha, ContentType: s.ct}
		total += s.size
	}
	// 2. manifest, 3. entry (both release-scoped)
	entrySrc.key = entryKey(o.GameID, rid, entry)
	man.Entry = File{Path: entry, Key: entrySrc.key, Size: entrySrc.size, SHA256: entrySrc.sha, ContentType: entrySrc.ct}
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return res, err
	}
	ms := bytesSource(manifestKey(o.GameID, rid), mb, "application/json")
	for _, s := range []source{ms, *entrySrc} {
		up, err := m.ensureObject(ctx, o.GameID, s)
		if err != nil {
			return res, err
		}
		if up {
			res.Uploaded++
			res.UploadedBytes += s.size
		}
		if err := l.renew(ctx, false); err != nil {
			return res, err
		}
	}
	// 4. record the release and switch the pointer in one transaction.
	rel := Release{GameID: o.GameID, ReleaseID: rid, Label: o.Label, ManifestKey: ms.key, ManifestSize: ms.size, ManifestSHA256: ms.sha,
		EntryPath: entry, EntryKey: entrySrc.key, EntrySize: entrySrc.size, EntrySHA256: entrySrc.sha,
		FileCount: len(files), TotalBytes: total, CreatedBy: actor, Status: "published", Current: true}
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := checkHolder(ctx, tx, o.GameID, l.holder); err != nil {
			return err
		}
		var prev sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT current_release FROM assets_state WHERE game_id=?`, o.GameID).Scan(&prev); err != nil {
			return err
		}
		res.Previous = prev.String
		rel.CreatedAt = m.now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `INSERT INTO assets_releases(`+releaseCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'published')`,
			rel.GameID, rel.ReleaseID, rel.Label, rel.ManifestKey, rel.ManifestSize, rel.ManifestSHA256, rel.EntryPath, rel.EntryKey,
			rel.EntrySize, rel.EntrySHA256, rel.FileCount, rel.TotalBytes, rel.CreatedAt, rel.CreatedBy); err != nil {
			return err
		}
		for _, s := range assets {
			if _, err := tx.ExecContext(ctx, `INSERT INTO assets_release_files(game_id, release_id, path, object_key, size, sha256, content_type)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, o.GameID, rid, s.rel, s.key, s.size, s.sha, s.ct); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE assets_state SET current_release=?, lock_holder=NULL, lock_expires_at=0, updated_at=?, updated_by=?
			WHERE game_id=?`, rid, rel.CreatedAt, actor, o.GameID)
		return err
	})
	if err != nil {
		return res, err
	}
	switched = true
	res.Release = rel
	return res, nil
}

// ---- queries ----

const releaseCols = `game_id, release_id, label, manifest_key, manifest_size, manifest_sha256, entry_path, entry_key, entry_size,
	entry_sha256, file_count, total_bytes, created_at, created_by, status`

type rowScanner interface{ Scan(...any) error }

func scanRelease(r rowScanner) (Release, error) {
	var x Release
	err := r.Scan(&x.GameID, &x.ReleaseID, &x.Label, &x.ManifestKey, &x.ManifestSize, &x.ManifestSHA256, &x.EntryPath, &x.EntryKey,
		&x.EntrySize, &x.EntrySHA256, &x.FileCount, &x.TotalBytes, &x.CreatedAt, &x.CreatedBy, &x.Status)
	return x, err
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func currentID(ctx context.Context, q querier, gameID string) (string, error) {
	var cur sql.NullString
	err := q.QueryRowContext(ctx, `SELECT current_release FROM assets_state WHERE game_id=?`, gameID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cur.String, err
}

// List returns the game's releases, newest first, and the current release id.
func (m *Module) List(ctx context.Context, gameID string) ([]Release, string, error) {
	cur, err := currentID(ctx, m.DB.Read, gameID)
	if err != nil {
		return nil, "", err
	}
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT `+releaseCols+` FROM assets_releases WHERE game_id=?
		ORDER BY created_at DESC, release_id DESC`, gameID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, "", err
		}
		r.Current = r.ReleaseID == cur
		out = append(out, r)
	}
	return out, cur, rows.Err()
}

// Get returns one release (httpx 404 when unknown).
func (m *Module) Get(ctx context.Context, gameID, releaseID string) (Release, error) {
	r, err := scanRelease(m.DB.Read.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM assets_releases WHERE game_id=? AND release_id=?`,
		gameID, releaseID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, httpx.NotFound("release not found")
	}
	if err != nil {
		return r, err
	}
	cur, err := currentID(ctx, m.DB.Read, gameID)
	r.Current = cur == r.ReleaseID
	return r, err
}

// Current returns the current release (ErrNoRelease when none).
func (m *Module) Current(ctx context.Context, gameID string) (Release, error) {
	r, err := scanRelease(m.DB.Read.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM assets_releases
		WHERE game_id=? AND release_id=(SELECT current_release FROM assets_state WHERE game_id=?)`, gameID, gameID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNoRelease
	}
	r.Current = true
	return r, err
}

// objects lists every object of a release: manifest, entry, then files by path.
func (m *Module) objects(ctx context.Context, r Release) ([]File, error) {
	out := []File{{Path: "(manifest)", Key: r.ManifestKey, Size: r.ManifestSize, SHA256: r.ManifestSHA256},
		{Path: r.EntryPath, Key: r.EntryKey, Size: r.EntrySize, SHA256: r.EntrySHA256}}
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT path, object_key, size, sha256, content_type FROM assets_release_files
		WHERE game_id=? AND release_id=? ORDER BY path`, r.GameID, r.ReleaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Key, &f.Size, &f.SHA256, &f.ContentType); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---- rollback ----

// Rollback makes releaseID current again after checking that every object of
// the release is present (Stat) with its recorded size. It returns the
// previously current release id. Errors are httpx errors for the admin API.
func (m *Module) Rollback(ctx context.Context, gameID, releaseID, actor string) (string, error) {
	if !ValidReleaseID(releaseID) {
		return "", httpx.NotFound("release not found")
	}
	r, err := m.Get(ctx, gameID, releaseID)
	if err != nil {
		return "", err
	}
	if r.Status != "published" {
		return "", httpx.Conflict("release_pruned", "release was pruned; its objects may be deleted")
	}
	objs, err := m.objects(ctx, r)
	if err != nil {
		return "", err
	}
	var missing []string
	for _, f := range objs {
		info, err := m.stat(ctx, f.Key)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			missing = append(missing, f.Key)
		case err != nil:
			return "", fmt.Errorf("stat %s: %w", f.Key, err)
		case info.Size != f.Size:
			missing = append(missing, f.Key+" (size mismatch)")
		}
	}
	if len(missing) > 0 {
		return "", httpx.Conflict("release_incomplete",
			fmt.Sprintf("%d of %d objects missing or wrong size, e.g. %s; run `assets verify`", len(missing), len(objs), missing[0]))
	}
	var prev string
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM assets_releases WHERE game_id=? AND release_id=?`, gameID, releaseID).Scan(&status); err != nil {
			return err
		}
		if status != "published" {
			return httpx.Conflict("release_pruned", "release was pruned; its objects may be deleted")
		}
		var err error
		if prev, err = currentID(ctx, tx, gameID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE assets_state SET current_release=?, updated_at=?, updated_by=? WHERE game_id=?`,
			releaseID, m.now().UnixMilli(), actor, gameID)
		return err
	})
	return prev, err
}

// ---- verify ----

// Problem is one verification failure.
type Problem struct {
	Path    string `json:"path"`
	Key     string `json:"key"`
	Problem string `json:"problem"` // missing | size_mismatch | sha256_mismatch | manifest_mismatch | read_error
	Detail  string `json:"detail,omitempty"`
}

// VerifyReport is the result of Verify (G-A07: no missing files).
type VerifyReport struct {
	GameID    string    `json:"game_id"`
	ReleaseID string    `json:"release_id"`
	Checked   int       `json:"checked"`
	Bytes     int64     `json:"bytes"`
	Problems  []Problem `json:"problems"`
}

// OK reports whether the release verified cleanly.
func (v VerifyReport) OK() bool { return len(v.Problems) == 0 }

// Verify downloads every object of a release (the current one when releaseID
// is empty) and checks size and SHA-256 against the database, and that the
// stored manifest lists exactly the recorded files.
func (m *Module) Verify(ctx context.Context, gameID, releaseID string) (VerifyReport, error) {
	var r Release
	var err error
	if releaseID == "" {
		r, err = m.Current(ctx, gameID)
	} else {
		r, err = m.Get(ctx, gameID, releaseID)
	}
	rep := VerifyReport{GameID: gameID, ReleaseID: r.ReleaseID, Problems: []Problem{}}
	if err != nil {
		return rep, err
	}
	objs, err := m.objects(ctx, r)
	if err != nil {
		return rep, err
	}
	add := func(f File, p, d string) {
		rep.Problems = append(rep.Problems, Problem{Path: f.Path, Key: f.Key, Problem: p, Detail: d})
	}
	// The stored manifest must describe exactly the recorded release.
	mf := objs[0]
	if man, _, err := m.loadManifest(ctx, mf.Key); err == nil {
		if d := manifestDiff(man, r, objs[2:]); d != "" {
			add(mf, "manifest_mismatch", d)
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		add(mf, "read_error", err.Error())
	}
	for _, f := range objs {
		rep.Checked++
		got, n, err := m.hashObject(ctx, f.Key)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			add(f, "missing", "")
		case err != nil:
			add(f, "read_error", err.Error())
		case n != f.Size:
			add(f, "size_mismatch", fmt.Sprintf("stored %d bytes, recorded %d", n, f.Size))
		case got != f.SHA256:
			add(f, "sha256_mismatch", "")
		default:
			rep.Bytes += n
		}
	}
	return rep, nil
}

func manifestDiff(man *Manifest, r Release, files []File) string {
	if man.GameID != r.GameID || man.ReleaseID != r.ReleaseID || man.Entry.Key != r.EntryKey || man.Entry.Path != r.EntryPath {
		return "manifest header (game, release or entry) differs from the database"
	}
	if man.Entry.Size != r.EntrySize || man.Entry.SHA256 != r.EntrySHA256 {
		return "manifest entry size or sha256 differs from the database"
	}
	if len(man.Files) != len(files) {
		return fmt.Sprintf("manifest lists %d files, database %d", len(man.Files), len(files))
	}
	for _, f := range files {
		g, ok := man.Files[f.Path]
		if !ok || g.Key != f.Key || g.Size != f.Size || g.SHA256 != f.SHA256 {
			return "file " + f.Path + " differs between manifest and database"
		}
	}
	return ""
}

func (m *Module) loadManifest(ctx context.Context, key string) (*Manifest, []byte, error) {
	rc, _, err := m.Storage.Get(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > maxManifestBytes {
		return nil, nil, errors.New("manifest too large")
	}
	var man Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, raw, fmt.Errorf("manifest: %w", err)
	}
	return &man, raw, nil
}

// ---- prune ----

// PrunePlan describes (and, when applied, reports) a prune.
type PrunePlan struct {
	GameID   string   `json:"game_id"`
	Keep     []string `json:"keep"`        // releases kept (newest N published + current)
	Prune    []string `json:"prune"`       // releases marked pruned
	Delete   []string `json:"delete_keys"` // objects referenced by no kept release
	Applied  bool     `json:"applied"`
	Deleted  int      `json:"deleted"`
	Failures []string `json:"failures,omitempty"`
}

func planPrune(ctx context.Context, q querier, gameID string, keep int) (PrunePlan, error) {
	p := PrunePlan{GameID: gameID, Keep: []string{}, Prune: []string{}, Delete: []string{}}
	cur, err := currentID(ctx, q, gameID)
	if err != nil {
		return p, err
	}
	rows, err := q.QueryContext(ctx, `SELECT release_id, status FROM assets_releases WHERE game_id=? ORDER BY created_at DESC, release_id DESC`, gameID)
	if err != nil {
		return p, err
	}
	kept := 0
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return p, err
		}
		switch {
		case status != "published":
		case id == cur || kept < keep:
			p.Keep = append(p.Keep, id)
			kept++
		default:
			p.Prune = append(p.Prune, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}
	// Referenced keys of kept releases; everything else the publisher stored is deletable.
	ref := map[string]bool{}
	for _, id := range p.Keep {
		var mk, ek string
		if err := q.QueryRowContext(ctx, `SELECT manifest_key, entry_key FROM assets_releases WHERE game_id=? AND release_id=?`, gameID, id).Scan(&mk, &ek); err != nil {
			return p, err
		}
		ref[mk], ref[ek] = true, true
		fr, err := q.QueryContext(ctx, `SELECT object_key FROM assets_release_files WHERE game_id=? AND release_id=?`, gameID, id)
		if err != nil {
			return p, err
		}
		for fr.Next() {
			var k string
			if err := fr.Scan(&k); err != nil {
				fr.Close()
				return p, err
			}
			ref[k] = true
		}
		fr.Close()
		if err := fr.Err(); err != nil {
			return p, err
		}
	}
	or, err := q.QueryContext(ctx, `SELECT object_key FROM assets_objects WHERE game_id=? ORDER BY object_key`, gameID)
	if err != nil {
		return p, err
	}
	defer or.Close()
	for or.Next() {
		var k string
		if err := or.Scan(&k); err != nil {
			return p, err
		}
		if !ref[k] {
			p.Delete = append(p.Delete, k)
		}
	}
	return p, or.Err()
}

// Prune keeps the newest keep published releases plus the current one, marks
// the others pruned and deletes every stored object no kept release references
// (including objects left by failed publishes). With apply=false it only
// returns the plan. Applying holds the publish lease for the whole run.
func (m *Module) Prune(ctx context.Context, gameID string, keep int, apply bool, actor string) (PrunePlan, error) {
	if keep < 1 {
		return PrunePlan{}, errors.New("assets: keep must be at least 1")
	}
	if err := m.checkGame(ctx, gameID); err != nil {
		return PrunePlan{}, err
	}
	if !apply {
		var p PrunePlan
		err := m.DB.ReadTx(ctx, func(tx *sql.Tx) error {
			var err error
			p, err = planPrune(ctx, tx, gameID, keep)
			return err
		})
		return p, err
	}
	start := m.now()
	l := &lease{m: m, gameID: gameID, holder: "prune:" + newReleaseID(start), renewed: start}
	if err := m.acquire(ctx, gameID, l.holder, actor); err != nil {
		return PrunePlan{}, err
	}
	defer l.release(context.WithoutCancel(ctx))
	var p PrunePlan
	err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if err := checkHolder(ctx, tx, gameID, l.holder); err != nil {
			return err
		}
		var err error
		if p, err = planPrune(ctx, tx, gameID, keep); err != nil {
			return err
		}
		for _, id := range p.Prune {
			if _, err := tx.ExecContext(ctx, `DELETE FROM assets_release_files WHERE game_id=? AND release_id=?`, gameID, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE assets_releases SET status='pruned' WHERE game_id=? AND release_id=?`, gameID, id); err != nil {
				return err
			}
		}
		for _, k := range p.Delete {
			if _, err := tx.ExecContext(ctx, `UPDATE assets_objects SET state='deleting' WHERE game_id=? AND object_key=?`, gameID, k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return p, err
	}
	p.Applied = true
	// Objects are now unreferenced and marked; delete them, then their rows. A
	// failed delete leaves the row 'deleting' and the next prune retries it.
	for _, k := range p.Delete {
		if err := l.renew(ctx, false); err != nil {
			return p, err
		}
		if err := m.Storage.Delete(ctx, k); err != nil {
			p.Failures = append(p.Failures, k+": "+err.Error())
			continue
		}
		if err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM assets_objects WHERE game_id=? AND object_key=? AND state='deleting'`, gameID, k)
			return err
		}); err != nil {
			p.Failures = append(p.Failures, k+": "+err.Error())
			continue
		}
		p.Deleted++
	}
	sort.Strings(p.Failures)
	return p, nil
}
