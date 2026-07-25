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

// TestCLITokenAuth_ValidBearer_MarksContextCLITokenAuthenticated pins the
// round-2 codex review Blocker 1 fix: a successfully authenticated request
// must carry auth.CLITokenAuthenticated(ctx)==true into the wrapped
// handler, so downstream handlers sharing this router with other
// transports (e.g. WSAttachHandler) can tell a CLI-token-authenticated
// request apart from one that still needs its own device-pair check.
func TestCLITokenAuth_ValidBearer_MarksContextCLITokenAuthenticated(t *testing.T) {
	var sawMarker bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMarker = CLITokenAuthenticated(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	h := NewCLITokenAuthMiddleware("secret-token")(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if !sawMarker {
		t.Error("CLITokenAuthenticated(ctx) = false in downstream handler, want true")
	}
}

// TestCLITokenEqual pins cliTokenEqual's correctness across a mix of
// equal-length and mismatched-length inputs (round-2 codex review Minor 1:
// both sides are now hashed before subtle.ConstantTimeCompare, so the
// comparison itself always runs over two fixed-length digests regardless
// of the raw input lengths below).
func TestCLITokenEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"equal", "secret-token", "secret-token", true},
		{"different same length", "secret-token", "wrong-token!", false},
		{"different length, b shorter", "secret-token", "short", false},
		{"different length, b longer", "secret-token", "a-much-longer-token-value", false},
		{"a empty", "", "secret-token", false},
		{"b empty", "secret-token", "", false},
		{"both empty", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cliTokenEqual(c.a, c.b); got != c.want {
				t.Errorf("cliTokenEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
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
