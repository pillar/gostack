package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pillar/gostack/core/config"
)

// Every kit renders a project whose main.go parses and whose gostack.yaml
// passes configuration validation. (Compiling a generated project needs
// `go mod tidy`; that is checked manually, see docs/api/agent.md / web.md.)
func TestInitKits(t *testing.T) {
	for _, kit := range []string{"", "game", "agent", "web"} {
		t.Run("kit="+kit, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "p")
			args := []string{"demo", "--dir", dir, "--gostack-path", "../.."}
			if kit != "" {
				args = append(args, "--kit", kit)
			}
			var out bytes.Buffer
			if err := initProject(args, &out); err != nil {
				t.Fatal(err)
			}
			src, err := os.ReadFile(filepath.Join(dir, "main.go"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0); err != nil {
				t.Fatalf("main.go: %v\n%s", err, src)
			}
			if got := strings.Contains(string(src), "Workflows:"); got != (kit == "agent") {
				t.Fatalf("workflow hook present=%v for kit %q", got, kit)
			}
			cfg, err := config.Load(filepath.Join(dir, "gostack.yaml"), func(string) (string, bool) { return "", false }, nil)
			if err != nil {
				t.Fatalf("gostack.yaml: %v", err)
			}
			want := []string{}
			if kit != "" {
				want = []string{kit}
			}
			if strings.Join(cfg.Kits, ",") != strings.Join(want, ",") {
				t.Fatalf("kits %v want %v", cfg.Kits, want)
			}
			if kit == "web" && !cfg.Billing.Enabled {
				t.Fatal("web project should enable billing for the order example")
			}
		})
	}
	if err := initProject([]string{"demo", "--kit", "crm", "--dir", t.TempDir()}, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown kit accepted")
	}
}
