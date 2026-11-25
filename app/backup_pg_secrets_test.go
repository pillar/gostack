package app

import (
	"context"
	"os"
	osexec "os/exec"
	"strings"
	"testing"

	"github.com/pillar/gostack/adapters/pgtest"
)

func envValue(env []string, key string) (string, bool) {
	v := ""
	found := false
	for _, e := range env {
		if k, val, _ := strings.Cut(e, "="); k == key {
			v, found = val, true
		}
	}
	return v, found
}

// Every secret connection parameter stays out of pg_dump/pg_restore argv,
// not only password (sslpassword unlocks the client key): the password goes
// to PGPASSWORD, sslpassword (libpq has no environment variable for it) to a
// temporary 0600 service file that is removed afterwards.
func TestPGDSNKeepsAllSecretsOutOfArgs(t *testing.T) {
	for _, in := range []string{
		"postgres://app:pw0rd@db.example:5433/game?sslmode=disable&sslpassword=k3y%20pass&application_name=x",
		"postgresql://app@db.example/game?sslpassword=k3y%20pass&password=pw0rd",
		"host=db.example port=5433 user=app password=pw0rd sslpassword='k3y pass' dbname=game sslmode=disable",
	} {
		d, err := parsePGDSN(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		bare := d.render(false)
		for _, secret := range []string{"pw0rd", "k3y"} {
			if strings.Contains(bare, secret) {
				t.Fatalf("secret %q left in argv DSN %q", secret, bare)
			}
		}
		if full := d.render(true); !strings.Contains(full, "pw0rd") || !strings.Contains(full, "k3y") {
			t.Fatalf("render(true) lost secrets: %q", full)
		}
		env, cleanup, err := d.toolEnv()
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := envValue(env, "PGPASSWORD"); v != "pw0rd" {
			t.Fatalf("PGPASSWORD %q", v)
		}
		sf, _ := envValue(env, "PGSERVICEFILE")
		if svc, _ := envValue(env, "PGSERVICE"); svc != toolServiceName || sf == "" {
			t.Fatalf("service env %q %q", svc, sf)
		}
		st, err := os.Stat(sf)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("service file %v %v", st, err)
		}
		b, _ := os.ReadFile(sf)
		if string(b) != "[gostack_tool]\nsslpassword=k3y pass\n" {
			t.Fatalf("service file %q", b)
		}
		cleanup()
		if _, err := os.Stat(sf); !os.IsNotExist(err) {
			t.Fatal("service file not removed")
		}
	}
	// No extra secrets: no service file.
	d, _ := parsePGDSN("postgres://app:pw@h/db")
	env, cleanup, err := d.toolEnv()
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, ok := envValue(env, "PGSERVICEFILE"); ok && os.Getenv("PGSERVICEFILE") == "" {
		t.Fatal("service file without sslpassword")
	}
	// Unrepresentable values and an existing service are refused, never put in argv.
	for _, in := range []string{"host=h sslpassword='a\nb'", "host=h service=x sslpassword=k"} {
		d, err := parsePGDSN(in)
		if err != nil {
			continue // rejected earlier is fine too
		}
		_, cleanup, err = d.toolEnv()
		cleanup()
		if err == nil {
			t.Fatalf("%q accepted", in)
		}
	}
}

// libpq (pg_dump) accepts the service file: a dump through runTool works with
// sslpassword in the DSN, which is no longer in argv.
func TestPGToolsWithSSLPasswordViaServiceFile(t *testing.T) {
	pgtest.RequirePostgres(t)
	if _, err := osexec.LookPath("pg_dump"); err != nil {
		t.Skip("pg_dump not in PATH")
	}
	base := pgtest.DSN()
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	for _, in := range []string{base + sep + "sslpassword=n0t-in-argv"} {
		d, err := parsePGDSN(in)
		if err != nil {
			t.Fatal(err)
		}
		arg := "--dbname=" + d.render(false)
		if strings.Contains(arg, "n0t-in-argv") {
			t.Fatalf("argv %q", arg)
		}
		if err := runTool(context.Background(), d, "pg_dump", "--schema-only", "--file="+os.DevNull, arg); err != nil {
			t.Fatal(err)
		}
	}
}
