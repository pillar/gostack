package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/assets"
)

const assetsUsage = "usage: assets publish|list|rollback|verify|prune --game G ... (see help)"

// assetsCmd implements `assets publish|list|rollback|verify|prune` (PRD §22,
// docs/ASSET_RELEASE.md). It needs storage.driver and the database; the game
// module "assets" only controls the HTTP routes.
func assetsCmd(ctx context.Context, cfg config.Config, args []string, asJSON bool, env Env) error {
	if len(args) == 0 {
		return UsageError{assetsUsage}
	}
	sub := args[0]
	fs := flag.NewFlagSet("assets "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	gameID := fs.String("game", "", "game id")
	dir := fs.String("dir", "", "build directory (publish)")
	entry := fs.String("entry", assets.DefaultEntry, "entry file relative to --dir (publish)")
	label := fs.String("version", "", "version label (publish)")
	actor := fs.String("actor", "cli", "recorded as created_by / updated_by")
	to := fs.String("to", "", "release id (rollback)")
	release := fs.String("release", "", "release id (verify; default current)")
	keep := fs.Int("keep", 0, "releases to keep (prune)")
	yes := fs.Bool("yes", false, "apply the prune (default: dry run)")
	if err := fs.Parse(args[1:]); err != nil {
		return UsageError{err.Error()}
	}
	if *gameID == "" || fs.NArg() != 0 {
		return UsageError{assetsUsage}
	}
	switch sub {
	case "publish":
		if *dir == "" {
			return UsageError{"assets publish: --dir is required"}
		}
	case "rollback":
		if *to == "" {
			return UsageError{"assets rollback: --to is required"}
		}
	case "prune":
		if *keep < 1 {
			return UsageError{"assets prune: --keep N (N >= 1) is required"}
		}
	case "list", "verify":
	default:
		return UsageError{assetsUsage}
	}
	st, _, err := newStorage(cfg.Storage)
	if err != nil {
		return err
	}
	if st == nil {
		return errors.New("assets: storage.driver is not configured")
	}
	db, err := openAdmin(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ac := cfg.Game.Assets
	m := &assets.Module{DB: db, Storage: st, PublicBaseURL: ac.PublicBaseURL}
	out := env.Stdout
	switch sub {
	case "publish":
		start := time.Now()
		res, err := m.Publish(ctx, assets.PublishOptions{GameID: *gameID, Dir: *dir, Entry: *entry, Label: *label, Actor: *actor})
		if err != nil {
			return err
		}
		if asJSON {
			PrintJSON(out, res)
			return nil
		}
		r := res.Release
		fmt.Fprintf(out, "release %s published for game %s (previous: %s)\n", r.ReleaseID, r.GameID, orNone(res.Previous))
		fmt.Fprintf(out, "files %d (%d bytes): uploaded %d objects (%d bytes), reused %d, %s\n",
			r.FileCount, r.TotalBytes, res.Uploaded, res.UploadedBytes, res.Reused, time.Since(start).Round(time.Millisecond))
		fmt.Fprintf(out, "manifest %s\nentry    %s\n", r.ManifestKey, r.EntryKey)
		return nil
	case "list":
		rels, cur, err := m.List(ctx, *gameID)
		if err != nil {
			return err
		}
		if asJSON {
			PrintJSON(out, map[string]any{"current_release_id": cur, "releases": rels})
			return nil
		}
		for _, r := range rels {
			mark := " "
			if r.Current {
				mark = "*"
			}
			fmt.Fprintf(out, "%s %s %-9s %s files=%d bytes=%d by=%s label=%q\n", mark, r.ReleaseID, r.Status,
				time.UnixMilli(r.CreatedAt).UTC().Format(time.RFC3339), r.FileCount, r.TotalBytes, r.CreatedBy, r.Label)
		}
		if len(rels) == 0 {
			fmt.Fprintln(out, "no releases")
		}
		return nil
	case "rollback":
		prev, err := m.Rollback(ctx, *gameID, *to, *actor)
		if err != nil {
			var he *httpx.Error
			if errors.As(err, &he) {
				return fmt.Errorf("rollback refused: %s (%s)", he.Message, he.Code)
			}
			return err
		}
		if asJSON {
			PrintJSON(out, map[string]any{"current_release_id": *to, "previous_release_id": prev})
		} else {
			fmt.Fprintf(out, "game %s current release %s (previous: %s)\n", *gameID, *to, orNone(prev))
		}
		return nil
	case "verify":
		rep, err := m.Verify(ctx, *gameID, *release)
		if err != nil {
			return err
		}
		if asJSON {
			PrintJSON(out, map[string]any{"ok": rep.OK(), "report": rep})
		} else {
			fmt.Fprintf(out, "release %s: %d objects checked, %d bytes, %d problems\n", rep.ReleaseID, rep.Checked, rep.Bytes, len(rep.Problems))
			for _, p := range rep.Problems {
				fmt.Fprintf(out, "  %s %s (%s) %s\n", p.Problem, p.Path, p.Key, p.Detail)
			}
		}
		if !rep.OK() {
			return CheckFailed{fmt.Sprintf("release %s: %d problems", rep.ReleaseID, len(rep.Problems))}
		}
		return nil
	case "prune":
		p, err := m.Prune(ctx, *gameID, *keep, *yes, *actor)
		if err != nil {
			return err
		}
		if asJSON {
			PrintJSON(out, p)
		} else {
			verb := "would prune"
			if p.Applied {
				verb = "pruned"
			}
			fmt.Fprintf(out, "keep %d releases %v\n%s %d releases %v, %d objects\n", len(p.Keep), p.Keep, verb, len(p.Prune), p.Prune, len(p.Delete))
			for _, k := range p.Delete {
				fmt.Fprintf(out, "  %s\n", k)
			}
			if p.Applied {
				fmt.Fprintf(out, "deleted %d objects, %d failures\n", p.Deleted, len(p.Failures))
				for _, f := range p.Failures {
					fmt.Fprintf(out, "  FAILED %s\n", f)
				}
			} else {
				fmt.Fprintln(out, "dry run; pass --yes to apply")
			}
		}
		if len(p.Failures) > 0 {
			return CheckFailed{fmt.Sprintf("%d objects could not be deleted; rerun prune", len(p.Failures))}
		}
		return nil
	}
	return UsageError{assetsUsage}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
