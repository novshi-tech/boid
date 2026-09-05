package api

// Trigger execution: scheduling, single-flight, and result recording for
// project.yaml `triggers[]`. TriggerLoop (below) mirrors QueueSweepLoop
// (queue_sweep.go): Run(ctx) ticks and runOnce delegates to SweepTriggers,
// TaskWorkflowService's per-tick work.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TriggerKey identifies one trigger within one project — the single-flight
// and elapsed-time scoping unit.
type TriggerKey struct {
	ProjectID   string
	TriggerName string
}

// TriggerFireResult records one trigger a sweep actually dispatched.
type TriggerFireResult struct {
	TriggerKey
	JobID string
}

// TriggerCompletionResult records one trigger_runs row a sweep reconciled
// (its job reached a terminal status this tick).
type TriggerCompletionResult struct {
	TriggerKey
	ExitCode int
}

// TriggerSkip records one trigger that was due this tick but single-flight
// blocked it.
type TriggerSkip struct {
	TriggerKey
	// Since is the blocking run's StartedAt.
	Since time.Time
	// Every is trigger.Every, parsed; zero only if the blocking run's own
	// trigger definition failed to parse (should not happen —
	// ValidateTriggers rejects that at load time).
	Every time.Duration
	// Timeout is the trigger's declared round bound, zero if undeclared.
	// trackSkipStreak prefers it over guessing from Every.
	Timeout time.Duration
}

// TriggerSweepResult is SweepTriggers' return shape, used by TriggerLoop for
// stuck/failure-streak bookkeeping and by tests to pin behavior.
type TriggerSweepResult struct {
	// Fired lists triggers this sweep actually dispatched.
	Fired []TriggerFireResult
	// Skipped lists triggers that were due but single-flight blocked them
	// this tick — not a trigger that simply hasn't reached its next `every`.
	Skipped []TriggerSkip
	// Completed lists trigger_runs rows this sweep newly marked finished.
	Completed []TriggerCompletionResult
}

// hydrateMetaForTriggers resolves projectID's ProjectMeta, preferring the
// workspace-hydrated view and falling back to the bare in-memory cache.
// Returns nil when neither succeeds ("no triggers for this project").
func (s *TaskWorkflowService) hydrateMetaForTriggers(ctx context.Context, projectID string) *orchestrator.ProjectMeta {
	if s.Meta == nil {
		return nil
	}
	if hydrated, err := s.Meta.GetWithWorkspace(ctx, projectID); err == nil && hydrated != nil {
		return hydrated
	}
	if m, ok := s.Meta.Get(projectID); ok {
		return m
	}
	return nil
}

// jobTerminalState reports whether jobID has reached a terminal status
// (JobStatusCompleted/JobStatusFailed) and, if so, its exit code. Any error
// from s.Jobs.GetJob is propagated as-is — reconcileInFlight decides how
// long to keep retrying via selfHealStaleTriggerRun.
func (s *TaskWorkflowService) jobTerminalState(jobID string) (exitCode int, terminal bool, err error) {
	if s.Jobs == nil {
		return 0, false, fmt.Errorf("job store not configured")
	}
	job, err := s.Jobs.GetJob(jobID)
	if err != nil {
		return 0, false, err
	}
	if job.Status == JobStatusRunning {
		return 0, false, nil
	}
	return job.ExitCode, true, nil
}

// TriggerRunSelfHealGrace bounds how long a trigger_runs row may sit
// in-flight without resolving to a real job's terminal state before
// reconcileInFlight force-closes it — otherwise a vanished job row (e.g.
// swept by GC) would wedge single-flight for that (project, trigger)
// forever. Expressed as wall-clock time rather than a tick count because
// reconcileInFlight also runs from RunTriggerNow's own goroutine.
const TriggerRunSelfHealGrace = 3 * time.Minute

// TriggerRunSelfHealExitCode is the sentinel CompleteTriggerRun records for
// a self-healed row — counts toward trackFailStreak like any other non-zero
// exit code. Outside the real 0-255 exit code range so it reads
// unambiguously in logs/notifications.
const TriggerRunSelfHealExitCode = -1

// TriggerDispatchFailureExitCode is the sentinel SweepTriggers reports in
// TriggerSweepResult.Completed for a StartExec (dispatch) failure —
// distinct from TriggerRunSelfHealExitCode so the two failure classes stay
// distinguishable (self-heal = a job was dispatched but its outcome was
// never observed; this = the job was never dispatched at all).
const TriggerDispatchFailureExitCode = -2

// TriggerTimeoutExitCode is the sentinel SweepTriggers records for a run
// ended by its trigger's own `timeout` — distinct from the other two
// (self-heal = dispatched but unobserved; dispatch failure = never
// dispatched; this = ran too long).
const TriggerTimeoutExitCode = -3

