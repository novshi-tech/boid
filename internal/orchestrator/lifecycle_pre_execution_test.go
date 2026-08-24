package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// IsInstructionsEditable / IsPreDispatchEditableStatus は pending に加えて
// parked (card machine v2 の主要な滞在先・整形セッション UC-3 のため —
// docs/plans/suggestion-as-state-transition-impl.md §3.5) と、legacy
// captured/triaged/ready (pre-cutover な DB 行を締め出さないための維持) でも
// 編集を許す。working は除外 — 子が specced/dispatched 済み、または人手対応
// 中のカードは編集対象外という意図は変わらない (PR #987 review, HIGH 7:
// parked は v1 では「後回し中」だったので除外が正しかったが、v2 で
// captured/triaged を吸収した結果カードの初期状態になった — 除外したままだと
// 生まれたばかりのカードが恒久的に編集不能になる)。
func TestIsInstructionsEditable_PreExecutionStatuses(t *testing.T) {
	editable := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusReady,
	}
	notEditable := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDropped,
	}
	for _, s := range editable {
		if !orchestrator.IsInstructionsEditable(s) {
			t.Errorf("IsInstructionsEditable(%q) = false, want true", s)
		}
	}
	for _, s := range notEditable {
		if orchestrator.IsInstructionsEditable(s) {
			t.Errorf("IsInstructionsEditable(%q) = true, want false", s)
		}
	}
}
