package dispatcher

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type fakeProjectLookup struct {
	projects []*orchestrator.Project
}

func (f fakeProjectLookup) GetProject(id string) (*orchestrator.Project, error) {
	for _, p := range f.projects {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, nil
}

func (f fakeProjectLookup) ListProjects() ([]*orchestrator.Project, error) {
	return f.projects, nil
}

func TestRunnerResolveWorkspacePeers_SameWorkspaceExcludesSelfAndOtherWorkspaces(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "ws-1", WorkDir: "/workspace/proj-1"},
			{ID: "proj-2", WorkspaceID: "ws-1", WorkDir: "/workspace/proj-2"},
			{ID: "proj-3", WorkspaceID: "ws-2", WorkDir: "/workspace/proj-3"},
		}},
	}

	peers := r.resolveWorkspacePeers("ws-1", "proj-1")
	if len(peers) != 1 {
		t.Fatalf("peers = %#v, want 1 entry", peers)
	}
	if peers["proj-2"] != "/workspace/proj-2" {
		t.Fatalf("peers[proj-2] = %q, want /workspace/proj-2", peers["proj-2"])
	}
	if _, ok := peers["proj-1"]; ok {
		t.Fatalf("peers should not contain self: %#v", peers)
	}
	if _, ok := peers["proj-3"]; ok {
		t.Fatalf("peers should not contain other workspace: %#v", peers)
	}
}

// Gate jobs historically passed Visibility.WorkspacePeers=nil, which caused
// AllowedProjectIDs to shrink to self-only and blocked `boid task create` on
// peer projects. Dispatcher now resolves peers independently from the
// project catalog, so gate jobs see the full peer allowlist too.
func TestRunnerResolveWorkspacePeers_GateStillSeesPeers(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "boid", WorkspaceID: "boid", WorkDir: "/work/boid"},
			{ID: "boid-kits", WorkspaceID: "boid", WorkDir: "/work/boid-kits"},
		}},
	}

	peers := r.resolveWorkspacePeers("boid", "boid")
	allowed := allowedProjectIDs("boid", peers)
	if len(allowed) != 2 {
		t.Fatalf("allowed = %#v, want [boid boid-kits]", allowed)
	}
	if allowed[0] != "boid" || allowed[1] != "boid-kits" {
		t.Fatalf("allowed = %#v, want [boid boid-kits]", allowed)
	}
}

func TestRunnerResolveWorkspacePeers_NilWhenWorkspaceEmpty(t *testing.T) {
	r := &Runner{
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkspaceID: "", WorkDir: "/workspace/proj-1"},
		}},
	}
	if peers := r.resolveWorkspacePeers("", "proj-1"); peers != nil {
		t.Fatalf("peers = %#v, want nil when workspace is empty", peers)
	}
}

// fakeWorkspaceLookup is a stub WorkspaceLookup for runner tests. It returns
// the per-slug meta from the map; an absent slug returns os.ErrNotExist
// (the documented degraded window for WorkspaceStore.Load).
type fakeWorkspaceLookup struct {
	metas map[string]*orchestrator.WorkspaceMeta
	err   error // when non-nil, every Load returns this error
}

func (f fakeWorkspaceLookup) Load(slug string) (*orchestrator.WorkspaceMeta, error) {
	if f.err != nil {
		return nil, f.err
	}
	if m, ok := f.metas[slug]; ok {
		return m, nil
	}
	return nil, fmt.Errorf("workspace %q: %w", slug, os.ErrNotExist)
}

// fakeProxyAllocator records its calls and lets each return a per-workspace
// port (or a forced error). Used to verify Dispatch wires the
// resolveAllowedDomains result into ProxyManager.GetOrCreate verbatim.
type fakeProxyAllocator struct {
	calls   []fakeProxyAllocCall
	ports   map[string]int    // workspaceID → port to return
	errs    map[string]error  // workspaceID → error to return
}

type fakeProxyAllocCall struct {
	workspaceID string
	allowed     []string
}

func (a *fakeProxyAllocator) GetOrCreate(workspaceID string, allowed []string) (int, error) {
	a.calls = append(a.calls, fakeProxyAllocCall{workspaceID: workspaceID, allowed: append([]string(nil), allowed...)})
	if err := a.errs[workspaceID]; err != nil {
		return 0, err
	}
	if p, ok := a.ports[workspaceID]; ok {
		return p, nil
	}
	return 9000, nil
}

