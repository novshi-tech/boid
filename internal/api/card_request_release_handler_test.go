package api_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type fakeCardRequestReleaseStore struct {
	err       error
	lastID    string
	lastReasn string
	byCard    map[string][]*orchestrator.CardRequest
	listErr   error
	active    []*orchestrator.CardRequest
	activeErr error
	getByID   map[string]*orchestrator.CardRequest
	getErr    error
}

func (f *fakeCardRequestReleaseStore) GetCardRequest(id string) (*orchestrator.CardRequest, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if req, ok := f.getByID[id]; ok {
		return req, nil
	}
	return nil, orchestrator.ErrCardRequestNotFound
}

func (f *fakeCardRequestReleaseStore) ForceReleaseCardRequest(id, reason string) error {
	f.lastID = id
	f.lastReasn = reason
	return f.err
}

func (f *fakeCardRequestReleaseStore) ListCardRequestsByCard(cardID string) ([]*orchestrator.CardRequest, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byCard[cardID], nil
}

func (f *fakeCardRequestReleaseStore) ListActiveCardRequests() ([]*orchestrator.CardRequest, error) {
	if f.activeErr != nil {
		return nil, f.activeErr
	}
	return f.active, nil
}

func TestCardRequestHandler_Release_Success(t *testing.T) {
	store := &fakeCardRequestReleaseStore{}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/req-1/release", strings.NewReader(`{"reason":"stuck"}`))
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if store.lastID != "req-1" || store.lastReasn != "stuck" {
		t.Errorf("ForceReleaseCardRequest called with (%q, %q), want (req-1, stuck)", store.lastID, store.lastReasn)
	}
}

func TestCardRequestHandler_Release_EmptyBodyUsesDefaultReason(t *testing.T) {
	store := &fakeCardRequestReleaseStore{}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/req-1/release", nil)
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if store.lastReasn != "" {
		t.Errorf("reason = %q, want empty (store's own default kicks in)", store.lastReasn)
	}
}

func TestCardRequestHandler_Release_NotFound(t *testing.T) {
	store := &fakeCardRequestReleaseStore{err: orchestrator.ErrCardRequestNotFound}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/does-not-exist/release", nil)
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestCardRequestHandler_List_NoCardID_ReturnsBulkActiveRequests pins List's
// bulk mode: omitting card_id lists every active (launching/attached) row
// across every card in one call, instead of requiring one GET per card
// (`boid task diagnose-cards`'s own N+1 fix).
func TestCardRequestHandler_List_NoCardID_ReturnsBulkActiveRequests(t *testing.T) {
	row := &orchestrator.CardRequest{ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1"}
	store := &fakeCardRequestReleaseStore{active: []*orchestrator.CardRequest{row}}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"req-1"`) || !strings.Contains(rec.Body.String(), `"card_id":"card-1"`) {
		t.Errorf("body = %s, want req-1/card-1", rec.Body.String())
	}
}

func TestCardRequestHandler_List_NoCardID_ServiceError_Returns500(t *testing.T) {
	store := &fakeCardRequestReleaseStore{activeErr: errors.New("db is on fire")}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

func TestCardRequestHandler_List_ReturnsCardsRequests(t *testing.T) {
	row := &orchestrator.CardRequest{ID: "req-1", CardID: "card-1", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "job-1"}
	store := &fakeCardRequestReleaseStore{byCard: map[string][]*orchestrator.CardRequest{"card-1": {row}}}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?card_id=card-1", nil)
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"req-1"`) || !strings.Contains(rec.Body.String(), `"status":"launching"`) {
		t.Errorf("body = %s, want req-1/launching", rec.Body.String())
	}
}

func TestCardRequestHandler_List_EmptyForUnknownCard(t *testing.T) {
	store := &fakeCardRequestReleaseStore{}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?card_id=no-such-card", nil)
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %s, want an empty JSON array", rec.Body.String())
	}
}

// TestCardRequestHandler_Release_LiveTarget_ReturnsOperatorNotice pins that
// releasing a request that WAS attached to a continuation surfaces that
// continuation and an explicit warning that force-release only frees the
// slot — it does not stop whatever the request was pointing at.
func TestCardRequestHandler_Release_LiveTarget_ReturnsOperatorNotice(t *testing.T) {
	store := &fakeCardRequestReleaseStore{
		getByID: map[string]*orchestrator.CardRequest{
			"req-1": {ID: "req-1", TargetKind: orchestrator.CardRequestTargetKindSession, TargetID: "job-42"},
		},
	}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/req-1/release", nil)
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"had_live_target":true`) {
		t.Errorf("body = %s, want had_live_target=true", body)
	}
	if !strings.Contains(body, `"target_id":"job-42"`) || !strings.Contains(body, `"target_kind":"session"`) {
		t.Errorf("body = %s, want the pre-release target echoed back", body)
	}
	if !strings.Contains(body, "operator_notice") {
		t.Errorf("body = %s, want an operator_notice warning it does not stop the continuation", body)
	}
}

// TestCardRequestHandler_Release_NoLiveTarget_NoNotice pins the common case
// (a queued/never-attached row, or the pre-release read failing) — no
// operator_notice, since there is nothing known to still be running.
func TestCardRequestHandler_Release_NoLiveTarget_NoNotice(t *testing.T) {
	store := &fakeCardRequestReleaseStore{}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/req-1/release", nil)
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "operator_notice") {
		t.Errorf("body = %s, want no operator_notice when no live target is known", body)
	}
}

func TestCardRequestHandler_Release_OtherError_Returns500(t *testing.T) {
	store := &fakeCardRequestReleaseStore{err: errors.New("db is on fire")}
	h := &api.CardRequestHandler{Store: store}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/req-1/release", nil)
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}
