// Package auth provides opaque secret tokens and API keys.
//
// Secrets are 256-bit random values; only their SHA-256 digest is stored. A slow
// password hash is unnecessary for high-entropy secrets (PRD review S3).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/httpx"
)

// NewSecret returns prefix + 43 url-safe chars (256 bits).
func NewSecret(prefix string) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// Hash returns the hex SHA-256 digest used for storage and lookup.
func Hash(secret string) string {
	s := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(s[:])
}

// APIKey is a stored key without its secret.
type APIKey struct {
	ID        string   `json:"id"`
	TenantID  string   `json:"tenant_id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	CreatedAt int64    `json:"created_at"`
	ExpiresAt *int64   `json:"expires_at,omitempty"`
	RevokedAt *int64   `json:"revoked_at,omitempty"`
}

func (k APIKey) Has(scope string) bool { return slices.Contains(k.Scopes, scope) }

// APIKeys stores keys in the main database. Lookups always hit the database so
// revocation takes effect immediately (no cache can mask it).
type APIKeys struct{ DB *store.DB }

const apiKeyPrefix = "gsk_"

// Create returns the stored key and the plaintext secret, shown once.
func (s APIKeys) Create(ctx context.Context, tenant, name string, scopes []string, ttl time.Duration) (APIKey, string, error) {
	if name == "" || len(scopes) == 0 {
		return APIKey{}, "", errors.New("name and at least one scope are required")
	}
	now := time.Now().UnixMilli()
	k := APIKey{ID: httpx.RandomID("key_"), TenantID: tenant, Name: name, Scopes: scopes, CreatedAt: now}
	if ttl > 0 {
		exp := now + ttl.Milliseconds()
		k.ExpiresAt = &exp
	}
	secret := NewSecret(apiKeyPrefix)
	err := s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO api_keys(id,tenant_id,name,key_hash,scopes,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`,
			k.ID, k.TenantID, k.Name, Hash(secret), strings.Join(scopes, " "), k.CreatedAt, k.ExpiresAt)
		return err
	})
	return k, secret, err
}

// Revoke marks a key revoked.
func (s APIKeys) Revoke(ctx context.Context, id string) error {
	return s.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, time.Now().UnixMilli(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("api key %s not found or already revoked", id)
		}
		return nil
	})
}

// Verify resolves an active key from its secret, or returns 401.
func (s APIKeys) Verify(ctx context.Context, secret string) (APIKey, error) {
	if !strings.HasPrefix(secret, apiKeyPrefix) {
		return APIKey{}, httpx.Unauthorized("invalid api key")
	}
	var k APIKey
	var scopes string
	err := s.DB.Read.QueryRowContext(ctx, `SELECT id,tenant_id,name,scopes,created_at,expires_at,revoked_at FROM api_keys WHERE key_hash=?`, Hash(secret)).
		Scan(&k.ID, &k.TenantID, &k.Name, &scopes, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, httpx.Unauthorized("invalid api key")
	}
	if err != nil {
		return APIKey{}, err
	}
	if k.RevokedAt != nil || (k.ExpiresAt != nil && *k.ExpiresAt <= time.Now().UnixMilli()) {
		return APIKey{}, httpx.Unauthorized("api key revoked or expired")
	}
	k.Scopes = strings.Fields(scopes)
	return k, nil
}
