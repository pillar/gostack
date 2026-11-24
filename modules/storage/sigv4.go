package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4 (header and query-string/presigned forms) for S3.
// Reference: AWS "Authenticating Requests (AWS Signature Version 4)".

const (
	sigAlgorithm = "AWS4-HMAC-SHA256"
	// UnsignedPayload is the x-amz-content-sha256 value for unsigned bodies.
	UnsignedPayload = "UNSIGNED-PAYLOAD"
	// EmptySHA256 is the hex SHA-256 of an empty body.
	EmptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	amzDateFmt  = "20060102T150405Z"
)

// Signer holds credentials and the signing scope.
type Signer struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // optional (temporary credentials)
	Region          string
	Service         string // "s3"
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// SigningKey derives kSigning = HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), service), "aws4_request").
func SigningKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// uriEncode percent-encodes every byte except A-Z a-z 0-9 - _ . ~ (and "/"
// when keepSlash), with uppercase hex, as SigV4 requires.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || (keepSlash && c == '/') {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// canonicalURI encodes the decoded path once (S3 does not double-encode).
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	return uriEncode(u.Path, true)
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func (s Signer) scope(date string) string {
	return date + "/" + s.Region + "/" + s.Service + "/aws4_request"
}

func (s Signer) signature(t time.Time, canonicalRequest string) string {
	date := t.Format("20060102")
	sts := sigAlgorithm + "\n" + t.Format(amzDateFmt) + "\n" + s.scope(date) + "\n" + sha256Hex(canonicalRequest)
	return hex.EncodeToString(hmacSHA256(SigningKey(s.SecretAccessKey, date, s.Region, s.Service), sts))
}

// signedHeader reports whether a header takes part in the signature: host,
// every x-amz-* header and the content headers S3 validates.
func signedHeader(name string) bool {
	switch name {
	case "content-type", "content-md5", "range", "date":
		return true
	}
	return strings.HasPrefix(name, "x-amz-")
}

// Sign adds X-Amz-Date, X-Amz-Content-Sha256, X-Amz-Security-Token (if any)
// and Authorization to req. payloadHash is the hex SHA-256 of the body,
// UnsignedPayload or EmptySHA256.
func (s Signer) Sign(req *http.Request, payloadHash string, t time.Time) {
	t = t.UTC()
	req.Header.Set("X-Amz-Date", t.Format(amzDateFmt))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if s.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.SessionToken)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	vals := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if signedHeader(lk) {
			trimmed := make([]string, len(v))
			for i, x := range v {
				trimmed[i] = strings.Join(strings.Fields(x), " ")
			}
			vals[lk] = strings.Join(trimmed, ",")
		}
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + vals[n] + "\n")
	}
	signed := strings.Join(names, ";")
	cr := req.Method + "\n" + canonicalURI(req.URL) + "\n" + canonicalQuery(req.URL.Query()) + "\n" + ch.String() + "\n" + signed + "\n" + payloadHash
	req.Header.Set("Authorization", sigAlgorithm+" Credential="+s.AccessKeyID+"/"+s.scope(t.Format("20060102"))+
		", SignedHeaders="+signed+", Signature="+s.signature(t, cr))
}

// Presign returns u with X-Amz-* query parameters authorizing method until
// t+expires. Only the host header is signed and the payload is UNSIGNED-PAYLOAD.
func (s Signer) Presign(method string, u *url.URL, t time.Time, expires time.Duration) *url.URL {
	t = t.UTC()
	out := *u
	q := u.Query()
	q.Set("X-Amz-Algorithm", sigAlgorithm)
	q.Set("X-Amz-Credential", s.AccessKeyID+"/"+s.scope(t.Format("20060102")))
	q.Set("X-Amz-Date", t.Format(amzDateFmt))
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(expires/time.Second), 10))
	q.Set("X-Amz-SignedHeaders", "host")
	if s.SessionToken != "" {
		q.Set("X-Amz-Security-Token", s.SessionToken)
	}
	cq := canonicalQuery(q)
	cr := method + "\n" + canonicalURI(u) + "\n" + cq + "\nhost:" + u.Host + "\n\nhost\n" + UnsignedPayload
	out.RawQuery = cq + "&X-Amz-Signature=" + s.signature(t, cr)
	return &out
}
