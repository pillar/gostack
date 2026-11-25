package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// An API key with an empty tenant would see every tenant's billing books, so
// `apikey create` refuses it as a usage error.
func TestAPIKeyCreateRejectsEmptyTenant(t *testing.T) {
	dir := t.TempDir()
	set := []string{"--set", "database.path=" + filepath.Join(dir, "app.db"), "--set", "database.analytics_path=" + filepath.Join(dir, "analytics.db")}
	if code, _, e := cli(t, append(set, "migrate", "up")...); code != ExitOK {
		t.Fatalf("migrate: %d %s", code, e)
	}
	for _, tenant := range []string{"--tenant=", "--tenant=  "} {
		code, out, e := cli(t, append(set, "apikey", "create", "--name", "x", "--scope", "billing", tenant)...)
		if code != ExitUsage || !strings.Contains(e, "tenant") {
			t.Fatalf("%s: exit %d out %q err %q", tenant, code, out, e)
		}
	}
	if code, out, e := cli(t, append(set, "apikey", "create", "--name", "x", "--scope", "billing", "--tenant", "t1")...); code != ExitOK || !strings.Contains(out, "secret") {
		t.Fatalf("exit %d out %q err %q", code, out, e)
	}
}
