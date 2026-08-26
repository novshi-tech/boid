package api

// docs/plans/ingestion-identity.md PR-4 (B-5): トリガ実行機構。本 doc の
// 中核。daemon が持つ 3 つの持ち分 (J-4) — スケジュール / single-flight /
// 実行結果の記録 — をこのファイルと trigger_run.go (store 層) で実装する。
//
// TriggerLoop (このファイル下半分) は QueueSweepLoop (queue_sweep.go) と
// 同型: Run(ctx) が ticker を回し、runOnce が 1 巡ぶんを見る。
// TaskWorkflowService.SweepTriggers (このファイル上半分) が 1 巡の実処理 —
// SweepWake/SweepTriage が TaskWorkflowService のメソッドなのと同じ配置。
//
// daemon は run の中身を一切知らない (J-2) — SweepTriggers が読むのは
// Trigger{Name, Every, Run} の 3 フィールドだけで、Run は
// api.ExecDispatcher.StartExec に文字列のまま渡すだけである。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TriggerKey identifies one trigger within one project — the single-flight
// and elapsed-time scoping unit (12 節 B-5 既定案「single-flight の粒度は
// トリガ単位」).
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

// TriggerSkip records one trigger that WAS due this tick but single-flight
// blocked it — Since/Every are the blocking run's StartedAt and the
// trigger's own configured interval, both needed by
// TriggerLoop.trackSkipStreak to judge whether the in-flight duration is
// actually anomalous relative to `every` (Opus review Blocker 2 (ii): a
// raw sweep-tick count conflates "waited 3 ticks" with "waited 3 minutes",
// which are wildly different things when `every` is 10m and the sweep
// interval is 1m).
type TriggerSkip struct {
	TriggerKey
	// Since is the blocking run's StartedAt — how long it has been in
	// flight as of this tick.
	Since time.Time
	// Every is trigger.Every, parsed. Zero if the blocking run's own
	// trigger definition's `every` could not be parsed (should not happen —
	// ValidateTriggers rejects that at project.yaml load time — but
	// trackSkipStreak treats a zero Every defensively, see its own doc
	// comment).
	Every time.Duration
}

// TriggerSweepResult is SweepTriggers' return shape. TriggerLoop.runOnce
// uses it for stuck/failure-streak bookkeeping (see TriggerLoop's own doc
// comment); tests use it to pin behavior without reaching into the DB.
type TriggerSweepResult struct {
	// Fired lists triggers this sweep actually dispatched (`every` had
	// elapsed and no in-flight run blocked it).
	Fired []TriggerFireResult
	// Skipped lists triggers that were DUE (`every` had elapsed) but
	// single-flight blocked them this tick (Opus review Blocker 2 (i)): a
	// trigger that simply hasn't reached its next `every` yet is NOT a
	// skip, even while a prior run of it is still in flight — counting that
	// as a skip is what caused every-10m/execution-takes-5m triggers to
	// rack up "3 consecutive skips" (and notify) on a completely healthy
	// schedule, once the sweep interval (1m) is much shorter than `every`.
	Skipped []TriggerSkip
	// Completed lists trigger_runs rows this sweep's reconciliation pass
	// newly marked finished (their job reached a terminal status).
	Completed []TriggerCompletionResult
}

