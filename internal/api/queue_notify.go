package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// notifyTimeout bounds notify.Service.Notify's exec.CommandContext call
// (internal/notify/notify.go) so a hung/misbehaving `notify.command` cannot
// stall a caller indefinitely. This matters most for QueueSweepLoop
// (queue_sweep.go): SweepWake's ctx is the long-lived daemon context with no
// deadline of its own, so without this timeout the FIRST urgency=now wake
// in a sweep batch that hits a hanging notify command would hang the whole
// sweep — every other parked task's wake evaluation, and every future
// sweep tick, forever (codex review round 1, Major). ApplyAction's HTTP
// request path benefits the same way (a slow notify command no longer
// stalls the response).
const notifyTimeout = 10 * time.Second

// queueMemberStatus reports whether status is one of the two queue-member
// statuses queue の決定論的評価 節 rule 2 defines (state ∈ {ready, triaged});
// urgency is checked separately.
func queueMemberStatus(status orchestrator.TaskStatus) bool {
	return status == orchestrator.TaskStatusReady || status == orchestrator.TaskStatusTriaged
}

// notifyQueueEntryIfUrgent implements queue 節 rule 4 (notify) — PR-3's
// scoped-down slice of it. The full rule is: now = immediate, today = 朝の
// digest + 到着時, week = 日次 digest のみ, with configurable times. Building
// the digest-scheduling half (batching "today"/"week" arrivals into a single
// scheduled notification) is out of PR-3's scope — it needs a scheduler
// primitive this PR doesn't otherwise need, and no caller (ingestion,
// PR-4) exists yet to observe real digest volume against. This function
// implements only the "now = immediate" tier concretely; a task entering
// the queue with urgency=today/week gets no notify from this PR (it's still
// visible in the queue view itself — rule 5, 隠さない, is unaffected).
//
// "Entry" is detected as a transition INTO a queue-member status (ready or
// triaged) FROM a non-member status — a within-queue transition (e.g.
// triaged->ready, which auto-chains into Dispatch and often never rests at
// "ready" long enough to matter) does not re-fire. Both ApplyAction (the
// "triage"/"ready" actions) and Wake (parked->triaged/ready) call this after
// their state transition commits.
func (s *TaskWorkflowService) notifyQueueEntryIfUrgent(ctx context.Context, task *orchestrator.Task, fromStatus orchestrator.TaskStatus) {
	if s.Notifier == nil || s.TaskTriage == nil || task == nil {
		return
	}
	if !queueMemberStatus(task.Status) || queueMemberStatus(fromStatus) {
		return
	}
	tt, err := s.TaskTriage.GetTaskTriage(task.ID)
	if err != nil {
		return // no sidecar row => no urgency recorded => nothing to notify
	}
	if tt.Urgency != orchestrator.UrgencyNow {
		return
	}
	ev := notify.Event{
		TaskID:  task.ID,
		Message: "queue: 今すぐ対応が必要な triage task が届きました",
		URLPath: "/tasks/" + task.ID,
	}
	if task.Title != "" {
		ev.TaskTitle = task.Title
	}
	notifyCtx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if err := s.Notifier.Notify(notifyCtx, ev); err != nil {
		slog.Warn("queue notify failed", "task_id", task.ID, "error", err)
	}
}
