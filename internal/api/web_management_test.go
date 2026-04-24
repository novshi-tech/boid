package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/api/auth"
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
