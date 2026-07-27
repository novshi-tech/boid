package server

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/dockerres"
)

// The wiring seam PR8 of docs/plans/workspace-home-volume-persistence.md adds
// (論点 f): POST /api/workspaces/{slug}/home/import -> WorkspaceHandler
// .HomeImporter -> the appRuntime's *dispatcher.Runner -> the sandbox backend's
// volume replacement and tar extraction.
//
// It gets an end-to-end test for the reason
// wire_workspace_home_volume_test.go's own header gives, and with a sharper
// edge here: every OTHER test of this feature stubs one end. internal/api's
// prove the handler calls whatever importer it is given; internal/dispatcher's
// prove the Runner sequences the volume and the marker correctly. Neither can
// notice that mountRoutes never assigned HomeImporter — the daemon would answer
// 501 to every migration, which reads exactly like "this build does not have
// PR8 yet".

// importerBackend is a sandbox backend whose workspace-home capabilities are
// real enough to observe: it models the engine's volume store as a map and
// extracts the archive with Go's own tar reader.
//
// It extends fakeSandboxBackend rather than replacing it so the rest of the
// daemon boots normally.
type importerBackend struct {
	fakeSandboxBackend

	mu sync.Mutex
	// volumes maps a volume name to its identity label, exactly as
	// EnsureWorkspaceHomeVolume's idempotent-VolumeCreate contract requires.
	volumes map[string]string
	// extracted maps an archive entry name to its content, for the most recent
	// import.
	extracted map[string]string
	removed   []string
	imports   []string

	// importGate/importEntered hold a migration open inside the extraction, so a
	// test can issue a second request while one is genuinely in flight. entered
	// is closed by the first extraction; the gate blocks until the test closes
	// it.
	importGate    chan struct{}
	importEntered chan struct{}
	importOnce    sync.Once
}

var (
	_ dispatcher.WorkspaceInitExecutor = (*importerBackend)(nil)
	_ dispatcher.WorkspaceHomeImporter = (*importerBackend)(nil)
)

func newImporterBackend() *importerBackend {
	return &importerBackend{volumes: map[string]string{}, extracted: map[string]string{}}
}

func (b *importerBackend) EnsureWorkspaceHomeVolume(_ context.Context, req dispatcher.WorkspaceHomeVolumeRequest) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id, ok := b.volumes[req.Name]; ok {
		return id, nil
	}
	b.volumes[req.Name] = req.CandidateID
	return req.CandidateID, nil
}

func (b *importerBackend) RunWorkspaceInit(context.Context, dispatcher.WorkspaceInitRequest) error {
	return nil
}

func (b *importerBackend) RemoveWorkspaceHomeVolume(_ context.Context, name string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removed = append(b.removed, name)
	_, existed := b.volumes[name]
	delete(b.volumes, name)
	return existed, nil
}

func (b *importerBackend) ImportWorkspaceHome(_ context.Context, req dispatcher.WorkspaceHomeImportRequest) error {
	if b.importEntered != nil {
		b.importOnce.Do(func() { close(b.importEntered) })
	}
	if b.importGate != nil {
		<-b.importGate
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.imports = append(b.imports, req.Slug)
	tr := tar.NewReader(req.Tar)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		b.extracted[h.Name] = string(body)
	}
}

func (b *importerBackend) snapshot() (removed, imports []string, extracted map[string]string, volumes map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	extracted = map[string]string{}
	for k, v := range b.extracted {
		extracted[k] = v
	}
	volumes = map[string]string{}
	for k, v := range b.volumes {
		volumes[k] = v
	}
	return append([]string(nil), b.removed...), append([]string(nil), b.imports...), extracted, volumes
}

