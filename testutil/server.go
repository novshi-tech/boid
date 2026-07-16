package testutil

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/server"
)

// TestServer wraps a running server for testing.
type TestServer struct {
	Server *server.Server
	Client *client.Client
}

// NewTestServer starts a server with a temp UNIX socket and in-memory DB.
func NewTestServer(t *testing.T) *TestServer {
	t.Helper()

	// Isolate $XDG_CONFIG_HOME from the developer's real
	// ~/.config/boid: server.New's buildProjectStore calls
	// orchestrator.MigrateWorkspaceYAMLToDB with workspaceDir="", which
	// resolves via orchestrator.DefaultWorkspaceDir() (os.UserConfigDir(),
	// i.e. $XDG_CONFIG_HOME or $HOME/.config) unless overridden. Without
	// this, every test using NewTestServer would migrate whatever real
	// workspace yaml/host_commands.yaml happens to exist on the machine
	// running the tests. This was previously harmless because an unresolved
	// kit reference only logged a warning and was skipped; MAJOR 2 (codex
	// review, workspace-db-consolidation PR3 3rd pass) made that a hard
	// preflight failure instead, which surfaced this pre-existing isolation
	// gap as a hard test failure on any machine with real workspace yaml
	// referencing kits absent from this (also isolated) test's KitsDir.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	cfg := server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}

	t.Cleanup(func() { _ = srv.Stop() })

	return &TestServer{
		Server: srv,
		Client: client.NewUnixClient(sockPath),
	}
}

// SeedWorkspace upserts an empty workspaces table row for slug directly via
// the daemon's DB, bypassing both the CLI (cmd/workspace.go's shadow-yaml
// path, out of scope until PR4 of docs/plans/workspace-db-consolidation.md)
// and the HTTP API (which as of PR3 only exposes GET /api/workspaces, no
// creation endpoint). Tests that assign a project to a workspace slug other
// than orchestrator.DefaultWorkspaceSlug must call this first: MAJOR 5
// (codex review) makes ProjectAppService.SetProjectWorkspace reject
// assignment to a slug with no corresponding row.
func SeedWorkspace(t *testing.T, ts *TestServer, slug string) {
	t.Helper()
	repo := orchestrator.NewWorkspaceRepository(ts.Server.DB())
	if err := repo.Save(slug, &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("SeedWorkspace(%q): %v", slug, err)
	}
}
