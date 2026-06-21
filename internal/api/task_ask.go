package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// AskTaskBlocking implements the harness-independent blocking Q&A RPC behind
// `boid task ask <question>`. The agent stays alive and blocks inside this
// call until the user/supervisor answers, so the round-trip works for every
// harness uniformly — there is no session-resume path anywhere in boid
// anymore (the legacy `notify --ask` → `claude --resume` flow was removed).
//
// Flow:
//  1. Register the answer channel (decision B1: a second pending ask for the
//     same task fails immediately with ErrAskPending).
//  2. Transition executing → awaiting via ApplyAction("ask") with a
//     blocking-mode awaiting payload, then fire the user notification (root
//     tasks only; child tasks are seen by their supervisor's monitor loop).
//  3. Block in the registry's Wait until an answer arrives or ctx is cancelled.
//  4. On answer, return it to the caller (the broker writes it back to the
//     sandbox over the held connection). On ctx cancellation (daemon shutdown
//     or agent disconnect), abort the task so it is not left dangling in
//     awaiting forever (decision C1: there is no timeout, only cancellation).
//
// Register happens before the awaiting transition so an answer that races in
// immediately afterwards is never dropped.
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
	if task.Status != orchestrator.TaskStatusExecuting {
		return "", &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("task is not executing (status: %s); cannot ask", task.Status),
		}
	}

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

	answer, err := s.BlockingAsk.Wait(ctx, qid)
	if err != nil {
		// ctx cancelled: daemon shutdown or the sandbox disconnected. Abort the
		// task so the owner sees a terminal state instead of a permanent
		// awaiting with no live agent behind it.
		s.abortDanglingAsk(taskID, err)
		return "", err
	}
	return answer, nil
}

// answerBlocking resolves a blocking ask (called from AnswerTask). It hands
// the answer to the agent parked in AskTaskBlocking via the registry and
// flips the task back to executing WITHOUT spawning a resume dispatch — the
// agent never exited.
//
// The state flip happens before Notify so the resumed agent always observes the
// executing state (its next move, e.g. notify --done, requires executing). The
// task pointer is the one returned by GetTask; mutating + UpdateTask persists it.
func (s *TaskAppService) answerBlocking(task *orchestrator.Task, answer string) error {
	if s.BlockingAsk == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "blocking ask is not configured"}
	}
	qid := orchestrator.GetAwaitingPayload(task.Payload).QuestionID
	if !s.BlockingAsk.Has(qid) {
		// No agent is parked on this question — it disconnected before the
		// answer arrived (the broker's connection-close watcher should have
		// already aborted the task). Reject rather than flip to a zombie
		// executing state with no live agent behind it.
		return &StatusError{
			Code:    http.StatusConflict,
			Message: "no agent is waiting for this answer (it may have disconnected)",
		}
	}

	fromStatus := task.Status
	task.Status = orchestrator.TaskStatusExecuting
	task.Payload = orchestrator.StripAwaitingTrait(task.Payload)
	if err := s.Tasks.UpdateTask(task); err != nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if s.Actions != nil {
		if err := s.Actions.CreateAction(&orchestrator.Action{
			TaskID:     task.ID,
			Type:       "answer",
			FromStatus: fromStatus,
			ToStatus:   orchestrator.TaskStatusExecuting,
		}); err != nil {
			slog.Warn("blocking answer: record answer action failed", "task_id", task.ID, "error", err)
		}
	}
	if !s.BlockingAsk.Notify(qid, answer) {
		// Raced with the agent disconnecting between the Has check and here. The
		// task is already executing again; the owner's stuck-task detection will
		// catch an executing task with no live job.
		slog.Warn("blocking answer: no waiter for question at delivery", "task_id", task.ID, "question_id", qid)
	}
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
