package storage

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Example credentials and requests published in the AWS documentation
// ("Signature Calculations for the Authorization Header" and
// "Authenticating Requests: Using Query Parameters" for S3). These three full
// signatures also exercise the signing-key derivation.
var (
	awsExampleSigner = Signer{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", Region: "us-east-1", Service: "s3"}
	awsExampleTime   = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
)

func TestSigV4PresignVector(t *testing.T) {
	u, _ := url.Parse("https://examplebucket.s3.amazonaws.com/test.txt")
	got := awsExampleSigner.Presign("GET", u, awsExampleTime, 86400*time.Second)
	want := "https://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got.String() != want {
		t.Fatalf("presign\n got %s\nwant %s", got, want)
	}
}

func TestSigV4HeaderVectors(t *testing.T) {
	// GET Object with a Range header.
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	awsExampleSigner.Sign(req, EmptySHA256, awsExampleTime)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("GET\n got %s\nwant %s", got, want)
	}

	// PUT Object with a signed payload; the key contains '$', which must be encoded.
	body := "Welcome to Amazon S3."
	if h := sha256Hex(body); h != "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072" {
		t.Fatalf("payload hash %s", h)
	}
	req, _ = http.NewRequest("PUT", "https://examplebucket.s3.amazonaws.com/test$file.text", strings.NewReader(body))
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	awsExampleSigner.Sign(req, sha256Hex(body), awsExampleTime)
	want = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, " +
		"Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("PUT\n got %s\nwant %s", got, want)
	}
}
