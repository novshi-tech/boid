package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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
}

// QueueSweepLoop periodically calls SweepWake, mirroring
// orchestrator.GCLoop's shape (internal/orchestrator/gc_loop.go) — same
// InitialDelay-then-Interval idiom, errors logged and never fatal to the
// loop.
type QueueSweepLoop struct {
	Store        QueueSweepStore
	Interval     time.Duration
	InitialDelay time.Duration
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
	woken, err := l.Store.SweepWake(ctx, time.Now())
	if err != nil {
		slog.Warn("queue wake sweep failed", "error", err)
		return
	}
	if len(woken) > 0 {
		slog.Info("queue wake sweep woke tasks", "count", len(woken), "task_ids", woken)
	}
}