// hydrateMetaForTriggers resolves projectID's ProjectMeta the same
// two-step way ResolveOrCapture does (task_resolve_or_capture.go): prefer
// the workspace-hydrated view (GetWithWorkspace) so a workspace-default
// project definition's fields are visible, falling back to the bare
// in-memory cache (Get) when hydration fails or Meta only implements the
// narrower shape. Returns nil when neither succeeds — callers treat that as
// "no triggers for this project" rather than erroring the whole sweep.
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
// (JobStatusCompleted/JobStatusFailed) and, if so, its exit code.
//
// Any error from s.Jobs.GetJob (including "no such job") is propagated
// as-is, NOT translated into a synthetic terminal state — internal/dispatcher's
// GetJob has no distinguishable not-found sentinel (its own scanJob wraps
// sql.ErrNoRows into a plain fmt.Errorf), so this deliberately does not try
// to guess "genuinely vanished" apart from "transient read error" itself;
// reconcileInFlight is what decides, via selfHealStaleTriggerRun, how long
// to keep retrying before giving up on a jobID it can never resolve (N-1,
// Opus review) — see that function's own doc comment.
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
// in-flight without ever resolving to a real job's terminal state — either
// because run.JobID is still empty (SetTriggerRunJobID hasn't landed yet,
// see fireTrigger) or because s.Jobs.GetJob(run.JobID) keeps erroring
// (typically: the job row itself is gone, e.g. swept by the 30-day taskless-
// job GC, since trigger_runs.job_id is not a real FK) — before
// reconcileInFlight gives up waiting and force-closes it (N-1, Opus
// review). Without this, a vanished job row wedges single-flight for that
// (project, trigger) forever: there is no other self-recovery path (a
// daemon restart's dispatcher.MarkStaleJobsFailed only touches rows that
// still EXIST in jobs, and `boid trigger run` only ever returns Skipped).
//
// Expressed as elapsed wall-clock time, not a tick counter, deliberately:
// reconcileInFlight runs from BOTH the periodic TriggerLoop AND
// RunTriggerNow's own HTTP-handler goroutine, so a shared in-memory counter
// would need its own mutex to stay -race clean (unlike
// TriggerLoop.skipStreak/failStreak, which only the single periodic-sweep
// goroutine ever touches). 3 minutes matches TriggerStuckSkipThreshold-era
// "3 consecutive sweep ticks" convention at TriggerLoop's own 1-minute
// Interval (wire.go), while working out to a duration regardless of what
// Interval or RunTriggerNow call pattern actually applies.
const TriggerRunSelfHealGrace = 3 * time.Minute

// TriggerRunSelfHealExitCode is the sentinel CompleteTriggerRun records for
// a self-healed row (N-1) — flows into TriggerCompletionResult like any
// other non-zero exit code, so it also counts toward trackFailStreak's
// consecutive-failure notification (a self-heal IS a failure worth
// surfacing). Outside a real command's 0-255 exit code range so it reads
// unambiguously in logs/notifications.
const TriggerRunSelfHealExitCode = -1

