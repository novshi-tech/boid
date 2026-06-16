package api

import (
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// recordingWorktreeCleaner captures arguments to CleanupForTask /
// SweepChildBranches for assertions, while behaving as a no-op cleaner.
type recordingWorktreeCleaner struct {
	cleanupCalls []cleanupCall
	sweepCalls   []sweepCall
}

type cleanupCall struct {
	taskID     string
	projectDir string
	newStatus  string
}

type sweepCall struct {
	projectDir string
	taskIDs    []string
}

func (r *recordingWorktreeCleaner) CleanupForTask(taskID, projectDir, newStatus string) error {
	r.cleanupCalls = append(r.cleanupCalls, cleanupCall{taskID, projectDir, newStatus})
	return nil
}

func (r *recordingWorktreeCleaner) SweepChildBranches(projectDir string, taskIDs []string) error {
	dup := make([]string, len(taskIDs))
	copy(dup, taskIDs)
	r.sweepCalls = append(r.sweepCalls, sweepCall{projectDir, dup})
	return nil
}

// childListingTaskStore returns a fixed list of children for a single parent ID
// and otherwise satisfies the TaskStore interface with no-op stubs.
type childListingTaskStore struct {
	stubTaskStore
	parentID string
	children []*orchestrator.Task
}

func (s *childListingTaskStore) ListChildren(parentID string) ([]*orchestrator.Task, error) {
	if parentID == s.parentID {
		return s.children, nil
	}
	return nil, nil
}

// TestFinalizeTerminal_SupervisorSweepsChildBranches verifies that when a
// supervisor task reaches done, finalizeTerminal invokes SweepChildBranches
// with the direct children's task IDs. This is the wiring tested by
// dispatcher-side unit tests for the underlying mechanism.
func TestFinalizeTerminal_SupervisorSweepsChildBranches(t *testing.T) {
	parent := &orchestrator.Task{
		ID:        "parent-id-001",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "supervisor",
	}
	children := []*orchestrator.Task{
		{ID: "child-id-aaa1", ParentID: parent.ID, Status: orchestrator.TaskStatusDone},
		{ID: "child-id-bbb2", ParentID: parent.ID, Status: orchestrator.TaskStatusDone},
	}

	wc := &recordingWorktreeCleaner{}
	svc := &TaskWorkflowService{
		Tasks:    &childListingTaskStore{parentID: parent.ID, children: children},
		Projects: &stubProjectRepository{projects: []*orchestrator.Project{{ID: "proj-1", WorkDir: "/tmp/p1"}}},
		Worktrees: wc,
		Tx:       &stubTx{},
	}

	svc.finalizeTerminal(t.Context(), parent)

	if len(wc.sweepCalls) != 1 {
		t.Fatalf("expected exactly one SweepChildBranches call, got %d", len(wc.sweepCalls))
	}
	got := wc.sweepCalls[0]
	if got.projectDir != "/tmp/p1" {
		t.Errorf("sweep projectDir = %q, want /tmp/p1", got.projectDir)
	}
	want := []string{"child-id-aaa1", "child-id-bbb2"}
	if !reflect.DeepEqual(got.taskIDs, want) {
		t.Errorf("sweep taskIDs = %v, want %v", got.taskIDs, want)
	}
}

// TestFinalizeTerminal_ExecutorSkipsSweep verifies that an executor (leaf
// task — no children) does not invoke SweepChildBranches.
func TestFinalizeTerminal_ExecutorSkipsSweep(t *testing.T) {
	leaf := &orchestrator.Task{
		ID:        "leaf-id-001",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "executor",
	}

	wc := &recordingWorktreeCleaner{}
	svc := &TaskWorkflowService{
		// Note: parentID doesn't match leaf.ID, so ListChildren returns nil.
		Tasks:    &childListingTaskStore{parentID: "other-parent", children: nil},
		Projects: &stubProjectRepository{projects: []*orchestrator.Project{{ID: "proj-1", WorkDir: "/tmp/p1"}}},
		Worktrees: wc,
		Tx:       &stubTx{},
	}

	svc.finalizeTerminal(t.Context(), leaf)

	if len(wc.sweepCalls) != 0 {
		t.Fatalf("executor with no children should not trigger sweep, got %d calls", len(wc.sweepCalls))
	}
}

// TestFinalizeTerminal_NonTerminalSkipsSweep guards the precondition: only
// terminal statuses (done / aborted) trigger any cleanup.
func TestFinalizeTerminal_NonTerminalSkipsSweep(t *testing.T) {
	executing := &orchestrator.Task{
		ID:        "exec-id-001",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "supervisor",
	}
	wc := &recordingWorktreeCleaner{}
	svc := &TaskWorkflowService{
		Tasks:    &childListingTaskStore{parentID: executing.ID, children: []*orchestrator.Task{{ID: "c1"}}},
		Projects: &stubProjectRepository{projects: []*orchestrator.Project{{ID: "proj-1", WorkDir: "/tmp/p1"}}},
		Worktrees: wc,
	}

	svc.finalizeTerminal(t.Context(), executing)

	if len(wc.cleanupCalls) != 0 || len(wc.sweepCalls) != 0 {
		t.Fatalf("non-terminal task should not trigger any cleanup, got cleanup=%d sweep=%d",
			len(wc.cleanupCalls), len(wc.sweepCalls))
	}
}
