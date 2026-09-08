package api

// A reopen proposal carries the work it proposes. The agent that reads the
// external signal already knows what the card should do next, so it must be
// able to write the child spec and the reopen suggestion in one pass rather
// than propose reopen, wait for a human accept, and only then attach the
// spec.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func terminalCardService(t *testing.T, status orchestrator.TaskStatus) (*TaskWorkflowService, *recordingTxStore, *orchestrator.Task) {
	t.Helper()
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: status, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1", Detail: json.RawMessage(`{}`)}},
	}
	svc := &TaskWorkflowService{
		Tasks:      &stubTaskStore{task: task},
		Tx:         recordingTransactor{store: txStore},
		TaskTriage: txStore,
		Meta:       stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"implement": {}}}},
	}
	return svc, txStore, task
}

func TestApplyAction_ChildAddedAndSpecced_AllowedOnTerminalCard(t *testing.T) {
	for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusDropped} {
		t.Run(string(status), func(t *testing.T) {
			svc, txStore, task := terminalCardService(t, status)

			addPayload := []byte(`{"id":"ch_00","title":"fix the thing"}`)
			if _, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "child_added", Payload: addPayload}); err != nil {
				t.Fatalf("ApplyAction(child_added) on a %s card: %v", status, err)
			}
			specPayload := []byte(`{"id":"ch_00","project":"p1","behavior":"implement","description":"d"}`)
			result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "child_specced", Payload: specPayload})
			if err != nil {
				t.Fatalf("ApplyAction(child_specced) on a %s card: %v", status, err)
			}

			if result.Task.Status != status {
				t.Fatalf("status = %q, want %q (writing a child spec must not transition the card)", result.Task.Status, status)
			}
			children, cerr := orchestrator.DetailChildren(txStore.triage["t1"].Detail)
			if cerr != nil {
				t.Fatalf("DetailChildren: %v", cerr)
			}
			if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusSpecced {
				t.Fatalf("children = %+v, want one specced child", children)
			}
		})
	}
}

// TestApplyAction_ChildDropped_StillRejectedOnTerminalCard keeps the rest of
// the frozen-children rule: withdrawing a child from a card nobody is going
// to reopen has no proposal to ride along with.
func TestApplyAction_ChildDropped_StillRejectedOnTerminalCard(t *testing.T) {
	svc, _, task := terminalCardService(t, orchestrator.TaskStatusDone)

	_, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "child_dropped", Payload: []byte(`{"id":"ch_00"}`)})
	if err == nil {
		t.Fatal("expected child_dropped on a done card to be rejected")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected a 409 StatusError, got %v", err)
	}
}
