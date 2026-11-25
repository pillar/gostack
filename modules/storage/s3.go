package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// S3Config configures the S3-compatible adapter (AWS S3, Cloudflare R2, MinIO).
type S3Config struct {
	// Endpoint is scheme://host[:port] without a path, e.g.
	// "https://<account>.r2.cloudflarestorage.com" or "https://s3.us-east-1.amazonaws.com".
	Endpoint string
	Region   string // "auto" for R2
	Bucket   string
	// Credentials. Never logged or returned in errors.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// PathStyle uses endpoint/bucket/key (MinIO, R2); otherwise bucket.endpoint/key.
	PathStyle bool
	// SignPayload signs the SHA-256 of upload bodies instead of UNSIGNED-PAYLOAD.
	// It requires spooling the body to TempDir first to hash it.
	SignPayload    bool
	MaxObjectBytes int64         // default DefaultMaxObjectBytes
	Timeout        time.Duration // per operation, including body transfer; default DefaultTimeout
	TempDir        string        // spool directory; default os.TempDir()
	Client         *http.Client  // default: a client without global timeout (operations use contexts)
	Now            func() time.Time
}

// S3 talks to an S3-compatible API with AWS SigV4.
type S3 struct {
	c        S3Config
	endpoint *url.URL
	signer   Signer
}

// NewS3 validates cfg.
func NewS3(cfg S3Config) (*S3, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, errors.New("storage: s3 endpoint must be scheme://host[:port] without path")
	}
	u.Path = ""
	if cfg.Region == "" || cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("storage: s3 region, bucket and credentials are required")
	}
	if ValidKey(cfg.Bucket) != nil || strings.Contains(cfg.Bucket, "/") {
		return nil, errors.New("storage: invalid s3 bucket name")
	}
	cfg.MaxObjectBytes = maxBytes(cfg.MaxObjectBytes)
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &S3{c: cfg, endpoint: u, signer: Signer{AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey,
		SessionToken: cfg.SessionToken, Region: cfg.Region, Service: "s3"}}, nil
}

func (s *S3) objectURL(key string) *url.URL {
	u := *s.endpoint
	if s.c.PathStyle {
		u.Path = "/" + s.c.Bucket + "/" + key
	} else {
		u.Host = s.c.Bucket + "." + u.Host
		u.Path = "/" + key
	}
	return &u
}

// s3Error is returned for non-success responses; it carries the S3 error code,
// never credentials.
type s3Error struct {
	Op     string
	Key    string
	Status int
	Code   string
}

func (e *s3Error) Error() string {
	return fmt.Sprintf("storage: s3 %s %q: HTTP %d %s", e.Op, e.Key, e.Status, e.Code)
}

func responseError(op, key string, resp *http.Response) error {
	var body struct {
		Code string `xml:"Code"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = xml.Unmarshal(b, &body)
	return &s3Error{Op: op, Key: key, Status: resp.StatusCode, Code: body.Code}
}

func (s *S3) do(ctx context.Context, method, key string, body io.Reader, size int64, payloadHash string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.objectURL(key).String(), body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	s.signer.Sign(req, payloadHash, s.c.Now())
	return s.c.Client.Do(req)
}

// Put uploads with a single PUT. Unknown sizes (size < 0) and SignPayload
// uploads are first spooled to a temp file (bounded by MaxObjectBytes), never
// buffered in memory.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	return s.PutObject(ctx, key, r, size, PutOptions{ContentType: contentType})
}

// PutObject is Put with options; CacheControl is sent as the object's
// Cache-Control header (stored by S3/R2/MinIO and returned on GET).
func (s *S3) PutObject(ctx context.Context, key string, r io.Reader, size int64, o PutOptions) error {
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
	if size > s.c.MaxObjectBytes {
		return ErrTooLarge
	}
	ctx, cancel := withTimeout(ctx, s.c.Timeout)
	defer cancel()

	var body io.Reader = &exactReader{r: ctxReader{ctx, r}, n: size, max: s.c.MaxObjectBytes}
	payload := UnsignedPayload
	if size < 0 || s.c.SignPayload {
		f, err := os.CreateTemp(s.c.TempDir, "gostack-s3-*")
		if err != nil {
			return err
		}
		defer func() { f.Close(); os.Remove(f.Name()) }()
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), body)
		if err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		size, body = n, ctxReader{ctx, f}
		if s.c.SignPayload {
			payload = hex.EncodeToString(h.Sum(nil))
		}
	}
	hdr := http.Header{"Content-Type": {ct}}
	if o.CacheControl != "" {
		hdr.Set("Cache-Control", o.CacheControl)
	}
	resp, err := s.do(ctx, http.MethodPut, key, body, size, payload, hdr)
	if err != nil {
		var se error
		for _, e := range []error{ErrTooLarge, ErrSizeMismatch} {
			if errors.Is(err, e) {
				se = e
			}
		}
		if se != nil {
			return se
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return responseError("PUT", key, resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// Get streams an object; the per-operation timeout keeps running until Close.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	if err := ValidKey(key); err != nil {
		return nil, Info{}, err
	}
	ctx, cancel := withTimeout(ctx, s.c.Timeout)
	resp, err := s.do(ctx, http.MethodGet, key, nil, 0, EmptySHA256, nil)
	if err != nil {
		cancel()
		return nil, Info{}, err
	}
	if resp.StatusCode != http.StatusOK {
		defer cancel()
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, Info{}, ErrNotFound
		}
		return nil, Info{}, responseError("GET", key, resp)
	}
	return cancelOnClose{resp.Body, cancel}, headerInfo(key, resp), nil
}

func headerInfo(key string, resp *http.Response) Info {
	return Info{Key: key, Size: resp.ContentLength, ContentType: resp.Header.Get("Content-Type"),
		ETag: resp.Header.Get("ETag"), CacheControl: resp.Header.Get("Cache-Control")}
}

// Stat issues a HEAD request.
func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	if err := ValidKey(key); err != nil {
		return Info{}, err
	}
	ctx, cancel := withTimeout(ctx, s.c.Timeout)
	defer cancel()
	resp, err := s.do(ctx, http.MethodHead, key, nil, 0, EmptySHA256, nil)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Info{}, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return Info{}, &s3Error{Op: "HEAD", Key: key, Status: resp.StatusCode} // HEAD responses carry no error body
	}
	return headerInfo(key, resp), nil
}

// Delete removes key (S3 reports success for missing keys too).
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	ctx, cancel := withTimeout(ctx, s.c.Timeout)
	defer cancel()
	resp, err := s.do(ctx, http.MethodDelete, key, nil, 0, EmptySHA256, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return responseError("DELETE", key, resp)
	}
	return nil
}

// Presign returns a SigV4 query-signed URL (X-Amz-*); no IO is performed.
func (s *S3) Presign(_ context.Context, method, key string, ttl time.Duration) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	if err := checkPresign(method, ttl); err != nil {
		return "", err
	}
	return s.signer.Presign(method, s.objectURL(key), s.c.Now(), ttl).String(), nil
}

var (
	_ Storage      = (*S3)(nil)
	_ ObjectPutter = (*S3)(nil)
	_ Stater       = (*S3)(nil)
)
