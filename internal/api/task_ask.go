package api

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

// AskTaskBlocking implements the harness-independent blocking Q&A RPC behind
// `boid task ask <question>`. The agent stays alive and blocks inside this call
// until the user/supervisor answers, so the round-trip works for every harness
// uniformly — there is no session-resume path anywhere in boid (the legacy
// `notify --ask` → `claude --resume` flow was removed).
//
// The call is disconnect-resilient and idempotent across re-asks. Harnesses kill
// long-running shell commands on a command-timeout (claude-code / opencode at
// ~120s), which severs the held connection. When that happens the model retries
// the identical `boid task ask`; this handler recognises the retry and recovers
// instead of treating it as a fresh question:
//
//   - executing  → fresh ask: register the answer channel, transition
//     executing → awaiting via ApplyAction("ask"), fire the user notification
//     (root tasks only; child tasks are seen by their supervisor's monitor
//     loop), then block in Wait.
//   - awaiting    → re-ask: if an answer was parked durably while the agent was
//     disconnected (PendingAnswer), consume it immediately and flip back to
//     executing. Otherwise re-attach to the SAME question id and block again —
//     no second "ask" transition, no new notification.
//
// Register happens before the awaiting transition so an answer that races in
// immediately afterwards is never dropped. Decision B1 (relaxed): a concurrent
// ask carrying a DIFFERENT question id still fails with ErrAskPending; a re-ask
// of the same question is a re-attach and is allowed.
func (s *TaskAppService) AskTaskBlocking(ctx context.Context, taskID, question string) (string, error) {
	if s.BlockingAsk == nil {
		return "", &StatusError{Code: http.StatusInternalServerError, Message: "blocking ask is not configured"}
	}
	if s.Workflow == nil {
		return "", &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}
	if question == "" {
		return "", &StatusError{Code: http.StatusBadRequest, Message: "question is required"}
	}
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return "", &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	// Re-ask path: the task is already awaiting from a prior ask whose foreground
	// command was killed by a harness command-timeout.
	if task.Status == orchestrator.TaskStatusAwaiting {
		ap := orchestrator.GetAwaitingPayload(task.Payload)
		if ap.QuestionID == "" {
			return "", &StatusError{
				Code:    http.StatusConflict,
				Message: "task is awaiting but has no pending question to attach to",
			}
		}
		if ap.PendingAnswer != "" {
			// An answer arrived while the agent was disconnected. Deliver it now.
			return s.consumePendingAnswer(task, ap.PendingAnswer)
		}
		// Re-attach to the existing question and block again (no fresh ask).
		if err := s.BlockingAsk.Register(taskID, ap.QuestionID); err != nil {
			return "", &StatusError{Code: http.StatusConflict, Message: err.Error()}
		}
		defer s.BlockingAsk.Cancel(ap.QuestionID)
		return s.blockForAnswer(ctx, taskID, ap.QuestionID)
	}

	if task.Status != orchestrator.TaskStatusExecuting {
		return "", &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("task is not executing (status: %s); cannot ask", task.Status),
		}
	}

	// Fresh ask: executing → awaiting.
	qid := newQuestionID()
	if err := s.BlockingAsk.Register(taskID, qid); err != nil {
		// ErrAskPending (B1) and any other registration failure surface as a
		// conflict so the agent's `boid task ask` exits non-zero with the reason.
		return "", &StatusError{Code: http.StatusConflict, Message: err.Error()}
	}
	defer s.BlockingAsk.Cancel(qid)

	ap := orchestrator.AwaitingPayload{
		Question:   question,
		QuestionID: qid,
	}
	apJSON, err := json.Marshal(ap)
	if err != nil {
		return "", &StatusError{Code: http.StatusInternalServerError, Message: "encode awaiting payload: " + err.Error()}
	}
	askPayload, err := json.Marshal(map[string]json.RawMessage{string(orchestrator.TraitAwaiting): apJSON})
	if err != nil {
		return "", &StatusError{Code: http.StatusInternalServerError, Message: "encode action payload: " + err.Error()}
	}
	if _, err := s.Workflow.ApplyAction(ctx, taskID, ApplyActionRequest{Type: "ask", Payload: askPayload}); err != nil {
		return "", err
	}

	// Surface the question to whoever owns this task. Best-effort: a failed
	// notification must not abort an otherwise-valid ask (the awaiting state is
	// already persisted and pollable).
	s.fireUserAskNotification(ctx, task, question, qid)

	return s.blockForAnswer(ctx, taskID, qid)
}

