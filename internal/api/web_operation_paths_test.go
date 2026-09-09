package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type observedGoService struct {
	WebService
	application *ActionApplication
	err         error
}

func (s observedGoService) ApplyActionWithResult(string, string) (*ActionApplication, error) {
	return s.application, s.err
}
func (s observedGoService) AnswerSuggestionWithResult(string, AnswerSuggestionRequest) (*ActionApplication, error) {
	return s.application, s.err
}

func TestGoEntryPointsPersistTheirOwnReceipt(t *testing.T) {
	for _, path := range []string{"action", "suggestion"} {
		for _, rejected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rejected=%v", path, rejected), func(t *testing.T) {
				h, conn, projectID := newCardTimelineTestHandler(t)
				newCardTimelineTestCard(t, conn, projectID, "card")
				svc := observedGoService{WebService: h.Service, application: &ActionApplication{TargetTaskID: "started-child"}}
				if rejected {
					svc.application = nil
					svc.err = &StatusError{Code: http.StatusConflict, OperationReason: orchestrator.OperationReasonNoReadyWork, Message: "not ready"}
				}
				h.Service = svc
				store := orchestrator.NewOperationResultStore(conn)
				h.OperationResults = store
				code, _, location := postFormHTML(t, h, "/tasks/card/"+path, url.Values{"type": {"go"}, "answer": {"accept"}, "verb": {"go"}})
				if code != http.StatusSeeOther {
					t.Fatalf("status=%d", code)
				}
				u, err := url.Parse(location)
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := store.GetOperationResult("card", u.Query().Get("operation"))
				if err != nil {
					t.Fatal(err)
				}
				want := orchestrator.OperationResultStarted
				if rejected {
					want = orchestrator.OperationResultRejected
				}
				if receipt.Result != want {
					t.Fatalf("result=%+v", receipt)
				}
				if !rejected && receipt.TargetTaskID != "started-child" {
					t.Fatalf("missing target: %+v", receipt)
				}
			})
		}
	}
}

type unreadableOperationStore struct{ OperationResultStore }

func (s unreadableOperationStore) ListOperationResults(string, int) ([]*orchestrator.OperationResult, error) {
	return nil, errors.New("read unavailable")
}

func TestSelectedOperationSurvivesHistoryLimitAndReadFailureIsNotEmpty(t *testing.T) {
	h, conn, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, conn, projectID, "card")
	store := orchestrator.NewOperationResultStore(conn)
	h.OperationResults = store
	for i := 0; i < 25; i++ {
		if err := store.CreateOperationResult(&orchestrator.OperationResult{ID: fmt.Sprintf("receipt-%d", i), TaskID: "card", OperationType: "go", Result: orchestrator.OperationResultAccepted, CreatedAt: time.Now().Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	code, body := getHTML(t, h, "/tasks/card/fragment?kind=operations&operation=receipt-0")
	if code != 200 || !strings.Contains(body, `data-operation-id="receipt-0"`) {
		t.Fatalf("selected receipt missing: %d %s", code, body)
	}
	if strings.Index(body, `data-operation-id="receipt-0"`) > strings.Index(body, `data-operation-id="receipt-24"`) {
		t.Fatal("selected receipt must stay visible above folded history")
	}
	h.OperationResults = unreadableOperationStore{store}
	code, _ = getHTML(t, h, "/tasks/card/fragment?kind=operations&operation=receipt-0")
	if code != http.StatusInternalServerError {
		t.Fatalf("read failure = %d", code)
	}
}
