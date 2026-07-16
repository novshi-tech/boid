package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// seedHostCommands hand-edits the aggregated host_commands.yaml and reloads
// it into the running daemon, so a subsequent workspace create/update body
// referencing one of names passes MAJOR 2's live-snapshot validation
// (docs/plans/workspace-db-consolidation.md, codex review: CreateWorkspace/
// UpdateWorkspace reject a meta.HostCommands reference the daemon does not
// know about). Tests below that reference "gh"/"aws" purely as CRUD-flow
// filler content (not testing host_commands validation itself) call this
// first so they keep exercising the rest of the flow instead of universally
// 400ing on an unrelated concern.
func seedHostCommands(t *testing.T, ts *testutil.TestServer, names ...string) {
	t.Helper()
	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	specs := make(map[string]orchestrator.HostCommandSpec, len(names))
	for _, name := range names {
		specs[name] = orchestrator.HostCommandSpec{}
	}
	if err := orchestrator.WriteHostCommandsConfig(hostCommandsPath, specs); err != nil {
		t.Fatalf("WriteHostCommandsConfig: %v", err)
	}
	if err := ts.Server.ReloadHostCommands(); err != nil {
		t.Fatalf("ReloadHostCommands: %v", err)
	}
}

// TestWorkspaceAPI_CreateShowUpdateRemove exercises the full workspace CRUD
// surface (docs/plans/workspace-db-consolidation.md PR4 Step C/D/E/F)
// end-to-end against a real daemon (testutil.NewTestServer): HTTP handler →
// ProjectAppService → orchestrator.WorkspaceStore/WorkspaceRepository → real
// SQLite. Unit tests elsewhere in this package exercise the handler and
// service layers against fakes; this test pins that the wiring in
// internal/server/wire.go (ProjectAppService.Workspaces =
// store.WorkspaceStore()) actually holds together.
func TestWorkspaceAPI_CreateShowUpdateRemove(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedHostCommands(t, ts, "gh", "aws")

	// Create.
	var created api.WorkspaceDetail
	createBody := []byte("slug: team-a\nhost_commands:\n  - gh\nenv:\n  FOO: bar\n")
	if err := ts.Client.DoWithContentType("POST", "/api/workspaces", "application/yaml", createBody, &created); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if created.Slug != "team-a" {
		t.Fatalf("created.Slug = %q, want team-a", created.Slug)
	}
	if created.Revision == "" {
		t.Fatal("expected non-empty revision after create")
	}
	if !equalStrSlice(created.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("created.Meta.HostCommands = %v", created.Meta.HostCommands)
	}

	// Create again with the same slug must 409.
	err := ts.Client.DoWithContentType("POST", "/api/workspaces", "application/yaml", createBody, &api.WorkspaceDetail{})
	if err == nil {
		t.Fatal("expected conflict creating team-a a second time, got nil")
	}

	// Show.
	var shown api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &shown); err != nil {
		t.Fatalf("show workspace: %v", err)
	}
	if shown.Revision != created.Revision {
		t.Errorf("shown.Revision = %q, want %q (unchanged since create)", shown.Revision, created.Revision)
	}

	// Update without If-Match must 428.
	updateBody := []byte("host_commands:\n  - gh\n  - aws\n")
	err = ts.Client.DoWithContentType("PUT", "/api/workspaces/team-a", "application/yaml", updateBody, &api.WorkspaceDetail{})
	if err == nil {
		t.Fatal("expected 428 for PUT without If-Match, got nil")
	}

	// Update with a stale If-Match must 412.
	statusCode, _, err := ts.Client.PutRawWithIfMatch("/api/workspaces/team-a", "application/yaml", updateBody, "stale-revision")
	if err != nil {
		t.Fatalf("PUT with stale If-Match transport error: %v", err)
	}
	if statusCode != http.StatusPreconditionFailed {
		t.Fatalf("PUT with stale If-Match: status = %d, want 412", statusCode)
	}

	// Update with the correct If-Match succeeds.
	statusCode, body, err := ts.Client.PutRawWithIfMatch("/api/workspaces/team-a", "application/yaml", updateBody, created.Revision)
	if err != nil {
		t.Fatalf("PUT with correct If-Match transport error: %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("PUT with correct If-Match: status = %d, want 200: %s", statusCode, body)
	}

	var updated api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &updated); err != nil {
		t.Fatalf("show after update: %v", err)
	}
	if !equalStrSlice(updated.Meta.HostCommands, []string{"gh", "aws"}) {
		t.Errorf("updated.Meta.HostCommands = %v, want [gh aws]", updated.Meta.HostCommands)
	}
	if updated.Revision == created.Revision {
		t.Error("expected revision to change after a successful update")
	}

	// Assign a project, then removing the workspace must succeed and
	// re-assign the project to default (docs/plans/workspace-db-consolidation.md
	// decision 8), rather than being blocked by the still-assigned project.
	project := createProject(t, ts, "proj-team-a", "Proj Team A")
	if err := ts.Client.Do("PUT", "/api/projects/"+project.ID+"/workspace", map[string]string{"workspace_id": "team-a"}, &orchestrator.Project{}); err != nil {
		t.Fatalf("assign project to team-a: %v", err)
	}

	if err := ts.Client.Do("DELETE", "/api/workspaces/team-a", nil, nil); err != nil {
		t.Fatalf("remove workspace: %v", err)
	}

	var reassigned orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/"+project.ID, nil, &reassigned); err != nil {
		t.Fatalf("get project after workspace removal: %v", err)
	}
	if reassigned.WorkspaceID != orchestrator.DefaultWorkspaceSlug {
		t.Errorf("project.WorkspaceID after workspace removal = %q, want %q (re-assigned to default)",
			reassigned.WorkspaceID, orchestrator.DefaultWorkspaceSlug)
	}

	// team-a itself is gone now.
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &api.WorkspaceDetail{}); err == nil {
		t.Fatal("expected 404 showing a removed workspace, got nil")
	}
}

