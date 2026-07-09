package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// NotifyTask invokes the configured notify command for the given task.
// Returns 501 when no notifier is wired and ask is empty (notifications disabled in config).
// When ask is non-empty the task is transitioned to awaiting; the notification is
// best-effort and skipped if no notifier is configured. questionID identifies the
// Q&A turn (generated when empty).
// When progress is non-empty (progress mode), no hook fires and no state transition
// occurs — only a progress Action is written to the timeline.
// ask and progress are mutually exclusive.
//
// Note: ask transitions the task to awaiting but does NOT spawn a resume
// dispatch when the user replies. The session-id resume path was removed
// (every dispatch is a fresh agent process); only `boid task ask` (the
// blocking RPC) can deliver an answer back to a live agent.
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
		}
		if err := s.Actions.CreateAction(action); err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		return nil
	}
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}

	// Lifecycle-accountability hard gate: only root tasks (parent_id == "")
	// fire user-facing notify hooks. Child tasks signal their parent supervisor
	// via the awaiting state transition (for ask mode) or are silently dropped
	// (for FYI mode) — the supervisor's monitoring loop is the canonical
	// delivery path. This is a daemon-level invariant rather than a project.yaml
	// hook expression, so child tasks cannot accidentally page the user when a
	// project author forgets the condition. See docs/plans/lifecycle-accountability.md.
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

	// Lifecycle signal: persist the agent's intent and stop the running jobs via the adapter.
	//
	// --ask still goes through ApplyAction(ask): the awaiting transition is
	// synchronous (the agent expects the task to be visibly in `awaiting`
	// immediately after the call returns so the parent supervisor's polling
	// loop sees it).
	//
	// --done / --fail record a `done_request` / `fail_request` action
	// directly WITHOUT calling ApplyAction. The state transition fires later
	// via the condition-based auto rule (`lifecycle.executed && lifecycle.done`
	// → done; ditto for fail), which only kicks in after the runtime has
	// cleanly exited and bash's EXIT trap has called `boid job done`. This
	// preserves the agent's payload_patch (session id) and avoids the race
	// where ApplyAction(done)'s spawned dispatch loop SIGTERM'd the still-
	// running runtime, leaving the job marked failed.
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
		if _, err := s.Workflow.ApplyAction(ctx, taskID, ApplyActionRequest{
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
			// Anti-confabulation gate: reject premature / fabricated done
			// reports before recording the intent (see verifyDoneClaim). The
			// agent's notify call fails loudly so it must actually wait /
			// re-verify instead of the daemon silently accepting fiction.
			if verr := s.verifyDoneClaim(ctx, task); verr != nil {
				return verr
			}
			actionType, msg = "done_request", done
		} else {
			actionType, msg = "fail_request", fail
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
		}
		if err := s.Actions.CreateAction(action); err != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
	}

	// Stop the agent of each running hook job gracefully. StopAgent delivers
	// SIGUSR1 to the runtime pgrp; claude.Adapter.Run()'s signal.Notify handler
	// translates that into a SIGTERM toward the claude child and returns with
	// Result.StoppedByDaemon=true so the surrounding runner-inner-child still
	// posts `boid job done --output-file payload_patch.json` through the broker.
	//
	// Crucially, we do NOT call CompleteJob preemptively here. CompleteJob's
	// finalize releases the broker token, which would reject the runner's
	// follow-up `boid job done` as "invalid token" — silently dropping the
	// agent's session id and breaking the next hook's resume. By letting the
	// runner-inner-child be the sole CompleteJob caller (through the broker),
	// the standard completion path runs with the agent's payload_patch intact.
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

