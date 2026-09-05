package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// notifyTimeout bounds notify.Service.Notify's exec.CommandContext call
// (internal/notify/notify.go) so a hung/misbehaving `notify.command` cannot
// stall a caller indefinitely. This matters most for ApplyAction's HTTP
// request path (a slow notify command must not stall the response) and, for
// the same reason, every other notify call site in this package
// (trigger_loop.go's own notify, triage_done.go's) wraps the same way.
const notifyTimeout = 10 * time.Second

// notifySuggestionArrived fires the queue's sole notify trigger: a
// suggestion attaching to a card is the queue-entry event.
//
// It is deliberately urgency-agnostic: ANY attrs_set that sets a verb
// notifies, regardless of urgency (including no urgency at all) — a
// suggestion is by definition something the queue predicate now surfaces
// and a human eventually has to accept/reject, so "a suggestion exists" is
// already the whole signal. It fires once per verb-setting attrs_set; the
// suggest engine's own cadence (one suggestion per card, latest wins) is
// what controls how often that happens in practice.
//
// A null-clearing patch (attrs_set {"suggestion": null} — patch.HasVerb=true
// but patch.Verb=="") does not notify: a suggestion was withdrawn, not
// attached — nothing new to look at.
func (s *TaskWorkflowService) notifySuggestionArrived(ctx context.Context, task *orchestrator.Task, patch *attrsSetPatch) {
	if patch == nil || !patch.HasVerb || patch.Verb == "" {
		return
	}
	if s.Notifier == nil || task == nil {
		return
	}
	ev := notify.Event{
		TaskID:  task.ID,
		Message: fmt.Sprintf("queue: %s の提案が届きました", patch.Verb),
		URLPath: "/tasks/" + task.ID,
	}
	if task.Title != "" {
		ev.TaskTitle = task.Title
	}
	notifyCtx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if err := s.Notifier.Notify(notifyCtx, ev); err != nil {
		slog.Warn("suggestion notify failed", "task_id", task.ID, "verb", patch.Verb, "error", err)
	}
}
