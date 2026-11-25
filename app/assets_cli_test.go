package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The assets CLI end to end on SQLite with the local storage driver:
// publish → publish (dedupe) → list → rollback → verify → prune, plus usage
// and failure exit codes.
func TestAssetsCLI(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "presign.key")
	if err := os.WriteFile(secret, []byte(strings.Repeat("p", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "gostack.yaml")
	cfg := "kits: [game]\ndatabase:\n  path: " + filepath.Join(dir, "app.db") + "\n  analytics_path: " + filepath.Join(dir, "analytics.db") +
		"\nstorage:\n  driver: local\n  local:\n    root: " + filepath.Join(dir, "objects") + "\n    public_url: /v1/files\n    presign_secret_ref: file:" + secret +
		"\ngame:\n  modules: [assets]\n  assets:\n    public_base_url: https://cdn.example.com/\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(want int, args ...string) string {
		t.Helper()
		code, out, e := cli(t, append([]string{"--config", cfgPath}, args...)...)
		if code != want {
			t.Fatalf("%v: exit %d (want %d)\nstdout: %s\nstderr: %s", args, code, want, out, e)
		}
		return out
	}
	run(ExitOK, "migrate", "up")
	run(ExitOK, "game", "create", "alpha")
	build := func(files map[string]string) string {
		d := t.TempDir()
		for p, b := range files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(d, p)), 0o755)
			if err := os.WriteFile(filepath.Join(d, p), []byte(b), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	type pub struct {
		Release struct {
			ReleaseID string `json:"release_id"`
		} `json:"release"`
		Previous string `json:"previous_release_id"`
		Uploaded int    `json:"uploaded"`
		Reused   int    `json:"reused"`
	}
	publish := func(d, label string) pub {
		t.Helper()
		var p pub
		if err := json.Unmarshal([]byte(run(ExitOK, "--json", "assets", "publish", "--game", "alpha", "--dir", d, "--version", label)), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p1 := publish(build(map[string]string{"index.html": "one", "a.js": "a", "b.css": "b"}), "1")
	p2 := publish(build(map[string]string{"index.html": "two", "a.js": "a2", "b.css": "b"}), "2")
	if p1.Uploaded != 4 || p2.Reused != 1 || p2.Uploaded != 3 || p2.Previous != p1.Release.ReleaseID {
		t.Fatalf("publishes %+v %+v", p1, p2)
	}
	out := run(ExitOK, "assets", "list", "--game", "alpha")
	if !strings.Contains(out, "* "+p2.Release.ReleaseID) || !strings.Contains(out, p1.Release.ReleaseID) {
		t.Fatalf("list:\n%s", out)
	}
	out = run(ExitOK, "assets", "rollback", "--game", "alpha", "--to", p1.Release.ReleaseID)
	if !strings.Contains(out, "current release "+p1.Release.ReleaseID) {
		t.Fatalf("rollback: %s", out)
	}
	if out := run(ExitOK, "assets", "verify", "--game", "alpha"); !strings.Contains(out, p1.Release.ReleaseID+": 4 objects checked") ||
		!strings.Contains(out, "0 problems") {
		t.Fatalf("verify: %s", out)
	}
	// keep=1 keeps p2 (newest) and p1 (current): nothing to prune.
	if out := run(ExitOK, "assets", "prune", "--game", "alpha", "--keep", "1"); !strings.Contains(out, "would prune 0 releases") ||
		!strings.Contains(out, "dry run") {
		t.Fatalf("prune dry run: %s", out)
	}
	run(ExitOK, "assets", "rollback", "--game", "alpha", "--to", p2.Release.ReleaseID)
	if out := run(ExitOK, "assets", "prune", "--game", "alpha", "--keep", "1", "--yes"); !strings.Contains(out, "pruned 1 releases") ||
		!strings.Contains(out, "deleted 3 objects, 0 failures") {
		t.Fatalf("prune: %s", out)
	}
	run(ExitError, "assets", "rollback", "--game", "alpha", "--to", p1.Release.ReleaseID) // pruned
	run(ExitOK, "assets", "verify", "--game", "alpha")
	// Missing objects make verify fail with exit 3.
	if err := os.RemoveAll(filepath.Join(dir, "objects", "objects")); err != nil {
		t.Fatal(err)
	}
	if out := run(ExitFailing, "assets", "verify", "--game", "alpha"); !strings.Contains(out, "missing") {
		t.Fatalf("verify after deletion: %s", out)
	}
	run(ExitUsage, "assets", "publish", "--game", "alpha")
	run(ExitUsage, "assets", "prune", "--game", "alpha")
	run(ExitUsage, "assets", "frobnicate", "--game", "alpha")
	run(ExitError, "assets", "publish", "--game", "nope", "--dir", build(map[string]string{"index.html": "x"}))
}

func TestAssetsConfigValidation(t *testing.T) {
	c := testConfig(t)
	c.Game.Modules = []string{"assets"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "assets requires storage.driver") {
		t.Fatalf("assets without storage: %v", err)
	}
	c = testConfig(t)
	for _, u := range []string{"ftp://cdn.example.com", "https://cdn.example.com/?v=1", "cdn.example.com"} {
		c.Game.Assets.PublicBaseURL = u
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "public_base_url") {
			t.Fatalf("%s: %v", u, err)
		}
	}
	c.Game.Assets.PublicBaseURL = "https://cdn.example.com/g/"
	c.Game.Assets.EntryURLTTL = 8 * 24 * 3600e9
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "entry_url_ttl") {
		t.Fatalf("ttl: %v", err)
	}
}
