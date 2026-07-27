package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/moby/moby/client"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

// The wiring seam PR7 of docs/plans/workspace-home-volume-persistence.md adds
// (論点 a-2): buildRuntime's one docker client -> appRuntime.workspaceHomes ->
// mountRoutes -> api.WorkspaceHandler.Homes / api.GCHandler.Homes -> a live
// HTTP response.
//
// It gets its own end-to-end test rather than two half-tests because this
// repo keeps producing the same failure: both ends of a seam are changed, each
// end's own package tests stay green, and nothing crosses. internal/api's
// tests prove the handlers use whatever store they are given; internal/
// dispatcher's prove the store calls the right engine endpoints. Neither can
// notice that mountRoutes never assigned the field — the daemon would just
// report Home=null for every workspace forever, which is EXACTLY the
// PR6-shaped bug this PR exists to fix, reintroduced silently.

// canaryHomeStore is a WorkspaceHomeStore whose answers are recognizable in a
// JSON response body, so a test can tell "the handler consulted THE store
// wire.go built" apart from "the handler consulted something".
type canaryHomeStore struct{ volume string }

func (c *canaryHomeStore) Get(_ context.Context, slug string) dispatcher.WorkspaceHomeVolume {
	return dispatcher.WorkspaceHomeVolume{Slug: slug, Volume: c.volume, Exists: true, Bytes: 4242}
}

func (c *canaryHomeStore) List(_ context.Context) ([]dispatcher.WorkspaceHomeVolume, error) {
	return []dispatcher.WorkspaceHomeVolume{{Slug: "canary-ws", Volume: c.volume, Exists: true, Bytes: 4242}}, nil
}

func (c *canaryHomeStore) Remove(_ context.Context, slug string) (dispatcher.WorkspaceHomeVolume, bool, error) {
	return dispatcher.WorkspaceHomeVolume{Slug: slug, Volume: c.volume, Exists: true, Bytes: 4242}, true, nil
}

// newProductionPathServer boots a Server the way
// TestServer_New_SelfInspectGetsTheBackendsDockerClient does: Backend
// deliberately unset, which is the only path that builds a real docker client
// and therefore the only one where the store is constructed at all.
func newProductionPathServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmpDir := t.TempDir()
	srv, err := New(Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv
}

// TestServer_New_WorkspaceHomeStoreGetsTheBackendsDockerClient pins the
// buildRuntime half: the store is constructed with the SAME *client.Client the
// container backend was built with (論点 a-2's D2 — "docker client は
// buildRuntime で既に単一化されているので、それを渡すこと"), not a second one.
//
// Pointer identity is the assertion, for the reason
// TestServer_New_SelfInspectGetsTheBackendsDockerClient spells out: merely
// receiving SOME non-nil client is also true of a buildRuntime that builds a
// second one, which costs a second DOCKER_CERT_PATH read on the startup path
// and gives the feature its own independent way to fail on a deployment whose
// backend is healthy.
func TestServer_New_WorkspaceHomeStoreGetsTheBackendsDockerClient(t *testing.T) {
	restore := newWorkspaceHomeStoreFn
	t.Cleanup(func() { newWorkspaceHomeStoreFn = restore })

	var gotEngine dispatcher.WorkspaceHomeVolumeAPI
	var gotInstallID string
	var called bool
	newWorkspaceHomeStoreFn = func(engine dispatcher.WorkspaceHomeVolumeAPI, installID string) api.WorkspaceHomeStore {
		gotEngine, gotInstallID, called = engine, installID, true
		return &canaryHomeStore{volume: "canary-volume"}
	}

	srv := newProductionPathServer(t)

	if !called {
		t.Fatal("the workspace home store was never constructed")
	}
	cli, ok := gotEngine.(*client.Client)
	if !ok {
		t.Fatalf("the store was handed a %T, want the *client.Client the container backend was built with", gotEngine)
	}
	if cli == nil {
		t.Fatal("the store was handed a typed-nil *client.Client: non-nil as an interface, panics on first use")
	}
	sandboxBackend := runnerFromServer(t, srv).Backend
	if !dispatcher.ContainerBackendUsesDockerAPI(sandboxBackend, cli) {
		t.Errorf("the workspace home store holds a DIFFERENT docker client (%p) than the container backend: "+
			"buildRuntime built two, so the size/delete surface can now fail on its own against an engine the backend "+
			"is talking to happily", cli)
	}
	if want := srv.installID; gotInstallID != want {
		t.Errorf("the store was scoped to install id %q, want %q — the volume names it generates would not match "+
			"the ones dispatcher.resolveWorkspaceHome mounts", gotInstallID, want)
	}
}