// TestMountRoutes_WorkspaceImportHome_ReachesTheRunnerAndTheBackend is the
// crossing itself, on the real router: a tar posted to the route has to come
// out of the backend's extraction, and the daemon's own marker has to be gone
// afterwards.
func TestMountRoutes_WorkspaceImportHome_ReachesTheRunnerAndTheBackend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmpDir := t.TempDir()
	be := newImporterBackend()
	srv, err := New(Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		Backend:    be,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	// A marker for the default workspace, in the very place the daemon keeps
	// its own (<dataHome>/homes-meta/<slug>.init.json). Its removal is 論点 f's
	// leg 2, and the CLI cannot reach this file at all — which is the whole
	// reason the migration is an endpoint rather than a host-side command (D1).
	markerPath := filepath.Join(tmpDir, "homes-meta", "default.init.json")
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(markerPath, []byte(`{"script_sha256":"x","home_id":"old","init_generation":2}`), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	body := tarWithOneFile(t, ".claude/.credentials.json", "token")
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/default/home/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-tar")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s\n(501 here means mountRoutes never assigned WorkspaceHandler.HomeImporter — "+
			"the whole feature would be dead on a daemon whose every other part works)", w.Code, w.Body.String())
	}

	var resp api.WorkspaceHomeImportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantVolume := dockerres.WorkspaceHomeVolumeName(srv.installID, "default")
	if resp.Volume != wantVolume {
		t.Errorf("Volume = %q, want %q — the daemon migrated something other than the volume it dispatches into",
			resp.Volume, wantVolume)
	}
	if !resp.MarkerRemoved {
		t.Errorf("MarkerRemoved = false (%s)", resp.MarkerRemoveError)
	}

	removed, imports, extracted, volumes := be.snapshot()
	if len(removed) != 1 || removed[0] != wantVolume {
		t.Errorf("removed volumes = %v, want [%s] (論点 f leg 1: the incarnation must be replaced, not written into)", removed, wantVolume)
	}
	if len(imports) != 1 {
		t.Errorf("the backend saw %d extractions, want 1", len(imports))
	}
	if got := extracted[".claude/.credentials.json"]; got != "token" {
		t.Errorf("extracted[.claude/.credentials.json] = %q, want %q — the request body did not reach the extraction", got, "token")
	}
	if volumes[wantVolume] == "" || volumes[wantVolume] == "old" {
		t.Errorf("the replacement volume carries identity %q, want a freshly minted one", volumes[wantVolume])
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("the completion marker survived at %s (stat err = %v): 論点 f leg 2 is not wired through the daemon",
			markerPath, err)
	}
}

// TestMountRoutes_WorkspaceImportHome_UnknownWorkspaceIs404 pins that the
// route's precondition is evaluated against the REAL workspace store, not the
// stub internal/api's own tests use.
func TestMountRoutes_WorkspaceImportHome_UnknownWorkspaceIs404(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmpDir := t.TempDir()
	be := newImporterBackend()
	srv, err := New(Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		Backend:    be,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/never-created/home/import",
		strings.NewReader("not a tar"))
	req.Header.Set("Content-Type", "application/x-tar")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	removed, imports, _, _ := be.snapshot()
	if len(removed) != 0 || len(imports) != 0 {
		t.Errorf("a workspace with no row reached the engine (removed=%v imports=%v)", removed, imports)
	}
}

func tarWithOneFile(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}

