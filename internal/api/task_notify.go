package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// NotifyTask invokes the configured notify command for taskID. Returns 501
// when no notifier is wired and ask is empty. With ask set, the task
// transitions to awaiting (notification is best-effort); questionID
// identifies the Q&A turn (generated when empty). With progress set, only
// a progress Action is written — no hook fires, no state transition. ask
// and progress are mutually exclusive.
//
// ask does NOT spawn a resume dispatch when the user replies — only the
// blocking `boid task ask` RPC can deliver an answer back to a live agent.
//
// Every Action this writes is stamped orchestrator.ActorTask(taskID), even
// when called from the host-side HTTP route by a human operator: this call
// always represents the task's own self-report.
func (s *TaskAppService) NotifyTask(ctx context.Context, taskID, message, ask, questionID, progress, done, fail string) error {
	// ask / progress / done / fail are mutually exclusive: each represents a
	// distinct lifecycle signal (Q&A pause, FYI-only progress, success
	// self-report, failure self-report). Allowing more than one would
	// ambiguate which state transition (if any) to fire.
	modes := 0
	for _, m := range []string{ask, progress, done, fail} {
		if m != "" {
			modes++
		}
	}
	if modes > 1 {
		return &StatusError{Code: http.StatusBadRequest, Message: "--ask, --progress, --done, --fail are mutually exclusive"}
	}
	if message == "" && progress == "" {
		return &StatusError{Code: http.StatusBadRequest, Message: "message is required"}
	}

	// Progress mode: write a timeline Action directly, skip hook firing entirely.
	// Progress is a pure observability event with no user-facing surface, so the
	// parent_id gate below does not apply — both root and child tasks can record
	// progress without further checks.
	if progress != "" {
		task, err := s.Tasks.GetTask(taskID)
		if err != nil {
			return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
		}
		// Action payload size cap: this writes actions.payload directly via
		// s.Actions.CreateAction, bypassing ApplyAction's built-in check, so
		// this call site needs its own — progress is an agent-supplied free
		// string, not a daemon-generated fixed format.
		if err := orchestrator.ValidateContentSize("action payload", []byte(progress)); err != nil {
			return &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		payload, err := json.Marshal(map[string]string{"message": progress})
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "encode progress payload: " + err.Error()}
		}
		action := &orchestrator.Action{
			TaskID:     taskID,
			Type:       "progress",
			FromStatus: task.Status,
			ToStatus:   task.Status,
			Payload:    payload,
			Actor:      orchestrator.ActorTask(taskID),
		}
		if err := s.Actions.CreateAction(ctx, action); err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		s.broadcastNotifyAction(task, action)
		return nil
	}
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	// Lifecycle-accountability hard gate: only root tasks (parent_id == "")
	// fire user-facing notify hooks. Child tasks signal their parent
	// supervisor via the awaiting transition (ask mode) or are silently
	// dropped (FYI mode) — a daemon-level invariant so a project author
	// can't accidentally make a child task page the user.
	fireUserNotify := task.ParentID == ""

	// Without ask, a working notifier is required to surface the FYI — but only
	// when we would actually fire the hook. Child tasks skip the hook unconditionally,
	// so a missing notifier is fine.
	if s.Notify == nil && ask == "" && fireUserNotify {
		return &StatusError{Code: http.StatusNotImplemented, Message: "notify is not configured"}
	}

	// Lifecycle signal modes (ask / done / fail) all advance the task state
	// machine and SIGTERM running hook runtimes. Plain FYI notify (none of
	// those flags) only fires the user-notify hook for root tasks.
	signalsTransition := ask != "" || done != "" || fail != ""

	ev := notify.Event{
		TaskID:    taskID,
		TaskTitle: task.Title,
		ProjectID: task.ProjectID,
		Message:   message,
	}
	// Deep-link target depends on mode:
	//   ask  → Q&A turn page (reply form)
	//   done → task detail (success outcome to inspect)
	//   fail → task detail (failure outcome to inspect / decide reopen)
	//   FYI  → most recent interactive running job (live session attach)
	switch {
	case ask != "":
		if questionID == "" {
			questionID = newQuestionID()
		}
		ev.URLPath = "/tasks/" + taskID + "/questions/" + questionID
	case done != "" || fail != "":
		ev.URLPath = "/tasks/" + taskID
	}
	// Project name is best-effort: omit silently if Projects lookup fails or is unwired.
	if s.Projects != nil {
		if proj, lookupErr := s.Projects.GetProject(task.ProjectID); lookupErr == nil && proj != nil {
			ev.ProjectName = proj.Meta.Name
		}
	}
	// FYI mode only: find the most recent interactive running job so the
	// notification deep-links to the live session. ask/done/fail set
	// URLPath above to a more specific destination.
	if !signalsTransition && s.Jobs != nil {
		if jobs, jobsErr := s.Jobs.ListJobsByTask(taskID); jobsErr == nil {
			for i := len(jobs) - 1; i >= 0; i-- {
				j := jobs[i]
				if j.Status == JobStatusRunning && j.Interactive {
					ev.JobID = j.ID
					break
				}
			}
		}
	}
	if fireUserNotify && s.Notify != nil {
		if err := s.Notify.Notify(ctx, ev); err != nil {
			if !signalsTransition {
				return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
			}
			slog.Warn("notify: notification failed in signal mode, continuing with state transition", "error", err, "mode", notifyModeName(ask, done, fail))
		}
	} else if !fireUserNotify {
		slog.Debug("notify: skipped user-facing hook (child task, owner is parent supervisor)",
			"task_id", taskID, "parent_id", task.ParentID, "mode", notifyModeName(ask, done, fail))
	}

	if !signalsTransition {
		return nil
	}

	// Lifecycle signal: persist the agent's intent and stop the running jobs.
	//
	// --ask goes through ApplyAction(ask) synchronously: the agent expects
	// the task to be visibly `awaiting` as soon as the call returns.
	//
	// --done / --fail record a `done_request`/`fail_request` action
	// directly, WITHOUT ApplyAction. The actual transition fires later via
	// the condition-based auto rule, once the runtime has cleanly exited and
	// `boid job done` lands through the broker — this avoids a race where
	// ApplyAction(done)'s dispatch loop would SIGTERM the still-running
	// runtime and leave the job marked failed.
	if s.Workflow == nil {
		return &StatusError{Code: http.StatusInternalServerError, Message: "workflow service not configured"}
	}
	if ask != "" {
		ap := orchestrator.AwaitingPayload{
			Question:   ask,
			QuestionID: questionID,
		}
		apJSON, err := json.Marshal(ap)
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "encode awaiting payload: " + err.Error()}
		}
		askPayload, err := json.Marshal(map[string]json.RawMessage{string(orchestrator.TraitAwaiting): apJSON})
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "encode action payload: " + err.Error()}
		}
		if _, err := s.Workflow.ApplyAction(orchestrator.WithActor(ctx, orchestrator.ActorTask(taskID)), taskID, ApplyActionRequest{
			Type:    "ask",
			Payload: askPayload,
		}); err != nil {
			return err
		}
	} else {
		// done / fail: record the intent as a non-transitioning action.
		// The dispatch loop's auto-advance picks it up once the runtime exits.
		if s.Actions == nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "action store not configured"}
		}
		if task.Status != orchestrator.TaskStatusExecuting {
			return &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf("task is not executing (status: %s); cannot record %s_request", task.Status, notifyModeName(ask, done, fail))}
		}
		var actionType, msg string
		if done != "" {
			actionType, msg = "done_request", done
		} else {
			actionType, msg = "fail_request", fail
		}
		// Action payload size cap (same reasoning as the progress-mode check
		// above: this call bypasses ApplyAction). Checked before
		// recording the completion request.
		if err := orchestrator.ValidateContentSize("action payload", []byte(msg)); err != nil {
			return &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		if done != "" {
			// Anti-confabulation gate: reject premature / fabricated done
			// reports before recording the intent (see verifyDoneClaim). The
			// agent's notify call fails loudly so it must actually wait /
			// re-verify instead of the daemon silently accepting fiction.
			if verr := s.verifyDoneClaim(ctx, task); verr != nil {
				return verr
			}
		}
		payload, err := json.Marshal(map[string]string{"message": msg})
		if err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: "encode " + actionType + " payload: " + err.Error()}
		}
		action := &orchestrator.Action{
			TaskID:     taskID,
			Type:       actionType,
			FromStatus: task.Status,
			ToStatus:   task.Status,
			Payload:    payload,
			Actor:      orchestrator.ActorTask(taskID),
		}
		if err := s.Actions.CreateAction(ctx, action); err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		s.broadcastNotifyAction(task, action)
	}

	// Stop the agent of each running hook job gracefully. StopAgent delivers
	// SIGUSR1 to the runtime pgrp, which the adapter translates into a
	// SIGTERM toward the child process; the runner still posts `boid job
	// done` through the broker afterward.
	//
	// We deliberately do NOT call CompleteJob preemptively here: its
	// finalize releases the broker token, which would reject the runner's
	// follow-up `boid job done` as "invalid token" and silently drop the
	// job's output. The runner remains the sole CompleteJob caller.
	if s.Jobs != nil {
		jobs, err := s.Jobs.ListJobsByTask(taskID)
		if err == nil {
			for _, j := range jobs {
				if j.Status != JobStatusRunning {
					continue
				}
				if j.RuntimeID == "" {
					continue
				}
				s.Workflow.StopAgent(j.RuntimeID)
			}
		} else {
			slog.Warn("notify: list running jobs failed", "task_id", taskID, "mode", notifyModeName(ask, done, fail), "error", err)
		}
	}
	return nil
}