// selfHealStaleTriggerRun force-closes run if it has been in flight, unable
// to resolve to a real job's terminal state, for at least
// TriggerRunSelfHealGrace (N-1, Opus review). Returns true when it closed
// the row (the caller must treat it as no-longer-in-flight and, if given a
// non-nil completed slice, record it there); false when the grace period
// has not elapsed yet (the caller leaves it in stillInFlight and retries
// next call, same as before this existed).
func (s *TaskWorkflowService) selfHealStaleTriggerRun(now time.Time, run *orchestrator.TriggerRun, completed *[]TriggerCompletionResult) bool {
	if now.Sub(run.StartedAt) < TriggerRunSelfHealGrace {
		return false
	}
	if err := s.Triggers.CompleteTriggerRun(run.ID, now, TriggerRunSelfHealExitCode); err != nil {
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
// NULL against its job's current status, completing (CompleteTriggerRun)
// any whose job has reached a terminal status, and returns the set that is
// STILL genuinely in flight after reconciliation — the single-flight
// membership test both SweepTriggers' per-trigger loop and RunTriggerNow
// use. completed, when non-nil, receives one entry per row newly completed
// this call (SweepTriggers' failure-streak bookkeeping; RunTriggerNow
// passes nil since a manual run has no streak to track).
//
// A single ListInFlightTriggerRuns() query up front, not one query per
// trigger definition — same "workspace-scoped batch read" posture PR-3's
// ListActionsSince established for action_list.
func (s *TaskWorkflowService) reconcileInFlight(now time.Time, completed *[]TriggerCompletionResult) (map[TriggerKey]*orchestrator.TriggerRun, error) {
	runs, err := s.Triggers.ListInFlightTriggerRuns()
	if err != nil {
		return nil, fmt.Errorf("list in-flight trigger runs: %w", err)
	}
	stillInFlight := make(map[TriggerKey]*orchestrator.TriggerRun, len(runs))
	for _, run := range runs {
		key := TriggerKey{ProjectID: run.ProjectID, TriggerName: run.TriggerName}

		// Opus review Blocker 1: fireTrigger inserts a row BEFORE dispatch,
		// so a row with JobID=="" is either mid-dispatch (a concurrent
		// fireTrigger call hasn't reached SetTriggerRunJobID yet — normal,
		// can take real wall-clock seconds for a container to start) or
		// that update itself failed (fireTrigger's own residual-gap note).
		// Both look identical from here; selfHealStaleTriggerRun's grace
		// period is what tells "still dispatching" from "stuck forever"
		// apart, exactly as it does for a GetJob error below.
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

// triggerIsDue reports whether every has elapsed since key's last recorded
// start — true unconditionally when the trigger has never run
// (LatestTriggerRun returns ErrTriggerRunNotFound). every is the caller's
// already-parsed trig.Every (ValidateTriggers guarantees it parses at
// project.yaml load time) — parsed once by the caller (SweepTriggers) rather
// than re-parsed here, since Opus review Blocker 2 (ii) also needs that same
// parsed value to size a TriggerSkip's overrun threshold.
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
// predicate (docs/plans/signal-ingest-detailed-design.md §4.2):
//
//	due := now - latestRun.StartedAt >= every        # triggerIsDue, unchanged
//	if trig.On == "signals":
//	    due = due && HasPendingSignals(workspaceOf(project))
//
// meta is the SAME hydrated *orchestrator.ProjectMeta the caller
// (SweepTriggers) already read off hydrateMetaForTriggers for this project —
// meta.SecretNamespace carries the resolved workspace id whenever the
// project has a linked workspace (ProjectStore.GetWithWorkspace always
// injects it, including in its degraded os.ErrNotExist window; see that
// method's own doc comment), so this issues NO new DB query to re-resolve
// the workspace, matching §4.2's explicit requirement.
//
// A project with no linked workspace (empty SecretNamespace) is always not
// due — logged at Debug, not Warn/error: this is an ordinary, expected
// project.yaml shape (an on:signals trigger declared on a workspace-unlinked
// project), not a failure (§4.2: "常に not due (debug log のみ、エラーにしない)").
// The same applies when no SignalStore is wired at all (s.Signals nil) —
// this deliberately does NOT fold into SweepTriggers' top-level nil guard,
// since that would also block every plain schedule trigger sharing this
// daemon; only on:signals triggers depend on Signals.
//
// A HasPendingSignals error also resolves to not-due (logged at Warn) rather
// than aborting the whole trigger's evaluation — matching triggerIsDue's own
// caller (SweepTriggers already treats a per-trigger evaluation error as
// "skip this trigger this tick, retry next tick", the same fail-open posture
// dispatch failures get.
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
// own stdin before running the command (N-4, Opus review). Needed because
// container_backend.go's Launch always sets OpenStdin/AttachStdin true
// (ContainerCreate's cfg), and the ONLY caller that ever half-closes that
// attach connection's write side is ws_attach.go's interactive-session
// CloseInput path — a daemon-originated exec job like this one has no
// attach client at all, so without this the container's stdin pipe stays
// open forever. Confirmed empirically against a real podman engine with the
// same OpenStdin:true/AttachStdin:true/nobody-writes-or-closes-it shape
// (this PR's report, N-4 節): a plain `cat` inside such a container hangs
// indefinitely (SIGTERM alone does not even stop it); prefixing the command
// with `exec 0</dev/null;` makes it exit immediately and cleanly. `exec` with
// no command argument applies the redirect to the CURRENT shell (dash, the
// sandbox's /bin/sh) rather than spawning a subprocess, so every descendant
// of `sh -c` — including whatever run itself execs — inherits /dev/null as
// fd 0.
func triggerRunArgv(run string) []string {
	return []string{"sh", "-c", "exec 0</dev/null; " + run}
}

// fireTrigger dispatches trig's `run` command as a readonly exec job (PR-4
// 節「実行」: Argv ["sh","-c",...trig.Run], Readonly: true 固定) and records
// the resulting trigger_runs row. Shared by SweepTriggers' per-trigger loop
// and RunTriggerNow — the single place that both callers hand off to
// daemon's mechanical part (claim + dispatch + record) after their own
// single-flight/elapsed gating differs.
//
// Opus review Blocker 1: the trigger_runs row is created FIRST, claiming
// single-flight via idx_trigger_runs_inflight_unique (migration 0043)
// succeeding, BEFORE StartExec is ever called — the reverse of this PR's
// original order (dispatch, then record). That original order left a
// window — StartExec can take real wall-clock seconds (spinning up a
// container) — during which two concurrent callers (two sweep ticks, two
// `boid trigger run` calls, or two daemon processes sharing one DB file)
// could both see "nothing in flight" and both dispatch. Insert-then-dispatch
// closes it: whichever caller's INSERT lands first wins; the loser gets
// orchestrator.ErrTriggerRunInFlight back from CreateTriggerRun and never
// calls StartExec at all.
//
// A CreateTriggerRun success has no job_id yet (StartExec hasn't run) —
// SetTriggerRunJobID fills it in once dispatch succeeds. If StartExec itself
// fails, the claimed row is deleted (not closed — see DeleteTriggerRun's own
// doc comment for why closing it would break the pre-existing "fail-open,
// retry the very same instant" contract).
func (s *TaskWorkflowService) fireTrigger(ctx context.Context, now time.Time, projectID string, trig orchestrator.Trigger) (jobID string, err error) {
	if s.Exec == nil {
		return "", fmt.Errorf("exec dispatcher not configured")
	}
	run := &orchestrator.TriggerRun{ProjectID: projectID, TriggerName: trig.Name, StartedAt: now}
	if err := s.Triggers.CreateTriggerRun(run); err != nil {
		return "", err
	}

	result, err := s.Exec.StartExec(ctx, StartExecRequest{
		ProjectID: projectID,
		Argv:      triggerRunArgv(trig.Run),
		// PR-4 節「実行」: Readonly true 固定。boid op の allowlist は role
		// にも readonly にも依存しない (internal/orchestrator/policy.go の
		// boidPolicy) ので、readonly のまま task_create / action_send が打てる
		// — この PR の前提そのもの。ExecuteBoidBuiltin を直接呼ぶ形での確認は
		// TestBoidBuiltinExecutor_TaskCreate_SucceedsFromWhatATriggerJobsSandboxWouldSend
		// (internal/server/trigger_readonly_task_create_test.go、その doc
		// comment に「本番の broker/executor 経路そのものではない」範囲が
		// 明記されている、N-5 Opus review)。
		Readonly:    true,
		DisplayName: "trigger:" + trig.Name,
	})
	if err != nil {
		if derr := s.Triggers.DeleteTriggerRun(run.ID); derr != nil {
			// Both the dispatch AND its own cleanup failed — the claimed
			// row is stuck in flight (StartedAt == now) until
			// TriggerRunSelfHealGrace force-closes it (N-1). Logged, not
			// swallowed: this is the residual gap DeleteTriggerRun's own
			// doc comment refers to.
			slog.Warn("trigger fire: dispatch failed AND cleanup of the claimed row also failed; single-flight for this (project,trigger) will wedge until self-heal", "run_id", run.ID, "project_id", projectID, "trigger", trig.Name, "dispatch_error", err, "cleanup_error", derr)
		}
		return "", fmt.Errorf("start exec: %w", err)
	}

	if serr := s.Triggers.SetTriggerRunJobID(run.ID, result.JobID); serr != nil {
		// The container IS running but this row never got its job_id —
		// reconcileInFlight's self-heal (TriggerRunSelfHealGrace) will
		// eventually force-close this row purely from its empty job_id;
		// see reconcileInFlight's own doc comment. Logged, not swallowed.
		slog.Warn("trigger fire: job dispatched but recording its job_id failed; self-heal will close this row after the grace period", "run_id", run.ID, "job_id", result.JobID, "error", serr)
	}
	return result.JobID, nil
}

// SweepTriggers is TriggerLoop's per-tick work: reconcile in-flight runs,
// then evaluate every project's triggers[] and fire whichever are due and
// not single-flight-blocked. Mirrors SweepWake/SweepTriage's shape (both
// TaskWorkflowService methods called from QueueSweepLoop) — daemon-actor
// context, tolerant of partial failures (one project/trigger's error is
// logged and does not abort the sweep for every other one).
//
// Nil Triggers/Projects/Meta/Jobs (any REQUIRED dependency unwired) makes
// this a no-op, matching TaskWorkflowService's other optional-field
// conventions (Notifier, TaskCreator) — a daemon built without trigger
// support wired in simply runs no triggers rather than panicking.
//
// Jobs joins this guard for a different reason than Triggers/Projects/Meta
// (N-10, Opus review): a nil s.Jobs does not fail loudly the way a nil
// Projects/Meta would — jobTerminalState just returns an error every call,
// which reconcileInFlight treats as "leave in flight, retry next tick"
// (indistinguishable from a transient GetJob hiccup). That is harmless for
// a tick with nothing in flight yet, but the FIRST successful fire creates a
// row reconcileInFlight can then never resolve — the exact permanent-wedge
// shape N-1's self-heal exists to recover from, except self-heal cannot
// help here either (TriggerRunSelfHealGrace only fires on a jobErr/empty-
// job-id it CAN observe; a nil s.Jobs makes every subsequent tick's
// jobTerminalState call fail identically, so it does eventually self-heal,
// just noisily and after silently wedging every trigger for
// TriggerRunSelfHealGrace first). s.Exec is deliberately NOT added here:
// its absence is already handled gracefully per-trigger by the existing
// fail-open dispatch-failure path below (fireTrigger returns a plain error
// when s.Exec is nil, same as any other StartExec failure) — folding it
// into this top-level guard would ALSO skip reconcileInFlight, which is
// still useful (closing out already-in-flight rows) even when nothing new
// can be dispatched.
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

			// Opus review Blocker 2 (i): due is checked BEFORE busy. A
			// trigger that simply hasn't reached its next `every` yet is
			// never a "skip", even while a PRIOR run of it is still in
			// flight — that combination (not due AND busy) is completely
			// normal whenever a trigger's own command runs longer than one
			// sweep tick but still finishes well within `every`.
			//
			// everyDue (NOT the final on:signals-adjusted due below) is
			// what decides whether a busy trigger counts as a "skip"
			// (Opus review F1, 2026-08-26): trackSkipStreak's own doc
			// comment requires a stuck key to appear in EITHER Skipped or
			// Fired on every tick until it resolves. If the on:signals
			// predicate were allowed to make an in-flight, every-elapsed
			// trigger's due false (e.g. the very job that's now wedged
			// acked every pending Signal right before hanging, so the
			// inbox is empty by the time this tick runs), that trigger
			// would silently vanish from both Skipped and Fired — wedged
			// forever with no stuck notification ever firing, unlike an
			// identically-configured `on: schedule` trigger. Evaluating
			// busy against everyDue, BEFORE the signals predicate is even
			// consulted, closes that gap: a busy, every-elapsed trigger is
			// always recorded as Skipped regardless of what
			// signalsPendingForTrigger would say (and, as a side benefit,
			// the extra HasPendingSignals read for on:signals triggers is
			// skipped entirely in the busy case, since it can't fire this
			// tick either way).
			everyDue, derr := s.triggerIsDue(now, key, every)
			if derr != nil {
				slog.Warn("trigger sweep: evaluate due failed", "project_id", p.ID, "trigger", trig.Name, "error", derr)
				continue
			}
			if !everyDue {
				continue
			}
			if run, busy := inFlight[key]; busy {
				result.Skipped = append(result.Skipped, TriggerSkip{TriggerKey: key, Since: run.StartedAt, Every: every})
				continue
			}

			// docs/plans/signal-ingest-detailed-design.md §4.2: on:signals
			// ANDs an extra "has a pending Signal" condition onto the
			// already-true everyDue above — evaluated only once we know
			// the trigger is NOT busy (see the F1 comment above for why
			// busy is checked against everyDue alone, not this).
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
					// The in-memory inFlight snapshot (built from
					// reconcileInFlight's read, above) missed a
					// concurrently-created row — the DB's own unique
					// constraint (idx_trigger_runs_inflight_unique) caught
					// what the read-then-check race didn't (Opus review
					// Blocker 1). Same outcome as an ordinary busy skip;
					// Since=now is an approximation (the real row's
					// StartedAt isn't available here) that self-corrects
					// next tick once reconcileInFlight's own read picks up
					// this row with its real StartedAt.
					result.Skipped = append(result.Skipped, TriggerSkip{TriggerKey: key, Since: now, Every: every})
					continue
				}
				// フェイルオープン (12 節 B-5 既定案): dispatch/record failed,
				// so no trigger_runs row exists for this attempt — the NEXT
				// tick's triggerIsDue sees the same stale last-run timestamp
				// (or ErrTriggerRunNotFound if this trigger has never
				// succeeded) and retries automatically. No separate
				// retry/backoff bookkeeping needed.
				slog.Warn("trigger sweep: dispatch failed (fail-open, will retry next tick)", "project_id", p.ID, "trigger", trig.Name, "error", ferr)
				continue
			}
			result.Fired = append(result.Fired, TriggerFireResult{TriggerKey: key, JobID: jobID})
		}
	}
	return result, nil
}