// selfHealStaleTriggerRun force-closes run if it has been in flight, unable
// to resolve to a real job's terminal state, for at least
// TriggerRunSelfHealGrace. Returns true when it closed the row (the caller
// must treat it as no-longer-in-flight); false when the grace period has
// not elapsed yet.
func (s *TaskWorkflowService) selfHealStaleTriggerRun(now time.Time, run *orchestrator.TriggerRun, completed *[]TriggerCompletionResult) bool {
	if now.Sub(run.StartedAt) < TriggerRunSelfHealGrace {
		return false
	}
	if err := s.Triggers.CompleteTriggerRun(run.ID, now, TriggerRunSelfHealExitCode); err != nil {
		if errors.Is(err, orchestrator.ErrTriggerRunAlreadyFinished) {
			// Closed by the timeout path first — report healed, no completion.
			slog.Debug("trigger reconcile: the stale run was already closed by the timeout path", "run_id", run.ID)
			return true
		}
		slog.Warn("trigger reconcile: self-heal close failed; leaving run in-flight", "run_id", run.ID, "error", err)
		return false
	}
	slog.Warn("trigger reconcile: self-healed a stuck in-flight run (job row unreachable/unrecorded past the grace period)",
		"run_id", run.ID, "project_id", run.ProjectID, "trigger", run.TriggerName, "job_id", run.JobID, "grace", TriggerRunSelfHealGrace)
	if completed != nil {
		*completed = append(*completed, TriggerCompletionResult{
			TriggerKey: TriggerKey{ProjectID: run.ProjectID, TriggerName: run.TriggerName},
			ExitCode:   TriggerRunSelfHealExitCode,
		})
	}
	return true
}

// reconcileInFlight re-checks every trigger_runs row with finished_at IS
// NULL against its job's current status, completing any whose job has
// reached a terminal status, and returns the set still genuinely in flight
// — the single-flight membership test both SweepTriggers and RunTriggerNow
// use. completed, when non-nil, receives one entry per row newly completed.
func (s *TaskWorkflowService) reconcileInFlight(now time.Time, completed *[]TriggerCompletionResult) (map[TriggerKey]*orchestrator.TriggerRun, error) {
	runs, err := s.Triggers.ListInFlightTriggerRuns()
	if err != nil {
		return nil, fmt.Errorf("list in-flight trigger runs: %w", err)
	}
	stillInFlight := make(map[TriggerKey]*orchestrator.TriggerRun, len(runs))
	for _, run := range runs {
		key := TriggerKey{ProjectID: run.ProjectID, TriggerName: run.TriggerName}

		// A row with JobID=="" is either mid-dispatch (SetTriggerRunJobID
		// hasn't landed yet) or that update itself failed — both look
		// identical from here, and selfHealStaleTriggerRun's grace period is
		// what tells them apart from "stuck forever".
		if run.JobID == "" {
			if s.selfHealStaleTriggerRun(now, run, completed) {
				continue
			}
			stillInFlight[key] = run
			continue
		}

		exitCode, terminal, jobErr := s.jobTerminalState(run.JobID)
		if jobErr != nil {
			if s.selfHealStaleTriggerRun(now, run, completed) {
				continue
			}
			slog.Warn("trigger reconcile: get job failed; leaving run in-flight", "run_id", run.ID, "job_id", run.JobID, "error", jobErr)
			stillInFlight[key] = run
			continue
		}
		if !terminal {
			stillInFlight[key] = run
			continue
		}
		if cerr := s.Triggers.CompleteTriggerRun(run.ID, now, exitCode); cerr != nil {
			if errors.Is(cerr, orchestrator.ErrTriggerRunAlreadyFinished) {
				// The timeout goroutine closed it first; stay quiet so this
				// completion isn't reported twice.
				slog.Debug("trigger reconcile: the run was already closed by the timeout path",
					"run_id", run.ID, "job_id", run.JobID)
				continue
			}
			slog.Warn("trigger reconcile: complete trigger run failed; leaving run in-flight", "run_id", run.ID, "error", cerr)
			stillInFlight[key] = run
			continue
		}
		if completed != nil {
			*completed = append(*completed, TriggerCompletionResult{TriggerKey: key, ExitCode: exitCode})
		}
	}
	return stillInFlight, nil
}

// beginTimeOutTriggerRun ends a round that outlived its trigger's `timeout`,
// off the sweep goroutine — Lifecycle.StopJobRuntime runs synchronously
// under its own deadline, and TriggerLoop.runOnce is a single goroutine
// covering every project, so doing this inline would let one wedged engine
// stall every trigger from being evaluated at all.
func (s *TaskWorkflowService) beginTimeOutTriggerRun(
	ctx context.Context,
	key TriggerKey,
	run *orchestrator.TriggerRun,
	timeout time.Duration,
) {
	if !s.claimTimeout(run.ID) {
		// Already being ended by a goroutine this or an earlier tick spawned.
		return
	}
	runID, jobID, startedAt := run.ID, run.JobID, run.StartedAt
	go func() {
		defer s.releaseTimeout(runID)
		s.finishTimeOutTriggerRun(ctx, key, runID, jobID, startedAt, timeout)
	}()
}

