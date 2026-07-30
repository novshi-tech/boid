package dispatcher

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/gitgateway"
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
// returns lands as HTTP_PROXY/HTTPS_PROXY in the docker container's Env —
// now addressed via the compose egress service DNS name
// (composeEgressServiceName, "boid-egress"), not the userns-only
// hostGatewayIP ("10.0.2.2") literal a docker sibling container has no
// projection for at all ([Blocker 2, PR7 codex review] — this test's own
// previous version explicitly flagged that mismatch as a "known,
// separately tracked gap"; it is closed as of this fix, via
// Runner.Dispatch's IsContainerBackend(r.Backend) branch feeding
// SandboxRuntimeInfo.ProxyHost). Real network reachability of that DNS
// name from a live compose deploy is still PR9's e2e-container job — this
// test pins the wiring, not a live dial.
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

	env := jobContainerCreate(t, api).Config.Env
	const wantProxy = "http://boid-egress:9321"
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
		t.Errorf("container Env HTTP_PROXY = %q, want %q (allocated port 9321, addressed via the compose egress service DNS name)", gotHTTPProxy, wantProxy)
	}
	if gotHTTPSProxy != wantProxy {
		t.Errorf("container Env HTTPS_PROXY = %q, want %q", gotHTTPSProxy, wantProxy)
	}
}

// TestDispatch_ContainerBackend_NoProxyExcludesGitGatewayHost pins the PR9
// e2e-container CI fix (docs/plans/phase6-container-backend.md §PR9): the
// container backend's git gateway ("boid-gateway", a distinct compose
// service DNS name from the egress proxy's own "boid-egress") must always
// be excluded from HTTPS_PROXY routing via no_proxy/NO_PROXY. Before this
// fix, applyProxyEnv's no_proxy only ever excluded the proxy's OWN host —
// correct for the userns backend, where the gateway and the proxy's
// hostGatewayIP fallback happen to share the same address ("10.0.2.2") by
// coincidence, but wrong for the container backend, where they are two
// distinct hostnames. A clone-visibility job's `git clone` against the
// gateway was silently routed through HTTPS_PROXY like any other outbound
// request and rejected by the egress proxy's own domain allowlist with a
// hard 403 ("CONNECT tunnel failed, response 403" — the real-docker
// e2e-container CI job's exact failure).
func TestDispatch_ContainerBackend_NoProxyExcludesGitGatewayHost(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	alloc := &fakeProxyAllocator{ports: map[string]int{"ws-a": 9321}}
	gwURL := "https://boid-gateway:39901"
	r := &Runner{
		DB:         d.Conn,
		Backend:    be,
		BoidBinary: "/boid",
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-a", WorkDir: "/tmp"},
		}},
		Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
			"ws-a": {AllowedDomains: []string{"registry.example.com"}},
		}},
		ProxyAllocator: alloc,
		GitGateway:     gitgateway.NewRegistry(),
		GatewayURL:     &gwURL,
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	env := jobContainerCreate(t, api).Config.Env
	var gotNoProxy string
	for _, kv := range env {
		if strings.HasPrefix(kv, "NO_PROXY=") {
			gotNoProxy = strings.TrimPrefix(kv, "NO_PROXY=")
		}
	}
	if !strings.Contains(gotNoProxy, "boid-gateway") {
		t.Errorf("container Env NO_PROXY = %q, want it to include the git gateway host %q (else the sandbox-internal clone gets routed through HTTPS_PROXY and rejected by the egress proxy's domain allowlist)",
			gotNoProxy, "boid-gateway")
	}
}

