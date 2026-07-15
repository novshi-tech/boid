package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestBuildCloneDeclaration_NilTask_ReturnsNil(t *testing.T) {
	if got := orchestrator.BuildCloneDeclaration(nil, ""); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestBuildCloneDeclaration_RootTask_ChecksOutBaseBranch(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "abcd1234-0000-0000-0000-000000000000",
		BaseBranch: "main",
		ParentID:   "",
	}
	got := orchestrator.BuildCloneDeclaration(task, "")
	want := &orchestrator.CloneDeclaration{
		Branch:       "main",
		BaseBranch:   "main",
		CheckoutOnly: true,
	}
	if *got != *want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestBuildCloneDeclaration_ChildTask_ChecksOutBaseBranch pins the core
// branch-policy-simplification change: a child task no longer gets an
// isolated "boid/<id8>" branch forked from the parent's HEAD. It checks out
// its own BaseBranch directly, exactly like a root task, regardless of
// Worktree — the per-task branch and fork-point concepts are retired.
func TestBuildCloneDeclaration_ChildTask_ChecksOutBaseBranch(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "childtask-0000-0000-0000-000000000000",
		BaseBranch: "feature/BGO-170",
		ParentID:   "parent-task-id",
		Worktree:   true,
	}
	got := orchestrator.BuildCloneDeclaration(task, "")
	want := &orchestrator.CloneDeclaration{
		Branch:       "feature/BGO-170",
		BaseBranch:   "feature/BGO-170",
		CheckoutOnly: true,
	}
	if *got != *want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestBuildCloneDeclaration_ChildTask_WorktreeFalse_ChecksOutBaseBranch(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "childtask-0000-0000-0000-000000000000",
		BaseBranch: "feature/BGO-170",
		ParentID:   "parent-task-id",
		Worktree:   false,
	}
	got := orchestrator.BuildCloneDeclaration(task, "")
	want := &orchestrator.CloneDeclaration{
		Branch:       "feature/BGO-170",
		BaseBranch:   "feature/BGO-170",
		CheckoutOnly: true,
	}
	if *got != *want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestBuildCloneDeclaration_PropagatesBaseBranchForkPoint pins that
// BaseBranchForkPoint (the ClassifyBaseBranch case-3 start point, unrelated
// to the retired per-task fork point) still flows through unchanged.
func TestBuildCloneDeclaration_PropagatesBaseBranchForkPoint(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "roottask-0000-0000-0000-000000000000",
		BaseBranch: "release/1.0",
	}
	got := orchestrator.BuildCloneDeclaration(task, "main")
	if got.BaseBranchForkPoint != "main" {
		t.Errorf("BaseBranchForkPoint = %q, want %q", got.BaseBranchForkPoint, "main")
	}
	if got.Branch != "release/1.0" || got.BaseBranch != "release/1.0" || !got.CheckoutOnly {
		t.Errorf("unexpected declaration: %+v", got)
	}
}