// finishTimeOutTriggerRun is beginTimeOutTriggerRun's body, off the sweep
// goroutine.
//
// Order is load-bearing: the task is ended first because it is the work —
// stopping only the job would leave the task running, release
// single-flight, and let the next tick start a second concurrent round.
// Stopping the job afterwards is belt-and-braces for a task that would not
// otherwise notice the abort.
func (s *TaskWorkflowService) finishTimeOutTriggerRun(
	ctx context.Context,
	key TriggerKey,
	runID, jobID string,
	startedAt time.Time,
	timeout time.Duration,
) {
	now := time.Now().UTC()
	overrun := now.Sub(startedAt)
	slog.Warn("trigger sweep: round exceeded its timeout; ending it",
		"project_id", key.ProjectID, "trigger", key.TriggerName,
		"run_id", runID, "job_id", jobID, "timeout", timeout, "ran_for", overrun.Round(time.Second))

	if s.TaskWaits == nil {
		slog.Warn("trigger sweep: no task-wait registry wired; a timed-out round can only stop its job, not the task it started",
			"project_id", key.ProjectID, "trigger", key.TriggerName)
	}
	if taskID, ok := s.TaskWaits.TaskFor(jobID); ok {
		if _, err := s.applyAction(ctx, taskID, ApplyActionRequest{
			Type: "abort",
			Payload: timeoutAbortPayload(fmt.Sprintf(
				"trigger %s/%s: この巡は timeout (%s) を超えたので打ち切りました (実行時間 %s)",
				key.ProjectID, key.TriggerName, timeout, overrun.Round(time.Second))),
		}, applyActionOptions{actionPayloadOnly: true}); err != nil {
			if isPermanentAbortRefusal(err) {
				// The task cannot be aborted and never will be (e.g. a card
				// has no `abort` rule) — retrying would wedge the trigger
				// forever, so fall through: stop the job and close the run.
				slog.Warn("trigger sweep: the task the timed-out round was waiting on refuses abort; ending the round without it",
					"project_id", key.ProjectID, "trigger", key.TriggerName, "task_id", taskID, "error", err)
			} else {
				// Transient — leave the run in flight and let the next tick retry.
				slog.Warn("trigger sweep: could not abort the task the timed-out round was waiting on; retrying next tick",
					"project_id", key.ProjectID, "trigger", key.TriggerName, "task_id", taskID, "error", err)
				return
			}
		}
	}

	if s.Lifecycle != nil && jobID != "" {
		if job, err := s.Jobs.GetJob(jobID); err == nil && job != nil && job.RuntimeID != "" {
			s.Lifecycle.StopJobRuntime(job.RuntimeID)
		}
	}

	if err := s.Triggers.CompleteTriggerRun(runID, now, TriggerTimeoutExitCode); err != nil {
		if errors.Is(err, orchestrator.ErrTriggerRunAlreadyFinished) {
			// reconcileInFlight got there first and already reported the
			// completion with the job's real exit code.
			slog.Info("trigger sweep: the timed-out run was already closed by the reconcile pass",
				"project_id", key.ProjectID, "trigger", key.TriggerName, "run_id", runID)
			return
		}
		// Leave it in flight: the next tick sees the same overrun and retries.
		slog.Warn("trigger sweep: could not close the timed-out run; retrying next tick",
			"project_id", key.ProjectID, "trigger", key.TriggerName, "run_id", runID, "error", err)
		return
	}
	s.stashTimeoutCompletion(TriggerCompletionResult{TriggerKey: key, ExitCode: TriggerTimeoutExitCode})
}

// isPermanentAbortRefusal reports whether an ApplyAction error means the task
// will never accept an abort, as opposed to a transient failure worth
// retrying: a 4xx means the request itself is wrong; anything else is
// treated as transient, since wrongly retrying costs a tick while wrongly
// giving up leaves a task running with single-flight released.
func isPermanentAbortRefusal(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 400 && se.Code < 500
	}
	return false
}

// claimTimeout marks runID as being ended, returning false when a goroutine
// already holds it.
func (s *TaskWorkflowService) claimTimeout(runID string) bool {
	s.timeoutMu.Lock()
	defer s.timeoutMu.Unlock()
	if s.timingOut == nil {
		s.timingOut = make(map[string]struct{})
	}
	if _, held := s.timingOut[runID]; held {
		return false
	}
	s.timingOut[runID] = struct{}{}
	return true
}

// timeoutHeld reports whether a timeout goroutine currently holds runID.
// Only tests use it.
func (s *TaskWorkflowService) timeoutHeld(runID string) (struct{}, bool) {
	s.timeoutMu.Lock()
	defer s.timeoutMu.Unlock()
	v, ok := s.timingOut[runID]
	return v, ok
}

