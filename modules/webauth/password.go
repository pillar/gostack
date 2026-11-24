package webauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters: OWASP Password Storage Cheat Sheet minimum
// configuration m=19 MiB, t=2, p=1. Memory per hash is ~19 MiB, so concurrent
// hashing is bounded by auth.web.max_concurrent_hashes (default 2) to protect a
// 1 GiB host. Raising these is safe: Verify reads parameters from the stored
// PHC string, so existing hashes keep working.
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16

	// Password length bounds in bytes. The upper cap bounds hashing input; it is
	// a byte count, so multi-byte characters count more than once.
	MinPasswordBytes = 8
	MaxPasswordBytes = 128
)

var b64 = base64.RawStdEncoding

// hashPassword returns an Argon2id PHC string.
func hashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

var errBadHash = errors.New("webauth: malformed password hash")

// verifyPassword checks password against a PHC string in constant time with
// respect to the key comparison.
func verifyPassword(password, phc string) (bool, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errBadHash
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false, errBadHash
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil ||
		m < 8 || m > 1<<20 || t < 1 || t > 16 || p < 1 {
		return false, errBadHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, errBadHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, errBadHash
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func validPassword(p string) bool {
	return len(p) >= MinPasswordBytes && len(p) <= MaxPasswordBytes
}
