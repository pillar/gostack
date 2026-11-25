// Package storage is the ObjectStorage port (PRD §4.2): Put/Get/Delete/Presign
// with streaming IO, per-operation timeouts, an object size limit and strict
// key validation. Adapters: Local (filesystem, HMAC-signed URLs served by
// Local.Handler) and S3 (S3-compatible: AWS S3, Cloudflare R2, MinIO) with a
// hand-written AWS Signature Version 4 signer.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Storage is implemented by Local and S3.
type Storage interface {
	// Put streams r to key. size is the exact byte count, or -1 if unknown.
	// Objects larger than the adapter's MaxObjectBytes fail with ErrTooLarge.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get streams an object. The caller must Close the reader; the operation
	// timeout covers reading the body.
	Get(ctx context.Context, key string) (io.ReadCloser, Info, error)
	// Delete removes key. Deleting a missing key succeeds.
	Delete(ctx context.Context, key string) error
	// Presign returns a URL valid for ttl that allows method (GET or PUT) on key
	// without other credentials.
	Presign(ctx context.Context, method, key string, ttl time.Duration) (string, error)
}

// Info describes a stored object.
type Info struct {
	Key         string
	Size        int64
	ContentType string
	ETag        string
	// CacheControl is the Cache-Control stored with the object ("" when none).
	CacheControl string
}

// PutOptions are object attributes for ObjectPutter.PutObject.
type PutOptions struct {
	ContentType string
	// CacheControl is stored with the object and returned by S3/CDN GETs
	// (e.g. "public, max-age=31536000, immutable"). Printable ASCII, <= 255 bytes.
	CacheControl string
}

// ObjectPutter is implemented by adapters that store attributes beyond the
// content type (Local and S3). Put(ctx, k, r, n, ct) equals
// PutObject(ctx, k, r, n, PutOptions{ContentType: ct}).
type ObjectPutter interface {
	PutObject(ctx context.Context, key string, r io.Reader, size int64, o PutOptions) error
}

// Stater reports an object's metadata without transferring its body
// (Local: reads the metadata trailer; S3: HEAD). Missing keys yield ErrNotFound.
type Stater interface {
	Stat(ctx context.Context, key string) (Info, error)
}

var (
	ErrNotFound     = errors.New("storage: object not found")
	ErrTooLarge     = errors.New("storage: object exceeds size limit")
	ErrInvalidKey   = errors.New("storage: invalid key")
	ErrSizeMismatch = errors.New("storage: body length does not match declared size")
	ErrContentType  = errors.New("storage: invalid content type")
)

// Defaults shared by the adapters.
const (
	DefaultMaxObjectBytes = 100 << 20
	DefaultTimeout        = 60 * time.Second
	MaxKeyLen             = 512
	// MaxKeySegmentLen bounds each '/'-separated segment: MinIO (and common
	// filesystems) reject path components longer than 255 bytes.
	MaxKeySegmentLen   = 255
	MaxPresignTTL      = 7 * 24 * time.Hour
	defaultContentType = "application/octet-stream"
)

// ValidKey enforces the portable key syntax: 1–512 bytes of [A-Za-z0-9._-]
// segments separated by single "/", no leading or trailing "/", and no "." or
// ".." segment. These keys need no escaping in URLs or SigV4 canonical paths.
func ValidKey(key string) error {
	if len(key) == 0 || len(key) > MaxKeyLen {
		return fmt.Errorf("%w: length must be 1-%d", ErrInvalidKey, MaxKeyLen)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: empty, '.' or '..' path segment", ErrInvalidKey)
		}
		if len(seg) > MaxKeySegmentLen {
			return fmt.Errorf("%w: path segment longer than %d bytes", ErrInvalidKey, MaxKeySegmentLen)
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return fmt.Errorf("%w: character %q not allowed", ErrInvalidKey, c)
			}
		}
	}
	return nil
}

func validContentType(ct string) (string, error) {
	if ct == "" {
		return defaultContentType, nil
	}
	if len(ct) > 255 {
		return "", fmt.Errorf("%w: too long", ErrContentType)
	}
	for i := 0; i < len(ct); i++ {
		if ct[i] < 0x20 || ct[i] > 0x7e {
			return "", fmt.Errorf("%w: invalid characters", ErrContentType)
		}
	}
	return ct, nil
}

func validCacheControl(cc string) error {
	if len(cc) > 255 {
		return errors.New("storage: cache control too long")
	}
	for i := 0; i < len(cc); i++ {
		if cc[i] < 0x20 || cc[i] > 0x7e {
			return errors.New("storage: cache control has invalid characters")
		}
	}
	return nil
}

func checkPresign(method string, ttl time.Duration) error {
	if method != http.MethodGet && method != http.MethodPut {
		return fmt.Errorf("storage: presign method must be GET or PUT, got %q", method)
	}
	if ttl < time.Second || ttl > MaxPresignTTL {
		return errors.New("storage: presign ttl must be between 1s and 7d")
	}
	return nil
}

// ctxReader fails reads once ctx is done, so a stalled or slow source cannot
// outlive the operation timeout.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// exactReader yields exactly n bytes from r and fails with ErrSizeMismatch if
// r is shorter or longer, or with ErrTooLarge once more than max bytes are seen.
type exactReader struct {
	r    io.Reader
	n    int64 // declared size, -1 unknown
	max  int64
	read int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	limit := e.max + 1
	if e.n >= 0 {
		limit = e.n + 1
	}
	if rem := limit - e.read; int64(len(p)) > rem {
		p = p[:rem]
	}
	if len(p) == 0 {
		return 0, ErrSizeMismatch
	}
	k, err := e.r.Read(p)
	e.read += int64(k)
	if e.n < 0 && e.read > e.max {
		return k, ErrTooLarge
	}
	if e.n >= 0 && e.read > e.n {
		return k, ErrSizeMismatch
	}
	if err == io.EOF && e.n >= 0 && e.read != e.n {
		return k, ErrSizeMismatch
	}
	return k, err
}

// cancelOnClose ties an operation context's cancel func to the body's lifetime.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = DefaultTimeout
	}
	return context.WithTimeout(ctx, d)
}

func maxBytes(n int64) int64 {
	if n <= 0 {
		return DefaultMaxObjectBytes
	}
	return n
}
