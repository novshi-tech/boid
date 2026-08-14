package dispatcher

import (
	"errors"
	"log/slog"
	"os"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// registerAPIGatewayToken registers this job's API gateway token (docs/plans/
// api-gateway.md PR1) — the effective (floor ∪ workspace) enabled-service
// set, this job's SecretNamespace, TaskID, and readonly flag — and returns
// the gateway's shared sandbox-facing base URL alongside the token, tracking
// the token so UnregisterJob can revoke it when the job completes.
//
// Both return values are empty when the API gateway isn't wired
// (r.APIGateway == nil) or r.GatewayURL hasn't been populated yet. Unlike
// registerGatewayToken (git), this needs no orchestrator.ProjectLookup at
// all — service enablement is purely a workspace concern — so callers may
// invoke it outside any project-registry lock.
func (r *Runner) registerAPIGatewayToken(jobID string, spec *orchestrator.JobSpec, workspaceID string) (baseURL, token string) {
	if r.APIGateway == nil {
		return "", ""
	}
	services := r.resolveEnabledAPIServices(workspaceID)
	readOnly := !spec.Visibility.Writable
	token = r.APIGateway.Register(apigateway.RegisterInput{
		Services:  services,
		Namespace: spec.SecretNamespace,
		TaskID:    spec.TaskID,
		ReadOnly:  readOnly,
	})

	r.apiGatewayMu.Lock()
	if r.apiGatewayTokens == nil {
		r.apiGatewayTokens = make(map[string]string)
	}
	r.apiGatewayTokens[jobID] = token
	r.apiGatewayMu.Unlock()

	if r.GatewayURL != nil {
		baseURL = *r.GatewayURL
	}
	return baseURL, token
}

// resolveEnabledAPIServices returns the effective (floor ∪ workspace)
// enabled-service set for workspaceID, mirroring Runner.resolveWorkspaceProxy's
// own "independently load this workspace's WorkspaceMeta, floor-only on any
// load failure" cascade for AllowedDomains (docs/plans/api-gateway.md §3).
// An empty workspaceID (`boid exec` / a project with no workspace) always
// gets just the floor — there is no workspace.yaml to load.
func (r *Runner) resolveEnabledAPIServices(workspaceID string) []string {
	if workspaceID == "" || r.Workspaces == nil {
		return r.APIGatewayServicesFloor
	}
	wsMeta, err := r.Workspaces.Load(workspaceID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("workspace load for API gateway service allowlist failed; using floor only",
				"workspace_id", workspaceID, "error", err)
		}
		return r.APIGatewayServicesFloor
	}
	return orchestrator.ResolveEnabledServices(r.APIGatewayServicesFloor, wsMeta)
}
