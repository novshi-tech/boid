package dispatcher

import (
	"context"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file is Phase 6 PR7's "e2e-skeleton" concern (docs/plans/
// phase6-container-backend.md §PR7: "config で container 選択時、
// allowed_domains が egress proxy に正しく渡る") pinned at the unit level — a
// real container-backend egress e2e is PR9's job (§決定5's dual-homed
// compose network + workspace-scoped internal networks are not built yet).
// What this test CAN and does pin: selecting the container backend
// (Runner.Backend) does not disturb the pre-existing, entirely
// backend-agnostic proxy wiring — Runner.resolveWorkspaceProxy →
// ProxyAllocator.GetOrCreate(workspace's resolved allowed_domains) →
// BuildSandboxSpec's applyProxyEnv → spec.Env's HTTP_PROXY/HTTPS_PROXY —
// reaches the container's own docker-create Env exactly as it already
// reaches a userns sandbox's Env (realization.Realize carries spec.Env
// through verbatim — see its own doc comment).

// TestDispatch_ContainerBackend_PropagatesWorkspaceProxyEnv pins that end
// to end: a workspace with a non-floor allowed_domains override still
// drives ProxyAllocator.GetOrCreate with that exact domain, and the port it
// returns lands as HTTP_PROXY/HTTPS_PROXY in the docker container's Env.
// applyProxyEnv's proxy URL host (hostGatewayIP, "10.0.2.2") is unchanged
// by this PR — reachability of that address from a real docker sibling
// container (vs. today's userns/pasta sandbox) is a known, separately
// tracked gap (docs/plans/phase6-container-backend.md's 現状棚卸し /
// §決定5), not something this wiring-preservation test claims to close.
func TestDispatch_ContainerBackend_PropagatesWorkspaceProxyEnv(t *testing.T) {
	d := newGatewayTestDB(t)
	// The jobs table FK-references projects(id) — r.Projects itself is an
	// in-memory fake (so its WorkspaceID doesn't need real project_workspaces
	// linking), but CreateJob still needs a matching DB row to satisfy the
	// constraint.
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	alloc := &fakeProxyAllocator{ports: map[string]int{"ws-a": 9321}}
	r := &Runner{
		DB:         d.Conn,
		Backend:    be,
		Sandbox:    &gwFakeSandboxPrep{dir: t.TempDir()},
		Runtime:    &gwFakeRuntime{},
		BoidBinary: "/boid",
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-a", WorkDir: "/tmp"},
		}},
		Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
			"ws-a": {AllowedDomains: []string{"registry.example.com"}},
		}},
		ProxyAllocator: alloc,
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(alloc.calls) != 1 || alloc.calls[0].workspaceID != "ws-a" {
		t.Fatalf("ProxyAllocator.GetOrCreate calls = %+v, want exactly one for ws-a", alloc.calls)
	}
	found := false
	for _, domain := range alloc.calls[0].allowed {
		if domain == "registry.example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("ProxyAllocator.GetOrCreate allowed domains = %v, want it to include registry.example.com (the workspace override)",
			alloc.calls[0].allowed)
	}

	if len(api.createCalls) != 1 {
		t.Fatalf("ContainerCreate calls = %d, want 1", len(api.createCalls))
	}
	env := api.createCalls[0].Config.Env
	const wantProxy = "http://10.0.2.2:9321"
	var gotHTTPProxy, gotHTTPSProxy string
	for _, kv := range env {
		if strings.HasPrefix(kv, "HTTP_PROXY=") {
			gotHTTPProxy = strings.TrimPrefix(kv, "HTTP_PROXY=")
		}
		if strings.HasPrefix(kv, "HTTPS_PROXY=") {
			gotHTTPSProxy = strings.TrimPrefix(kv, "HTTPS_PROXY=")
		}
	}
	if gotHTTPProxy != wantProxy {
		t.Errorf("container Env HTTP_PROXY = %q, want %q (allocated port 9321 reaching the container unchanged)", gotHTTPProxy, wantProxy)
	}
	if gotHTTPSProxy != wantProxy {
		t.Errorf("container Env HTTPS_PROXY = %q, want %q", gotHTTPSProxy, wantProxy)
	}
}