// TestDispatch_ContainerBackend_NoProxyIncludesWorkspaceNetworkCIDRs pins the
// job→sibling half of §決定5's sibling connectivity contract ("job → sibling
// の到達は container IP + container port の直アクセス").
//
// That contract was only ever satisfied at the NETWORK layer: a job and every
// sibling its dockerproxy creates land on the same per-workspace network
// (containerBackend.ensureWorkspaceNetwork + dockerproxy's forced network
// injection), so the TCP route exists. But every proxy-env-respecting client
// inside the sandbox — curl, git, .NET's HttpClient, python-requests — reads
// HTTP_PROXY from the env applyProxyEnv writes, and applyProxyEnv's no_proxy
// listed only boid's own infrastructure names. A sibling's address matched
// nothing in it, so a plain `curl http://<sibling-ip>:10000/` was routed to
// the egress proxy instead of dialed directly, and the egress proxy — which
// lives in the daemon container and is deliberately NOT a route into any
// workspace network (see internal/sandbox/proxy.go's isRefusedDotlessTarget
// doc comment for why relaying there would break the isolation half of the
// same 決定) — answered 403 (dotted/IP target, not in allowed_domains) or 502
// (single-label sibling name). Measured against a live workspace: IP target
// 403, container-name target 502, and the same request with the workspace
// subnet added to no_proxy 404 — i.e. reaching the sibling's own HTTP server.
//
// So the fix is per-workspace: no_proxy must carry the CIDRs of the job's OWN
// workspace network. Every OTHER workspace's network keeps its subnet out of
// this job's no_proxy, so a cross-workspace address still goes to the egress
// proxy and still gets refused there — the isolation requirement is unchanged.
func TestDispatch_ContainerBackend_NoProxyIncludesWorkspaceNetworkCIDRs(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	api := &fakeDockerAPI{}
	// Both families: podman/docker hand out an IPv6 subnet alongside the IPv4
	// one whenever the engine has IPv6 enabled, and a sibling reached over
	// either address must bypass the proxy the same way.
	api.NetworkInspectFunc = func(_ context.Context, _ string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
		var res client.NetworkInspectResult
		res.Network.IPAM.Config = []network.IPAMConfig{
			{Subnet: netip.MustParsePrefix("10.89.9.0/24")},
			{Subnet: netip.MustParsePrefix("fd00:b01d::/64")},
		}
		return res, nil
	}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "inst-1"})

	alloc := &fakeProxyAllocator{ports: map[string]int{"ws-a": 9321}}
	// The gateway is wired here too: the CIDR entries must be ADDITIVE, not a
	// replacement for the infrastructure exemptions PR9 added.
	gwURL := "https://boid-gateway:39901"
	r := &Runner{
		DB:         d.Conn,
		Backend:    be,
		BoidBinary: "/boid",
		InstallID:  "inst-1",
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-a", WorkDir: "/tmp"},
		}},
		Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
			"ws-a": {AllowedDomains: []string{"registry.example.com"}},
		}},
		ProxyAllocator: alloc,
		GitGateway:     gitgateway.NewRegistry(),
		GatewayURL:     &gwURL,
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	env := jobContainerCreate(t, api).Config.Env
	gotNoProxy := envValue(env, "NO_PROXY")
	for _, want := range []string{"10.89.9.0/24", "fd00:b01d::/64"} {
		if !containsNoProxyEntry(gotNoProxy, want) {
			t.Errorf("container Env NO_PROXY = %q, want it to include the workspace network CIDR %q (else a sibling dialed by container IP is routed through the egress proxy and refused there)",
				gotNoProxy, want)
		}
	}
	// The lower-case twin is what several clients actually read; applyProxyEnv
	// writes both, so a CIDR reaching only one of them is a half fix.
	if got := envValue(env, "no_proxy"); got != gotNoProxy {
		t.Errorf("container Env no_proxy = %q, NO_PROXY = %q, want both to carry the same value", got, gotNoProxy)
	}
	// The pre-existing infrastructure exemptions must survive the addition.
	if !containsNoProxyEntry(gotNoProxy, "boid-gateway") {
		t.Errorf("container Env NO_PROXY = %q, want it to still include the git gateway host", gotNoProxy)
	}

	// The subnets must come from THIS job's own workspace network, named by
	// the same pure function containerBackend.Launch and the runner's
	// SetWorkspaceNetwork call already compute independently.
	wantNet := containerWorkspaceNetworkName("inst-1", "ws-a")
	api.mu.Lock()
	inspected := append([]string(nil), api.networkInspectIDs...)
	api.mu.Unlock()
	if len(inspected) == 0 {
		t.Fatalf("NetworkInspect was never called, want an inspect of %q", wantNet)
	}
	for _, got := range inspected {
		if got != wantNet {
			t.Errorf("NetworkInspect called for %q, want %q (a different workspace's subnets must never land in this job's no_proxy)", got, wantNet)
		}
	}
}

// TestDispatch_ContainerBackend_NoProxySurvivesWorkspaceNetworkInspectFailure
// pins the failure mode as fail-OPEN, deliberately: not knowing the workspace
// subnet costs a job its direct sibling route (a degraded sandbox), while
// failing the dispatch costs it the whole job. The isolation invariant does
// not depend on this call succeeding — it is enforced by
// ensureWorkspaceNetwork's own fail-closed NetworkCreate inside Launch, which
// still runs, and by the egress proxy's refusal, which is what an unknown
// subnet falls back to.
func TestDispatch_ContainerBackend_NoProxySurvivesWorkspaceNetworkInspectFailure(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	api := &fakeDockerAPI{}
	api.NetworkInspectFunc = func(_ context.Context, _ string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
		return client.NetworkInspectResult{}, errors.New("engine unreachable")
	}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "inst-1"})

	alloc := &fakeProxyAllocator{ports: map[string]int{"ws-a": 9321}}
	gwURL := "https://boid-gateway:39901"
	r := &Runner{
		DB:         d.Conn,
		Backend:    be,
		BoidBinary: "/boid",
		InstallID:  "inst-1",
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-a", WorkDir: "/tmp"},
		}},
		Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
			"ws-a": {AllowedDomains: []string{"registry.example.com"}},
		}},
		ProxyAllocator: alloc,
		GitGateway:     gitgateway.NewRegistry(),
		GatewayURL:     &gwURL,
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	if _, err := r.Dispatch(context.Background(), spec, nil); err != nil {
		t.Fatalf("Dispatch: %v, want the dispatch to survive an unresolvable workspace subnet", err)
	}

	gotNoProxy := envValue(jobContainerCreate(t, api).Config.Env, "NO_PROXY")
	if !containsNoProxyEntry(gotNoProxy, "boid-gateway") {
		t.Errorf("container Env NO_PROXY = %q, want the pre-existing infrastructure exemptions intact even when the subnet lookup failed", gotNoProxy)
	}
	if strings.Contains(gotNoProxy, "/") {
		t.Errorf("container Env NO_PROXY = %q, want no CIDR entry at all when the engine could not report one", gotNoProxy)
	}
}