// broadcastNotifyAction broadcasts a NotifyTask self-report to task's own
// SSE subscribers, then fans it out to its parent card, if any.
func (s *TaskAppService) broadcastNotifyAction(task *orchestrator.Task, action *orchestrator.Action) {
	if s.Hub == nil {
		return
	}
	s.Hub.Broadcast(task.ID, TaskEvent{
		Kind: "action",
		Payload: map[string]any{
			"action_id":  action.ID,
			"new_status": string(action.ToStatus),
		},
	})
	fanOutChildEventToParent(s.Hub, s.Tasks, task, TaskEvent{
		Kind:    "child",
		Payload: childEventPayload(task.ID, action.Type, "action_id", action.ID),
	})
}

// notifyModeName returns a short label identifying which lifecycle signal
// (if any) was supplied to NotifyTask. Used only for slog context.
func notifyModeName(ask, done, fail string) string {
	switch {
	case ask != "":
		return "ask"
	case done != "":
		return "done"
	case fail != "":
		return "fail"
	default:
		return "fyi"
	}
}

// verifyDoneClaim rejects open children and verifies pushed branch tips through the gateway.
func (s *TaskAppService) verifyDoneClaim(ctx context.Context, task *orchestrator.Task) *StatusError {
	if task.OpenChildCount > 0 {
		return &StatusError{
			Code: http.StatusConflict,
			Message: fmt.Sprintf(
				"cannot report done: %d child task(s) are still open (not done/aborted). "+
					"You own your children's lifecycle — wait for every child to reach a "+
					"terminal state (arm a Monitor and stop generating), verify their results, "+
					"then report done.", task.OpenChildCount),
		}
	}

	commit, branch, pushed := releaseClaim(taskPayloadForNotify(task))
	if !pushed || commit == "" || branch == "" || s.VerifyRelease == nil || s.Projects == nil || task.ProjectID == "" {
		return nil
	}
	project, err := s.Projects.GetProject(task.ProjectID)
	if err != nil || project == nil {
		return nil
	}
	matches, err := s.VerifyRelease(ctx, project, branch, commit)
	if err != nil {
		slog.Warn("notify: release verification unavailable", "project_id", project.ID, "error", err)
		return nil
	}
	if !matches {
		return &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf("reported release commit %q is not the remote tip of %s; verify the push and report again", commit, branch)}
	}

	return nil
}

