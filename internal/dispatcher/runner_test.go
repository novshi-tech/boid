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
	calls []fakeProxyAllocCall
	ports map[string]int   // workspaceID → port to return
	errs  map[string]error // workspaceID → error to return
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
		AllowedDomains: floor,
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
		AllowedDomains: floor,
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
		AllowedDomains: floor,
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
		AllowedDomains: floor,
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

// TestResolveWorkspaceProxy_EmptyWorkspaceID_NeverTouchesAllocator pins the
// PR #830 round-4 simplification (nose directive): a no-workspace dispatch
// must never call ProxyAllocator.GetOrCreate at all. The no-workspace
// listener is now bound ONCE at daemon startup (internal/server's
// Server.Start, keyed by NoWorkspaceProxyKey) and its port captured into
// Runner.NoWorkspaceProxyPort — not re-resolved per dispatch, since
// sandbox.allowed_domains is ReloadRestartRequired now (nothing to refresh
// mid-process; see ReloadDynamic's own doc comment, internal/config/
// schema.go). This structurally closes round-4 blocker 1 (a runtime
// allocation-error fallback path that could return the widened default
// listener): there is no runtime allocation call here left to fail.
func TestResolveWorkspaceProxy_EmptyWorkspaceID_NeverTouchesAllocator(t *testing.T) {
	floor := []string{"pypi.org"}
	noWSPort := 9200
	alloc := &fakeProxyAllocator{}
	r := &Runner{
		AllowedDomains:       floor,
		ProxyAllocator:       alloc,
		NoWorkspaceProxyPort: &noWSPort,
	}

	allowed, port := r.resolveWorkspaceProxy("")
	if port != noWSPort {
		t.Errorf("port = %d, want the captured NoWorkspaceProxyPort %d", port, noWSPort)
	}
	if !equalSlice(allowed, floor) {
		t.Errorf("allowed = %v, want %v", allowed, floor)
	}
	if len(alloc.calls) != 0 {
		t.Errorf("alloc.calls = %v, want none — resolveWorkspaceProxy(\"\") must never call the allocator", alloc.calls)
	}
}

// TestResolveWorkspaceProxy_EmptyWorkspaceID_UnwiredNoWorkspacePort_NeverFallsBackToDefault
// pins the flip side: an unwired NoWorkspaceProxyPort (test wiring, or a
// daemon build that never called Server.Start) must return port 0, NEVER
// r.ProxyPort's value. Falling back to the real default-slug workspace's
// own (editable, live-widening) listener is exactly the isolation leak
// NoWorkspaceProxyKey's distinctness exists to prevent (BLOCKER, codex
// review round 3) — a port-0 dispatch failing loudly is safer than a silent
// aliasing regression.
func TestResolveWorkspaceProxy_EmptyWorkspaceID_UnwiredNoWorkspacePort_NeverFallsBackToDefault(t *testing.T) {
	floor := []string{"pypi.org"}
	defaultPort := 8000
	r := &Runner{
		AllowedDomains: floor,
		ProxyPort:      &defaultPort,
		// NoWorkspaceProxyPort deliberately left nil.
	}
	_, port := r.resolveWorkspaceProxy("")
	if port != 0 {
		t.Errorf("port = %d, want 0 (must never silently fall back to ProxyPort=%d, the default workspace's own listener)", port, defaultPort)
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

	// Mirrors what internal/server's Server.Start does once at daemon
	// startup: bind the no-workspace listener under NoWorkspaceProxyKey and
	// capture its port. resolveWorkspaceProxy("") below must never call the
	// allocator itself (PR #830 round-4 simplification, nose directive).
	noWSPort, err := mgr.GetOrCreate(NoWorkspaceProxyKey, floor)
	if err != nil {
		t.Fatalf("seed no-workspace listener: %v", err)
	}

	r := &Runner{
		AllowedDomains:       floor,
		ProxyAllocator:       mgr,
		NoWorkspaceProxyPort: &noWSPort,
		Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
			orchestrator.DefaultWorkspaceSlug: {AllowedDomains: []string{"evil.example.com"}},
		}},
	}

	noWSAllowed, gotNoWSPort := r.resolveWorkspaceProxy("")
	if !equalSlice(noWSAllowed, floor) {
		t.Fatalf("no-workspace allowed = %v, want floor only %v", noWSAllowed, floor)
	}
	if gotNoWSPort != noWSPort {
		t.Fatalf("no-workspace port = %d, want the captured startup port %d", gotNoWSPort, noWSPort)
	}

	defaultAllowed, defaultPort := r.resolveWorkspaceProxy(orchestrator.DefaultWorkspaceSlug)
	wantDefault := []string{"pypi.org", "evil.example.com"}
	if !equalSlice(defaultAllowed, wantDefault) {
		t.Fatalf("default-workspace allowed = %v, want %v", defaultAllowed, wantDefault)
	}

	if gotNoWSPort == defaultPort {
		t.Fatalf("no-workspace and default-workspace share port %d; want distinct listeners", gotNoWSPort)
	}

	// The no-workspace listener must remain floor-only after the
	// default-workspace dispatch widened ITS OWN listener.
	if got := quickConnectStatus(t, gotNoWSPort, "evil.example.com:443"); got != http.StatusForbidden {
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
