package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type failingOperationResultStore struct {
	OperationResultStore
	createErr error
	updateErr error
}

func (s failingOperationResultStore) CreateOperationResult(result *orchestrator.OperationResult) error {
	if s.createErr != nil {
		return s.createErr
	}
	return s.OperationResultStore.CreateOperationResult(result)
}

func (s failingOperationResultStore) UpdateOperationResult(result *orchestrator.OperationResult) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.OperationResultStore.UpdateOperationResult(result)
}

type observingOperationWebService struct {
	WebService
	beforeApply func()
	result      *ActionApplication
	err         error
}

func (s observingOperationWebService) ApplyActionWithResult(string, string) (*ActionApplication, error) {
	if s.beforeApply != nil {
		s.beforeApply()
	}
	return s.result, s.err
}

func (s observingOperationWebService) AnswerSuggestionWithResult(string, AnswerSuggestionRequest) (*ActionApplication, error) {
	if s.beforeApply != nil {
		s.beforeApply()
	}
	return s.result, s.err
}

type observingCommandWebService struct {
	WebService
	beforeRun func()
}

type failingReadOperationResultStore struct{ OperationResultStore }

func (s failingReadOperationResultStore) ListOperationResults(string, int) ([]*orchestrator.OperationResult, error) {
	return nil, errors.New("private read failure")
}

func (s observingCommandWebService) RunCardCommandAsHuman(ctx context.Context, cardID, commandKey, instruction string) (*RunCardCommandResult, error) {
	if s.beforeRun != nil {
		s.beforeRun()
	}
	return s.WebService.RunCardCommandAsHuman(ctx, cardID, commandKey, instruction)
}

func TestOperationReceiptExistsBeforeMutationAtEveryEntryPoint(t *testing.T) {
	for _, path := range []string{"action", "suggestion"} {
		t.Run(path, func(t *testing.T) {
			h, conn, projectID := newCardTimelineTestHandler(t)
			newCardTimelineTestCard(t, conn, projectID, "card")
			store := orchestrator.NewOperationResultStore(conn)
			h.OperationResults = store
			observed := false
			h.Service = observingOperationWebService{WebService: h.Service, result: &ActionApplication{TargetTaskID: "child"}, beforeApply: func() {
				rows, err := store.ListOperationResults("card", 20)
				if err != nil || len(rows) != 1 || rows[0].Result != orchestrator.OperationResultUnknown || rows[0].ReasonCode != orchestrator.OperationReasonOutcomePending {
					t.Fatalf("receipt at mutation boundary = %+v, err=%v", rows, err)
				}
				observed = true
			}}
			code, _, _ := postFormHTML(t, h, "/tasks/card/"+path, url.Values{"type": {"go"}, "answer": {"accept"}, "verb": {"go"}})
			if code != http.StatusSeeOther || !observed {
				t.Fatalf("status=%d observed=%v", code, observed)
			}
			rows, err := store.ListOperationResults("card", 20)
			if err != nil || len(rows) != 1 || rows[0].Result != orchestrator.OperationResultStarted {
				t.Fatalf("final receipt = %+v, err=%v", rows, err)
			}
		})
	}

	t.Run("command", func(t *testing.T) {
		h, conn, projectID := newCardCommandWebTestHandler(t, cardCommandMeta([]string{"review"}, map[string]orchestrator.CardCommand{"review": {Label: "Review", Run: "echo"}}))
		newCardTimelineTestCard(t, conn, projectID, "card")
		store := orchestrator.NewOperationResultStore(conn)
		h.OperationResults = store
		observed := false
		h.Service = observingCommandWebService{WebService: h.Service, beforeRun: func() {
			rows, err := store.ListOperationResults("card", 20)
			if err != nil || len(rows) != 1 || rows[0].ReasonCode != orchestrator.OperationReasonOutcomePending {
				t.Fatalf("receipt at command boundary = %+v, err=%v", rows, err)
			}
			observed = true
		}}
		code, _, _ := postFormHTML(t, h, "/tasks/card/commands", url.Values{"key": {"review"}})
		if code != http.StatusSeeOther || !observed {
			t.Fatalf("status=%d observed=%v", code, observed)
		}
	})
}

func TestInitialReceiptFailureDoesNotMutate(t *testing.T) {
	h, conn, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, conn, projectID, "card")
	called := false
	h.Service = observingOperationWebService{WebService: h.Service, beforeApply: func() { called = true }}
	h.OperationResults = failingOperationResultStore{OperationResultStore: orchestrator.NewOperationResultStore(conn), createErr: errors.New("private database path")}

	form := url.Values{"type": {"go"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/card/action", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("status=%d mutation_called=%v", rec.Code, called)
	}
	if rec.Header().Get("X-Boid-Operation-Status") != "not-submitted" || !strings.Contains(rec.Body.String(), "Nothing was submitted") {
		t.Fatalf("missing not-submitted response: headers=%v body=%s", rec.Header(), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "private database path") {
		t.Fatal("internal receipt error leaked to response")
	}
}

func TestFinalReceiptFailureRetainsUnknownAndShowsObservedOutcome(t *testing.T) {
	h, conn, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, conn, projectID, "card")
	store := orchestrator.NewOperationResultStore(conn)
	h.OperationResults = failingOperationResultStore{OperationResultStore: store, updateErr: errors.New("write unavailable")}
	h.Service = observingOperationWebService{WebService: h.Service, result: &ActionApplication{TargetTaskID: "child"}}
	code, body, _ := postFormHTML(t, h, "/tasks/card/action", url.Values{"type": {"go"}})
	if code != http.StatusOK || !strings.Contains(body, operationReceiptUpdateWarning) || !strings.Contains(body, `data-operation-result="started"`) {
		t.Fatalf("status=%d body=%s", code, body)
	}
	stored, err := store.ListOperationResults("card", 20)
	if err != nil || len(stored) != 1 || stored[0].Result != orchestrator.OperationResultUnknown || stored[0].ReasonCode != orchestrator.OperationReasonOutcomePending {
		t.Fatalf("durable receipt = %+v, err=%v", stored, err)
	}
}

func TestInternalOperationErrorDoesNotLeakThroughRedirect(t *testing.T) {
	h, conn, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, conn, projectID, "card")
	h.OperationResults = orchestrator.NewOperationResultStore(conn)
	h.Service = observingOperationWebService{WebService: h.Service, err: errors.New("sqlite /private/path unavailable")}
	code, _, location := postFormHTML(t, h, "/tasks/card/action", url.Values{"type": {"go"}})
	if code != http.StatusSeeOther || strings.Contains(location, "private") || strings.Contains(location, "error=") {
		t.Fatalf("status=%d location=%q", code, location)
	}
}

func TestFullTaskDetailShowsOperationHistoryReadFailure(t *testing.T) {
	h, conn, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, conn, projectID, "card")
	h.OperationResults = failingReadOperationResultStore{orchestrator.NewOperationResultStore(conn)}
	code, body := getHTML(t, h, "/tasks/card")
	if code != http.StatusOK || !strings.Contains(body, "Operation results are temporarily unavailable") {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if strings.Contains(body, "private read failure") {
		t.Fatal("internal read error leaked to full task detail")
	}
}

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
	if _, err := conn.Exec(`INSERT INTO jobs(id,project_id) VALUES('session-job-42',?)`, projectID); err != nil {
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
