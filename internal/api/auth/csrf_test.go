package auth

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Plain HTML form (application/x-www-form-urlencoded) carries the token in
// the body as _csrf when JS can't inject the header.
func TestCSRFMiddleware_POST_FormBodyToken(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))
	body := strings.NewReader("_csrf=my-token&title=hello")
	req := httptest.NewRequest(http.MethodPost, "/action", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "my-token"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// multipart/form-data forms (the attachment-upload path) also fall back to
// the _csrf field. Regression test for the bug where ParseForm alone didn't
// read the multipart body so the hidden _csrf field was invisible to the
// middleware and every upload 403'd.
func TestCSRFMiddleware_POST_MultipartBodyToken(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("title", "hello"); err != nil {
		t.Fatalf("write field title: %v", err)
	}
	if err := mw.WriteField(csrfFormField, "my-token"); err != nil {
		t.Fatalf("write field _csrf: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/tasks", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "my-token"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("multipart with _csrf body field should pass: status = %d, want %d", w.Code, http.StatusOK)
	}
}

// Multipart submit without a token (neither header nor body field) must still
// be rejected — proves we don't silently accept multipart just because the
// content-type is exotic.
func TestCSRFMiddleware_POST_MultipartNoToken(t *testing.T) {
	h := CSRFMiddleware(http.HandlerFunc(okHandler))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("title", "hello"); err != nil {
		t.Fatalf("write field title: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/tasks", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "my-token"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("multipart without token must 403: status = %d, want %d", w.Code, http.StatusForbidden)
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
