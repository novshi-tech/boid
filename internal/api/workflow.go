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
	Hub         *TaskEventHub
	// TaskTriage provides non-transactional (pre-tx-open) read access to the
	// task_triage sidecar for TaskWorkflowService.Dispatch's pre-check /
	// children read (docs/plans/cross-project-issue-triage.md Phase 1 PR-2).
	// TxStore already embeds CardStore for the in-transaction writes
	// Dispatch and applyParkSideEffect need — this field is only for the read
	// that has to happen BEFORE opening a transaction (see Dispatch's own doc
	// comment for why child-task creation can't run nested inside one). Nil
	// is tolerated (treated as "no children"); wire.go always sets this.
	TaskTriage CardStore
	// Actions provides the workspace-scoped BoidOpActionList read (docs/plans/
	// ingestion-identity.md PR-3, B-3) — same "narrower interface over the
	// same underlying taskRepo value" pattern as TaskTriage above. Nil is
	// tolerated (ListActions returns an "unavailable" error), matching every
	// other optional dependency's convention in this file.
	Actions ActionListStore
	// TaskCreator creates the real boid tasks Dispatch task-ifies specced
	// children into. Nil is tolerated as long as there are no specced
	// children to dispatch — see Dispatch's own doc comment.
	TaskCreator TaskCreator
	// Adapter is the harness adapter used to query post-run usage. Phase 3-b
	// dropped the StopAgent role: graceful stop is delivered as a SIGUSR1
	// directly via Lifecycle.SignalJobRuntime, which claude.Adapter.Run()'s
	// signal.Notify handler intercepts. Adapter remains optional; when nil,
	// usage / future per-harness queries become no-ops.
	Adapter adapters.HarnessAdapter
	// Notifier fires queue の決定論的評価 節 rule 4 (notify) whenever a
	// suggestion attaches to a card — see notifySuggestionArrived in
	// queue_notify.go (PR-2, docs/plans/suggestion-as-state-transition-impl.md
	// §4.2, replacing v1's urgency=now-gated notifyQueueEntryIfUrgent /
	// notifyUrgencyRaised entirely). Nil is tolerated (no-op), same as
	// TaskAppService.Notify's contract.
	Notifier Notifier
	// TaskWaits is the shared registry recording which task each in-flight
	// `boid task wait` is parked on (see TaskWaitRegistry). SweepTriggers reads
	// it to know WHAT to end when a round outruns its trigger's `timeout` — the
	// task doing the work, not just the job that launched it. Nil is tolerated
	// (a timed-out round then only has its job stopped), same convention as
	// every other optional dependency here.
	TaskWaits *TaskWaitRegistry
	// timeoutMu guards timingOut and pendingTimeouts, both touched by the
	// sweep goroutine AND by the goroutines beginTimeOutTriggerRun spawns.
	timeoutMu sync.Mutex
	// timingOut is the set of trigger_runs ids a timeout goroutine currently
	// holds, so a tick that fires while a slow abort is still running does not
	// spawn a second one for the same run.
	timingOut map[string]struct{}
	// pendingTimeouts carries completions recorded off the sweep goroutine
	// until the next sweep can report them (see stashTimeoutCompletion).
	pendingTimeouts []TriggerCompletionResult
	// Triggers backs docs/plans/ingestion-identity.md PR-4 (B-5)'s
	// trigger_runs single-flight/execution-record read+write
	// (SweepTriggers/RunTriggerNow, trigger_loop.go). Nil is tolerated —
	// SweepTriggers/RunTriggerNow no-op (matching Notifier/TaskCreator's own
	// convention above) rather than panicking, since a daemon built without
	// this dependency wired should simply run no triggers, not crash.
	Triggers TriggerRunStore
	// Exec dispatches the exec job (api.ExecDispatcher.StartExec) a due
	// trigger's `run` command executes as (PR-4's "実行" section — daemon
	// starts an exec job through the SAME Runner.Dispatch() path `boid exec`
	// uses). Nil is tolerated the same way Triggers above is: wire.go can
	// only assign this once sessionDispatcherAdapter exists (mountRoutes,
	// after buildRuntime constructs this TaskWorkflowService), so a
	// construction-order gap must not be fatal — see wire.go's own comment
	// at the assignment site.
	Exec ExecDispatcher
	// Signals backs the `on: signals` trigger predicate's HasPendingSignals
	// read (docs/plans/signal-ingest-detailed-design.md §4.2, PR-6,
	// SweepTriggers/signalsPendingForTrigger, trigger_loop.go). Nil is
	// tolerated the same way Triggers/Exec above are: an on:signals trigger
	// simply never becomes due (fail-closed, not a panic) when no
	// SignalStore is wired — a plain schedule (on=""/on:schedule) trigger is
	// entirely unaffected either way.
	Signals SignalStore

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

// taskBehaviorOrEmpty reads task.Exec.Behavior, or "" for a card
// (card-model-cleanup PR-2: Behavior is execution-only, design doc §3.2).
// enrichJobDisplayName already treats an empty behavior as "nothing to
// enrich" (a card has no hook jobs to enrich display names for in the first
// place), so this is a safe, no-special-casing-needed default.
func taskBehaviorOrEmpty(task *orchestrator.Task) string {
	if task == nil || task.Exec == nil {
		return ""
	}
	return task.Exec.Behavior
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
