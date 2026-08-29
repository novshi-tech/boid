package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// defaultWaitPollInterval is how often WaitTaskTerminal re-reads the task row
// while waiting. Sized against what actually gets waited on: a trigger's
// judgment task runs for minutes, so a second of latency on noticing it
// finished is invisible, while the read itself is a single indexed SQLite
// lookup. Deliberately a poll rather than a wakeup registry (the shape
// BlockingAskRegistry uses): an answer is a transient event with no other
// trace, so it MUST be delivered to a waiter, whereas a task's terminal status
// is durable state — polling it cannot miss a wakeup, needs no hook in any of
// the several code paths that can land a task in a terminal status, and
// recovers on its own if one of them is ever added without knowing about this.
const defaultWaitPollInterval = time.Second

// TaskOutcome is how a waited-on task ended. AbortCode/AbortMessage are the
// abort action's payload (abortOnDispatchError writes "dispatch_error" /
// "daemon_shutdown"; a session failure routed through job_failed carries no
// payload and so leaves both empty) and are set only when Status is aborted.
type TaskOutcome struct {
	Status       orchestrator.TaskStatus
	AbortCode    string
	AbortMessage string
}

// Succeeded reports whether the task ended the way the caller wanted. Only
// `done` counts: this is what `boid task wait`'s exit code is derived from, and
// a trigger script's contract is "fail if the task you started did not
// succeed".
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
// Only ctx cancellation ends the wait early — the broker cancels on daemon
// shutdown and on sandbox disconnect, so a caller cannot leak this goroutine.
// A trigger that must not wait forever bounds it from the outside.
func (s *TaskAppService) WaitTaskTerminal(ctx context.Context, taskID string) (TaskOutcome, error) {
	if taskID == "" {
		return TaskOutcome{}, &StatusError{Code: http.StatusBadRequest, Message: "task id is required"}
	}
	if s.Tasks == nil {
		return TaskOutcome{}, &StatusError{Code: http.StatusInternalServerError, Message: "task store is not configured"}
	}

	interval := s.WaitPollInterval
	if interval <= 0 {
		interval = defaultWaitPollInterval
	}

	var ticker *time.Ticker
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()

	for {
		task, err := s.Tasks.GetTask(taskID)
		if err != nil {
			// A task id that does not resolve is reported rather than waited
			// on: a task cannot come into existence under an id the caller
			// already holds, so retrying would just hold the caller open until
			// something else killed it.
			return TaskOutcome{}, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
		}
		if orchestrator.IsTerminalStatus(task.Status) {
			outcome := TaskOutcome{Status: task.Status}
			if task.Status == orchestrator.TaskStatusAborted {
				outcome.AbortCode, outcome.AbortMessage = s.latestAbortReason(taskID)
			}
			return outcome, nil
		}

		if ticker == nil {
			ticker = time.NewTicker(interval)
		}
		select {
		case <-ctx.Done():
			return TaskOutcome{}, ctx.Err()
		case <-ticker.C:
		}
	}
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
// when there is nothing to say. Shared by the host CLI and the brokered op so
// both report a failed round identically.
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
