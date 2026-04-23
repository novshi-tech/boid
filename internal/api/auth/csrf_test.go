package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func TestCSRFMiddleware_GET_IssuesCookie(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == csrfCookieName {
			found = true
			if c.Value == "" {
				t.Error("csrf_token cookie value is empty")
			}
		}
	}
	if !found {
		t.Error("csrf_token cookie not set on GET")
	}
}

func TestCSRFMiddleware_GET_NoCookieIfAlreadySet(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "existing-token"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	for _, c := range w.Result().Cookies() {
		if c.Name == csrfCookieName {
			t.Error("csrf_token cookie should not be re-issued when already present")
		}
	}
}

func TestCSRFMiddleware_POST_Valid(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "my-token"})
	req.Header.Set(csrfHeaderName, "my-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCSRFMiddleware_POST_Mismatch(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "correct-token"})
	req.Header.Set(csrfHeaderName, "wrong-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestCSRFMiddleware_POST_NoCookie(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.Header.Set(csrfHeaderName, "some-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestCSRFMiddleware_AuthExempt(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))
	req := httptest.NewRequest(http.MethodPost, "/auth", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/auth POST should be exempt: status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestCSRFMiddleware_APIExempt(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))
	req := httptest.NewRequest(http.MethodPost, "/api/web/pair", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/api/* POST should be exempt: status = %d, want %d", w.Code, http.StatusOK)
	}
}