// blockForAnswer parks until an answer is delivered over the registry (fast
// path) or ctx is cancelled (daemon shutdown / agent disconnect). On answer it
// returns it — the awaiting → executing flip was already done by answerBlocking's
// fast path.
//
// On cancellation it does NOT abort immediately: a disconnect is almost always a
// harness command-timeout killing the foreground `boid task ask`, after which
// the model re-asks and re-attaches (the task must survive that gap). Instead it
// schedules a grace-period reaper; only if the task is still awaiting on the same
// dead question — no re-attach, no parked answer — when the grace expires is it
// reclaimed.
func (s *TaskAppService) blockForAnswer(ctx context.Context, taskID, qid string) (string, error) {
	answer, err := s.BlockingAsk.Wait(ctx, qid)
	if err != nil {
		s.scheduleGraceAbort(taskID, qid)
		return "", err
	}
	return answer, nil
}

// defaultAskDisconnectGrace bounds how long an awaiting task may sit with no
// live agent before the reaper reclaims it, when AskDisconnectGrace is unset.
const defaultAskDisconnectGrace = 30 * time.Minute

// scheduleGraceAbort arms a one-shot reaper for a disconnected blocking ask.
// After the grace period graceAbortCheck decides whether the task is a genuine
// zombie. The timer is in-process only; a daemon restart drops it, but startup
// MarkStaleAwaitingTasksAborted reclaims any awaiting leftovers, so coverage is
// complete across both running and restarted daemons.
func (s *TaskAppService) scheduleGraceAbort(taskID, qid string) {
	grace := s.AskDisconnectGrace
	if grace <= 0 {
		grace = defaultAskDisconnectGrace
	}
	time.AfterFunc(grace, func() { s.graceAbortCheck(taskID, qid) })
}

// graceAbortCheck reclaims a task whose blocking ask disconnected and never
// recovered. It aborts only when ALL hold: the task is still awaiting, on the
// SAME question id (no new ask episode began), with no parked answer (it would
// be consumed on the next ask) and no live agent re-attached (Has). Any of those
// being false means the ask recovered or moved on, so it is left alone.
func (s *TaskAppService) graceAbortCheck(taskID, qid string) {
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return
	}
	if task.Status != orchestrator.TaskStatusAwaiting {
		return // answered (executing), aborted, or otherwise moved on
	}
	ap := orchestrator.GetAwaitingPayload(task.Payload)
	if ap.QuestionID != qid {
		return // a different ask episode is now in flight
	}
	if ap.PendingAnswer != "" {
		return // an answer is parked; the next re-ask will consume it
	}
	if s.BlockingAsk != nil && s.BlockingAsk.Has(qid) {
		return // an agent re-attached and is live
	}
	s.abortDanglingAsk(taskID, errors.New("blocking ask grace period expired with no live agent"))
}