// TestContainerBackend_WorkspaceNetworkCIDRs_EnsuresNetworkBeforeInspect pins
// the ordering the very first dispatch for a workspace depends on: the subnet
// is assigned BY the engine at network-create time, so a plain inspect of a
// not-yet-created network answers 404 and the first job of every fresh
// workspace would silently ship without its own subnet in no_proxy. The
// resolver therefore goes through the same idempotent ensureWorkspaceNetwork
// Launch uses, and only then inspects.
func TestContainerBackend_WorkspaceNetworkCIDRs_EnsuresNetworkBeforeInspect(t *testing.T) {
	api := &fakeDockerAPI{}
	api.NetworkInspectFunc = func(_ context.Context, _ string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
		var res client.NetworkInspectResult
		res.Network.IPAM.Config = []network.IPAMConfig{{Subnet: netip.MustParsePrefix("10.89.9.0/24")}}
		return res, nil
	}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "inst-1"})

	resolver, ok := be.(workspaceNetworkCIDRResolver)
	if !ok {
		t.Fatalf("container backend does not implement workspaceNetworkCIDRResolver; Runner.Dispatch resolves the subnet through that optional interface, so a missing method silently drops the fix")
	}

	cidrs, err := resolver.WorkspaceNetworkCIDRs(context.Background(), "ws-a")
	if err != nil {
		t.Fatalf("WorkspaceNetworkCIDRs: %v", err)
	}
	if len(cidrs) != 1 || cidrs[0] != "10.89.9.0/24" {
		t.Errorf("WorkspaceNetworkCIDRs = %v, want [10.89.9.0/24]", cidrs)
	}

	wantNet := containerWorkspaceNetworkName("inst-1", "ws-a")
	api.mu.Lock()
	createdNames := append([]string(nil), api.networkCreateNames...)
	inspected := append([]string(nil), api.networkInspectIDs...)
	api.mu.Unlock()
	if len(createdNames) != 1 || createdNames[0] != wantNet {
		t.Errorf("NetworkCreate names = %v, want exactly [%s] (the subnet only exists once the network does)", createdNames, wantNet)
	}
	if len(inspected) != 1 || inspected[0] != wantNet {
		t.Errorf("NetworkInspect ids = %v, want exactly [%s]", inspected, wantNet)
	}
}

// envValue returns the value of key in a docker-style "K=V" env slice, or ""
// when absent. Last occurrence wins, matching how a container's environment
// resolves duplicates.
func envValue(env []string, key string) string {
	prefix := key + "="
	got := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			got = strings.TrimPrefix(kv, prefix)
		}
	}
	return got
}

// containsNoProxyEntry reports whether noProxy has want as a whole
// comma-separated entry, rather than as any substring — a substring check
// would pass on a truncated or run-together value (e.g. "10.89.9.0/2" inside
// "10.89.9.0/24", or a "boid-gateway" that is really "notboid-gateway").
func containsNoProxyEntry(noProxy, want string) bool {
	for _, entry := range strings.Split(noProxy, ",") {
		if strings.TrimSpace(entry) == want {
			return true
		}
	}
	return false
}

// jobContainerCreate picks the JOB container out of everything a Dispatch
// created. Since PR5 a dispatch can also create a workspace-home init
// container (docs/plans/workspace-home-volume-persistence.md 論点 c), so
// "the only create" is no longer a safe way to name the one under test —
// and picking by index would be even worse, since the init comes first.
func jobContainerCreate(t *testing.T, api *fakeDockerAPI) client.ContainerCreateOptions {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	var found []client.ContainerCreateOptions
	for _, c := range api.createCalls {
		if strings.HasPrefix(c.Name, "boid-job-") {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d job container creates among %d creates, want exactly 1", len(found), len(api.createCalls))
	}
	return found[0]
}
