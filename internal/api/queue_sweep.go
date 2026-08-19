package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// SweepWake implements queue の決定論的評価 節 rule 1 (wake 評価) as a
// periodic, decision-only sweep (決定12: no agent judgment — clock + task DB
// only). For every parked task it evaluates orchestrator.ShouldWake against
// wall-clock now and (when wake_task_id is set) that task's current status,
// and calls Wake for any that qualify.
//
// A failure evaluating or waking one task is logged and does not abort the
// sweep — a single malformed task_triage row or a transient Wake conflict
// (e.g. concurrent park/wake racing with a direct nose action) must not
// block wake evaluation for every other parked task. Returns the ids of
// tasks that were successfully woken.
func (s *TaskWorkflowService) SweepWake(ctx context.Context, now time.Time) (woken []string, err error) {
	if s.Tasks == nil || s.TaskTriage == nil {
		return nil, nil
	}
	// 論点11: this is the machine-driven wake sweep (決定12, no human in the
	// loop) — must never be confused with a human pressing Wake
	// (web_service.go's WebAppService.Wake stamps ActorHuman).
	ctx = orchestrator.WithActor(ctx, orchestrator.ActorDaemon)
	parked, err := s.Tasks.ListTasks(orchestrator.TaskFilter{Status: string(orchestrator.TaskStatusParked)})
	if err != nil {
		return nil, fmt.Errorf("sweep wake: list parked tasks: %w", err)
	}
	for _, t := range parked {
		tt, ttErr := s.TaskTriage.GetTaskTriage(t.ID)
		if ttErr != nil {
			if !errors.Is(ttErr, sql.ErrNoRows) {
				slog.Warn("queue sweep: get task_triage failed", "task_id", t.ID, "error", ttErr)
			}
			// sql.ErrNoRows: a parked task with no sidecar row at all has no
			// wake condition to evaluate — not an error, just nothing to do.
			continue
		}

		wakeTaskFound := false
		var wakeTaskStatus orchestrator.TaskStatus
		if tt.WakeTaskID != "" {
			wt, wErr := s.Tasks.GetTask(tt.WakeTaskID)
			switch {
			case wErr == nil:
				wakeTaskFound = true
				wakeTaskStatus = wt.Status
			case errors.Is(wErr, orchestrator.ErrTaskNotFound):
				// wakeTaskFound stays false: ShouldWake treats a vanished
				// reference as terminal (fail-open, see its own doc comment).
			default:
				slog.Warn("queue sweep: get wake_task_id failed", "task_id", t.ID, "wake_task_id", tt.WakeTaskID, "error", wErr)
				continue
			}
		}

		if !orchestrator.ShouldWake(now, tt, wakeTaskFound, wakeTaskStatus) {
			continue
		}
		if _, wakeErr := s.Wake(ctx, t.ID); wakeErr != nil {
			slog.Warn("queue sweep: wake failed", "task_id", t.ID, "error", wakeErr)
			continue
		}
		woken = append(woken, t.ID)
	}
	return woken, nil
}

// QueueSweepStore is the interface QueueSweepLoop needs — narrowed from
// *TaskWorkflowService so the loop can be unit tested against a fake.
type QueueSweepStore interface {
	SweepWake(ctx context.Context, now time.Time) ([]string, error)
	// SweepTriage is 決定15/16's periodic evaluation (PR-5b, triage_done.go).
	// It shares this loop rather than getting a timer of its own: both are
	// decision-only passes over the same task set, and running them on one
	// tick keeps "when does the queue re-evaluate itself" a single answer.
	SweepTriage(ctx context.Context, now time.Time) (TriageSweepResult, error)
	// SweepReopen is I-5's periodic evaluation (docs/plans/ingestion-identity.md
	// PR-5, B-6). Shares this SAME loop rather than a timer of its own per the
	// design doc's own placement instruction ("評価契機は QueueSweepLoop —
	// 決定15のSweepDoneと同じ場所に並べる").
	SweepReopen(ctx context.Context, now time.Time) (ReopenSweepResult, error)
}