func (s *TaskWorkflowService) releaseTimeout(runID string) {
	s.timeoutMu.Lock()
	defer s.timeoutMu.Unlock()
	delete(s.timingOut, runID)
}

// stashTimeoutCompletion parks a completion recorded off the sweep goroutine
// so the next sweep can report it.
func (s *TaskWorkflowService) stashTimeoutCompletion(c TriggerCompletionResult) {
	s.timeoutMu.Lock()
	defer s.timeoutMu.Unlock()
	s.pendingTimeouts = append(s.pendingTimeouts, c)
}

// drainTimeoutCompletions hands the sweep every completion recorded
// off-thread since the last tick.
func (s *TaskWorkflowService) drainTimeoutCompletions() []TriggerCompletionResult {
	s.timeoutMu.Lock()
	defer s.timeoutMu.Unlock()
	drained := s.pendingTimeouts
	s.pendingTimeouts = nil
	return drained
}

// timeoutAbortPayload renders the abort action payload a timed-out round
// records, matching abortOnDispatchError's {code, message} shape. Passed
// with applyActionOptions{actionPayloadOnly: true} so it lands only on the
// action, not merged into the task's own payload.
func timeoutAbortPayload(message string) json.RawMessage {
	payload, err := json.Marshal(map[string]string{"code": "trigger_timeout", "message": message})
	if err != nil {
		// Marshalling two string fields cannot fail; fall back to the code
		// alone rather than dropping the abort.
		return json.RawMessage(`{"code":"trigger_timeout"}`)
	}
	return payload
}

