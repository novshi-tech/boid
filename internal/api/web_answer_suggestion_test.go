package api

// docs/plans/ingestion-identity.md PR-3 (B-3+B-4, J-6): the Web UI accept/
// reject button's POST handler. Unit-level tests here use stubWebService
// (form parsing / redirect / call recording, mirroring
// TestWebHandlerPostAction_*); TestWebAnswerSuggestion_Reject_RecordsAnsweredAction
// below is the doc's own 検証 item ("Web UI の却下が answered を残す"),
// exercised end to end against a REAL TaskWorkflowService/sqlite (same idiom
// as action_list_read_test.go — PR-1/PR-2 review note #3: testutil here
// would cycle through internal/server) so the assertion is about the actual
// action row landing, not just that a mock got called.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestWebHandlerPostAnswerSuggestion_Success(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandler(svc)

	body := url.Values{"answer": {"reject"}, "verb": {"go"}, "basis": {"issue #42"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/suggestion", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.answerSuggestionCalls) != 1 {
		t.Fatalf("AnswerSuggestion calls = %d, want 1", len(svc.answerSuggestionCalls))
	}
	got := svc.answerSuggestionCalls[0]
	if got.taskID != "task-1" {
		t.Errorf("taskID = %q, want task-1", got.taskID)
	}
	want := AnswerSuggestionRequest{Answer: "reject", Verb: "go", Basis: "issue #42"}
	if got.req != want {
		t.Errorf("request = %+v, want %+v", got.req, want)
	}
}

func TestWebHandlerPostAnswerSuggestion_MissingAnswer(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandler(svc)

	body := url.Values{"verb": {"go"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/suggestion", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
	if len(svc.answerSuggestionCalls) != 0 {
		t.Error("AnswerSuggestion should not be called when answer is missing")
	}
}

func TestWebHandlerPostAnswerSuggestion_ServiceError(t *testing.T) {
	svc := &stubWebService{answerSuggestionErr: errFakeConflict}
	r := newTestWebHandler(svc)

	body := url.Values{"answer": {"accept"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/suggestion", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1") || !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want redirect to task with error param", loc)
	}
}

var errFakeConflict = &StatusError{Code: http.StatusConflict, Message: "not answerable"}

// TestWebAnswerSuggestion_Reject_RecordsAnsweredAction is the doc's own
// 検証 item: "Web UI の却下が answered を残す". Exercised end to end — a
// real WebAppService.AnswerSuggestion call against a real
// TaskWorkflowService/sqlite — so the assertion is about the action actually
// landing (readable back via ListActionsByTask), not a mock recording a
// call. This is the path khi's real data shows NEVER fired before this PR
// (25/25 suggestion_answered claims were "accept", 0 "reject" — 実測,
// 2026-08-19), so it is deliberately the reject case under test here, not
// accept.
func TestWebAnswerSuggestion_Reject_RecordsAnsweredAction(t *testing.T) {
	svc, taskRepo := newActionListTestService(t)
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "T", Status: orchestrator.TaskStatusTriaged, Behavior: "triage"}
	if err := taskRepo.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Seed a suggestion the reject answers, via a real attrs_set — this is
	// the same fold path note-suggest's push uses in production.
	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"go","basis":"issue #42"}}`),
	}); err != nil {
		t.Fatalf("seed suggestion via attrs_set: %v", err)
	}

	webSvc := &WebAppService{Workflow: svc}
	if err := webSvc.AnswerSuggestion(task.ID, AnswerSuggestionRequest{Answer: "reject", Verb: "go", Basis: "issue #42"}); err != nil {
		t.Fatalf("AnswerSuggestion(reject): %v", err)
	}

	actions, err := taskRepo.ListActionsByTask(task.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	var answered *orchestrator.Action
	for _, a := range actions {
		if a.Type == "answered" {
			answered = a
		}
	}
	if answered == nil {
		t.Fatalf("no answered action recorded; actions = %+v", actions)
	}
	// Opus review finding #5 (2026-08-19 revisit of PR-3): web_service.go's
	// AnswerSuggestion passes orchestrator.WithActor(ctx, ActorHuman) —
	// same shape as the other 3 Web UI mutation methods — but nothing here
	// asserted it. Pin it so a future refactor that drops the WithActor
	// call (e.g. accidentally routing through a helper that defaults to
	// ActorDaemon) fails a test instead of silently mislabeling every
	// Web UI accept/reject as daemon-originated.
	if answered.Actor != orchestrator.ActorHuman {
		t.Errorf("answered.Actor = %q, want %q (Web UI clicks are human-originated)", answered.Actor, orchestrator.ActorHuman)
	}
	if !strings.Contains(string(answered.Payload), `"answer":"reject"`) {
		t.Errorf("answered payload = %s, want it to carry answer:reject", answered.Payload)
	}

	// The suggestion must be gone from the sidecar (applyAnsweredSideEffect).
	tt, err := taskRepo.GetTaskTriage(task.ID)
	if err != nil {
		t.Fatalf("GetTaskTriage: %v", err)
	}
	if suggestion, ok := orchestrator.DetailSuggestion(tt.Detail); ok || suggestion.Verb != "" {
		t.Errorf("suggestion still present after reject: %+v", suggestion)
	}

	// And the read口 itself (action_list) must surface the reject too — the
	// point of this whole PR: a script reading action_list can now see it.
	result, err := svc.ListActions(orchestrator.ActionListFilter{ProjectIDs: []string{"proj-1"}})
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	found := false
	for _, a := range result.Actions {
		if a.Type == "answered" && strings.Contains(string(a.Payload), `"answer":"reject"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("action_list did not surface the reject: %+v", result.Actions)
	}
}
