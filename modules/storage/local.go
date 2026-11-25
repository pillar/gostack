package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pillar/gostack/core/httpx"
)

// LocalConfig configures the filesystem adapter.
type LocalConfig struct {
	// Root directory; created if missing. Objects live under Root/objects,
	// in-flight uploads under Root/tmp (same filesystem, so rename is atomic).
	Root string
	// PublicURL is the absolute URL (or absolute path) where Handler is mounted,
	// e.g. "https://api.example.com/v1/files". Presigned URLs are PublicURL/<key>?...
	PublicURL string
	// Secret signs presigned URLs (HMAC-SHA256); at least 32 bytes.
	Secret         []byte
	MaxObjectBytes int64         // default DefaultMaxObjectBytes
	Timeout        time.Duration // per operation, default DefaultTimeout
	Now            func() time.Time
}

// Local stores each object as one file named by the SHA-256 of its key (so
// keys never become filesystem paths and case-insensitive filesystems cannot
// alias two keys). The file holds the body followed by a JSON metadata trailer
// and its 4-byte length; it is written to a temp file, fsynced and renamed, so
// readers see either the old or the new object, never a partial one.
type Local struct {
	c      LocalConfig
	prefix string // URL path prefix served by Handler
}

// NewLocal validates cfg and creates the directories.
func NewLocal(cfg LocalConfig) (*Local, error) {
	if cfg.Root == "" {
		return nil, errors.New("storage: local root required")
	}
	if len(cfg.Secret) < 32 {
		return nil, errors.New("storage: local presign secret must be at least 32 bytes")
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || cfg.PublicURL == "" || !strings.HasPrefix(u.Path, "/") || u.RawQuery != "" {
		return nil, errors.New("storage: local public URL must be an absolute URL or path without query")
	}
	for _, d := range []string{"objects", "tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.Root, d), 0o750); err != nil {
			return nil, err
		}
	}
	cfg.MaxObjectBytes = maxBytes(cfg.MaxObjectBytes)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.PublicURL = strings.TrimSuffix(cfg.PublicURL, "/")
	return &Local{c: cfg, prefix: strings.TrimSuffix(u.Path, "/") + "/"}, nil
}

type localMeta struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	// CacheControl is optional so objects written before it existed still read.
	CacheControl string `json:"cache_control,omitempty"`
}

func (l *Local) path(key string) string {
	h := sha256.Sum256([]byte(key))
	x := hex.EncodeToString(h[:])
	return filepath.Join(l.c.Root, "objects", x[:2], x)
}

// Put writes atomically; size -1 means unknown (still bounded by MaxObjectBytes).
func (l *Local) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	return l.PutObject(ctx, key, r, size, PutOptions{ContentType: contentType})
}

