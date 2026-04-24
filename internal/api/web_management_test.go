package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

type stubPairer struct {
	capturedLabel string
	code          string
}

func (s *stubPairer) Issue(_ context.Context, label string) (string, error) {
	s.capturedLabel = label
	return s.code, nil
}

func newTestWebManagementRouter(p Pairer) http.Handler {
	h := &WebManagementHandler{Pairing: p}
	return h.Routes()
}

func newTestAuthStore(t *testing.T) *auth.Store {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return auth.NewStore(d.Conn)
}

func TestWebManagementHandler_PostPair_LabelFromJSONBody(t *testing.T) {
	stub := &stubPairer{code: "ABCD-1234"}
	r := newTestWebManagementRouter(stub)

	body, _ := json.Marshal(auth.PairRequest{Label: "my-laptop"})
	req := httptest.NewRequest(http.MethodPost, "/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if stub.capturedLabel != "my-laptop" {
		t.Errorf("label = %q, want %q", stub.capturedLabel, "my-laptop")
	}

	var resp pairResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Code != "ABCD-1234" {
		t.Errorf("code = %q, want ABCD-1234", resp.Code)
	}
}

func TestWebManagementHandler_GetDevices_ExcludesRevoked(t *testing.T) {
	store := newTestAuthStore(t)
	ctx := context.Background()

	if err := store.InsertDevice(ctx, "active-1", "laptop", []byte("h1")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	if err := store.InsertDevice(ctx, "revoked-1", "old-phone", []byte("h2")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	if err := store.RevokeDevice(ctx, "revoked-1"); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	h := &WebManagementHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	w := httptest.NewRecorder()
	h.GetDevices(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp []deviceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("got %d devices, want 1", len(resp))
	}
	if resp[0].ID != "active-1" {
		t.Errorf("device ID = %q, want %q", resp[0].ID, "active-1")
	}
}

func TestWebManagementHandler_DeleteAllDevices_DevicesDisappear(t *testing.T) {
	store := newTestAuthStore(t)
	ctx := context.Background()

	for _, id := range []string{"d1", "d2"} {
		if err := store.InsertDevice(ctx, id, "", []byte("h")); err != nil {
			t.Fatalf("InsertDevice %s: %v", id, err)
		}
	}

	h := &WebManagementHandler{Store: store}

	// revoke all
	req := httptest.NewRequest(http.MethodDelete, "/devices", nil)
	w := httptest.NewRecorder()
	h.DeleteAllDevices(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteAllDevices status = %d, want 204", w.Code)
	}

	// list should be empty
	req2 := httptest.NewRequest(http.MethodGet, "/devices", nil)
	w2 := httptest.NewRecorder()
	h.GetDevices(w2, req2)

	var resp []deviceResponse
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("got %d devices after revoke-all, want 0", len(resp))
	}
}

func TestWebManagementHandler_PostPair_NoBody_LabelEmpty(t *testing.T) {
	stub := &stubPairer{code: "EFGH-5678"}
	r := newTestWebManagementRouter(stub)

	req := httptest.NewRequest(http.MethodPost, "/pair", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if stub.capturedLabel != "" {
		t.Errorf("label = %q, want empty", stub.capturedLabel)
	}
}