// releaseCommitRe matches a bare git object name (7–40 hex chars). It validates
// the structured release.commit field only — it is deliberately NOT used to
// scan free-form prose, where session ids, UUIDs and issue numbers would
// false-match.
var releaseCommitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// verifyDoneClaim guards `notify --done` against two confabulation patterns
// observed in supervisor agents (2026-05-30): (1) reporting done while child
// tasks are still open — a parent owns its children's lifecycle and a premature
// done orphans them; and (2) citing a release commit that does not exist in the
// repository — a fabricated success report. A rejected claim is returned as a
// StatusError so the agent's `notify` call fails loudly (the runtime is NOT
// signalled to stop) and it must actually wait / re-verify. All git checks are
// best-effort: any inconclusive result (missing repo, exec/network error,
// timeout) skips the check so infrastructure hiccups never block a real done.
//
// Before checking, we `git fetch origin` in the project work_dir (see
// gitFetchOrigin): sandbox-internal clones (git-gateway-cutover) never share
// an object database with the project work_dir, so a commit the agent made
// inside the sandbox is only visible here once it has been pushed to origin
// and fetched back. This makes the semantics "only commits pushed to origin
// pass release verification" (docs/plans/git-gateway-cutover.md PR1) — a
// change that is a no-op in the current shared-worktree world (the fetch adds
// objects but never removes ones already present locally) and load-bearing
// once sandbox clones land.
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

	commit, branch, pushed := releaseClaim(task.Payload)
	if commit == "" || s.Projects == nil {
		return nil
	}
	proj, err := s.Projects.GetProject(task.ProjectID)
	if err != nil || proj == nil || proj.WorkDir == "" {
		return nil
	}
	gitFetchOrigin(ctx, proj.WorkDir)
	if exists, conclusive := gitObjectExists(ctx, proj.WorkDir, commit); conclusive && !exists {
		return &StatusError{
			Code: http.StatusConflict,
			Message: fmt.Sprintf(
				"reported release commit %q does not exist in the repository. Do not write a "+
					"commit hash you have not seen in actual git output this session. Run the "+
					"real merge, capture the true hash (git rev-parse HEAD), and report again.",
				commit),
		}
	}
	if pushed && branch != "" {
		if tip, ok := gitRemoteTip(ctx, proj.WorkDir, branch); ok && tip != "" &&
			!strings.HasPrefix(tip, commit) && !strings.HasPrefix(commit, tip) {
			return &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"reported a push of %q to %s, but origin/%s is at %s. Re-run the real push "+
						"and report the actually-pushed commit.", commit, branch, branch, tip),
			}
		}
	}
	return nil
}

// releaseClaim extracts the structured release report
// (payload.artifact.report.release) the boid-task skill (Supervisor Mode) asks
// agents to populate from real git output before `notify --done`. Missing or
// malformed fields yield zero values, which callers treat as "nothing to verify".
func releaseClaim(payload json.RawMessage) (commit, branch string, pushed bool) {
	if len(payload) == 0 {
		return "", "", false
	}
	var p struct {
		Artifact struct {
			Report struct {
				Release struct {
					Commit string `json:"commit"`
					Branch string `json:"branch"`
					Pushed bool   `json:"pushed"`
				} `json:"release"`
			} `json:"report"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", "", false
	}
	commit = strings.TrimSpace(p.Artifact.Report.Release.Commit)
	if !releaseCommitRe.MatchString(commit) {
		commit = "" // only verify well-formed object names
	}
	return commit, strings.TrimSpace(p.Artifact.Report.Release.Branch), p.Artifact.Report.Release.Pushed
}

// gitFetchOrigin best-effort runs `git fetch origin` in workdir before the
// cat-file / ls-remote checks below. It never blocks the caller: any failure
// (no origin remote, network error, timeout, binary missing) is logged and
// swallowed, and the subsequent checks simply run against whatever is already
// in the local object database — exactly as they did before this call existed.
// See verifyDoneClaim for why this fetch is necessary once sandbox-internal
// clones (git-gateway-cutover) no longer share an object store with the
// project work_dir.
func gitFetchOrigin(ctx context.Context, workdir string) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", workdir, "fetch", "--quiet", "origin")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		slog.Warn("notify: git fetch origin failed before release verification; falling back to local git data",
			"workdir", workdir, "error", err, "stderr", strings.TrimSpace(errb.String()))
	}
}

// gitObjectExists reports whether hash resolves to an object in the repo at
// workdir. conclusive is false when git cannot give a definitive answer (binary
// missing, not a repo, timeout) so the caller can skip rather than block.
func gitObjectExists(ctx context.Context, workdir, hash string) (exists, conclusive bool) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", workdir, "cat-file", "-t", hash)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err == nil {
		return true, true
	}
	es := errb.String()
	if strings.Contains(es, "Not a valid object name") || strings.Contains(es, "could not get object info") {
		return false, true
	}
	return false, false
}

// gitRemoteTip returns the origin tip of branch (best-effort: ok is false on any
// exec/network error so the caller skips the push check rather than blocking).
func gitRemoteTip(ctx context.Context, workdir, branch string) (tip string, ok bool) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", workdir, "ls-remote", "origin", branch)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", false
	}
	fields := strings.Fields(out.String())
	if len(fields) == 0 {
		return "", true // ran cleanly; branch absent on origin
	}
	return fields[0], true
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

	// The only supported delivery path is the blocking RPC: the agent must be
	// parked inside `boid task ask` for the answer to reach it. Legacy
	// `notify --ask` calls exit the agent and the daemon no longer dispatches a
	// resume hook, so an answer there has nowhere to land — reject with a clear
	// error rather than silently flipping the task back to executing with no
	// live agent behind it.
	return s.answerBlocking(task, answer)
}

// newQuestionID generates a random hex identifier for a Q&A turn.
func newQuestionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("newQuestionID: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