// PutObject is Put with options. The Cache-Control is stored in the metadata
// trailer and reported by Get/Stat; Handler still serves presigned downloads
// with "private, no-store" (they are bearer capabilities, not CDN objects).
func (l *Local) PutObject(ctx context.Context, key string, r io.Reader, size int64, o PutOptions) (err error) {
	if err := ValidKey(key); err != nil {
		return err
	}
	ct, err := validContentType(o.ContentType)
	if err != nil {
		return err
	}
	if err := validCacheControl(o.CacheControl); err != nil {
		return err
	}
	if size > l.c.MaxObjectBytes {
		return ErrTooLarge
	}
	ctx, cancel := withTimeout(ctx, l.c.Timeout)
	defer cancel()

	f, err := os.CreateTemp(filepath.Join(l.c.Root, "tmp"), "put-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), &exactReader{r: ctxReader{ctx, r}, n: size, max: l.c.MaxObjectBytes})
	if err != nil {
		return err
	}
	meta, err := json.Marshal(localMeta{Key: key, Size: n, ContentType: ct, SHA256: hex.EncodeToString(h.Sum(nil)), CacheControl: o.CacheControl})
	if err != nil {
		return err
	}
	meta = binary.BigEndian.AppendUint32(meta, uint32(len(meta)))
	if _, err = f.Write(meta); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	dst := l.path(key)
	if err = os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type fileBody struct {
	io.Reader
	f *os.File
}

func (b fileBody) Close() error { return b.f.Close() }

// Get opens an object for streaming.
func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	if err := ValidKey(key); err != nil {
		return nil, Info{}, err
	}
	f, err := os.Open(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, Info{}, ErrNotFound
	}
	if err != nil {
		return nil, Info{}, err
	}
	m, err := readTrailer(f)
	if err != nil {
		f.Close()
		return nil, Info{}, err
	}
	if m.Key != key {
		f.Close()
		return nil, Info{}, fmt.Errorf("storage: object file for %q holds key %q", key, m.Key)
	}
	ctx, cancel := withTimeout(ctx, l.c.Timeout)
	body := fileBody{Reader: ctxReader{ctx, io.NewSectionReader(f, 0, m.Size)}, f: f}
	return cancelOnClose{body, cancel}, m.info(), nil
}

func (m localMeta) info() Info {
	return Info{Key: m.Key, Size: m.Size, ContentType: m.ContentType, ETag: `"` + m.SHA256 + `"`, CacheControl: m.CacheControl}
}

// Stat reads an object's metadata trailer.
func (l *Local) Stat(_ context.Context, key string) (Info, error) {
	if err := ValidKey(key); err != nil {
		return Info{}, err
	}
	f, err := os.Open(l.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, ErrNotFound
	}
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	m, err := readTrailer(f)
	if err != nil {
		return Info{}, err
	}
	if m.Key != key {
		return Info{}, fmt.Errorf("storage: object file for %q holds key %q", key, m.Key)
	}
	return m.info(), nil
}

func readTrailer(f *os.File) (localMeta, error) {
	var m localMeta
	st, err := f.Stat()
	if err != nil {
		return m, err
	}
	var lb [4]byte
	if st.Size() < 4 {
		return m, errors.New("storage: corrupt object file")
	}
	if _, err := f.ReadAt(lb[:], st.Size()-4); err != nil {
		return m, err
	}
	n := int64(binary.BigEndian.Uint32(lb[:]))
	if n > 4096 || n+4 > st.Size() {
		return m, errors.New("storage: corrupt object trailer")
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-4-n); err != nil {
		return m, err
	}
	if err := json.Unmarshal(buf, &m); err != nil || m.Size != st.Size()-4-n {
		return m, errors.New("storage: corrupt object trailer")
	}
	return m, nil
}

// Delete removes key; a missing key is not an error.
func (l *Local) Delete(ctx context.Context, key string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	err := os.Remove(l.path(key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (l *Local) mac(method, key string, expires int64) []byte {
	var m hash.Hash = hmac.New(sha256.New, l.c.Secret)
	fmt.Fprintf(m, "GOSTACK-LOCAL-V1\n%s\n%s\n%d", method, key, expires)
	return m.Sum(nil)
}

// Presign returns PublicURL/<key>?X-GS-Expires=<unix>&X-GS-Signature=<hex>,
// signed over method, key and expiry. No IO is performed.
func (l *Local) Presign(_ context.Context, method, key string, ttl time.Duration) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	if err := checkPresign(method, ttl); err != nil {
		return "", err
	}
	exp := l.c.Now().Add(ttl).Unix()
	return fmt.Sprintf("%s/%s?X-GS-Expires=%d&X-GS-Signature=%s", l.c.PublicURL, key, exp, hex.EncodeToString(l.mac(method, key, exp))), nil
}

// Handler serves presigned GET and PUT requests under PublicURL's path; mount
// it at that path with a trailing slash (e.g. mux.Handle("/v1/files/", h)).
// It verifies method, expiry and signature (constant-time comparison).
// Downloads are sent with nosniff and a sandbox CSP so uploaded HTML cannot run
// script on the API origin.
func (l *Local) Handler() http.Handler {
	return httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		key, ok := strings.CutPrefix(r.URL.Path, l.prefix)
		if !ok || ValidKey(key) != nil {
			return httpx.NotFound("object not found")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPut {
			w.Header().Set("Allow", "GET, PUT")
			return httpx.NewError(http.StatusMethodNotAllowed, "method_not_allowed", "only GET and PUT are allowed")
		}
		q := r.URL.Query()
		exp, err := strconv.ParseInt(q.Get("X-GS-Expires"), 10, 64)
		sig, err2 := hex.DecodeString(q.Get("X-GS-Signature"))
		now := l.c.Now().Unix()
		if err != nil || err2 != nil || exp < now || exp-now > int64(MaxPresignTTL/time.Second) ||
			!hmac.Equal(sig, l.mac(r.Method, key, exp)) {
			return httpx.Forbidden("invalid or expired signature")
		}
		if r.Method == http.MethodPut {
			err := l.Put(r.Context(), key, r.Body, r.ContentLength, r.Header.Get("Content-Type"))
			switch {
			case errors.Is(err, ErrTooLarge):
				return httpx.TooLarge("object too large")
			case errors.Is(err, ErrSizeMismatch):
				return httpx.BadRequest("body length does not match Content-Length")
			case errors.Is(err, ErrContentType):
				return httpx.BadRequest(err.Error())
			case err != nil:
				return err
			}
			w.WriteHeader(http.StatusOK)
			return nil
		}
		body, info, err := l.Get(r.Context(), key)
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound("object not found")
		}
		if err != nil {
			return err
		}
		defer body.Close()
		h := w.Header()
		h.Set("Content-Type", info.ContentType)
		h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
		h.Set("ETag", info.ETag)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "sandbox")
		h.Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, body)
		return nil
	})
}

var (
	_ Storage      = (*Local)(nil)
	_ ObjectPutter = (*Local)(nil)
	_ Stater       = (*Local)(nil)
)
