package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestComputeHeadBranch_RootTask_ReturnsBaseBranch(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "abcd1234-0000-0000-0000-000000000000",
		BaseBranch: "main",
		ParentID:   "",
	}
	got := orchestrator.ComputeHeadBranch(task)
	if got != "main" {
		t.Errorf("expected %q, got %q", "main", got)
	}
}

func TestComputeHeadBranch_RootTask_EmptyBaseBranch(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "abcd1234-0000-0000-0000-000000000000",
		ParentID: "",
	}
	got := orchestrator.ComputeHeadBranch(task)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestComputeHeadBranch_ChildTask_ReturnsBoidPrefix(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "abcd1234-0000-0000-0000-000000000000",
		ParentID: "parent-task-id",
		Worktree: true,
	}
	got := orchestrator.ComputeHeadBranch(task)
	if got != "boid/abcd1234" {
		t.Errorf("expected %q, got %q", "boid/abcd1234", got)
	}
}

func TestComputeHeadBranch_ChildTask_ShortID(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "abc",
		ParentID: "parent-task-id",
		Worktree: true,
	}
	got := orchestrator.ComputeHeadBranch(task)
	if got != "boid/abc" {
		t.Errorf("expected %q, got %q", "boid/abc", got)
	}
}

func TestComputeHeadBranch_ChildTask_IgnoresBaseBranch(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "abcd1234-0000-0000-0000-000000000000",
		BaseBranch: "main",
		ParentID:   "parent-task-id",
		Worktree:   true,
	}
	got := orchestrator.ComputeHeadBranch(task)
	if got != "boid/abcd1234" {
		t.Errorf("child task should use boid/<id8>, not base_branch; got %q", got)
	}
}

// TestComputeHeadBranch_ChildTask_NoWorktree_ReturnsBaseBranch covers the
// Phase 2-2 supervisor case 1 path: a child task with Worktree=false runs in
// the host project dir on its base_branch and does not occupy a boid/<id8>
// branch. Returning base_branch lets the worktree resolver point grandchild
// fork points at an existing branch instead of the never-created boid/<id8>.
func TestComputeHeadBranch_ChildTask_NoWorktree_ReturnsBaseBranch(t *testing.T) {
	task := &orchestrator.Task{
		ID:         "abcd1234-0000-0000-0000-000000000000",
		BaseBranch: "feature/BGO-170",
		ParentID:   "parent-task-id",
		Worktree:   false,
	}
	got := orchestrator.ComputeHeadBranch(task)
	if got != "feature/BGO-170" {
		t.Errorf("worktree-less child should return base_branch, got %q", got)
	}
}
