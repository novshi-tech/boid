package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// defaultWaitPollInterval is how often WaitTaskTerminal re-reads a task's status
// while waiting. Sized against what actually gets waited on: a judgment task
// runs for minutes, so a second of latency on noticing it finished is
// invisible.
//
// Deliberately a poll rather than a wakeup registry (the shape
// BlockingAskRegistry uses): an answer is a transient event with no other
// trace, so it MUST be delivered to a waiter, whereas a task's terminal status
// is durable state — polling it cannot miss a wakeup, needs no hook in any of
// the several code paths that can land a task in a terminal status, and
// recovers on its own if one of them is ever added without knowing about this.
//
// What makes the poll affordable is TaskStatusReader below, NOT the interval:
// orchestrator.GetTask costs five unindexed scans of the tasks table (its
// taskChildCountCols subqueries) on a pool that is one connection, so polling
// it at any interval taxes every other daemon operation.
const defaultWaitPollInterval = time.Second

// TaskStatusReader is the narrow re-read WaitTaskTerminal polls with, kept
// separate from TaskStore so adding it did not have to touch every
// implementation of that much larger interface.
//
// *orchestrator.TaskRepository is the production implementation and the
// compile-time assertion below pins that, rather than leaving it to a runtime
// type check that would silently degrade to the expensive path if the real
// wired type ever stopped satisfying it — the exact failure PR #1014's review
// found for the signals dependency (docs/plans/signal-ingest-detailed-design.md
// PR-3, M1), where every unit test passed against a hand-built double while
// production never picked the interface up at all.
type TaskStatusReader interface {
	GetTaskStatus(id string) (orchestrator.TaskStatus, error)
}

var _ TaskStatusReader = (*orchestrator.TaskRepository)(nil)

// TaskOutcome is how a waited-on task ended. AbortCode/AbortMessage carry the
// abort action's payload and are set only when Status is aborted:
// abortOnDispatchError writes "dispatch_error" / "daemon_shutdown", while an
// abort recorded without a payload (job_failed, i.e. a failure inside the
// agent's own session) leaves both empty. Neither is a closed vocabulary — read
// them as decoration for a human, never as a branch condition.
type TaskOutcome struct {
	// ID is the resolved task id (WaitTaskTerminal accepts the same id prefixes
	// orchestrator.GetTask does).
	ID           string
	Status       orchestrator.TaskStatus
	AbortCode    string
	AbortMessage string
}

// Succeeded reports whether the task ended the way the caller wanted. Only
// `done` counts — every other terminal status, including `dropped`, is a
// failure. This is what `boid task wait`'s exit code is derived from, and a
// trigger script's contract is "fail if the task you started did not succeed".
func (o TaskOutcome) Succeeded() bool {
	return o.Status == orchestrator.TaskStatusDone
}

// WaitTaskTerminal blocks until taskID reaches a terminal status and reports
// how it ended.
//
// This is the daemon half of `boid task wait <id>`, which exists so a trigger's
// `run:` command can live exactly as long as the task it started. That single
// change makes the trigger's own machinery cover the task: single-flight stops
// measuring a launcher that exits in seconds and starts measuring the work, and
// a failed round arrives at TriggerLoop.trackFailStreak as an ordinary non-zero
// exit instead of having to be re-derived by the workspace from the action log.
//
// Only ctx cancellation ends the wait early. The broker gives a blocking op a
// context tied to its connection (sandbox.isBlockingBoidRequest — task_wait must
// stay listed there or an abandoned wait runs until daemon shutdown), so a
// sandbox that dies unblocks this. It is NOT a timeout: a task that parks in a
// non-terminal resting state (`awaiting` on a question nobody answers,
// `pending` with auto_start off) waits indefinitely. Bounding a round's total
// duration is the trigger's job, not this call's.
func (s *TaskAppService) WaitTaskTerminal(ctx context.Context, taskID string) (TaskOutcome, error) {
	if taskID == "" {
		return TaskOutcome{}, &StatusError{Code: http.StatusBadRequest, Message: "task id is required"}
	}
	if s.Tasks == nil {
		return TaskOutcome{}, &StatusError{Code: http.StatusInternalServerError, Message: "task store is not configured"}
	}

	// Resolve once through the full lookup: it is the only read here that
	// accepts an id prefix, and it is what turns an unresolvable id into an
	// error instead of an endless wait. Every subsequent read is the narrow one
	// against the id it returned.
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return TaskOutcome{}, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	resolvedID := task.ID
	if orchestrator.IsTerminalStatus(task.Status) {
		return s.outcomeOf(resolvedID, task.Status), nil
	}

	interval := s.WaitPollInterval
	if interval <= 0 {
		interval = defaultWaitPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return TaskOutcome{}, ctx.Err()
		case <-ticker.C:
		}

		status, err := s.readStatus(resolvedID)
		if err != nil {
			if errors.Is(err, orchestrator.ErrTaskNotFound) {
				// The row is gone (deleted / GC'd) — it will not come back
				// under this id, so waiting on it is pointless.
				return TaskOutcome{}, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
			}
			// Any other read failure is transient by assumption — the pool is a
			// single connection, so a busy-timeout under a dispatch or GC burst
			// is an ordinary thing to see. Reporting it would end the wait,
			// fail the caller's job, release the trigger's single-flight and
			// let the NEXT tick start a second concurrent round of work that is
			// still running — the exact property this op exists to establish,
			// undone by one unlucky read. Log it and read again.
			slog.Warn("task wait: could not read task status, retrying",
				"task_id", resolvedID, "error", err)
			continue
		}
		if orchestrator.IsTerminalStatus(status) {
			return s.outcomeOf(resolvedID, status), nil
		}
	}
}

