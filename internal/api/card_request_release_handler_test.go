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
}

func (f *fakeCardRequestReleaseStore) ForceReleaseCardRequest(id, reason string) error {
	f.lastID = id
	f.lastReasn = reason
	return f.err
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
