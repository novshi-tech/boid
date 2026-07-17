package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file holds the Dispatch-level wiring guards for docs/plans/
// home-workspace-volume.md Phase 4 PR3's embedded-skills-sync half: proof
// that Runner.Dispatch calls skills.DeployAll right after resolving the
// workspace home directory, and that a sync failure fails the dispatch the
// same way an init.sh failure does (failJob + cleanup + error return). Test
// helpers (setupWorkspaceHomeTestDirs, fakeProjectLookup, gwFakeSandboxPrep,
// gwFakeRuntime, newGatewayTestDB) are shared with workspace_home_test.go /
// workspace_home_dispatch_test.go / gitgateway_wire_test.go — all in this
// same package.

// TestDispatch_SkillsSyncSucceeds_DeploysEmbeddedSkillsToWorkspaceHome proves
// a successful Dispatch leaves the embedded skill set synced under the
// resolved workspace home's ~/.claude/skills/ — the copy-based replacement
// for the bind-mounts claude.Adapter.Bindings used to declare per skill
// (retired this same PR; see internal/adapters/claude/bindings.go).
func TestDispatch_SkillsSyncSucceeds_DeploysEmbeddedSkillsToWorkspaceHome(t *testing.T) {
	dataDir, _ := setupWorkspaceHomeTestDirs(t)

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

	skillFile := filepath.Join(dataDir, "boid", "homes", "myws", ".claude", "skills", "boid-task", "SKILL.md")
	content, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatalf("read synced skill file at %s: %v", skillFile, err)
	}
	if !strings.Contains(string(content), "boid-task") {
		t.Errorf("synced SKILL.md missing expected content: %s", content)
	}
}

// TestDispatch_SkillsSyncFails_MarksJobFailedAndCallsCleanup is the
// Dispatch-level guard for the sync error path: it forces skills.DeployAll
// to fail deterministically (pre-creating a plain *file* at
// <home>/.claude so DeployAll's os.MkdirAll for a skill directory underneath
// it hits ENOTDIR) and asserts Dispatch follows the same failJob + cleanup +
// error-return pattern as every other pre-BuildSandboxSpec dispatch error
// (e.g. an init.sh failure — see TestDispatch_WorkspaceHomeInitFails_MarksJobFailedAndCallsCleanup
// in workspace_home_dispatch_test.go).
func TestDispatch_SkillsSyncFails_MarksJobFailedAndCallsCleanup(t *testing.T) {
	dataDir, _ := setupWorkspaceHomeTestDirs(t)

	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	homeDir := filepath.Join(dataDir, "boid", "homes", orchestrator.DefaultWorkspaceSlug)
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatalf("mkdir home dir: %v", err)
	}
	// A plain file at .claude blocks DeployAll from mkdir'ing
	// .claude/skills/<name> underneath it.
	if err := os.WriteFile(filepath.Join(homeDir, ".claude"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	r := &Runner{
		DB: d.Conn,
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkDir: "/tmp"},
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
		t.Fatal("expected Dispatch to fail when embedded skill sync errors")
	}
	if jobID != "" {
		t.Errorf("jobID = %q, want empty on failure", jobID)
	}
	if !cleanupCalled {
		t.Error("cleanup callback was not called on skills sync error")
	}

	jobs, listErr := ListJobsFiltered(d.Conn, JobFilter{Status: string(JobStatusFailed)})
	if listErr != nil {
		t.Fatalf("list failed jobs: %v", listErr)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 failed job, got %d", len(jobs))
	}
	if jobs[0].Output == "" {
		t.Error("failed job Output should contain the skills sync error message")
	}
}