// triggerIsDue reports whether every has elapsed since key's last recorded
// start — true unconditionally when the trigger has never run.
func (s *TaskWorkflowService) triggerIsDue(now time.Time, key TriggerKey, every time.Duration) (bool, error) {
	latest, err := s.Triggers.LatestTriggerRun(key.ProjectID, key.TriggerName)
	if errors.Is(err, orchestrator.ErrTriggerRunNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return now.Sub(latest.StartedAt) >= every, nil
}

// signalsPendingForTrigger evaluates the extra `on: signals` half of the due
// predicate:
//
//	due := now - latestRun.StartedAt >= every        # triggerIsDue, unchanged
//	if trig.On == "signals":
//	    due = due && HasPendingSignals(workspaceOf(project))
//
// meta is the same hydrated *orchestrator.ProjectMeta the caller already
// read off hydrateMetaForTriggers, so this issues no extra query to
// re-resolve the workspace. A project with no linked workspace, no
// SignalStore wired, or a HasPendingSignals error all resolve to "not due"
// rather than erroring the trigger's evaluation.
func (s *TaskWorkflowService) signalsPendingForTrigger(meta *orchestrator.ProjectMeta, key TriggerKey) bool {
	if meta.SecretNamespace == "" {
		slog.Debug("trigger sweep: on:signals trigger's project has no linked workspace; never due", "project_id", key.ProjectID, "trigger", key.TriggerName)
		return false
	}
	if s.Signals == nil {
		slog.Debug("trigger sweep: on:signals trigger evaluated but no SignalStore is wired; never due", "project_id", key.ProjectID, "trigger", key.TriggerName)
		return false
	}
	pending, err := s.Signals.HasPendingSignals(meta.SecretNamespace, orchestrator.MaxSignalAttempts)
	if err != nil {
		slog.Warn("trigger sweep: on:signals HasPendingSignals failed; treating as not due", "project_id", key.ProjectID, "trigger", key.TriggerName, "workspace_id", meta.SecretNamespace, "error", err)
		return false
	}
	return pending
}

// triggerRunArgv wraps run the way every trigger-fired exec job's Argv is
// built: `sh -c` plus an `exec 0</dev/null;` prefix that closes the shell's
// own stdin first. Needed because the container backend always attaches
// stdin, and a daemon-originated job like this one has no attach client to
// ever close it — without the prefix the container's stdin pipe (and the
// command reading it) never sees EOF.
func triggerRunArgv(run string) []string {
	return []string{"sh", "-c", "exec 0</dev/null; " + run}
}

// fireTrigger dispatches trig's `run` command as a readonly exec job and
// records the resulting trigger_runs row. Shared by SweepTriggers' per-
// trigger loop and RunTriggerNow — the single place both callers hand off
// to daemon's mechanical part (claim + dispatch + record) after their own
// single-flight/elapsed gating differs.
//
// The trigger_runs row is created FIRST, claiming single-flight via a
// unique index, BEFORE StartExec is ever called — this closes the race
// window StartExec's real wall-clock latency would otherwise leave open for
// two concurrent callers to both see "nothing in flight" and both dispatch.
// Whichever INSERT lands first wins; the loser gets
// orchestrator.ErrTriggerRunInFlight back from CreateTriggerRun and never
// calls StartExec.
//
// A CreateTriggerRun success has no job_id yet; SetTriggerRunJobID fills it
// in once dispatch succeeds. If StartExec itself fails, the claimed row is
// deleted (not closed — see DeleteTriggerRun's own doc comment) so the
// existing fail-open "retry next tick" behavior isn't delayed a full `every`.
func (s *TaskWorkflowService) fireTrigger(ctx context.Context, now time.Time, projectID string, trig orchestrator.Trigger) (jobID string, err error) {
	if s.Exec == nil {
		return "", fmt.Errorf("exec dispatcher not configured")
	}
	run := &orchestrator.TriggerRun{ProjectID: projectID, TriggerName: trig.Name, StartedAt: now}
	if err := s.Triggers.CreateTriggerRun(run); err != nil {
		return "", err
	}

	req := StartExecRequest{
		ProjectID:   projectID,
		Argv:        triggerRunArgv(trig.Run),
		Readonly:    true,
		DisplayName: "trigger:" + trig.Name,
	}
	// A hydrate-derived connector trigger's raw connector declaration is
	// copied through verbatim — no Pack-registry resolution happens here
	// (internal/api must not import internal/integrationpack).
	// sessionDispatcherAdapter.StartExec (internal/server/wire.go) resolves
	// it into the connector job's actual env/bind/policy/service-allowlist.
	// nil for every ordinary trigger.
	if trig.Connector != nil {
		req.Connector = &ConnectorRef{
			Pack:          trig.Connector.Pack,
			ConnectorName: trig.Connector.ConnectorName,
			Service:       trig.Connector.Service,
			Config:        trig.Connector.Config,
		}
	}
	result, err := s.Exec.StartExec(ctx, req)
	if err != nil {
		if derr := s.Triggers.DeleteTriggerRun(run.ID); derr != nil {
			// Both the dispatch AND its own cleanup failed — the claimed
			// row is stuck in flight until TriggerRunSelfHealGrace force-closes it.
			slog.Warn("trigger fire: dispatch failed AND cleanup of the claimed row also failed; single-flight for this (project,trigger) will wedge until self-heal", "run_id", run.ID, "project_id", projectID, "trigger", trig.Name, "dispatch_error", err, "cleanup_error", derr)
		}
		return "", fmt.Errorf("start exec: %w", err)
	}

	if serr := s.Triggers.SetTriggerRunJobID(run.ID, result.JobID); serr != nil {
		// The container IS running but this row never got its job_id;
		// reconcileInFlight's self-heal will eventually force-close it.
		slog.Warn("trigger fire: job dispatched but recording its job_id failed; self-heal will close this row after the grace period", "run_id", run.ID, "job_id", result.JobID, "error", serr)
	}
	return result.JobID, nil
}

// SweepTriggers is TriggerLoop's per-tick work: reconcile in-flight runs,
// then evaluate every project's triggers[] and fire whichever are due and
// not single-flight-blocked. Tolerant of partial failures — one project's
// or trigger's error is logged and does not abort the sweep for any other.
//
// Nil Triggers/Projects/Meta/Jobs makes this a no-op: a daemon built
// without trigger support wired in simply runs no triggers. s.Exec is
// deliberately not part of this guard — its absence is already handled
// per-trigger by the fail-open dispatch-failure path below, and folding it
// in here would also skip reconcileInFlight, which stays useful (closing
// out already-in-flight rows) even when nothing new can be dispatched.
func (s *TaskWorkflowService) SweepTriggers(ctx context.Context, now time.Time) (TriggerSweepResult, error) {
	var result TriggerSweepResult
	if s.Triggers == nil || s.Projects == nil || s.Meta == nil || s.Jobs == nil {
		return result, nil
	}
	ctx = orchestrator.WithActor(ctx, orchestrator.ActorDaemon)

	inFlight, err := s.reconcileInFlight(now, &result.Completed)
	if err != nil {
		return result, fmt.Errorf("sweep triggers: %w", err)
	}

	projects, err := s.Projects.ListProjects()
	if err != nil {
		return result, fmt.Errorf("sweep triggers: list projects: %w", err)
	}

	// Completions recorded off-thread by a previous tick's timeout handling,
	// reported here so trackFailStreak counts them like any other
	// completion. Drained after the two early returns above: draining
	// empties pendingTimeouts, and an early return would discard them along
	// with the rest of result.
	result.Completed = append(result.Completed, s.drainTimeoutCompletions()...)
	for _, p := range projects {
		meta := s.hydrateMetaForTriggers(ctx, p.ID)
		if meta == nil || len(meta.Triggers) == 0 {
			continue
		}
		for _, trig := range meta.Triggers {
			key := TriggerKey{ProjectID: p.ID, TriggerName: trig.Name}
			every, everr := time.ParseDuration(trig.Every)
			if everr != nil {
				slog.Warn("trigger sweep: invalid every (should have been rejected at project.yaml load time)", "project_id", p.ID, "trigger", trig.Name, "every", trig.Every, "error", everr)
				continue
			}

			// A trigger that simply hasn't reached its next `every` yet is
			// never a "skip", even while a prior run of it is still in
			// flight — so busy is checked against everyDue (below), not the
			// final on:signals-adjusted due, to avoid a busy/every-elapsed
			// trigger silently vanishing from both Skipped and Fired just
			// because its own in-flight run drained the signal queue it
			// depends on.
			//
			// The declared `timeout` bound is checked before the due gate,
			// deliberately: `timeout` and `every` answer different
			// questions (how long a round may run vs. how often to look),
			// so an overrun must be caught the moment it is seen. This is
			// the only place with both the in-flight run and the trigger
			// definition it came from — reconcileInFlight walks trigger_runs
			// rows alone, which carry no `timeout`.
			if run, busy := inFlight[key]; busy {
				if timeout := trig.TriggerTimeout(); timeout > 0 && now.Sub(run.StartedAt) >= timeout {
					s.beginTimeOutTriggerRun(ctx, key, run, timeout)
					// Recorded as a Skip too, not `continue`d past: the
					// ending is asynchronous and can fail, and while it does
					// the row stays in flight, so without this the key would
					// appear in neither Skipped nor Fired.
					result.Skipped = append(result.Skipped, TriggerSkip{
						TriggerKey: key, Since: run.StartedAt, Every: every, Timeout: timeout,
					})
					continue
				}
			}

			everyDue, derr := s.triggerIsDue(now, key, every)
			if derr != nil {
				slog.Warn("trigger sweep: evaluate due failed", "project_id", p.ID, "trigger", trig.Name, "error", derr)
				continue
			}
			if !everyDue {
				continue
			}
			if run, busy := inFlight[key]; busy {
				result.Skipped = append(result.Skipped, TriggerSkip{
					TriggerKey: key, Since: run.StartedAt, Every: every, Timeout: trig.TriggerTimeout(),
				})
				continue
			}

			// on:signals ANDs an extra "has a pending Signal" condition onto
			// the already-true everyDue above, evaluated only once we know
			// the trigger is not busy.
			due := everyDue
			if trig.On == orchestrator.TriggerOnSignals {
				due = s.signalsPendingForTrigger(meta, key)
			}
			if !due {
				continue
			}

			jobID, ferr := s.fireTrigger(ctx, now, p.ID, trig)
			if ferr != nil {
				if errors.Is(ferr, orchestrator.ErrTriggerRunInFlight) {
					// The in-memory inFlight snapshot missed a concurrently-
					// created row that the DB's unique constraint caught.
					// Same outcome as an ordinary busy skip; Since=now
					// self-corrects next tick once reconcileInFlight's own
					// read picks up this row with its real StartedAt.
					result.Skipped = append(result.Skipped, TriggerSkip{TriggerKey: key, Since: now, Every: every, Timeout: trig.TriggerTimeout()})
					continue
				}
				// Fail-open: no trigger_runs row exists for this attempt, so
				// the next tick's triggerIsDue retries automatically.
				slog.Warn("trigger sweep: dispatch failed (fail-open, will retry next tick)", "project_id", p.ID, "trigger", trig.Name, "error", ferr)
				// Reported to trackFailStreak too, alongside the fail-open
				// retry above — see TriggerDispatchFailureExitCode's doc.
				result.Completed = append(result.Completed, TriggerCompletionResult{TriggerKey: key, ExitCode: TriggerDispatchFailureExitCode})
				continue
			}
			result.Fired = append(result.Fired, TriggerFireResult{TriggerKey: key, JobID: jobID})
		}
	}
	return result, nil
}

// lookupTrigger resolves projectID's hydrated meta and finds the Trigger
// named triggerName within it, for RunTriggerNow. Returns a *StatusError
// (404) for an unknown project or trigger name.
func (s *TaskWorkflowService) lookupTrigger(ctx context.Context, projectID, triggerName string) (*orchestrator.Trigger, error) {
	meta := s.hydrateMetaForTriggers(ctx, projectID)
	if meta == nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("trigger run: project %q meta not loaded", projectID)}
	}
	for i := range meta.Triggers {
		if meta.Triggers[i].Name == triggerName {
			return &meta.Triggers[i], nil
		}
	}
	return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("trigger run: no trigger named %q in project.yaml", triggerName)}
}

