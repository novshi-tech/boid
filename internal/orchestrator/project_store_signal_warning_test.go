package orchestrator_test

// docs/plans/signal-ingest-detailed-design.md §5.1/§5.2 (PR-5): "connector
// が呼ぶ service が workspace の enabled services に入っている必要がある —
// load 時に警告". ProjectStore.GetWithWorkspace is where this check lives —
// see warnSignalConnectorServicesNotEnabled's own doc comment for why (it
// is the only point in this layer with both a hydrated Triggers list AND
// the linked WorkspaceMeta at once). captureSlog is defined in
// spec_loader_test.go (same package).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func setupProjectDirWithSignals(t *testing.T, dir, id, name string) {
	t.Helper()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := "id: " + id + "\nname: " + name + `
signals:
  sources:
    - connector: slack/mentions
      service: slack-api
      every: 10m
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
}

func TestGetWithWorkspace_SignalConnector_ServiceNotEnabled_Warns(t *testing.T) {
	projectDir := t.TempDir()
	setupProjectDirWithSignals(t, projectDir, "proj-sig-warn", "Signal Warn Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "myworkspace", `
services:
  - some-other-service
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-sig-warn", WorkDir: projectDir, WorkspaceID: "myworkspace"},
	})

	buf := captureSlog(t)
	if _, err := s.GetWithWorkspace(context.Background(), "proj-sig-warn"); err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "not in this workspace's enabled services") {
		t.Fatalf("expected a warning about the unenabled service, log = %s", logged)
	}
	if !strings.Contains(logged, "slack-api") {
		t.Fatalf("expected the warning to name the declared service, log = %s", logged)
	}
}

func TestGetWithWorkspace_SignalConnector_ServiceEnabled_NoWarning(t *testing.T) {
	projectDir := t.TempDir()
	setupProjectDirWithSignals(t, projectDir, "proj-sig-ok", "Signal OK Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "myworkspace", `
services:
  - slack-api
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-sig-ok", WorkDir: projectDir, WorkspaceID: "myworkspace"},
	})

	buf := captureSlog(t)
	if _, err := s.GetWithWorkspace(context.Background(), "proj-sig-ok"); err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if strings.Contains(buf.String(), "not in this workspace's enabled services") {
		t.Fatalf("expected no warning when the declared service IS enabled, log = %s", buf.String())
	}
}

// TestGetWithWorkspace_NoSignalConnector_NoWarning pins that a project with
// no signals.sources at all never emits the warning (nothing to check).
func TestGetWithWorkspace_NoSignalConnector_NoWarning(t *testing.T) {
	projectDir := t.TempDir()
	setupProjectDir(t, projectDir, "proj-nosig", "No Signal Project")

	wsDir := t.TempDir()
	setupWorkspaceDir(t, wsDir, "myworkspace", `
services:
  - some-service
`)

	s := orchestrator.NewProjectStore()
	s.SetWorkspaceStore(orchestrator.NewWorkspaceStore(wsDir))
	loadProjectIntoStore(t, s, []*orchestrator.Project{
		{ID: "proj-nosig", WorkDir: projectDir, WorkspaceID: "myworkspace"},
	})

	buf := captureSlog(t)
	if _, err := s.GetWithWorkspace(context.Background(), "proj-nosig"); err != nil {
		t.Fatalf("GetWithWorkspace: %v", err)
	}
	if strings.Contains(buf.String(), "not in this workspace's enabled services") {
		t.Fatalf("expected no warning for a project with no signals.sources, log = %s", buf.String())
	}
}
