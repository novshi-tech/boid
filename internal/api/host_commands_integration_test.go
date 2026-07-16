package api_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// TestHostCommandsAPI_ListAndReload exercises GET /api/host_commands and
// POST /api/host_commands/reload end-to-end against a real daemon
// (docs/plans/workspace-db-consolidation.md PR4 Step G), pinning that
// internal/server/wire.go's `&api.HostCommandsHandler{Service: srv}` wiring
// (srv satisfying api.HostCommandsService directly) actually holds
// together.
func TestHostCommandsAPI_ListAndReload(t *testing.T) {
	ts := testutil.NewTestServer(t)

	var before []string
	if err := ts.Client.Do("GET", "/api/host_commands", nil, &before); err != nil {
		t.Fatalf("GET /api/host_commands: %v", err)
	}
	for _, name := range before {
		if name == "aws" {
			t.Fatalf("expected no 'aws' command before hand edit, got %v", before)
		}
	}

	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	if err := orchestrator.WriteHostCommandsConfig(hostCommandsPath, map[string]orchestrator.HostCommandSpec{
		"aws": {Allow: []string{"s3"}},
	}); err != nil {
		t.Fatalf("hand-edit WriteHostCommandsConfig: %v", err)
	}

	if err := ts.Client.Do("POST", "/api/host_commands/reload", nil, nil); err != nil {
		t.Fatalf("POST /api/host_commands/reload: %v", err)
	}

	var after []string
	if err := ts.Client.Do("GET", "/api/host_commands", nil, &after); err != nil {
		t.Fatalf("GET /api/host_commands (after reload): %v", err)
	}
	found := false
	for _, name := range after {
		if name == "aws" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'aws' command after reload, got %v", after)
	}
}