// TestNew_SweepsInterruptedWorkspaceHomeMigrationsAtStartup is the startup half
// of codex round 2's Major 2.
//
// A daemon that is SIGKILLed (OOM, `docker compose down -t 0`, a power cut)
// mid-migration runs no defer and no cleanup, and leaves a HOME volume holding a
// partial home with its completion marker already deleted. Nothing else in
// startup collects it: the reap sweep next to this one reaps CONTAINERS by
// label, and the container is not what survives.
//
// The record on disk is what makes it findable, and this test pins that the
// daemon actually looks — a sweep that exists in internal/dispatcher and is
// never called from startup is the same as no sweep at all.
func TestNew_SweepsInterruptedWorkspaceHomeMigrationsAtStartup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmpDir := t.TempDir()
	be := newImporterBackend()

	// The volume a killed migration left behind, and the record that says so.
	// The volume name is written into the record precisely so this does not
	// depend on the install id, which the daemon has not minted yet.
	const wreckage = "boid-ws-home-deadbeef-default"
	be.volumes[wreckage] = "half-written"
	metaDir := filepath.Join(tmpDir, "homes-meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	recordPath := filepath.Join(metaDir, "default.migrating.json")
	if err := os.WriteFile(recordPath,
		[]byte(`{"slug":"default","volume":"`+wreckage+`","started_at":"2026-07-27T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	markerPath := filepath.Join(metaDir, "default.init.json")
	if err := os.WriteFile(markerPath, []byte(`{"script_sha256":"x","home_id":"half-written","init_generation":2}`), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	srv, err := New(Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		Backend:    be,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	removed, _, _, volumes := be.snapshot()
	if _, still := volumes[wreckage]; still {
		t.Errorf("the home volume %q left by an interrupted migration survived daemon startup (removals seen: %v): the "+
			"next dispatch runs init.sh against a partial home, and an init.sh that probes for what it installs "+
			"concludes the toolchain is present and lets a fresh completion marker freeze the breakage", wreckage, removed)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the in-progress record survived the startup sweep (stat err = %v): every later start repeats it", err)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("the completion marker survived the startup sweep (stat err = %v)", err)
	}
}

// TestNew_LeavesSettledWorkspaceHomesAloneAtStartup is that sweep's vacuity
// guard at the wiring level: a startup that removed home volumes without a
// record to justify it would destroy every workspace on the installation at
// every restart, and would pass the test above.
func TestNew_LeavesSettledWorkspaceHomesAloneAtStartup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmpDir := t.TempDir()
	be := newImporterBackend()
	be.volumes["boid-ws-home-deadbeef-default"] = "settled"

	srv, err := New(Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		Backend:    be,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	removed, _, _, volumes := be.snapshot()
	if len(removed) != 0 {
		t.Errorf("startup removed %v with no interrupted-migration record to justify it", removed)
	}
	if volumes["boid-ws-home-deadbeef-default"] != "settled" {
		t.Error("startup destroyed a settled workspace home")
	}
}

// TestMountRoutes_WorkspaceRemove_RefusedWhileAMigrationIsInFlight is codex
// round 2's Major 1, end to end on the real router.
//
// It is the crossing the unit tests on either side cannot see: internal/api's
// prove the handler calls whatever guard it is given, internal/dispatcher's
// prove the registry excludes what it should, and neither notices that
// WorkspaceHandler.Remove and WorkspaceHandler.ImportHome could be wired to two
// DIFFERENT objects with two different registries — which would exclude nothing
// while every unit test stayed green.
func TestMountRoutes_WorkspaceRemove_RefusedWhileAMigrationIsInFlight(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tmpDir := t.TempDir()
	be := newImporterBackend()
	be.importGate = make(chan struct{})
	be.importEntered = make(chan struct{})

	srv, err := New(Config{
		DBPath:     filepath.Join(tmpDir, "boid.db"),
		SocketPath: filepath.Join(tmpDir, "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
		TLSDir:     filepath.Join(tmpDir, "tls"),
		Backend:    be,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	// A removable workspace: the default one is refused at the service layer for
	// its own reasons, which would make the status below ambiguous.
	create := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader("slug: team-a\n"))
	create.Header.Set("Content-Type", "application/yaml")
	cw := httptest.NewRecorder()
	srv.Router().ServeHTTP(cw, create)
	if cw.Code != http.StatusOK && cw.Code != http.StatusCreated {
		t.Fatalf("create workspace: status = %d: %s", cw.Code, cw.Body.String())
	}

	importDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/team-a/home/import",
			bytes.NewReader(tarWithOneFile(t, ".claude/.credentials.json", "token")))
		req.Header.Set("Content-Type", "application/x-tar")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		importDone <- w.Code
	}()

	select {
	case <-be.importEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the migration never reached the extraction")
	}

	del := httptest.NewRequest(http.MethodDelete, "/api/workspaces/team-a", nil)
	dw := httptest.NewRecorder()
	srv.Router().ServeHTTP(dw, del)
	if dw.Code != http.StatusConflict {
		t.Errorf("DELETE while a migration was extracting into this workspace's HOME volume: status = %d, want 409 "+
			"(%s)\nThe removal deletes the workspace ROW; the migration then finishes and leaves a volume full of "+
			"harness credentials that no dispatch can mount and `boid workspace remove` can no longer target",
			dw.Code, dw.Body.String())
	}

	close(be.importGate)
	if code := <-importDone; code != http.StatusOK {
		t.Fatalf("the migration itself failed: status = %d", code)
	}

	// The refusal was free: the workspace is still there and removable now.
	dw2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(dw2, httptest.NewRequest(http.MethodDelete, "/api/workspaces/team-a", nil))
	if dw2.Code != http.StatusOK {
		t.Errorf("the workspace could not be removed after the migration finished: status = %d: %s", dw2.Code, dw2.Body.String())
	}
}
