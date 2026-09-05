package dispatcher

import (
	"errors"
	"log/slog"
	"os"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// registerAPIGatewayToken registers this job's API gateway token — the
// effective (floor ∪ workspace) enabled-service set, this job's
// SecretNamespace, TaskID, and readonly flag — and returns the gateway's
// shared sandbox-facing base URL alongside the token, tracking the token so
// UnregisterJob can revoke it when the job completes.
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
	// A signal-derived connector job restricts its token to the one service
	// it declared (spec.APIGatewayServices), but we INTERSECT rather than
	// trust it outright: the declaration is only warned about (never
	// blocked) at project.yaml load time, so a stale declaration must not
	// grant reach the workspace no longer actually enables.
	if spec.APIGatewayServices != nil {
		services = intersectServiceNames(services, spec.APIGatewayServices)
	}
	readOnly := !spec.Visibility.Writable
	token = r.APIGateway.Register(services, spec.SecretNamespace, spec.TaskID, readOnly)

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
// enabled-service set for workspaceID, falling back to the floor alone on
// any load failure (mirrors Runner.resolveWorkspaceProxy's AllowedDomains
// cascade). An empty workspaceID (`boid exec` / a project with no
// workspace) always gets just the floor — there is no workspace.yaml to load.
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

// intersectServiceNames returns the entries of want that also appear in
// resolved, preserving want's order. An empty result (want names nothing
// resolved actually enables) is a legitimate, safe outcome: the connector
// job's gateway token simply authorizes no service, and every gateway call
// it attempts gets an ordinary 403 — not a panic, not a silent full-access
// fallback.
func intersectServiceNames(resolved, want []string) []string {
	allowed := make(map[string]bool, len(resolved))
	for _, s := range resolved {
		allowed[s] = true
	}
	out := make([]string, 0, len(want))
	for _, s := range want {
		if allowed[s] {
			out = append(out, s)
		}
	}
	return out
}