// QueueSweepLoop periodically calls SweepWake, mirroring
// orchestrator.GCLoop's shape (internal/orchestrator/gc_loop.go) — same
// InitialDelay-then-Interval idiom, errors logged and never fatal to the
// loop.
type QueueSweepLoop struct {
	Store        QueueSweepStore
	Interval     time.Duration
	InitialDelay time.Duration

	// lastBreachFingerprint is the previously-reported 決定16 breach set, so
	// the report fires on change instead of on every tick. Only touched from
	// runOnce, which the loop calls sequentially.
	lastBreachFingerprint string
}

// Run blocks until ctx is done. It waits InitialDelay before the first
// sweep, then calls Store.SweepWake every Interval.
func (l *QueueSweepLoop) Run(ctx context.Context) {
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

func (l *QueueSweepLoop) runOnce(ctx context.Context) {
	now := time.Now()

	woken, err := l.Store.SweepWake(ctx, now)
	if err != nil {
		slog.Warn("queue wake sweep failed", "error", err)
		// Deliberately NOT a return: the done sweep and the canonical-source
		// report are independent of wake evaluation, and a wake failure must
		// not silently stop cards from finishing.
	} else if len(woken) > 0 {
		slog.Info("queue wake sweep woke tasks", "count", len(woken), "task_ids", woken)
	}

	result, err := l.Store.SweepTriage(ctx, now)
	if err != nil {
		slog.Warn("queue triage sweep failed", "error", err)
	} else {
		if len(result.Completed) > 0 {
			slog.Info("queue triage sweep completed tasks", "count", len(result.Completed), "task_ids", result.Completed)
		}
		l.logCanonicalSourceBreaches(result.Breaches)
	}

	// docs/plans/ingestion-identity.md PR-5 (B-6): I-5's auto-reopen, on the
	// SAME tick as the two sweeps above — deliberately NOT gated behind
	// either of their errors (Opus review pattern already established for
	// wake vs triage above): a reopen sweep failure must not depend on the
	// unrelated done/breach sweep having succeeded, and vice versa.
	reopenResult, reopenErr := l.Store.SweepReopen(ctx, now)
	if reopenErr != nil {
		slog.Warn("queue reopen sweep failed", "error", reopenErr)
		return
	}
	if len(reopenResult.Reopened) > 0 {
		slog.Info("queue reopen sweep reopened tasks", "count", len(reopenResult.Reopened), "task_ids", reopenResult.Reopened)
	}
}

// logCanonicalSourceBreaches reports 決定16 breaches ONLY WHEN THE SET CHANGES.
//
// Two reasons, both about this loop's one-minute tick. A breach persists until
// someone fixes the card (queue 節 rule 7's 「解消されるまで出し続ける」 is about
// the queue 案内 card, not about a log line), so a per-tick line would emit
// 1,440 times a day per unfixed card. Worse, until khi starts delivering
// observed.source_closed — future work in the same 決定14 cutover — EVERY card
// is a breach, so an unconditional line would bury every other daemon log from
// the moment this ships (Opus review round 2, Medium). Logging on change keeps
// the signal (a new breach appears, or the last one clears) without the flood.
func (l *QueueSweepLoop) logCanonicalSourceBreaches(breaches []CanonicalSourceBreach) {
	ids := make([]string, 0, len(breaches))
	for _, b := range breaches {
		ids = append(ids, b.TaskID)
	}
	sort.Strings(ids)
	fingerprint := strings.Join(ids, ",")
	if fingerprint == l.lastBreachFingerprint {
		return
	}
	l.lastBreachFingerprint = fingerprint
	if len(breaches) == 0 {
		slog.Info("triage tasks with no canonical source: すべて解消しました (決定16)")
		return
	}
	slog.Warn("triage tasks with no canonical source (決定16): これらは done 自動落ちに到達できません",
		"count", len(breaches), "task_ids", ids, "guidance", breaches[0].Guidance)
}
