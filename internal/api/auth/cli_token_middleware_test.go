package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newCLITokenTestHandler(t *testing.T, token string) http.Handler {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return NewCLITokenAuthMiddleware(token)(inner)
}

func TestCLITokenAuth_ValidBearer_Allowed(t *testing.T) {
	h := newCLITokenTestHandler(t, "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestCLITokenAuth_WrongBearer_Rejected(t *testing.T) {
	h := newCLITokenTestHandler(t, "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestCLITokenAuth_NoBearer_Rejected(t *testing.T) {
	h := newCLITokenTestHandler(t, "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestCLITokenAuth_EmptyConfiguredToken_AlwaysRejects pins the fail-closed
// contract: an unset BOID_CLI_TOKEN must never let ANY request through,
// even one that happens to carry an empty/absent Bearer header — there is
// no bootstrap window on this listener, unlike NewTCPAPIAuthMiddleware's
// pre-first-pairing exemption.
func TestCLITokenAuth_EmptyConfiguredToken_AlwaysRejects(t *testing.T) {
	h := newCLITokenTestHandler(t, "")
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestCLITokenAuth_PublicPaths_NoAuthRequired mirrors
// NewTCPAPIAuthMiddleware's own apiAuthRequired exemptions (/api/health,
// /api/auth/device) — reused as-is rather than reimplemented, so both TCP
// listeners agree on what "public" means.
func TestCLITokenAuth_PublicPaths_NoAuthRequired(t *testing.T) {
	h := newCLITokenTestHandler(t, "secret-token")
	for _, path := range []string{"/api/health", "/health-not-api", "/login"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if path == "/api/health" {
			if rec.Code != http.StatusNoContent {
				t.Errorf("path %s: status = %d, want %d (public)", path, rec.Code, http.StatusNoContent)
			}
			continue
		}
		// Non-/api/* paths are outside apiAuthRequired's scope entirely
		// (the router's own handlers apply); this middleware must not
		// gate them.
		if rec.Code != http.StatusNoContent {
			t.Errorf("path %s: status = %d, want %d (not gated by this middleware)", path, rec.Code, http.StatusNoContent)
		}
	}
}

func TestCLITokenAuth_MalformedBearerHeader_Rejected(t *testing.T) {
	h := newCLITokenTestHandler(t, "secret-token")
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
