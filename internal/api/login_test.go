package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// stubLoginPairing is a test double for loginPairing.
type stubLoginPairing struct {
	label string
	err   error
}

func (s *stubLoginPairing) Redeem(_ context.Context, _ string) (string, error) {
	return s.label, s.err
}

// stubLoginSigner is a test double for loginSigner.
type stubLoginSigner struct {
	err error
}

func (s *stubLoginSigner) Issue(w http.ResponseWriter, _ string) error {
	if s.err != nil {
		return s.err
	}
	http.SetCookie(w, &http.Cookie{Name: "boid_session", Value: "stub-session"})
	return nil
}

// stubLoginDeviceStore is a test double for loginDeviceStore.
type stubLoginDeviceStore struct {
	err error
}

func (s *stubLoginDeviceStore) InsertDevice(_ context.Context, _, _ string, _ []byte) error {
	return s.err
}

// stubLoginRateLimiter is a test double for loginRateLimiter.
type stubLoginRateLimiter struct {
	allow bool
}

func (s *stubLoginRateLimiter) Allow(_ string) bool { return s.allow }

// newTestLoginHandler builds a chi.Mux with the LoginHandler routes.
func newTestLoginHandler(h *LoginHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/login", h.GetLogin)
	r.Post("/login", h.PostLogin)
	r.Get("/auth", h.GetAuth)
	return r
}

func TestLoginHandlerGetLogin_OK(t *testing.T) {
	h := &LoginHandler{
		Pairing: &stubLoginPairing{},
		Signer:  &stubLoginSigner{},
		Store:   &stubLoginDeviceStore{},
		Limiter: &stubLoginRateLimiter{allow: true},
	}
	r := newTestLoginHandler(h)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="login-form"`) {
		t.Errorf("response body missing login form element")
	}
	if !strings.Contains(body, `id="code"`) {
		t.Errorf("response body missing code input")
	}
}

func TestLoginHandlerPostLogin_ValidCode(t *testing.T) {
	h := &LoginHandler{
		Pairing: &stubLoginPairing{label: "my-device"},
		Signer:  &stubLoginSigner{},
		Store:   &stubLoginDeviceStore{},
		Limiter: &stubLoginRateLimiter{allow: true},
	}
	r := newTestLoginHandler(h)

	body := url.Values{"code": {"ABCD-EFGH"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "boid_session" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Set-Cookie boid_session not found in response")
	}
}

func TestLoginHandlerPostLogin_InvalidCode(t *testing.T) {
	h := &LoginHandler{
		Pairing: &stubLoginPairing{err: errors.New("code not found")},
		Signer:  &stubLoginSigner{},
		Store:   &stubLoginDeviceStore{},
		Limiter: &stubLoginRateLimiter{allow: true},
	}
	r := newTestLoginHandler(h)

	body := url.Values{"code": {"XXXX-XXXX"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "無効なペアリングコードです") {
		t.Errorf("response body missing error message")
	}
}

func TestLoginHandlerPostLogin_RateLimited(t *testing.T) {
	h := &LoginHandler{
		Pairing: &stubLoginPairing{},
		Signer:  &stubLoginSigner{},
		Store:   &stubLoginDeviceStore{},
		Limiter: &stubLoginRateLimiter{allow: false},
	}
	r := newTestLoginHandler(h)

	body := url.Values{"code": {"ABCD-EFGH"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
}

func TestLoginHandlerGetAuth_ValidToken(t *testing.T) {
	h := &LoginHandler{
		Pairing: &stubLoginPairing{label: ""},
		Signer:  &stubLoginSigner{},
		Store:   &stubLoginDeviceStore{},
		Limiter: &stubLoginRateLimiter{allow: true},
	}
	r := newTestLoginHandler(h)

	req := httptest.NewRequest(http.MethodGet, "/auth?token=ABCD-EFGH", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
}

func TestLoginHandlerGetAuth_InvalidToken(t *testing.T) {
	h := &LoginHandler{
		Pairing: &stubLoginPairing{err: errors.New("code not found")},
		Signer:  &stubLoginSigner{},
		Store:   &stubLoginDeviceStore{},
		Limiter: &stubLoginRateLimiter{allow: true},
	}
	r := newTestLoginHandler(h)

	req := httptest.NewRequest(http.MethodGet, "/auth?token=XXXX-XXXX", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/login") {
		t.Errorf("Location = %q, want redirect to /login", loc)
	}
}