func TestResolveWorkspaceProxy_AppliesWorkspaceOverrides(t *testing.T) {
	floor := []string{"pypi.org", "github.com"}
	defaultPort := 8000
	alloc := &fakeProxyAllocator{ports: map[string]int{"ws-a": 9100}}
	lookup := fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
		"ws-a": {AllowedDomains: []string{".cosmos.azure.com"}},
	}}
	r := &Runner{
		AllowedDomains: func() []string { return floor },
		ProxyPort:      &defaultPort,
		ProxyAllocator: alloc,
		Workspaces:     lookup,
	}

	allowed, port := r.resolveWorkspaceProxy("ws-a")

	if port != 9100 {
		t.Errorf("port = %d, want 9100 (workspace-specific listener)", port)
	}
	want := []string{"pypi.org", "github.com", ".cosmos.azure.com"}
	if !equalSlice(allowed, want) {
		t.Errorf("allowed = %v, want %v", allowed, want)
	}

	if len(alloc.calls) != 1 {
		t.Fatalf("alloc calls = %d, want 1", len(alloc.calls))
	}
	got := alloc.calls[0]
	if got.workspaceID != "ws-a" {
		t.Errorf("alloc call workspaceID = %q, want ws-a", got.workspaceID)
	}
	if !equalSlice(got.allowed, want) {
		t.Errorf("alloc call allowed = %v, want %v", got.allowed, want)
	}
}

func TestResolveWorkspaceProxy_FloorOnlyWhenWorkspaceMissing(t *testing.T) {
	// workspace.yaml not on disk → ErrNotExist → degrade to floor without warn.
	floor := []string{"pypi.org"}
	defaultPort := 8000
	alloc := &fakeProxyAllocator{ports: map[string]int{"ws-a": 9100}}
	lookup := fakeWorkspaceLookup{metas: nil} // every Load returns ErrNotExist
	r := &Runner{
		AllowedDomains: func() []string { return floor },
		ProxyPort:      &defaultPort,
		ProxyAllocator: alloc,
		Workspaces:     lookup,
	}

	allowed, port := r.resolveWorkspaceProxy("ws-a")

	if port != 9100 {
		t.Errorf("port = %d, want 9100", port)
	}
	if !equalSlice(allowed, floor) {
		t.Errorf("allowed = %v, want %v (floor only)", allowed, floor)
	}
	if len(alloc.calls) != 1 || !equalSlice(alloc.calls[0].allowed, floor) {
		t.Errorf("alloc received %v, want exactly the floor", alloc.calls)
	}
}

func TestResolveWorkspaceProxy_FallbackWhenAllocatorErrors(t *testing.T) {
	floor := []string{"pypi.org"}
	defaultPort := 8000
	alloc := &fakeProxyAllocator{errs: map[string]error{"ws-a": fmt.Errorf("listener bind failed")}}
	r := &Runner{
		AllowedDomains: func() []string { return floor },
		ProxyPort:      &defaultPort,
		ProxyAllocator: alloc,
		Workspaces:     fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{"ws-a": {AllowedDomains: []string{"new.example.com"}}}},
	}

	allowed, port := r.resolveWorkspaceProxy("ws-a")

	if port != defaultPort {
		t.Errorf("fallback port = %d, want default %d", port, defaultPort)
	}
	if !equalSlice(allowed, floor) {
		t.Errorf("fallback allowed = %v, want %v (floor only)", allowed, floor)
	}
}

func TestResolveWorkspaceProxy_AllocatorUnwired_StaysOnFloor(t *testing.T) {
	floor := []string{"pypi.org"}
	defaultPort := 8000
	r := &Runner{
		AllowedDomains: func() []string { return floor },
		ProxyPort:      &defaultPort,
		// ProxyAllocator deliberately nil — test-wired runners (and the
		// initial daemon boot path before proxy_manager is started) must
		// still produce a working dispatch.
	}
	allowed, port := r.resolveWorkspaceProxy("ws-a")
	if port != defaultPort {
		t.Errorf("port = %d, want %d", port, defaultPort)
	}
	if !equalSlice(allowed, floor) {
		t.Errorf("allowed = %v, want %v", allowed, floor)
	}
}

