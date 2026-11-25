// Command gostack is the GoStack CLI: project generation (init) plus the
// standard application commands (serve, migrate, doctor, config validate, ...).
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/templates"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "init" {
		if err := initProject(args[1:], stdout); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			var ue app.UsageError
			if errors.As(err, &ue) {
				return app.ExitUsage
			}
			return app.ExitError
		}
		return app.ExitOK
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, "Usage: gostack init <name> [--profile lite] [--kit game|agent|web] [--module path] [--gostack-path dir] [--dir dir]\n       gostack [global flags] <command>\n\n")
		args = []string{"help"}
	}
	return app.Main(args, app.Hooks{}, app.Env{Stdout: stdout, Stderr: stderr, Getenv: os.LookupEnv})
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

type tmplData struct {
	Name, Module, GoStackVersion, Replace string
	Kit                                   string
	Game, Agent, Web                      bool
}

// initProject renders the skeleton. It never overwrites: if any target file
// exists, nothing is written.
func initProject(args []string, stdout io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return app.UsageError{Msg: "usage: gostack init <name> [flags]"}
	}
	name := args[0]
	fs_ := flag.NewFlagSet("init", flag.ContinueOnError)
	fs_.SetOutput(io.Discard)
	profile := fs_.String("profile", "lite", "deployment profile")
	kit := fs_.String("kit", "", "business kit (game, agent, web)")
	module := fs_.String("module", "", "Go module path (default: name)")
	dir := fs_.String("dir", "", "target directory (default: ./<name>)")
	local := fs_.String("gostack-path", "", "local GoStack checkout to use via a replace directive")
	version := fs_.String("gostack-version", "", "GoStack module version to require (default: latest, resolved by go mod tidy)")
	if err := fs_.Parse(args[1:]); err != nil {
		return app.UsageError{Msg: err.Error()}
	}
	if !namePattern.MatchString(name) {
		return app.UsageError{Msg: "name must match [a-z][a-z0-9-]{0,30}"}
	}
	if *profile != "lite" {
		return fmt.Errorf("profile %q is not implemented (implemented: lite)", *profile)
	}
	switch *kit {
	case "", "game", "agent", "web":
	default:
		return fmt.Errorf("kit %q is not implemented (implemented: game, agent, web)", *kit)
	}
	d := tmplData{Name: name, Module: *module, GoStackVersion: *version, Kit: *kit,
		Game: *kit == "game", Agent: *kit == "agent", Web: *kit == "web"}
	if d.GoStackVersion == "" && *local != "" {
		d.GoStackVersion = "v0.0.0" // satisfied by the replace directive
	}
	if d.Module == "" {
		d.Module = name
	}
	if *local != "" {
		abs, err := filepath.Abs(*local)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
			return fmt.Errorf("--gostack-path %s: no go.mod", abs)
		}
		d.Replace = abs
	}
	target := *dir
	if target == "" {
		target = name
	}

	files := map[string][]byte{}
	err := fs.WalkDir(templates.App, "app", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "app/"), ".tmpl")
		switch {
		case rel == "game-config.json" && !d.Game:
			return nil
		case rel == "gitignore":
			rel = ".gitignore"
		case rel == "deploy/systemd/app.service":
			rel = "deploy/systemd/" + name + ".service"
		}
		raw, err := fs.ReadFile(templates.App, p)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(p, ".tmpl") {
			// Verbatim assets (deploy scripts, alert rules) may contain {{ }}.
			files[filepath.Join(target, filepath.FromSlash(rel))] = raw
			return nil
		}
		t, err := template.New(p).Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, d); err != nil {
			return err
		}
		files[filepath.Join(target, filepath.FromSlash(rel))] = buf.Bytes()
		return nil
	})
	if err != nil {
		return err
	}
	var existing []string
	for p := range files {
		if _, err := os.Lstat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) > 0 {
		return fmt.Errorf("refusing to overwrite existing files: %s", strings.Join(existing, ", "))
	}
	for p, b := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "created %s (%d files). Next:\n  cd %s && go mod tidy && go build -o bin/%s . && ./bin/%s --config gostack.yaml doctor\n",
		target, len(files), target, name, name)
	return nil
}