// TriggerRunNowResult is RunTriggerNow's / `boid trigger run`'s response
// shape.
type TriggerRunNowResult struct {
	// Skipped is true when single-flight blocked this manual run (a prior
	// run for the same (project, trigger) is still in flight). A manual run
	// bypasses only the `every` elapsed check, never single-flight.
	Skipped bool   `json:"skipped"`
	Reason  string `json:"reason,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}

// RunTriggerNow fires projectID's triggerName trigger immediately, ignoring
// the `every` elapsed check but not single-flight — the manual debugging
// entry point behind `boid trigger run <name>`.
func (s *TaskWorkflowService) RunTriggerNow(ctx context.Context, projectID, triggerName string) (*TriggerRunNowResult, error) {
	if s.Triggers == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "trigger run: TriggerRunStore not configured"}
	}
	if s.Exec == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "trigger run: ExecDispatcher not configured"}
	}
	trig, err := s.lookupTrigger(ctx, projectID, triggerName)
	if err != nil {
		return nil, err
	}
	ctx = orchestrator.WithActor(ctx, orchestrator.ActorDaemon)
	now := time.Now().UTC()

	key := TriggerKey{ProjectID: projectID, TriggerName: triggerName}
	inFlight, err := s.reconcileInFlight(now, nil)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("trigger run: %s", err.Error())}
	}
	if _, busy := inFlight[key]; busy {
		return &TriggerRunNowResult{Skipped: true, Reason: "single-flight: a run for this trigger is already in flight"}, nil
	}

	jobID, err := s.fireTrigger(ctx, now, projectID, *trig)
	if err != nil {
		if errors.Is(err, orchestrator.ErrTriggerRunInFlight) {
			// Same DB-level race the in-memory inFlight busy-check above can
			// miss — e.g. a sweep tick's own fireTrigger call won the race.
			return &TriggerRunNowResult{Skipped: true, Reason: "single-flight: a run for this trigger is already in flight"}, nil
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("trigger run: %s", err.Error())}
	}
	return &TriggerRunNowResult{JobID: jobID}, nil
}

// ---- TriggerLoop: the periodic scheduler (QueueSweepLoop-shaped) ----

// TriggerStuckOverrunMultiplier: a run is "stuck" once it has been
// continuously in flight for longer than TriggerStuckOverrunMultiplier ×
// its own `every`, scaling the threshold to each trigger's own schedule
// rather than counting raw sweep ticks (see TriggerLoop.trackSkipStreak).
//
// TriggerStuckFailStreakThreshold counts consecutive completed runs with a
// non-zero exit code, one data point per actual execution.
const (
	TriggerStuckOverrunMultiplier   = 1.5
	TriggerStuckFailStreakThreshold = 3
)

// stuckThreshold is how long a run may stay continuously in flight before
// trackSkipStreak calls it stuck. The float64 round-trip is load-bearing:
// TriggerStuckOverrunMultiplier is fractional, and `every *
// time.Duration(1.5)` would truncate the multiplier to 1ns first.
func stuckThreshold(every time.Duration) time.Duration {
	return time.Duration(float64(every) * TriggerStuckOverrunMultiplier)
}

// TriggerLoopStore is TriggerLoop's dependency, narrowed from
// *TaskWorkflowService — same idiom as QueueSweepStore (queue_sweep.go).
type TriggerLoopStore interface {
	SweepTriggers(ctx context.Context, now time.Time) (TriggerSweepResult, error)
}

// TriggerLoop periodically calls SweepTriggers, mirroring QueueSweepLoop's
// shape: InitialDelay then Interval, Run(ctx) blocks until ctx is done, a
// sweep error is logged and never stops the loop.
//
// skipStreak/failStreak (stuck/failure-streak notification bookkeeping) are
// kept in-memory, not persisted — a daemon restart resets the streak count
// to zero, at worst costing one missed or duplicated notification around a
// restart; trigger_runs itself (the single-flight source of truth) is
// unaffected.
type TriggerLoop struct {
	Store        TriggerLoopStore
	Interval     time.Duration
	InitialDelay time.Duration
	// Notifier is optional (nil disables stuck/failure notifications).
	Notifier Notifier

	// skipStreak/failStreak are read/written only from runOnce, which Run
	// calls sequentially from a single goroutine — no locking needed.
	//
	// skipStreak's value is the highest TriggerStuckOverrunMultiplier
	// multiple already notified for that key's current stuck episode, not a
	// raw tick count — see trackSkipStreak.
	skipStreak map[TriggerKey]int
	failStreak map[TriggerKey]int
}

// Run blocks until ctx is done. It waits InitialDelay before the first
// sweep, then calls Store.SweepTriggers every Interval.
func (l *TriggerLoop) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(l.InitialDelay):
	}

	l.runOnce(ctx)

	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.runOnce(ctx)
		}
	}
}