// TestResolveWorkspaceProxy_EmptyWorkspaceID_UsesDedicatedNoWorkspaceListener
// pins MAJOR 3 (codex review round 2): a job with no workspace at all
// (`boid exec` / ProfileInit against an unlinked project) must still push
// the current floor into its listener via ProxyAllocator.GetOrCreate on
// every call — not just read a static, boot-time-frozen port — so a
// hot-reloaded sandbox.allowed_domains reaches it immediately.
//
// It also pins the BLOCKER fix (codex review round 3): that call must use
// noWorkspaceProxyKey, NOT orchestrator.DefaultWorkspaceSlug — pre-fix, this
// test asserted the workspaceID passed to the allocator was
// orchestrator.DefaultWorkspaceSlug, the exact aliasing bug
// resolveWorkspaceProxy's own doc comment describes (a real "default"-slug
// workspace's own AllowedDomains would widen the SAME shared listener a
// no-workspace job was using). See
// TestResolveWorkspaceProxy_EmptyWorkspaceAndDefaultWorkspace_DistinctListeners
// for the end-to-end proof against a real ProxyManager.
func TestResolveWorkspaceProxy_EmptyWorkspaceID_UsesDedicatedNoWorkspaceListener(t *testing.T) {
	floor := []string{"pypi.org"}
	defaultPort := 8000
	alloc := &fakeProxyAllocator{ports: map[string]int{noWorkspaceProxyKey: 8000}}
	r := &Runner{
		AllowedDomains: func() []string { return floor },
		ProxyPort:      &defaultPort,
		ProxyAllocator: alloc,
	}
	allowed, port := r.resolveWorkspaceProxy("")
	if port != defaultPort {
		t.Errorf("port = %d, want %d", port, defaultPort)
	}
	if !equalSlice(allowed, floor) {
		t.Errorf("allowed = %v, want %v", allowed, floor)
	}
	if len(alloc.calls) != 1 {
		t.Fatalf("alloc calls = %d, want exactly 1 (the no-workspace listener must be refreshed, not skipped)", len(alloc.calls))
	}
	got := alloc.calls[0]
	if got.workspaceID != noWorkspaceProxyKey {
		t.Errorf("alloc call workspaceID = %q, want %q (NOT orchestrator.DefaultWorkspaceSlug — that key belongs to the real default workspace)", got.workspaceID, noWorkspaceProxyKey)
	}
	if !equalSlice(got.allowed, floor) {
		t.Errorf("alloc call allowed = %v, want %v", got.allowed, floor)
	}
}

// TestResolveWorkspaceProxy_EmptyWorkspaceAndDefaultWorkspace_DistinctListeners
// is the BLOCKER regression test (codex review round 3): it exercises the
// REAL sandbox.ProxyManager (not the fake) because the bug is specifically
// about a shared, mutable *sandbox.Proxy instance — a fake keyed by a plain
// map cannot reproduce the aliasing the pre-fix code exhibited when both
// call sites passed orchestrator.DefaultWorkspaceSlug.
//
// Scenario: a no-workspace job (`boid exec`) dispatches first, floor-only.
// Then a REAL "default"-slug workspace — which does have its own
// workspace.yaml AllowedDomains — dispatches with an extra domain. The two
// must land on distinct listeners: the default-workspace dispatch must not
// widen the no-workspace listener, and the no-workspace job must never gain
// the default workspace's own domain.
func TestResolveWorkspaceProxy_EmptyWorkspaceAndDefaultWorkspace_DistinctListeners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := sandbox.NewProxyManager()
	mgr.Start(ctx)
	defer mgr.StopAll()

	floor := []string{"pypi.org"}
	r := &Runner{
		AllowedDomains: func() []string { return floor },
		ProxyAllocator: mgr,
		Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
			orchestrator.DefaultWorkspaceSlug: {AllowedDomains: []string{"evil.example.com"}},
		}},
	}

	noWSAllowed, noWSPort := r.resolveWorkspaceProxy("")
	if !equalSlice(noWSAllowed, floor) {
		t.Fatalf("no-workspace allowed = %v, want floor only %v", noWSAllowed, floor)
	}

	defaultAllowed, defaultPort := r.resolveWorkspaceProxy(orchestrator.DefaultWorkspaceSlug)
	wantDefault := []string{"pypi.org", "evil.example.com"}
	if !equalSlice(defaultAllowed, wantDefault) {
		t.Fatalf("default-workspace allowed = %v, want %v", defaultAllowed, wantDefault)
	}

	if noWSPort == defaultPort {
		t.Fatalf("no-workspace and default-workspace share port %d; want distinct listeners", noWSPort)
	}

	// The no-workspace listener must remain floor-only after the
	// default-workspace dispatch widened ITS OWN listener.
	if got := quickConnectStatus(t, noWSPort, "evil.example.com:443"); got != http.StatusForbidden {
		t.Errorf("no-workspace listener allowed evil.example.com after an unrelated default-workspace dispatch widened its own listener: status = %d, want 403", got)
	}

	// And the reverse: the default workspace's own listener does grant what
	// its own workspace.yaml resolved to.
	if got := quickConnectStatus(t, defaultPort, "evil.example.com:443"); got != http.StatusBadGateway && got != http.StatusOK {
		t.Errorf("default-workspace listener blocked its own allowed domain: status = %d, want 200 or 502", got)
	}
}

