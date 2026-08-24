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

// notifySuggestionArrived is PR-2's replacement for rule 4 (queue の決定論的評価
// 節, docs/plans/suggestion-as-state-transition-impl.md §4.2): a suggestion
// attaching to a card IS the queue-entry event now, full stop. v1's two
// functions this replaces — notifyQueueEntryIfUrgent (a status-transition
// detector keyed on entering ready/triaged) and notifyUrgencyRaised (its
// urgency="now"-on-an-already-member-card companion) — are both DELETED, not
// just superseded:
//
//   - notifyQueueEntryIfUrgent was already dead code under card machine v2
//     (PR-1's review flagged this explicitly): it keyed off transitions INTO
//     TaskStatusReady/TaskStatusTriaged, which nothing in the v2 rule table
//     (machine_card.go) ever produces for a new card.
//   - notifyUrgencyRaised is deleted, not merely renamed, because its entire
//     premise no longer holds: it existed to catch "urgency reached the
//     'now' tier on a card already sitting in the queue", but queue
//     membership is no longer urgency-gated at all (design doc §3.6 —
//     urgency demoted to an ORDER BY-only attribute, store.go's "queue_next"
//     branch). Keeping an urgency-gated notify would smuggle urgency back in
//     as a decision-relevant field the queue predicate itself no longer
//     treats that way, and — concretely — could notify about urgency="now"
//     on a card with NO suggestion at all (urgency and suggestion are
//     independent attrs_set keys), pointing nose at the Queue tab for a card
//     that will not actually be there.
//
// This function is deliberately urgency-agnostic: ANY attrs_set that sets a
// verb notifies, regardless of urgency (including no urgency at all) — a
// suggestion is by definition something the queue predicate now surfaces and
// a human eventually has to accept/reject, so "a suggestion exists" is
// already the whole signal. There is no separate "arrived but not urgent
// enough to tell nose yet" tier: unlike v1's rule 4, which explicitly scoped
// down to "now = immediate" and left today/week digest-batching as future
// work never built, this function does not need that distinction — it fires
// once per verb-setting attrs_set, and khi's own suggest cadence (design doc
// §3.9: 1 枚 1 提案・最新が勝つ) is what controls how often that happens in
// practice.
//
// A null-clearing patch (attrs_set {"suggestion": null} — patch.HasVerb=true
// but patch.Verb=="") does not notify: a suggestion was withdrawn, not
// attached: nothing new for nose to look at.
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