// TestUpdateWorkspace_RejectsConcurrentPUTOnSameETag pins MAJOR 1 (codex
// review, docs/plans/workspace-db-consolidation.md): PUT /api/workspaces/{slug}
// used to check the current revision (a separate read) and then Save
// unconditionally (a separate upsert) — two concurrent PUTs starting from
// the same If-Match ETag could both pass that check before either had
// written, so both would succeed and one writer's update would be silently
// lost. The CAS-based fix (a single UPDATE ... WHERE updated_at = ?
// statement) makes this deterministic: of N concurrent PUTs racing from the
// same starting ETag, exactly one must succeed (200) and the rest must be
// rejected (412).
func TestUpdateWorkspace_RejectsConcurrentPUTOnSameETag(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedHostCommands(t, ts, "gh", "writer-0", "writer-1", "writer-2", "writer-3", "writer-4", "writer-5", "writer-6", "writer-7")

	var created api.WorkspaceDetail
	if err := ts.Client.DoWithContentType("POST", "/api/workspaces", "application/yaml",
		[]byte("slug: team-a\nhost_commands:\n  - gh\n"), &created); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	statusCodes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(fmt.Sprintf("host_commands:\n  - writer-%d\n", i))
			code, _, err := ts.Client.PutRawWithIfMatch("/api/workspaces/team-a", "application/yaml", body, created.Revision)
			if err != nil {
				t.Errorf("PUT #%d transport error: %v", i, err)
				return
			}
			statusCodes[i] = code
		}(i)
	}
	wg.Wait()

	okCount, preconditionFailedCount := 0, 0
	for _, code := range statusCodes {
		switch code {
		case http.StatusOK:
			okCount++
		case http.StatusPreconditionFailed:
			preconditionFailedCount++
		default:
			t.Errorf("unexpected status code %d among concurrent PUTs", code)
		}
	}
	if okCount != 1 {
		t.Errorf("okCount = %d, want exactly 1 (only one of %d concurrent PUTs on the same ETag should win)", okCount, n)
	}
	if preconditionFailedCount != n-1 {
		t.Errorf("preconditionFailedCount = %d, want %d", preconditionFailedCount, n-1)
	}
}

// TestUpdateWorkspace_RejectsAfterDeleteBetweenGETAndPUT pins MAJOR 1: a
// GET followed by a DELETE followed by a PUT carrying the GET's (now-stale)
// revision must 404, not silently resurrect the deleted workspace via
// Save's upsert semantics (the pre-fix behavior — Save is INSERT ... ON
// CONFLICT DO UPDATE, so an unconditional Save after a passed revision
// check would recreate the row even though it no longer exists).
func TestUpdateWorkspace_RejectsAfterDeleteBetweenGETAndPUT(t *testing.T) {
	ts := testutil.NewTestServer(t)
	seedHostCommands(t, ts, "gh", "aws")

	var created api.WorkspaceDetail
	if err := ts.Client.DoWithContentType("POST", "/api/workspaces", "application/yaml",
		[]byte("slug: team-a\nhost_commands:\n  - gh\n"), &created); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	var shown api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &shown); err != nil {
		t.Fatalf("get workspace: %v", err)
	}

	if err := ts.Client.Do("DELETE", "/api/workspaces/team-a", nil, nil); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}

	statusCode, _, err := ts.Client.PutRawWithIfMatch("/api/workspaces/team-a", "application/yaml",
		[]byte("host_commands:\n  - aws\n"), shown.Revision)
	if err != nil {
		t.Fatalf("PUT with stale (pre-delete) revision: transport error: %v", err)
	}
	if statusCode != http.StatusNotFound {
		t.Fatalf("PUT after delete: status = %d, want 404 (must not silently resurrect the deleted workspace)", statusCode)
	}

	// The workspace must still be gone — not resurrected by the rejected PUT.
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &api.WorkspaceDetail{}); err == nil {
		t.Fatal("expected 404 showing team-a after the rejected PUT, got nil (it was resurrected)")
	}
}

// TestWorkspaceAPI_RemoveDefaultRejected pins decision 8: the default
// workspace can never be removed via the API.
func TestWorkspaceAPI_RemoveDefaultRejected(t *testing.T) {
	ts := testutil.NewTestServer(t)
	err := ts.Client.Do("DELETE", "/api/workspaces/"+orchestrator.DefaultWorkspaceSlug, nil, nil)
	if err == nil {
		t.Fatal("expected error removing the default workspace, got nil")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("expected 'reserved' in error message, got: %v", err)
	}
}

func equalStrSlice(a, b []string) bool {
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