// lookupTrigger resolves projectID's hydrated meta and finds the Trigger
// named triggerName within it, for RunTriggerNow. Returns a *StatusError
// (404) for an unknown project or an unknown trigger name — both are
// caller mistakes (a bad project ref or a typo'd trigger name), not
// internal failures.
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
	// run for the same (project, trigger) is still in flight). Manual runs
	// deliberately do NOT bypass single-flight — only the `every` elapsed
	// check is bypassed (仕分け B「手動 1 巡の口」既定案: デバッグに要る, not
	// "force a second concurrent run").
	Skipped bool   `json:"skipped"`
	Reason  string `json:"reason,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}

// RunTriggerNow fires projectID's triggerName trigger immediately,
// ignoring the `every` elapsed check but NOT single-flight — the manual
// debugging entry point 12 節 B-5 既定案「手動 1 巡の口」calls for
// (`boid trigger run <name>`, cmd/trigger.go).
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
	now := time.Now().UTC() // N-9, Opus review: see CreateTriggerRun's own doc comment

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
			// Same DB-level race the in-memory inFlight busy-check above
			// can miss (Opus review Blocker 1) — e.g. a sweep tick's own
			// fireTrigger call won the race between this method's
			// reconcileInFlight read and its own CreateTriggerRun.
			return &TriggerRunNowResult{Skipped: true, Reason: "single-flight: a run for this trigger is already in flight"}, nil
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("trigger run: %s", err.Error())}
	}
	return &TriggerRunNowResult{JobID: jobID}, nil
}

// ---- TriggerLoop: the periodic scheduler (QueueSweepLoop-shaped) ----

// TriggerStuckOverrunMultiplier implements 12 節 B-5 既定案「詰まり検出は N
// 連続見送りで通知」, revised by Opus review Blocker 2 (ii): a run is
// "stuck" once it has been continuously in flight for longer than
// TriggerStuckOverrunMultiplier × its OWN `every` — not after N raw sweep
// ticks. A tick-counted threshold implicitly assumes the sweep interval is
// close to `every`; TriggerLoop's actual Interval (1m, wire.go) is 10× the
// example `every: 10m` the reviewer's report used, so 3 raw ticks would fire
// after 3 minutes even for a trigger whose command normally, harmlessly,
// takes 5 of every 10 minutes. Expressing the threshold as a multiple of
// `every` instead scales with each trigger's own schedule automatically —
// see TriggerLoop.trackSkipStreak.
//
// TriggerStuckFailStreakThreshold is unchanged: it counts consecutive
// COMPLETED runs with a non-zero exit code, one data point per actual
// execution regardless of sweep interval — there is no analogous
// tick-vs-every mismatch for it to correct.
//
// The multiplier moved 3 → 1.5 on 2026-08-24 (nose). At 3 the threshold for
// khi's sweep (`every: 10m`) was 30 minutes, and both stalls that actually
// happened that day — 25 and 26 minutes, an agent that ended its turn without
// calling a tool, then one that forked a subagent and ended its turn waiting
// for a reply — were resolved by a human noticing before the notification
// would have fired. A detector that never fires because people are faster
// than it is not a detector. Healthy runs of that sweep finished in 1–8
// minutes, so 1.5× (15m) still clears them comfortably.
//
// It is deliberately still a multiple of `every` rather than a wall-clock
// constant: the reason for the relative form (each trigger's own schedule
// setting its own sense of "too long") did not change, only the constant did.
const (
	TriggerStuckOverrunMultiplier   = 1.5
	TriggerStuckFailStreakThreshold = 3
)

// stuckThreshold is how long a run may stay continuously in flight before
// trackSkipStreak calls it stuck.
//
// The float64 round-trip is load-bearing: TriggerStuckOverrunMultiplier is
// fractional, and `every * time.Duration(1.5)` would convert the multiplier
// to an integer Duration of 1ns first, making the threshold 1ns and firing
// on every tick.
func stuckThreshold(every time.Duration) time.Duration {
	return time.Duration(float64(every) * TriggerStuckOverrunMultiplier)
}

// TriggerLoopStore is TriggerLoop's dependency, narrowed from
// *TaskWorkflowService — same idiom as QueueSweepStore (queue_sweep.go).
type TriggerLoopStore interface {
	SweepTriggers(ctx context.Context, now time.Time) (TriggerSweepResult, error)
}

// TriggerLoop periodically calls SweepTriggers, mirroring QueueSweepLoop's
// shape (queue_sweep.go) exactly: same InitialDelay-then-Interval idiom,
// Run(ctx) blocks until ctx is done, a sweep error is logged and never
// stops the loop.
//
// skipStreak/failStreak (stuck/failure-streak notification bookkeeping) are
// deliberately kept IN-MEMORY on the loop, not persisted to trigger_runs —
// mirroring QueueSweepLoop.lastBreachFingerprint's own precedent (also
// process-local, edge-triggered dedup state). A daemon restart resets the
// streak count to zero; the worst case is one missed or duplicated
// notification around a restart, never an incorrect fire/skip decision —
// trigger_runs itself (the single-flight source of truth) is unaffected.
type TriggerLoop struct {
	Store        TriggerLoopStore
	Interval     time.Duration
	InitialDelay time.Duration
	// Notifier is optional (nil disables stuck/failure notifications,
	// matching TaskWorkflowService.Notifier's own convention).
	Notifier Notifier

	// skipStreak/failStreak are read/written ONLY from runOnce, which Run
	// calls sequentially from a single goroutine — no locking needed, same
	// reasoning as QueueSweepLoop.lastBreachFingerprint.
	//
	// skipStreak's value is the highest TriggerStuckOverrunMultiplier
	// multiple already notified for that key's CURRENT stuck episode (Opus
	// review Blocker 2 (ii)) — not a raw tick count. See trackSkipStreak.
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
	now := time.Now().UTC() // N-9, Opus review: see CreateTriggerRun's own doc comment
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
// continuously in flight for longer than TriggerStuckOverrunMultiplier ×
// its own `every` (Opus review Blocker 2 (ii) — see
// TriggerStuckOverrunMultiplier's own doc comment for why this replaced a
// raw sweep-tick count), and again each time the overrun crosses another
// whole multiple of that threshold, so a trigger stuck for a very long time
// still gets periodic reminders rather than exactly one notification ever.
//
// Any previously-tracked key NOT skipped this tick is reset (deleted) —
// correct because, unlike "not due yet", a key that WAS stuck stays due
// (and therefore either Skipped or Fired) on every subsequent tick until it
// resolves; a key silently going quiet only happens when it actually fired,
// or its trigger definition was removed from project.yaml — both cases
// where resetting is the right behavior, and both start a fresh episode
// (multiple 0) if it gets stuck again later.
func (l *TriggerLoop) trackSkipStreak(ctx context.Context, now time.Time, skipped []TriggerSkip) {
	if l.skipStreak == nil {
		l.skipStreak = make(map[TriggerKey]int)
	}
	skippedThisTick := make(map[TriggerKey]bool, len(skipped))
	for _, skip := range skipped {
		skippedThisTick[skip.TriggerKey] = true
		if skip.Every <= 0 {
			// Defensive only — ValidateTriggers rejects every<=0 at
			// project.yaml load time, so this should be unreachable in
			// practice. Skip rather than divide by/compare against a
			// meaningless zero threshold.
			continue
		}
		threshold := stuckThreshold(skip.Every)
		overrun := now.Sub(skip.Since)
		multiple := int(overrun / threshold)
		if multiple < 1 {
			continue
		}
		if l.skipStreak[skip.TriggerKey] >= multiple {
			continue // already notified for this multiple (or a later one)
		}
		l.skipStreak[skip.TriggerKey] = multiple
		// multiple counts crossings of THRESHOLD, not of `every` — naming the
		// threshold here (and `every` beside it) keeps the two apart now that
		// they differ by a fraction rather than a whole number.
		l.notify(ctx, skip.TriggerKey, fmt.Sprintf(
			"trigger %s/%s: single-flight で %s 詰まっています (`every` %s、詰まり閾値 %s の %d 倍以上)",
			skip.ProjectID, skip.TriggerName, overrun.Round(time.Second), skip.Every, threshold, multiple))
	}
	for key := range l.skipStreak {
		if !skippedThisTick[key] {
			delete(l.skipStreak, key)
		}
	}
}

// trackFailStreak increments the consecutive-failure counter for every
// completion whose ExitCode != 0 this tick, notifying every
// TriggerStuckFailStreakThreshold-th time, and resets (deletes) it on the
// first ExitCode == 0 completion. Unlike skip streak, a key simply not
// completing this tick (nothing to reconcile) leaves its existing count
// untouched — absence here means "no new data point", not "resolved".
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
// notifyTimeout (N-3, Opus review) — notify.Service.Notify runs the
// configured notify command via exec.CommandContext SYNCHRONOUSLY, so an
// unbounded ctx (this is the TriggerLoop's own long-lived server ctx) would
// let a hanging notify command wedge runOnce, and therefore every future
// sweep tick, until daemon shutdown. queue_notify.go's
// notifySuggestionArrived and triage_done.go's own notify call already wrap
// their Notify calls the same way; this makes trigger_loop.go's the third,
// not an exception.
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