func (l *TriggerLoop) runOnce(ctx context.Context) {
	if l.Store == nil {
		return
	}
	now := time.Now().UTC()
	result, err := l.Store.SweepTriggers(ctx, now)
	if err != nil {
		slog.Warn("trigger sweep failed", "error", err)
		return
	}
	if len(result.Fired) > 0 {
		keys := make([]string, 0, len(result.Fired))
		for _, f := range result.Fired {
			keys = append(keys, f.ProjectID+"/"+f.TriggerName)
		}
		slog.Info("trigger sweep fired", "count", len(result.Fired), "triggers", keys)
	}
	l.trackSkipStreak(ctx, now, result.Skipped)
	l.trackFailStreak(ctx, result.Completed)
}

// trackSkipStreak notifies once a skipped trigger's blocking run has been
// continuously in flight for longer than stuckThreshold, and again each
// time the overrun crosses another whole multiple of that threshold, so a
// long-stuck trigger gets periodic reminders rather than exactly one
// notification ever.
//
// Any previously-tracked key not skipped this tick is reset: a key that was
// stuck stays due (and therefore either Skipped or Fired) on every
// subsequent tick until it resolves, so it going quiet means it either
// fired or its trigger definition was removed — both cases where a fresh
// episode (multiple 0) is correct if it gets stuck again later.
func (l *TriggerLoop) trackSkipStreak(ctx context.Context, now time.Time, skipped []TriggerSkip) {
	if l.skipStreak == nil {
		l.skipStreak = make(map[TriggerKey]int)
	}
	skippedThisTick := make(map[TriggerKey]bool, len(skipped))
	for _, skip := range skipped {
		skippedThisTick[skip.TriggerKey] = true
		if skip.Every <= 0 {
			// Defensive only — ValidateTriggers rejects every<=0 at load time.
			continue
		}
		// A declared `timeout` replaces the guess outright: below it a long
		// round is doing what its author allowed, and past it the round is
		// ended rather than reported, so this notification only ever fires
		// on healthy work once one is declared.
		threshold := skip.Timeout
		if threshold <= 0 {
			threshold = stuckThreshold(skip.Every)
		}
		overrun := now.Sub(skip.Since)
		multiple := int(overrun / threshold)
		if multiple < 1 {
			continue
		}
		if l.skipStreak[skip.TriggerKey] >= multiple {
			continue // already notified for this multiple (or a later one)
		}
		l.skipStreak[skip.TriggerKey] = multiple
		if skip.Timeout > 0 {
			// A declared bound was crossed and the daemon is ending the
			// round — say so rather than reading as "nothing is happening".
			l.notify(ctx, skip.TriggerKey, fmt.Sprintf(
				"trigger %s/%s: 1 巡が %s 走って timeout (%s) を超えました。daemon が打ち切ります (超過 %d 回目)",
				skip.ProjectID, skip.TriggerName, overrun.Round(time.Second), skip.Timeout, multiple))
		} else {
			l.notify(ctx, skip.TriggerKey, fmt.Sprintf(
				"trigger %s/%s: single-flight で %s 詰まっています (`every` %s、詰まり閾値 %s の %d 倍以上)",
				skip.ProjectID, skip.TriggerName, overrun.Round(time.Second), skip.Every, threshold, multiple))
		}
	}
	for key := range l.skipStreak {
		if !skippedThisTick[key] {
			delete(l.skipStreak, key)
		}
	}
}

