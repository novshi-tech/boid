package api

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type TaskWorkflowService struct {
	Tasks       TaskStore
	Jobs        JobStore
	Projects    ProjectRepository
	Tx          Transactor
	Meta        MetaStore
	Coordinator DispatchCoordinator
	Lifecycle   JobLifecycle
	Worktrees   WorktreeCleaner
	Hub         *TaskEventHub
	// Locks pins the branch lock to the executing lifetime of each task.
	// Optional: when nil, no branch locking is performed (matches pre-P0-2
	// behaviour for tests that don't exercise concurrency).
	Locks *orchestrator.BranchLockManager
	// Adapter is the harness adapter used to stop agents and query usage.
	// When nil, StopAgent is a no-op (safe for tests that don't exercise the path).
	Adapter adapters.HarnessAdapter

	dispatchCtx    context.Context
	dispatchCancel context.CancelFunc
	dispatchWG     sync.WaitGroup
}

// InitDispatch initialises the lifecycle context used by dispatch-loop
// goroutines. Must be called before the first action is applied. The returned
// cancel is stored internally; call Shutdown to invoke it.
func (s *TaskWorkflowService) InitDispatch(ctx context.Context) {
	s.dispatchCtx, s.dispatchCancel = context.WithCancel(ctx)
}

// Shutdown cancels the dispatch context and blocks until all in-flight dispatch
// loops have returned. Call this before closing the database.
func (s *TaskWorkflowService) Shutdown() {
	if s.dispatchCancel != nil {
		s.dispatchCancel()
	}
	s.dispatchWG.Wait()
}

// releaseProjectLock drops the executing-lifetime branch lock for the given
// task. Safe to call multiple times; safe when the task never acquired a lock.
func (s *TaskWorkflowService) releaseProjectLock(taskID string) {
	if s.Locks == nil || taskID == "" {
		return
	}
	s.Locks.ReleaseForTask(taskID)
}

// StopAgent delegates to the configured HarnessAdapter to gracefully stop the
// agent backing runtimeID, leaving bash and the EXIT trap alive. No-op when
// runtimeID is empty or no Adapter has been configured.
func (s *TaskWorkflowService) StopAgent(runtimeID string) {
	if runtimeID == "" || s.Adapter == nil {
		return
	}
	go s.Adapter.StopAgent(context.Background(), runtimeID) //nolint:errcheck
}

// enrichJob fills WorkspacePath from RuntimesDir and the job's RuntimeID.
// If either is empty the field is left unchanged (omitempty will omit it in JSON).
func enrichJob(runtimesDir string, job *Job) {
	if runtimesDir == "" || job.RuntimeID == "" {
		return
	}
	job.WorkspacePath = filepath.Join(runtimesDir, job.RuntimeID)
}

// enrichJobDisplayName sets job.DisplayName from the project meta's hook definitions
// when the job is a hook job and DisplayName is not yet set. This resolves the
// display name in-memory from the project meta store (no DB read needed).
func enrichJobDisplayName(job *Job, behavior string, meta MetaStore) {
	if job.DisplayName != "" || job.Role != "hook" || behavior == "" || meta == nil {
		return
	}
	projectMeta, ok := meta.Get(job.ProjectID)
	if !ok {
		return
	}
	tb, ok := projectMeta.TaskBehaviors[behavior]
	if !ok {
		return
	}
	for _, h := range tb.Hooks {
		if h.ID == job.HandlerID && h.Name != "" {
			job.DisplayName = h.Name
			return
		}
	}
}
