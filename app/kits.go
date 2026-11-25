package app

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/agent"
	"github.com/pillar/gostack/kits/web"
	"github.com/pillar/gostack/modules/jobs"
)

// initKits builds AgentKit and WebKit when configured (after initModules:
// they use the object storage and billing modules).
func (a *App) initKits(cfg config.Config) {
	if slices.Contains(cfg.Kits, "agent") {
		a.Agent = &agent.Kit{DB: a.DB, Jobs: a.Jobs, Storage: a.Mod.Storage, Cfg: cfg.Agent, Lease: cfg.Queue.Lease, Log: a.Log}
	}
	if slices.Contains(cfg.Kits, "web") {
		a.Web = &web.Kit{DB: a.DB, Jobs: a.Jobs, Keys: auth.APIKeys{DB: a.DB}, Billing: a.Mod.Billing, WebAuth: a.WebAuth,
			Cfg: cfg.Web, Production: cfg.Environment == "production", Log: a.Log}
	}
}

// kitRoutes registers hook workflows and mounts the kits' routes.
func (a *App) kitRoutes(mux *http.ServeMux, h Hooks) error {
	var errs []error
	if a.Agent != nil {
		for name, w := range h.Workflows {
			if err := a.Agent.Register(name, w); err != nil {
				errs = append(errs, err)
			}
		}
		a.Agent.Routes(mux, auth.APIKeys{DB: a.DB})
	} else if len(h.Workflows) > 0 {
		errs = append(errs, fmt.Errorf("hooks define %d workflows but kits does not include agent", len(h.Workflows)))
	}
	if a.Web != nil {
		a.Web.Routes(mux)
	}
	return errors.Join(errs...)
}

func (a *App) kitJobHandlers() map[string]jobs.Handler {
	out := map[string]jobs.Handler{}
	if a.Agent != nil {
		out[agent.JobType] = a.Agent.HandleJob
	}
	if a.Web != nil {
		for k, v := range a.Web.JobHandlers() {
			out[k] = v
		}
	}
	return out
}
