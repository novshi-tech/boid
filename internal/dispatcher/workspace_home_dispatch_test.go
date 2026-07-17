package dispatcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file holds the Dispatch-level wiring guards for
// docs/plans/home-workspace-volume.md Phase 4 PR1: proof that
// Runner.Dispatch actually reaches resolveWorkspaceHome (not just that the
// resolver works in isolation — see workspace_home_test.go for that — but
// that the call site wired into Dispatch, and the rtInfo.WorkspaceHomeDir
// field it feeds, are not silently dropped). Matches
// .claude/skills/boid-review's "wiring seam" doctrine: a unit test of the
// inner helper alone would not catch a dropped call site in Dispatch.
//
// Test helpers (setupWorkspaceHomeTestDirs, writeInitScript) and DB/Project
// fixtures (newGatewayTestDB, fakeProjectLookup, gwFakeSandboxPrep,
// gwFakeRuntime) are shared with workspace_home_test.go / gitgateway_wire_test.go
// / runner_test.go — all in this same package.

// TestDispatch_WorkspaceHomeInitFails_MarksJobFailedAndCallsCleanup is the
// Dispatch-level guard for the wiring seam: it proves Runner.Dispatch
// actually reaches resolveWorkspaceHome and, on failure, follows the same
// failJob + cleanup + error-return pattern as every other pre-BuildSandboxSpec
// dispatch error.
func TestDispatch_WorkspaceHomeInitFails_MarksJobFailedAndCallsCleanup(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	r := &Runner{
		DB: d.Conn,
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkDir: "/tmp", WorkspaceID: "Not A Valid Slug!"},
		}},
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	var cleanupCalled bool
	jobID, err := r.Dispatch(context.Background(), spec, func() { cleanupCalled = true })
	if err == nil {
		t.Fatal("expected Dispatch to fail for an invalid workspace slug")
	}
	if jobID != "" {
		t.Errorf("jobID = %q, want empty on failure", jobID)
	}
	if !cleanupCalled {
		t.Error("cleanup callback was not called on resolveWorkspaceHome error")
	}

	jobs, listErr := ListJobsFiltered(d.Conn, JobFilter{Status: string(JobStatusFailed)})
	if listErr != nil {
		t.Fatalf("list failed jobs: %v", listErr)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 failed job, got %d", len(jobs))
	}
	if jobs[0].Output == "" {
		t.Error("failed job Output should contain the resolveWorkspaceHome error message")
	}
}

// TestDispatch_WorkspaceHomeInitSucceeds_ThreadsCorrectSlugThroughDispatch is
// the Dispatch-level guard on the wiring itself, not just resolveWorkspaceHome
// in isolation: it plants an init.sh for the project's *actual* workspace
// slug ("myws", resolved from Projects via the fakeProjectLookup below) and
// asserts the resulting sentinel file lands under the home dir
// resolveWorkspaceHome would independently compute for that same slug. That
// proves Dispatch calls resolveWorkspaceHome with the workspaceID it just
// resolved from the project — not a stale, empty, or wrong slug — before
// reaching BuildSandboxSpec, and that a successful init lets dispatch proceed
// normally to a running job.
func TestDispatch_WorkspaceHomeInitSucceeds_ThreadsCorrectSlugThroughDispatch(t *testing.T) {
	dataDir, configDir := setupWorkspaceHomeTestDirs(t)
	writeInitScript(t, configDir, "myws", "#!/bin/bash\ntouch \"$BOID_WORKSPACE_HOME/sentinel\"\n")

	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	r := &Runner{
		DB: d.Conn,
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkDir: "/tmp", WorkspaceID: "myws"},
		}},
		Sandbox:    &gwFakeSandboxPrep{dir: t.TempDir()},
		Runtime:    &gwFakeRuntime{},
		BoidBinary: "/boid",
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected a non-empty job ID from a successful Dispatch")
	}

	sentinel := filepath.Join(dataDir, "boid", "homes", "myws", "sentinel")
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("init.sh sentinel not found at %s (Dispatch did not run resolveWorkspaceHome for the project's workspace slug %q): %v",
			sentinel, "myws", err)
	}
}

// TestDispatch_ProjectLookupError_FailsLoudBeforeWorkspaceHomeInit is the
// Dispatch-level guard for the resolveProjectRuntime error-handling fix
// (codex review PR #787): when Projects.GetProject fails outright (a torn
// registry / DB read failure, simulated here with erroringProjectLookup —
// shared with gitgateway_wire_test.go's clone-mode guards, same package),
// Dispatch must fail the job and call cleanup via the same
// failJob+cleanup+return "" pattern as every other pre-BuildSandboxSpec
// error path, instead of silently treating the failed lookup as "no
// workspace" (empty workspaceID) and going on to run the *default*
// workspace's init.sh for a project that might belong to a different one.
func TestDispatch_ProjectLookupError_FailsLoudBeforeWorkspaceHomeInit(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	r := &Runner{
		DB:       d.Conn,
		Projects: erroringProjectLookup{err: fmt.Errorf("db read failed")},
	}

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	var cleanupCalled bool
	jobID, err := r.Dispatch(context.Background(), spec, func() { cleanupCalled = true })
	if err == nil {
		t.Fatal("expected Dispatch to fail when Projects.GetProject errors")
	}
	if !strings.Contains(err.Error(), "proj-1") {
		t.Errorf("error = %q, want to name the project id proj-1", err.Error())
	}
	if jobID != "" {
		t.Errorf("jobID = %q, want empty on failure", jobID)
	}
	if !cleanupCalled {
		t.Error("cleanup callback was not called on resolveProjectRuntime error")
	}

	jobs, listErr := ListJobsFiltered(d.Conn, JobFilter{Status: string(JobStatusFailed)})
	if listErr != nil {
		t.Fatalf("list failed jobs: %v", listErr)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 failed job, got %d", len(jobs))
	}
	if jobs[0].Output == "" {
		t.Error("failed job Output should contain the resolveProjectRuntime error message")
	}

	// No workspace home directory should have been created for the default
	// slug (or any slug) — the dispatch must fail before resolveWorkspaceHome
	// ever runs.
	homesDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "boid", "homes")
	if entries, statErr := os.ReadDir(homesDir); statErr == nil && len(entries) != 0 {
		t.Errorf("homesDir %s should be empty (resolveWorkspaceHome must not run after a resolveProjectRuntime error), got %v", homesDir, entries)
	}
}
