package game

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/pillar/gostack/core/httpx"
)

// parseVersion parses a dotted numeric version ("1", "1.2", "1.2.3", up to 4 parts).
func parseVersion(v string) ([]int64, bool) {
	if v == "" || len(v) > 64 {
		return nil, false
	}
	parts := strings.Split(v, ".")
	if len(parts) > 4 {
		return nil, false
	}
	out := make([]int64, 4)
	for i, p := range parts {
		if p == "" || len(p) > 9 || strings.TrimLeft(p, "0123456789") != "" {
			return nil, false
		}
		out[i], _ = strconv.ParseInt(p, 10, 64)
	}
	return out, true
}

// compareVersions compares two valid versions (missing parts count as 0).
func compareVersions(a, b string) int {
	va, _ := parseVersion(a)
	vb, _ := parseVersion(b)
	return slices.Compare(va, vb)
}

// bucket maps (game, rollout, key) to a stable bucket in [0,100).
func bucket(gameID, rollout, key string) int {
	h := sha256.Sum256([]byte(gameID + "\x00" + rollout + "\x00" + key))
	return int(binary.BigEndian.Uint64(h[:8]) % 100)
}

type rolloutTarget struct {
	version  string // "" = unknown
	platform string // "" = unknown
	key      string // "" = no stable bucketing key
}

func (ro RolloutDef) applies(gameID string, t rolloutTarget) bool {
	if !ro.Enabled || ro.Percent == 0 {
		return false
	}
	if len(ro.Platforms) > 0 && !slices.Contains(ro.Platforms, t.platform) {
		return false
	}
	if ro.MinClientVersion != "" && (t.version == "" || compareVersions(t.version, ro.MinClientVersion) < 0) {
		return false
	}
	if ro.MaxClientVersion != "" && (t.version == "" || compareVersions(t.version, ro.MaxClientVersion) > 0) {
		return false
	}
	if ro.Percent < 100 {
		return t.key != "" && bucket(gameID, ro.ID, t.key) < ro.Percent
	}
	return true
}

// getConfig returns the client section with matching rollouts merged shallowly
// in document order (G06). A bearer token is optional: a valid session buckets
// by player_id; an invalid one is ignored so /config never fails on auth.
func (k *Kit) getConfig(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := gameFrom(ctx)
	q := r.URL.Query()
	t := rolloutTarget{version: q.Get("client_version"), platform: q.Get("platform"), key: q.Get("bucket_key")}
	if t.version != "" {
		if _, ok := parseVersion(t.version); !ok {
			return httpx.BadRequest("client_version must be dotted numbers like 1.2.3")
		}
	}
	if len(t.platform) > 32 || len(t.key) > 128 {
		return httpx.BadRequest("platform or bucket_key too long")
	}
	if s, err := k.resolveSession(r); err == nil && s != nil {
		t.key = s.PlayerID
	}
	c, err := k.currentConfig(ctx, k.DB.Read, g.ID)
	if err != nil {
		return err
	}
	body := c.Client
	var applied []string
	var merged map[string]json.RawMessage
	for _, ro := range c.Doc.Rollouts {
		if !ro.applies(g.ID, t) {
			continue
		}
		if merged == nil {
			merged = maps.Clone(c.Doc.Client)
		}
		maps.Copy(merged, ro.Client)
		applied = append(applied, ro.ID)
	}
	etag := `"` + strconv.FormatInt(c.Revision, 10) + `"`
	if len(applied) > 0 {
		if body, err = json.Marshal(merged); err != nil {
			return err
		}
		h := sha256.Sum256([]byte(strings.Join(applied, ",")))
		etag = `"` + strconv.FormatInt(c.Revision, 10) + "-" + hex.EncodeToString(h[:6]) + `"`
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Add("Vary", "Authorization")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	w.Header().Set("Content-Type", "application/json")
	if len(applied) == 0 {
		fmt.Fprintf(w, `{"revision":%d,"data":%s}`+"\n", c.Revision, body)
		return nil
	}
	ids, _ := json.Marshal(applied)
	fmt.Fprintf(w, `{"revision":%d,"data":%s,"rollouts":%s}`+"\n", c.Revision, body, ids)
	return nil
}
