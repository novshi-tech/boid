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
