package api

import (
	"context"
	"path/filepath"
	"sync"
	"syscall"

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
	// Adapter is the harness adapter used to query post-run usage. Phase 3-b
	// dropped the StopAgent role: graceful stop is delivered as a SIGUSR1
	// directly via Lifecycle.SignalJobRuntime, which claude.Adapter.Run()'s
	// signal.Notify handler intercepts. Adapter remains optional; when nil,
	// usage / future per-harness queries become no-ops.
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

// StopAgent gracefully stops the agent backing runtimeID by delivering
// SIGUSR1 to its process group. claude.Adapter.Run()'s signal.Notify(SIGUSR1)
// handler forwards a SIGTERM to the claude child and returns
// Result.StoppedByDaemon=true, so the surrounding sandbox runtime survives
// long enough to post `boid job done` through the broker normally. No-op
// when runtimeID is empty or no JobLifecycle has been configured.
func (s *TaskWorkflowService) StopAgent(runtimeID string) {
	if runtimeID == "" || s.Lifecycle == nil {
		return
	}
	go s.Lifecycle.SignalJobRuntime(runtimeID, syscall.SIGUSR1)
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