// TestMountRoutes_WorkspaceShowUsesTheWiredHomeStore is the crossing itself,
// on the real router: the canary the constructor returned has to come back out
// of GET /api/workspaces/{slug}.
func TestMountRoutes_WorkspaceShowUsesTheWiredHomeStore(t *testing.T) {
	restore := newWorkspaceHomeStoreFn
	t.Cleanup(func() { newWorkspaceHomeStoreFn = restore })
	newWorkspaceHomeStoreFn = func(dispatcher.WorkspaceHomeVolumeAPI, string) api.WorkspaceHomeStore {
		return &canaryHomeStore{volume: "canary-volume"}
	}

	srv := newProductionPathServer(t)

	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/workspaces/default", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var detail api.WorkspaceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.Home == nil {
		t.Fatal("Home = nil: mountRoutes did not give WorkspaceHandler the home store buildRuntime built")
	}
	if detail.Home.Volume != "canary-volume" || detail.Home.Bytes != 4242 {
		t.Errorf("Home = %+v, want the wired store's canary (volume=canary-volume, bytes=4242)", detail.Home)
	}
}

// TestMountRoutes_GCUsesTheWiredHomeStore is the same crossing for the other
// handler. Both are asserted, not just one: mountRoutes assigns the field
// twice, and PR6 left exactly this kind of half-rewiring behind.
func TestMountRoutes_GCUsesTheWiredHomeStore(t *testing.T) {
	restore := newWorkspaceHomeStoreFn
	t.Cleanup(func() { newWorkspaceHomeStoreFn = restore })
	newWorkspaceHomeStoreFn = func(dispatcher.WorkspaceHomeVolumeAPI, string) api.WorkspaceHomeStore {
		return &canaryHomeStore{volume: "canary-volume"}
	}

	srv := newProductionPathServer(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/gc", bytes.NewBufferString(`{"dry_run":true}`))
	srv.Router().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		WorkspaceHomes []api.WorkspaceHomeSize `json:"workspace_homes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.WorkspaceHomes) != 1 {
		t.Fatalf("workspace_homes = %+v, want the wired store's single canary entry", resp.WorkspaceHomes)
	}
	if resp.WorkspaceHomes[0].Volume != "canary-volume" {
		t.Errorf("workspace_homes[0].Volume = %q, want canary-volume", resp.WorkspaceHomes[0].Volume)
	}
	if !resp.WorkspaceHomes[0].Orphan {
		t.Error("workspace_homes[0].Orphan = false, want true: the canary slug has no workspace row, " +
			"so GCHandler.Workspaces must also have been wired for the diff to be computed")
	}
}

// TestMountRoutes_NoDockerClient_OmitsHomeReporting pins the other side of
// 論点 a-2's D5 gate at the wiring level: with a DI backend injected there is
// no docker client, so no store is built, and both handlers omit home
// information entirely — the same degradation an empty RuntimesDir used to
// produce.
func TestMountRoutes_NoDockerClient_OmitsHomeReporting(t *testing.T) {
	restore := newWorkspaceHomeStoreFn
	t.Cleanup(func() { newWorkspaceHomeStoreFn = restore })
	var constructed bool
	newWorkspaceHomeStoreFn = func(dispatcher.WorkspaceHomeVolumeAPI, string) api.WorkspaceHomeStore {
		constructed = true
		return &canaryHomeStore{volume: "canary-volume"}
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmpDir := t.TempDir()
	srv, err := New(Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		Backend:    &fakeSandboxBackend{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if constructed {
		t.Error("a home store was constructed with no docker client to build it from: " +
			"a typed-nil handed through the interface is non-nil as an interface and turns the feature silently ON")
	}

	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/workspaces/default", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var detail api.WorkspaceDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.Home != nil {
		t.Errorf("Home = %+v, want nil with no engine handle wired", detail.Home)
	}

	gcW := httptest.NewRecorder()
	srv.Router().ServeHTTP(gcW, httptest.NewRequest(http.MethodPost, "/api/gc", bytes.NewBufferString(`{"dry_run":true}`)))
	if gcW.Code != http.StatusOK {
		t.Fatalf("gc status = %d: %s", gcW.Code, gcW.Body.String())
	}
	var gcResp struct {
		WorkspaceHomes []api.WorkspaceHomeSize `json:"workspace_homes"`
	}
	if err := json.Unmarshal(gcW.Body.Bytes(), &gcResp); err != nil {
		t.Fatalf("unmarshal gc: %v", err)
	}
	if len(gcResp.WorkspaceHomes) != 0 {
		t.Errorf("workspace_homes = %+v, want omitted with no engine handle wired", gcResp.WorkspaceHomes)
	}
}
