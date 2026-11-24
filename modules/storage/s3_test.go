package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- Independent SigV4 verifier (deliberately not using the production signer). ----

func vHMAC(k []byte, s string) []byte {
	m := hmac.New(sha256.New, k)
	m.Write([]byte(s))
	return m.Sum(nil)
}
func vHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// vEscape: QueryEscape, then fix the differences from RFC 3986 unreserved encoding.
func vEscape(s string) string {
	return strings.NewReplacer("+", "%20", "%7E", "~").Replace(url.QueryEscape(s))
}

func vQuery(q url.Values, skip string) string {
	var parts []string
	for k, vs := range q {
		if k == skip {
			continue
		}
		for _, v := range vs {
			parts = append(parts, vEscape(k)+"="+vEscape(v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func vSignature(secret, region, amzDate, canonical string) string {
	date := amzDate[:8]
	scope := date + "/" + region + "/s3/aws4_request"
	k := vHMAC(vHMAC(vHMAC(vHMAC([]byte("AWS4"+secret), date), region), "s3"), "aws4_request")
	return hex.EncodeToString(vHMAC(k, "AWS4-HMAC-SHA256\n"+amzDate+"\n"+scope+"\n"+vHash([]byte(canonical))))
}

// verifySigV4 checks a header-signed or presigned request. body is the full
// request body (nil for presigned/bodiless requests).
func verifySigV4(r *http.Request, body []byte, akid, secret, region string, now time.Time) error {
	q := r.URL.Query()
	path := r.URL.EscapedPath()
	if sig := q.Get("X-Amz-Signature"); sig != "" {
		amzDate := q.Get("X-Amz-Date")
		t, err := time.Parse("20060102T150405Z", amzDate)
		if err != nil {
			return err
		}
		exp, _ := strconv.Atoi(q.Get("X-Amz-Expires"))
		if now.Before(t.Add(-5*time.Minute)) || now.After(t.Add(time.Duration(exp)*time.Second)) || exp > 604800 {
			return errors.New("presigned URL expired")
		}
		if q.Get("X-Amz-Credential") != akid+"/"+amzDate[:8]+"/"+region+"/s3/aws4_request" || q.Get("X-Amz-SignedHeaders") != "host" {
			return errors.New("bad credential scope")
		}
		cr := r.Method + "\n" + path + "\n" + vQuery(q, "X-Amz-Signature") + "\nhost:" + r.Host + "\n\nhost\nUNSIGNED-PAYLOAD"
		if !hmac.Equal([]byte(sig), []byte(vSignature(secret, region, amzDate, cr))) {
			return errors.New("SignatureDoesNotMatch")
		}
		return nil
	}
	auth := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(auth, "AWS4-HMAC-SHA256 ")
	if !ok {
		return errors.New("missing authorization")
	}
	fields := map[string]string{}
	for _, p := range strings.Split(rest, ", ") {
		k, v, _ := strings.Cut(p, "=")
		fields[k] = v
	}
	amzDate := r.Header.Get("X-Amz-Date")
	t, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil || now.Sub(t) > 15*time.Minute || t.Sub(now) > 15*time.Minute {
		return errors.New("request time skewed")
	}
	if fields["Credential"] != akid+"/"+amzDate[:8]+"/"+region+"/s3/aws4_request" {
		return errors.New("bad credential scope")
	}
	signed := strings.Split(fields["SignedHeaders"], ";")
	var ch strings.Builder
	seen := map[string]bool{}
	for _, h := range signed {
		seen[h] = true
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		ch.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	for _, need := range []string{"host", "x-amz-date", "x-amz-content-sha256"} {
		if !seen[need] {
			return fmt.Errorf("%s not signed", need)
		}
	}
	if r.Header.Get("X-Amz-Security-Token") != "" && !seen["x-amz-security-token"] {
		return errors.New("security token not signed")
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload != "UNSIGNED-PAYLOAD" && body != nil && payload != vHash(body) {
		return errors.New("XAmzContentSHA256Mismatch")
	}
	cr := r.Method + "\n" + path + "\n" + vQuery(q, "") + "\n" + ch.String() + "\n" + fields["SignedHeaders"] + "\n" + payload
	if !hmac.Equal([]byte(fields["Signature"]), []byte(vSignature(secret, region, amzDate, cr))) {
		return errors.New("SignatureDoesNotMatch")
	}
	return nil
}

// The verifier itself is anchored to the AWS documentation examples.
func TestVerifierAgainstAWSVectors(t *testing.T) {
	secret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	r := httptest.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	r.Header.Set("Range", "bytes=0-9")
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("X-Amz-Content-Sha256", EmptySHA256)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, "+
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
	if err := verifySigV4(r, nil, "AKIAIOSFODNN7EXAMPLE", secret, "us-east-1", awsExampleTime); err != nil {
		t.Fatal(err)
	}
	p := httptest.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256"+
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z"+
		"&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404", nil)
	if err := verifySigV4(p, nil, "AKIAIOSFODNN7EXAMPLE", secret, "us-east-1", awsExampleTime); err != nil {
		t.Fatal(err)
	}
	if err := verifySigV4(p, nil, "AKIAIOSFODNN7EXAMPLE", secret+"x", "us-east-1", awsExampleTime); err == nil {
		t.Fatal("wrong secret accepted")
	}
}

// ---- Fake S3 ----

type fakeObject struct {
	body        []byte
	contentType string
}

type fakeS3 struct {
	akid, secret, region, bucket string
	pathStyle                    bool
	now                          func() time.Time
	mu                           sync.Mutex
	objects                      map[string]fakeObject
	requests                     atomic.Int64
	slow                         time.Duration
}

func (f *fakeS3) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<?xml version=\"1.0\"?><Error><Code>%s</Code><Message>x</Message></Error>", code)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	var bucket, key string
	if f.pathStyle {
		bucket, key, _ = strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	} else {
		bucket, _, _ = strings.Cut(r.Host, ".")
		key = strings.TrimPrefix(r.URL.Path, "/")
	}
	if bucket != f.bucket {
		f.fail(w, 404, "NoSuchBucket")
		return
	}
	var body []byte
	if r.Method == http.MethodPut {
		if r.ContentLength < 0 {
			f.fail(w, 411, "MissingContentLength")
			return
		}
		var err error
		if body, err = io.ReadAll(r.Body); err != nil || int64(len(body)) != r.ContentLength {
			f.fail(w, 400, "IncompleteBody")
			return
		}
	}
	if err := verifySigV4(r, body, f.akid, f.secret, f.region, f.now()); err != nil {
		f.fail(w, 403, strings.SplitN(err.Error(), " ", 2)[0])
		return
	}
	if f.slow > 0 {
		time.Sleep(f.slow)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		f.objects[key] = fakeObject{body, r.Header.Get("Content-Type")}
		w.Header().Set("ETag", `"`+vHash(body)[:32]+`"`)
	case http.MethodGet:
		o, ok := f.objects[key]
		if !ok {
			f.fail(w, 404, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Type", o.contentType)
		w.Header().Set("ETag", `"`+vHash(o.body)[:32]+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
		w.Write(o.body)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(204)
	default:
		f.fail(w, 405, "MethodNotAllowed")
	}
}

func newFakeS3(t *testing.T, pathStyle, signPayload bool, token string) (*S3, *fakeS3, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	f := &fakeS3{akid: "AKIDTEST", secret: "secret/test+key", region: "auto", bucket: "assets", pathStyle: pathStyle,
		now: clk.Now, objects: map[string]fakeObject{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	endpoint := srv.URL
	client := srv.Client()
	if !pathStyle {
		// Virtual-host style: resolve any *.s3.fake.test host to the test server.
		addr := srv.Listener.Addr().String()
		_, port, _ := net.SplitHostPort(addr)
		endpoint = "http://s3.fake.test:" + port
		client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
	}
	s, err := NewS3(S3Config{Endpoint: endpoint, Region: "auto", Bucket: "assets", AccessKeyID: "AKIDTEST", SecretAccessKey: "secret/test+key",
		SessionToken: token, PathStyle: pathStyle, SignPayload: signPayload, MaxObjectBytes: 1024, Timeout: 2 * time.Second,
		TempDir: t.TempDir(), Client: client, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return s, f, clk
}

func exerciseS3(t *testing.T, s *S3, f *fakeS3) {
	t.Helper()
	ctx := context.Background()
	if err := s.Put(ctx, "dir/file-1.json", strings.NewReader(`{"a":1}`), 7, "application/json"); err != nil {
		t.Fatal(err)
	}
	body, info := readAll(t, s, "dir/file-1.json")
	if body != `{"a":1}` || info.Size != 7 || info.ContentType != "application/json" || info.ETag == "" {
		t.Fatalf("get: %q %+v", body, info)
	}
	// Unknown size is spooled and still uploaded with Content-Length.
	if err := s.Put(ctx, "stream", strings.NewReader("streamed body"), -1, ""); err != nil {
		t.Fatal(err)
	}
	if body, info := readAll(t, s, "stream"); body != "streamed body" || info.ContentType != "application/octet-stream" {
		t.Fatalf("stream: %q %+v", body, info)
	}
	if err := s.Put(ctx, "empty", strings.NewReader(""), 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "dir/file-1.json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, "dir/file-1.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.Delete(ctx, "dir/file-1.json"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestS3PathStyleUnsignedPayload(t *testing.T) {
	s, f, _ := newFakeS3(t, true, false, "")
	exerciseS3(t, s, f)
}

func TestS3VirtualHostSignedPayloadWithToken(t *testing.T) {
	s, f, _ := newFakeS3(t, false, true, "session-token")
	if u := s.objectURL("k").String(); !strings.HasPrefix(u, "http://assets.s3.fake.test:") {
		t.Fatalf("virtual host url %s", u)
	}
	exerciseS3(t, s, f)
}

func TestS3RejectsBadCredentialsAndLimits(t *testing.T) {
	s, f, _ := newFakeS3(t, true, false, "")
	ctx := context.Background()
	bad := *s
	bad.signer.SecretAccessKey = "wrong-secret-value"
	err := bad.Put(ctx, "k", strings.NewReader("x"), 1, "")
	var se *s3Error
	if !errors.As(err, &se) || se.Status != 403 || se.Code != "SignatureDoesNotMatch" || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("bad secret: %v", err)
	}
	before := f.requests.Load()
	if err := s.Put(ctx, "k", strings.NewReader(strings.Repeat("x", 2000)), 2000, ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("declared too large: %v", err)
	}
	if err := s.Put(ctx, "k", strings.NewReader(strings.Repeat("x", 2000)), -1, ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("streamed too large: %v", err)
	}
	if f.requests.Load() != before {
		t.Fatal("oversized upload reached the server")
	}
	if err := s.Put(ctx, "k", strings.NewReader("short"), 10, ""); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("short body: %v", err)
	}
	if _, ok := f.objects["k"]; ok {
		t.Fatal("short body stored")
	}
	if err := s.Put(ctx, "../k", strings.NewReader("x"), 1, ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid key: %v", err)
	}
	f.slow = 300 * time.Millisecond
	s.c.Timeout = 100 * time.Millisecond
	if _, _, err := s.Get(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
}

func TestS3PresignedURLs(t *testing.T) {
	s, _, clk := newFakeS3(t, true, false, "tok")
	ctx := context.Background()
	put, err := s.Presign(ctx, "PUT", "up/x.bin", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(put, "X-Amz-Security-Token=tok") || !strings.Contains(put, "X-Amz-Expires=300") {
		t.Fatalf("url %s", put)
	}
	req, _ := http.NewRequest("PUT", put, strings.NewReader("payload"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("presigned put: %v %v", resp, err)
	}
	resp.Body.Close()
	get, _ := s.Presign(ctx, "GET", "up/x.bin", 5*time.Minute)
	resp, err = http.Get(get)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "payload" {
		t.Fatalf("presigned get: %d %s", resp.StatusCode, b)
	}
	// The GET signature does not authorize another key or method.
	for _, c := range []struct{ method, url string }{
		{"GET", strings.Replace(get, "up/x.bin", "up/y.bin", 1)},
		{"PUT", get},
	} {
		req, _ := http.NewRequest(c.method, c.url, strings.NewReader(""))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("%s %s: %d", c.method, c.url, resp.StatusCode)
		}
	}
	clk.Add(6 * time.Minute)
	resp, _ = http.Get(get)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("expired url: %d", resp.StatusCode)
	}
	if _, err := s.Presign(ctx, "DELETE", "k", time.Minute); err == nil {
		t.Fatal("presign DELETE accepted")
	}
}

func TestS3ConfigValidation(t *testing.T) {
	base := S3Config{Endpoint: "https://x.r2.cloudflarestorage.com", Region: "auto", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"}
	if _, err := NewS3(base); err != nil {
		t.Fatal(err)
	}
	for _, mut := range []func(*S3Config){
		func(c *S3Config) { c.Endpoint = "ftp://x" },
		func(c *S3Config) { c.Endpoint = "https://x/base" },
		func(c *S3Config) { c.Bucket = "" },
		func(c *S3Config) { c.Bucket = "a/b" },
		func(c *S3Config) { c.SecretAccessKey = "" },
	} {
		c := base
		mut(&c)
		if _, err := NewS3(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}