// trackFailStreak increments the consecutive-failure counter for every
// completion whose ExitCode != 0 this tick, notifying every
// TriggerStuckFailStreakThreshold-th time, and resets it on the first
// ExitCode == 0 completion. A key simply not completing this tick leaves
// its existing count untouched — absence means "no new data point", not
// "resolved".
func (l *TriggerLoop) trackFailStreak(ctx context.Context, completed []TriggerCompletionResult) {
	if l.failStreak == nil {
		l.failStreak = make(map[TriggerKey]int)
	}
	for _, c := range completed {
		if c.ExitCode == 0 {
			delete(l.failStreak, c.TriggerKey)
			continue
		}
		l.failStreak[c.TriggerKey]++
		if l.failStreak[c.TriggerKey]%TriggerStuckFailStreakThreshold == 0 {
			l.notify(ctx, c.TriggerKey, fmt.Sprintf(
				"trigger %s/%s: %d 回連続で失敗しました (直近 exit code %d)",
				c.ProjectID, c.TriggerName, l.failStreak[c.TriggerKey], c.ExitCode))
		}
	}
}

// notify sends one stuck/failure-streak notification, bounded by
// notifyTimeout since notify.Service.Notify runs the configured command
// synchronously and an unbounded ctx would let a hanging command wedge
// every future sweep tick until daemon shutdown.
func (l *TriggerLoop) notify(ctx context.Context, key TriggerKey, message string) {
	if l.Notifier == nil {
		return
	}
	notifyCtx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if err := l.Notifier.Notify(notifyCtx, notify.Event{ProjectID: key.ProjectID, Message: message}); err != nil {
		slog.Warn("trigger loop notify failed", "trigger", key.ProjectID+"/"+key.TriggerName, "error", err)
	}
}