// AnswerTask saves the user's reply and transitions the task awaiting → executing.
func (s *TaskAppService) AnswerTask(ctx context.Context, taskID, questionID, answer string) error {
	if questionID == "" {
		return &StatusError{Code: http.StatusBadRequest, Message: "question_id is required"}
	}
	if answer == "" {
		return &StatusError{Code: http.StatusBadRequest, Message: "answer is required"}
	}
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if task.Status != orchestrator.TaskStatusAwaiting {
		return &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("task is not awaiting (status: %s)", task.Status),
		}
	}

	// The only supported delivery path is the blocking RPC: the agent must
	// be parked inside `boid task ask` for the answer to reach it —
	// otherwise there is no live agent to flip the task back to executing for.
	return s.answerBlocking(ctx, task, answer)
}

// newQuestionID generates a random hex identifier for a Q&A turn.
func newQuestionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("newQuestionID: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func taskPayloadForNotify(task *orchestrator.Task) json.RawMessage {
	if task.Exec == nil {
		return nil
	}
	return task.Exec.Payload
}

func releaseClaim(payload json.RawMessage) (commit, branch string, pushed bool) {
	var report struct {
		Artifact struct {
			Report struct {
				Release struct {
					Commit string
					Branch string
					Pushed bool
				}
			}
		}
	}
	if err := json.Unmarshal(payload, &report); err != nil {
		return "", "", false
	}
	release := report.Artifact.Report.Release
	commit = strings.TrimSpace(release.Commit)
	if len(commit) < 7 || len(commit) > 40 {
		return "", "", false
	}
	for _, r := range commit {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return "", "", false
		}
	}
	return commit, strings.TrimSpace(release.Branch), release.Pushed
}
