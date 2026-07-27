package testutil

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/novshi-tech/boid/testutil/homeenv"
)

// TestServer wraps a running server for testing.
type TestServer struct {
	Server *server.Server
	Client *client.Client
}

// NewTestServer starts a server with a temp UNIX socket and in-memory DB.
func NewTestServer(t *testing.T) *TestServer {
	t.Helper()

	// Refuse to boot a daemon into an un-isolated environment (PR7 round-2
	// codex review, Blocker 2 of docs/plans/workspace-home-volume-persistence.md
	// 論点 a-2).
	//
	// This is a full production-path server: Config.Backend is left unset, so
	// buildRuntime builds a real docker client from client.New(client.FromEnv)
	// and wires it into WorkspaceHandler.Homes / GCHandler.Homes. Every test
	// that then issues DELETE /api/workspaces/{slug} against it reaches
	// VolumeRemove(Force: true) on whatever engine DOCKER_HOST names — the
	// developer's own, with their real workspace HOME volumes on it. Measured
	// on the machine this was found on (2026-07-27): one `go test ./cmd/` run
	// destroyed a `boid-ws-home-noinst-team-c` volume.
	//
	// A guard here rather than only an AssertIsolated test per package,
	// because the failure mode is a package that has NO isolation and
	// therefore also no assertion to fail — internal/api was exactly that
	// until this review. This is the single choke point every test daemon goes
	// through, so a missing TestMain surfaces as a loud fatal on the first
	// test that boots one instead of as silent damage to the developer's
	// engine and boid installation.
	if keys := homeenv.Unisolated(); len(keys) > 0 {
		t.Fatalf("NewTestServer: this test binary is not isolated from the real user environment (%v still hold the values the binary started with). "+
			"A production-path daemon started here would resolve boid's data/config roots — and its docker engine, whose named volumes it can DELETE — "+
			"from the developer's own machine. Add `func TestMain(m *testing.M) { os.Exit(homeenv.Run(m)) }` to this package.", keys)
	}

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
