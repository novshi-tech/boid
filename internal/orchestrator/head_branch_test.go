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
	}
	got := orchestrator.ComputeHeadBranch(task)
	if got != "boid/abcd1234" {
		t.Errorf("child task should use boid/<id8>, not base_branch; got %q", got)
	}
}

func TestComputeForkPoint_NilParent(t *testing.T) {
	got := orchestrator.ComputeForkPoint(nil)
	if got != "" {
		t.Errorf("nil parent should return empty string, got %q", got)
	}
}

func TestComputeForkPoint_RootParent_ReturnsBaseBranch(t *testing.T) {
	parent := &orchestrator.Task{
		ID:         "root00001234567",
		BaseBranch: "main",
		ParentID:   "",
		Worktree:   true,
	}
	got := orchestrator.ComputeForkPoint(parent)
	if got != "main" {
		t.Errorf("expected %q, got %q", "main", got)
	}
}

func TestComputeForkPoint_ChildParent_Worktree_ReturnsBoidPrefix(t *testing.T) {
	parent := &orchestrator.Task{
		ID:         "abcd1234-0000-0000-0000-000000000000",
		BaseBranch: "main",
		ParentID:   "grandparent-id",
		Worktree:   true,
	}
	got := orchestrator.ComputeForkPoint(parent)
	if got != "boid/abcd1234" {
		t.Errorf("worktree=true child parent should return boid/<id8>, got %q", got)
	}
}

// TestComputeForkPoint_WorktreeLessParent_ReturnsBaseBranch covers the
// Phase 2-2 supervisor case 1 path: a parent task with Worktree=false runs in
// the host project dir on its base_branch and never creates a boid/<id8>
// branch. The child must fork from parent.BaseBranch instead of the
// never-created boid/<id8> — otherwise worktree creation fails with
// `fork point boid/<id8> not found locally (parent task worktree missing?)`.
func TestComputeForkPoint_WorktreeLessParent_ReturnsBaseBranch(t *testing.T) {
	parent := &orchestrator.Task{
		ID:         "abcd1234-0000-0000-0000-000000000000",
		BaseBranch: "feature/BGO-170",
		ParentID:   "grandparent-id",
		Worktree:   false,
	}
	got := orchestrator.ComputeForkPoint(parent)
	if got != "feature/BGO-170" {
		t.Errorf("worktree-less parent should return base_branch, got %q", got)
	}
}