// quickConnectStatus dials 127.0.0.1:port, sends a CONNECT for host, and
// returns the response status code. Mirrors internal/sandbox's
// (unexported-to-this-package) proxy_manager_test.go helper of the same
// shape, duplicated here because that one lives in package sandbox_test.
func quickConnectStatus(t *testing.T, port int, host string) int {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode
}

// TestResolveWorkspaceProxy_EmptyWorkspaceID_AllowedDomainsRefreshedEveryCall
// is MAJOR 3's direct hot-reload regression test: two resolveWorkspaceProxy("")
// calls on the SAME *Runner, with the floor mutated in between (exactly what
// internal/server's applyDynamicConfigLocked does to whatever
// Runner.AllowedDomains reads — mirrors
// TestResolveWorkspaceProxy_AllowedDomainsGetterReadFreshEveryCall's
// established pattern for the named-workspace path), must push the NEW
// floor into the default listener on the very next call — not "after
// another dispatch" and not "only after a restart".
func TestResolveWorkspaceProxy_EmptyWorkspaceID_AllowedDomainsRefreshedEveryCall(t *testing.T) {
	current := []string{"pypi.org"}
	defaultPort := 8000
	alloc := &fakeProxyAllocator{}
	r := &Runner{
		AllowedDomains: func() []string { return current },
		ProxyPort:      &defaultPort,
		ProxyAllocator: alloc,
	}

	if _, _ = r.resolveWorkspaceProxy(""); !equalSlice(alloc.calls[0].allowed, []string{"pypi.org"}) {
		t.Fatalf("first call: alloc received %v, want [pypi.org]", alloc.calls[0].allowed)
	}

	// Simulate a config hot-reload (e.g. `boid config set
	// sandbox.allowed_domains ...`) removing the exfiltration-relevant
	// domain that was there before.
	current = []string{"pypi.org", "registry.npmjs.org"}

	if _, _ = r.resolveWorkspaceProxy(""); len(alloc.calls) != 2 {
		t.Fatalf("alloc calls = %d, want 2 (refreshed on the very next call, not deferred)", len(alloc.calls))
	}
	if !equalSlice(alloc.calls[1].allowed, current) {
		t.Errorf("second call: alloc received %v, want %v (the just-hot-reloaded floor)", alloc.calls[1].allowed, current)
	}
}

// TestResolveWorkspaceProxy_AllowedDomainsGetterReadFreshEveryCall pins
// BLOCKER 2 (codex review round 1): Runner.AllowedDomains is a getter, not a
// captured slice, precisely so a later swap of whatever backs the getter
// (internal/server's ApplyConfigYAML hot-reload, in production) is observed
// by the NEXT dispatch without reconstructing the Runner. This test proves
// the mechanism directly: mutate the state the getter closes over between
// two resolveWorkspaceProxy calls on the SAME *Runner and confirm the
// second call sees the new value — a pre-fix plain []string field could
// never do this, since the slice would have been copied in at Wire() time.
func TestResolveWorkspaceProxy_AllowedDomainsGetterReadFreshEveryCall(t *testing.T) {
	current := []string{"pypi.org"}
	defaultPort := 8000
	r := &Runner{
		AllowedDomains: func() []string { return current },
		ProxyPort:      &defaultPort,
	}

	allowed1, _ := r.resolveWorkspaceProxy("")
	if !equalSlice(allowed1, []string{"pypi.org"}) {
		t.Fatalf("first call: allowed = %v, want [pypi.org]", allowed1)
	}

	// Simulate a config hot-reload swapping the underlying state — no
	// Runner reconstruction, exactly what internal/server's
	// applyDynamicConfigLocked does to whatever srv.AllowedDomains reads.
	current = []string{"pypi.org", "registry.npmjs.org"}

	allowed2, _ := r.resolveWorkspaceProxy("")
	if !equalSlice(allowed2, current) {
		t.Errorf("second call (after hot-reload): allowed = %v, want %v", allowed2, current)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