// readStatus performs the per-poll re-read, preferring the narrow
// TaskStatusReader and falling back to the full GetTask for a TaskStore that
// does not implement it (test doubles, in practice — see TaskStatusReader's own
// doc comment for why production is pinned at compile time instead).
func (s *TaskAppService) readStatus(taskID string) (orchestrator.TaskStatus, error) {
	if reader, ok := s.Tasks.(TaskStatusReader); ok {
		return reader.GetTaskStatus(taskID)
	}
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return "", err
	}
	return task.Status, nil
}

func (s *TaskAppService) outcomeOf(taskID string, status orchestrator.TaskStatus) TaskOutcome {
	outcome := TaskOutcome{ID: taskID, Status: status}
	if status == orchestrator.TaskStatusAborted {
		outcome.AbortCode, outcome.AbortMessage = s.latestAbortReason(taskID)
	}
	return outcome
}

// latestAbortReason returns the code/message of the MOST RECENT abort in
// taskID's action list.
//
// Deliberately not orchestrator.DeriveLifecycle: that keeps the FIRST abort
// (lifecycle.go's `lc.Abort == nil` guard) and, unlike the done/fail reports
// beside it, never clears it on a transition back to executing. That is right
// for a task that aborts once and stays terminal, and wrong for one that is
// reopened — it would report an abort from an arbitrarily old cycle as the
// reason this round failed.
//
// Failures are swallowed on purpose: the caller already has the real answer
// (the task aborted), and the reason is decoration for a human reading the
// trigger job's log. Turning "could not read the action list" into an error
// would convert a correctly-detected failure into a different, misleading one.
func (s *TaskAppService) latestAbortReason(taskID string) (code, message string) {
	if s.Actions == nil {
		return "", ""
	}
	actions, err := s.Actions.ListActionsByTask(taskID)
	if err != nil {
		slog.Warn("task wait: could not read the action list for the abort reason",
			"task_id", taskID, "error", err)
		return "", ""
	}
	// ListActionsByTask is ordered created_at ASC, so the last matching row is
	// the most recent abort.
	for i := len(actions) - 1; i >= 0; i-- {
		a := actions[i]
		if a == nil || a.ToStatus != orchestrator.TaskStatusAborted {
			continue
		}
		var payload struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if len(a.Payload) > 0 {
			if err := json.Unmarshal(a.Payload, &payload); err != nil {
				slog.Warn("task wait: abort payload is not readable",
					"task_id", taskID, "action_id", a.ID, "error", err)
			}
		}
		return payload.Code, payload.Message
	}
	return "", ""
}

// FormatAbortReason renders an outcome's abort reason as one human line, or ""
// when there is nothing to say — which is the common case for a failure inside
// the agent's own session, since job_failed records no payload.
func FormatAbortReason(o TaskOutcome) string {
	switch {
	case o.AbortCode != "" && o.AbortMessage != "":
		return fmt.Sprintf("%s: %s", o.AbortCode, o.AbortMessage)
	case o.AbortCode != "":
		return o.AbortCode
	default:
		return o.AbortMessage
	}
}
