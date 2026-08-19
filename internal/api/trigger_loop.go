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

// TriggerSweepResult is SweepTriggers' return shape. TriggerLoop.runOnce
// uses it for stuck/failure-streak bookkeeping (see TriggerLoop's own doc
// comment); tests use it to pin behavior without reaching into the DB.
type TriggerSweepResult struct {
	// Fired lists triggers this sweep actually dispatched (`every` had
	// elapsed and no in-flight run blocked it).
	Fired []TriggerFireResult
	// Skipped lists triggers that were due but single-flight blocked (an
	// existing finished_at IS NULL row for the same key survived
	// reconciliation this tick).
	Skipped []TriggerKey
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
// to guess "genuinely vanished" apart from "transient read error"; both
// leave the trigger_runs row in flight for the caller (reconcileInFlight)
// to retry next tick. This is safe, not merely convenient: the daemon
// startup path already has dispatcher.MarkStaleJobsFailed (wire.go), which
// unconditionally flips every job left in "running" status (from a crashed
// daemon) to "failed" BEFORE this loop ever runs again — see this PR's
// report ("single-flight の実装方法") for the full argument for why no
// additional trigger-specific recovery is needed for that case.
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
		exitCode, terminal, jobErr := s.jobTerminalState(run.JobID)
		if jobErr != nil {
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

// triggerIsDue reports whether `every` has elapsed since key's last
// recorded start — true unconditionally when the trigger has never run
// (LatestTriggerRun returns ErrTriggerRunNotFound). trig.Every is expected
// to already be a valid time.ParseDuration string (ValidateTriggers,
// enforced at project.yaml load time) but is re-parsed defensively here
// rather than trusted blindly.
func (s *TaskWorkflowService) triggerIsDue(now time.Time, key TriggerKey, trig orchestrator.Trigger) (bool, error) {
	every, err := time.ParseDuration(trig.Every)
	if err != nil {
		return false, fmt.Errorf("invalid every %q: %w", trig.Every, err)
	}
	latest, err := s.Triggers.LatestTriggerRun(key.ProjectID, key.TriggerName)
	if errors.Is(err, orchestrator.ErrTriggerRunNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return now.Sub(latest.StartedAt) >= every, nil
}

// fireTrigger dispatches trig's `run` command as a readonly exec job (PR-4
// 節「実行」: Argv ["sh","-c",trig.Run], Readonly: true 固定) and records the
// resulting trigger_runs row. Shared by SweepTriggers' per-trigger loop and
// RunTriggerNow — the single place that both callers hand off to daemon's
// mechanical part (dispatch + record) after their own single-flight/elapsed
// gating differs.
func (s *TaskWorkflowService) fireTrigger(ctx context.Context, now time.Time, projectID string, trig orchestrator.Trigger) (jobID string, err error) {
	if s.Exec == nil {
		return "", fmt.Errorf("exec dispatcher not configured")
	}
	result, err := s.Exec.StartExec(ctx, StartExecRequest{
		ProjectID: projectID,
		Argv:      []string{"sh", "-c", trig.Run},
		// PR-4 節「実行」: Readonly true 固定。boid op の allowlist は role
		// にも readonly にも依存しない (internal/orchestrator/policy.go の
		// boidPolicy) ので、readonly のまま task_create / action_send が打てる
		// — この PR の前提そのもの、TestSweepTriggers_ReadonlyJob_CanCreateTask
		// (trigger_loop_readonly_test.go) で実際に通して pin する。
		Readonly:    true,
		DisplayName: "trigger:" + trig.Name,
	})
	if err != nil {
		return "", fmt.Errorf("start exec: %w", err)
	}
	run := &orchestrator.TriggerRun{ProjectID: projectID, TriggerName: trig.Name, JobID: result.JobID, StartedAt: now}
	if err := s.Triggers.CreateTriggerRun(run); err != nil {
		// The exec job WAS dispatched (a container is starting/running) but
		// the daemon failed to record it — single-flight now has no row to
		// key off for this (project, trigger) until this job eventually
		// completes and jobs.status leaves "running" some OTHER way (it
		// never will from this loop's perspective, since there's no
		// trigger_runs row pointing at it). This is a narrow, logged
		// (not silently swallowed) residual gap: see this PR's report
		// ("single-flight の実装方法") for why CreateTriggerRun cannot be
		// made to happen BEFORE StartExec instead (there is no job_id to
		// record until StartExec returns one, and trigger_runs.job_id is
		// NOT NULL).
		return "", fmt.Errorf("create trigger run (job %s WAS dispatched): %w", result.JobID, err)
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
// Nil Triggers/Projects/Meta (any optional dependency unwired) makes this a
// no-op, matching TaskWorkflowService's other optional-field conventions
// (Notifier, TaskCreator) — a daemon built without trigger support wired in
// simply runs no triggers rather than panicking.
func (s *TaskWorkflowService) SweepTriggers(ctx context.Context, now time.Time) (TriggerSweepResult, error) {
	var result TriggerSweepResult
	if s.Triggers == nil || s.Projects == nil || s.Meta == nil {
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
			if _, busy := inFlight[key]; busy {
				result.Skipped = append(result.Skipped, key)
				continue
			}
			due, derr := s.triggerIsDue(now, key, trig)
			if derr != nil {
				slog.Warn("trigger sweep: evaluate due failed", "project_id", p.ID, "trigger", trig.Name, "error", derr)
				continue
			}
			if !due {
				continue
			}
			jobID, ferr := s.fireTrigger(ctx, now, p.ID, trig)
			if ferr != nil {
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
	now := time.Now()

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
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("trigger run: %s", err.Error())}
	}
	return &TriggerRunNowResult{JobID: jobID}, nil
}

// ---- TriggerLoop: the periodic scheduler (QueueSweepLoop-shaped) ----

// TriggerStuckSkipThreshold / TriggerStuckFailStreakThreshold implement 12
// 節 B-5 既定案「詰まり検出は N 連続見送りで通知」「連続失敗で通知」. Picked,
// not measured — 3 consecutive sweep ticks of "still single-flight-blocked"
// or "still failing" is long enough to rule out an ordinary one-tick blip
// (a trigger command that legitimately runs longer than one sweep interval)
// while still surfacing a genuinely stuck/broken trigger promptly. No
// workspace drives this loop in production yet, so — same framing as
// action_list.go's DefaultActionListLimit/MaxActionListLimit — revisit once
// one does and these prove too chatty or too quiet.
const (
	TriggerStuckSkipThreshold       = 3
	TriggerStuckFailStreakThreshold = 3
)

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
	now := time.Now()
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
	l.trackSkipStreak(ctx, result.Skipped)
	l.trackFailStreak(ctx, result.Completed)
}

// trackSkipStreak increments the consecutive-skip counter for every key
// skipped this tick and notifies every TriggerStuckSkipThreshold-th time it
// does. Any previously-tracked key NOT skipped this tick is reset to zero
// (deleted) — correct because, unlike "not due yet", a key that WAS stuck
// stays due (and therefore either Skipped or Fired) on every subsequent
// tick until it resolves; a key silently going quiet only happens when it
// actually fired, or its trigger definition was removed from project.yaml
// — both cases where resetting is the right behavior.
func (l *TriggerLoop) trackSkipStreak(ctx context.Context, skipped []TriggerKey) {
	if l.skipStreak == nil {
		l.skipStreak = make(map[TriggerKey]int)
	}
	skippedThisTick := make(map[TriggerKey]bool, len(skipped))
	for _, key := range skipped {
		skippedThisTick[key] = true
		l.skipStreak[key]++
		if l.skipStreak[key]%TriggerStuckSkipThreshold == 0 {
			l.notify(ctx, key, fmt.Sprintf(
				"trigger %s/%s: single-flight で %d 回連続で見送られました (詰まっている可能性があります)",
				key.ProjectID, key.TriggerName, l.skipStreak[key]))
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

func (l *TriggerLoop) notify(ctx context.Context, key TriggerKey, message string) {
	if l.Notifier == nil {
		return
	}
	if err := l.Notifier.Notify(ctx, notify.Event{ProjectID: key.ProjectID, Message: message}); err != nil {
		slog.Warn("trigger loop notify failed", "trigger", key.ProjectID+"/"+key.TriggerName, "error", err)
	}
}
