package server_test

// docs/plans/signal-ingest-detailed-design.md §5.2 (PR-5): end-to-end wiring
// pin for sessionDispatcherAdapter.StartExec's connector branch — the seam
// exec_dispatch_test.go's existing TestServer_ExecDispatch_ExitCodeRoundTrips
// covers for the plain-exec path. Drives the REAL POST
// /api/projects/{id}/exec round trip (StartExecRequest.Connector set) with a
// real Pack manifest on disk, resolved through the real daemon-startup
// integrationpack.LoadPacks wiring (buildRuntime), and inspects the actual
// sandbox.Spec a (fake) SandboxBackend.Launch received — proving the 4 §5.2
// items (env / bind / connector policy selection / gateway allowlist source)
// actually reach the dispatched job, not just the unit-level builders in
// internal/dispatcher.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/novshi-tech/boid/testutil"
)

// specCapturingBackend wraps noopBackend, additionally recording the last
// sandbox.Spec Launch received.
type specCapturingBackend struct {
	noopBackend
	lastSpec sandbox.Spec
}

func (b *specCapturingBackend) Launch(ctx context.Context, spec sandbox.Spec, opts backend.LaunchOptions) (backend.SandboxSession, error) {
	b.lastSpec = spec
	return b.noopBackend.Launch(ctx, spec, opts)
}

// writeTestPack materializes a minimal, valid Integration Pack under
// <dir>/slack/1.1.0/integration.yaml — enough for integrationpack.LoadPacks
// to accept it and for resolveConnectorExec to resolve the "mentions"
// connector against it.
func writeTestPack(t *testing.T, packsDir string) {
	t.Helper()
	verDir := filepath.Join(packsDir, "slack", "1.1.0")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatalf("mkdir pack dir: %v", err)
	}
	manifest := `
apiVersion: boid.dev/v1
kind: IntegrationPack
metadata:
  name: slack
  version: 1.1.0
serviceProfiles:
  - name: slack-cloud
    endpoint:
      configurable: false
connectors:
  - name: mentions
    executable: connectors/mentions
    serviceProfile: slack-cloud
    configSchema:
      type: object
      properties:
        include_threads: {type: boolean}
`
	if err := os.WriteFile(filepath.Join(verDir, "integration.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// newSmokeServerWithPacks mirrors newSmokeServer (server_phase6_test.go) but
// additionally: (1) writes a config.yaml under the isolated XDG_CONFIG_HOME
// pointing integrations.dir at a temp Pack directory containing a real
// "slack" Pack, so buildRuntime's LoadPacks call actually finds it; (2)
// wires a specCapturingBackend instead of a bare noopBackend so the test can
// inspect the dispatched sandbox.Spec.
func newSmokeServerWithPacks(t *testing.T) (*testutil.TestServer, *specCapturingBackend, string) {
	t.Helper()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	packsDir := t.TempDir()
	writeTestPack(t, packsDir)

	configDir := filepath.Join(configHome, "boid")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	configYAML := "integrations:\n  dir: " + packsDir + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")
	dbPath := filepath.Join(tmpDir, "boid.db")

	backend := &specCapturingBackend{}
	srv, err := server.New(server.Config{
		DBPath:     dbPath,
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		Backend:    backend,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	return &testutil.TestServer{Server: srv, Client: client.NewUnixClient(sockPath)}, backend, packsDir
}

// TestServer_ExecDispatch_ConnectorReference_ResolvesEnv is the §5.2
// end-to-end pin: env (item 1) actually reaches the dispatched sandbox.Spec
// when StartExecRequest.Connector names a real, installed Pack connector.
// No bind mount is involved — daemon and job container share the same base
// image (compose.yml 決定2), so BOID_CONNECTOR_EXEC points at pack.Dir
// directly, a path already present inside the job container's own
// filesystem (nose 2026-08-27: a DooD bind mount's source is resolved by
// the HOST docker/podman, not the daemon container's own filesystem view —
// daemon-container-only paths like an image-baked integrations.dir can
// never be a valid bind source there).
func TestServer_ExecDispatch_ConnectorReference_ResolvesEnv(t *testing.T) {
	ts, backend, packsDir := newSmokeServerWithPacks(t)
	projectDir := writeSmokeProject(t)

	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]any{
		"work_dir": projectDir,
	}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var exec api.StartExecResult
	err := ts.Client.Do("POST", "/api/projects/"+project.ID+"/exec", api.StartExecRequest{
		Argv:     []string{"sh", "-c", `exec "$BOID_CONNECTOR_EXEC"`},
		Readonly: true,
		Connector: &api.ConnectorRef{
			Pack:          "slack",
			ConnectorName: "mentions",
			Service:       "slack-api",
			Config:        map[string]any{"include_threads": true},
		},
	}, &exec)
	if err != nil {
		t.Fatalf("start exec: %v", err)
	}
	if exec.JobID == "" {
		t.Fatal("expected non-empty job id")
	}

	spec := backend.lastSpec
	if spec.Env["BOID_SIGNAL_SERVICE"] != "slack-api" {
		t.Errorf("BOID_SIGNAL_SERVICE = %q, want slack-api (env = %+v)", spec.Env["BOID_SIGNAL_SERVICE"], spec.Env)
	}
	if spec.Env["BOID_SIGNAL_CONNECTOR"] != "slack/mentions" {
		t.Errorf("BOID_SIGNAL_CONNECTOR = %q, want slack/mentions", spec.Env["BOID_SIGNAL_CONNECTOR"])
	}
	wantExec := filepath.Join(packsDir, "slack", "1.1.0", "connectors", "mentions")
	if spec.Env["BOID_CONNECTOR_EXEC"] != wantExec {
		t.Errorf("BOID_CONNECTOR_EXEC = %q, want %q (pack.Dir directly, no bind mount)", spec.Env["BOID_CONNECTOR_EXEC"], wantExec)
	}
	if spec.Env["BOID_SIGNAL_CONFIG"] != `{"include_threads":true}` {
		t.Errorf("BOID_SIGNAL_CONFIG = %q, want {\"include_threads\":true}", spec.Env["BOID_SIGNAL_CONFIG"])
	}

	for _, m := range spec.Mounts {
		if m.Target == "/run/boid/integrations/slack" {
			t.Errorf("unexpected Pack bind mount %+v — daemon and job container share the same base image, no bind mount should be added", m)
		}
	}
}

// TestServer_ExecDispatch_ConnectorReference_UnknownPack_Rejected pins the
// "resolution failure fails StartExec outright" contract end to end (no
// packs installed at all — bare newSmokeServer, no config.yaml override).
func TestServer_ExecDispatch_ConnectorReference_UnknownPack_Rejected(t *testing.T) {
	ts := newSmokeServer(t)
	projectDir := writeSmokeProject(t)

	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]any{
		"work_dir": projectDir,
	}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var exec api.StartExecResult
	err := ts.Client.Do("POST", "/api/projects/"+project.ID+"/exec", api.StartExecRequest{
		Argv: []string{"true"},
		Connector: &api.ConnectorRef{
			Pack:          "slack",
			ConnectorName: "mentions",
			Service:       "slack-api",
		},
	}, &exec)
	if err == nil {
		t.Fatal("expected an error for a connector referencing an uninstalled pack")
	}
}
