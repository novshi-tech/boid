package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestPostCardCommandFailurePreservesInputAndRecordsStableRejection(t *testing.T) {
	h, conn, projectID := newCardCommandWebTestHandler(t, cardCommandMeta([]string{"discuss"}, map[string]orchestrator.CardCommand{"discuss": {Label: "Discuss", Run: "echo"}}))
	newCardTimelineTestCard(t, conn, projectID, "card-1")
	card, err := orchestrator.GetTask(conn, "card-1")
	if err != nil {
		t.Fatal(err)
	}
	store := orchestrator.NewOperationResultStore(conn)
	h.OperationResults = store
	// A terminal status makes the server reject even if a stale form is submitted.
	card.Status = orchestrator.TaskStatusDone
	if err := orchestrator.UpdateTask(conn, card); err != nil {
		t.Fatal(err)
	}
	code, body, _ := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{"key": {"discuss"}, "instruction": {"keep this exact input"}})
	if code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", code, body)
	}
	for _, want := range []string{"keep this exact input", "was not accepted", "not_available"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	got, err := store.ListOperationResults("card-1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Result != orchestrator.OperationResultRejected || got[0].ReasonCode != orchestrator.OperationReasonNotAvailable {
		t.Fatalf("results=%+v", got)
	}
}

func TestPostCardCommandAcceptedPersistsRequestAndLaterSessionLink(t *testing.T) {
	h, conn, projectID := newCardCommandWebTestHandler(t, cardCommandMeta([]string{"discuss"}, map[string]orchestrator.CardCommand{"discuss": {Label: "Discuss", Run: "echo"}}))
	newCardTimelineTestCard(t, conn, projectID, "card-1")
	store := orchestrator.NewOperationResultStore(conn)
	h.OperationResults = store
	code, _, location := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{"key": {"discuss"}, "instruction": {"secret body"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(location, "/tasks/card-1?operation=") {
		t.Fatalf("status=%d location=%q", code, location)
	}
	got, err := store.ListOperationResults("card-1", 20)
	if err != nil || len(got) != 1 {
		t.Fatalf("results=%+v err=%v", got, err)
	}
	if got[0].Result != orchestrator.OperationResultAccepted || got[0].TargetRequestID == "" {
		t.Fatalf("result=%+v", got[0])
	}
	if _, err := conn.Exec(`UPDATE card_requests SET status='attached', target_kind='session', target_id='session-job-42' WHERE id=?`, got[0].TargetRequestID); err != nil {
		t.Fatal(err)
	}
	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatal(code)
	}
	if !strings.Contains(body, `/jobs/session-job-42`) || !strings.Contains(body, `Open session`) {
		t.Fatalf("missing resolved session link: %s", body)
	}
	if strings.Contains(body, "secret body") {
		t.Fatalf("operation history duplicated instruction: %s", body)
	}
	code, body = getHTML(t, h, "/tasks/card-1/fragment?kind=operations&operation="+got[0].ID)
	if code != http.StatusOK || !strings.Contains(body, `data-operation-id="`+got[0].ID+`"`) {
		t.Fatalf("selected operation fragment status=%d body=%s", code, body)
	}
	code, _ = getHTML(t, h, "/tasks/card-1/fragment?kind=operations&operation=missing")
	if code != http.StatusNotFound {
		t.Fatalf("missing selected operation status=%d, want 404", code)
	}
}

func TestOperationReasonCodeDoesNotParseErrorText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&StatusError{Code: http.StatusConflict, Message: "completely changed wording"}, orchestrator.OperationReasonConflict},
		{&StatusError{Code: http.StatusBadRequest, Message: "slot busy words should not matter"}, orchestrator.OperationReasonInvalidRequest},
		{errors.New("slot busy"), orchestrator.OperationReasonInternalError},
	} {
		if got := operationReasonForError(tc.err); got != tc.want {
			t.Errorf("reason=%q want=%q", got, tc.want)
		}
	}
}

func TestCompletedGoOperationDistinguishesAcceptedStartedAndUnknown(t *testing.T) {
	accepted := completedGoOperation("card", "Go", &ActionApplication{}, nil)
	if accepted.Result != orchestrator.OperationResultAccepted {
		t.Fatalf("accepted=%+v", accepted)
	}
	started := completedGoOperation("card", "Go", &ActionApplication{TargetTaskID: "child"}, nil)
	if started.Result != orchestrator.OperationResultStarted || started.TargetTaskID != "child" {
		t.Fatalf("started=%+v", started)
	}
	unknown := completedGoOperation("card", "Go", nil, errors.New("database response lost"))
	if unknown.Result != orchestrator.OperationResultUnknown || unknown.ReasonCode != orchestrator.OperationReasonInternalError {
		t.Fatalf("unknown=%+v", unknown)
	}
	rejected := completedGoOperation("card", "Go", nil, &StatusError{Code: http.StatusConflict, Message: "any", OperationReason: orchestrator.OperationReasonNoReadyWork})
	if rejected.Result != orchestrator.OperationResultRejected || rejected.ReasonCode != orchestrator.OperationReasonNoReadyWork {
		t.Fatalf("rejected=%+v", rejected)
	}
}
