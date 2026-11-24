package server

import (
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/tunnel"
	"github.com/novshi-tech/boid/internal/version"
)

// tunnelDescription is the agent-facing description boid sends in hello.
const tunnelDescription = `boid: the owner's personal sandbox runner on their own machine.
Read-only for now. All responses are JSON.

- GET /jobs?status=<running|completed|failed>  list recent jobs (newest first)
- GET /jobs/<id>                             one job (status, workspace, task, timestamps)
- GET /jobs/<id>/log                         the job's terminal transcript (raw text with ANSI escapes)

Starting jobs over the tunnel is not available yet.`

// newTunnelManager returns the daemon's tunnel, or nil when there is no
// data home to keep the key in.
func newTunnelManager(cfg Config) *tunnel.Manager {
	home := dataHomeFor(cfg)
	if home == "" {
		return nil
	}
	return &tunnel.Manager{
		IdentityPath: filepath.Join(home, "tunnel.json"),
		Description:  tunnelDescription,
		Version:      "boid " + version.Version(),
	}
}

// newTunnelRelay lets jobs in callbackWorkspaces call the gateway back with
// their API gateway job token.
func newTunnelRelay(m *tunnel.Manager, registry *apigateway.Registry, callbackWorkspaces []string) *tunnel.Relay {
	allowed := make(map[string]bool, len(callbackWorkspaces))
	for _, ws := range callbackWorkspaces {
		allowed[ws] = true
	}
	return &tunnel.Relay{
		Lookup: func(token string) (tunnel.JobGrant, bool) {
			e, ok := registry.Lookup(token)
			if !ok {
				return tunnel.JobGrant{}, false
			}
			return tunnel.JobGrant{WorkspaceID: e.Namespace, JobRef: e.TaskID, ReadOnly: e.ReadOnly}, true
		},
		AllowWorkspace: func(ws string) bool { return ws != "" && allowed[ws] },
		Caller:         m.Caller,
	}
}

// tunnelSurface is everything the owner reaches through the tunnel: a
// read-only view of jobs. The rest of the daemon API (secrets, config,
// workspaces) is deliberately absent.
func tunnelSurface(jobs *api.JobHandler) http.Handler {
	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(tunnelDescription + "\n"))
	})
	r.Get("/jobs", jobs.List)
	r.Get("/jobs/{id}", jobs.Get)
	if jobs.LogReader != nil {
		r.Get("/jobs/{id}/log", jobs.Log)
	}
	return r
}