// consumePendingAnswer delivers a durably-parked answer to a re-asking agent: it
// flips the task awaiting → executing, strips the awaiting trait (removing the
// parked answer), records the audit action, and returns the answer. Used when an
// answer arrived while the agent was disconnected and the agent has re-asked.
func (s *TaskAppService) consumePendingAnswer(task *orchestrator.Task, answer string) (string, error) {
	fromStatus := task.Status
	task.Status = orchestrator.TaskStatusExecuting
	task.Payload = orchestrator.StripAwaitingTrait(task.Payload)
	if err := s.Tasks.UpdateTask(task); err != nil {
		return "", &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	s.recordAnswerAction(task.ID, fromStatus)
	return answer, nil
}

// recordAnswerAction writes the awaiting → executing "answer" audit action.
// Best-effort: a failure is logged, never returned.
func (s *TaskAppService) recordAnswerAction(taskID string, fromStatus orchestrator.TaskStatus) {
	if s.Actions == nil {
		return
	}
	if err := s.Actions.CreateAction(&orchestrator.Action{
		TaskID:     taskID,
		Type:       "answer",
		FromStatus: fromStatus,
		ToStatus:   orchestrator.TaskStatusExecuting,
	}); err != nil {
		slog.Warn("blocking answer: record answer action failed", "task_id", taskID, "error", err)
	}
}

// answerBlocking resolves a blocking ask (called from AnswerTask). There are two
// delivery paths, chosen by whether a live agent is currently parked:
//
//   - Fast path (agent connected): Notify hands the answer to the agent blocked
//     in blockForAnswer and we flip the task awaiting → executing immediately.
//     The agent resumes over the held connection; its next move (e.g. notify
//     --done) requires executing, so the flip must precede return.
//   - Slow path (agent disconnected): Notify finds no waiter because the agent's
//     `boid task ask` was killed by a harness command-timeout. We park the
//     answer durably in PendingAnswer and leave the task awaiting. The agent
//     picks it up on its next ask (consumePendingAnswer) — the answer is never
//     dropped and the task is never flipped to a zombie executing state with no
//     live agent behind it.
//
// The task pointer is the one returned by GetTask; mutating + UpdateTask
// persists it.
func (s *TaskAppService) answerBlocking(task *orchestrator.Task, answer string) error {
	if s.BlockingAsk == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "blocking ask is not configured"}
	}
	qid := orchestrator.GetAwaitingPayload(task.Payload).QuestionID

	if s.BlockingAsk.Notify(qid, answer) {
		// Fast path: a live agent received the answer in-memory.
		fromStatus := task.Status
		task.Status = orchestrator.TaskStatusExecuting
		task.Payload = orchestrator.StripAwaitingTrait(task.Payload)
		if err := s.Tasks.UpdateTask(task); err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		s.recordAnswerAction(task.ID, fromStatus)
		return nil
	}

	// Slow path: no live waiter. Park the answer durably; the agent collects it
	// on its next ask. The task stays awaiting until then.
	task.Payload = orchestrator.SetPendingAnswer(task.Payload, answer)
	if err := s.Tasks.UpdateTask(task); err != nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	slog.Info("blocking answer parked durably (no live agent); will deliver on the agent's next ask",
		"task_id", task.ID, "question_id", qid)
	return nil
}

// fireUserAskNotification sends the user-facing Q&A notification for a blocking
// ask. Mirrors NotifyTask's ask branch but is deliberately separate so the
// blocking path never calls StopAgent (the agent must stay alive). Child tasks
// (parent_id != "") never page the user — their supervisor's monitoring loop
// notices the awaiting transition — matching the lifecycle-accountability gate.
func (s *TaskAppService) fireUserAskNotification(ctx context.Context, task *orchestrator.Task, question, questionID string) {
	if task.ParentID != "" || s.Notify == nil {
		return
	}
	ev := notify.Event{
		TaskID:    task.ID,
		TaskTitle: task.Title,
		ProjectID: task.ProjectID,
		Message:   question,
		URLPath:   "/tasks/" + task.ID + "/questions/" + questionID,
	}
	if s.Projects != nil {
		if proj, lookupErr := s.Projects.GetProject(task.ProjectID); lookupErr == nil && proj != nil {
			ev.ProjectName = proj.Meta.Name
		}
	}
	if err := s.Notify.Notify(ctx, ev); err != nil {
		slog.Warn("blocking ask: user notification failed", "task_id", task.ID, "error", err)
	}
}

// abortDanglingAsk transitions a task out of awaiting to aborted when its
// blocking ask was cancelled (daemon shutdown / agent disconnect). Best-effort:
// any failure is logged, never returned, because the original Wait error is
// what the caller acts on. Uses context.Background() since the inbound ctx is
// already cancelled and the abort transition must still run.
func (s *TaskAppService) abortDanglingAsk(taskID string, cause error) {
	if s.Workflow == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"code":    "ask_canceled",
		"message": "blocking ask was canceled (daemon shutdown or agent disconnect): " + cause.Error(),
	})
	if _, err := s.Workflow.ApplyAction(context.Background(), taskID, ApplyActionRequest{
		Type:    "abort",
		Payload: payload,
	}); err != nil {
		slog.Warn("blocking ask: abort dangling task failed", "task_id", taskID, "error", err)
	}
}
