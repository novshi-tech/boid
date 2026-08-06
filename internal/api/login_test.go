package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/novshi-tech/boid/internal/api/auth"
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

func (s *stubLoginRateLimiter) Allowed(_ string) bool  { return s.allow }
func (s *stubLoginRateLimiter) RecordFailure(_ string) {}

// newTestLoginHandler builds a chi.Mux with the LoginHandler routes.
func newTestLoginHandler(h *LoginHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/login", h.GetLogin)
	r.Post("/login", h.PostLogin)
	r.Get("/auth", h.GetAuth)
	r.Post("/auth", h.PostAuth)
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
		Pairing: &stubLoginPairing{err: auth.ErrCodeNotFound},
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
	if !strings.Contains(w.Body.String(), "見つかりません") {
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

// countingLoginPairing records how many times Redeem was called so a test can
// assert that a GET never burns the one-time code.
type countingLoginPairing struct {
	label  string
	err    error
	calls  int
	tokens []string
}

func (s *countingLoginPairing) Redeem(_ context.Context, code string) (string, error) {
	s.calls++
	s.tokens = append(s.tokens, code)
	return s.label, s.err
}

// TestGetAuth_DoesNotRedeem is the regression test for the 2026-08-06
// incident: GET /auth?token=... consumed the single-use code, so any browser
// preload, QR-scanner link preview, or in-app-browser prefetch burned the code
// before the human's real navigation arrived — which then failed with
// "invalid_code". A GET must be side-effect free; only the POST redeems.
func TestGetAuth_DoesNotRedeem(t *testing.T) {
	pairing := &countingLoginPairing{label: "phone"}
	h := &LoginHandler{
		Pairing: pairing,
		Signer:  &stubLoginSigner{},
		Store:   &stubLoginDeviceStore{},
		Limiter: &stubLoginRateLimiter{allow: true},
	}
	r := newTestLoginHandler(h)

	// Three prefetch-shaped GETs in a row.
	for range 3 {
		req := httptest.NewRequest(http.MethodGet, "/auth?token=ABCD-EFGH", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("GET /auth status = %d, want 200 (confirmation page)", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `id="auth-confirm-form"`) {
			t.Errorf("GET /auth body missing confirmation form")
		}
		if !strings.Contains(body, "ABCD-EFGH") {
			t.Errorf("GET /auth body missing the token to submit")
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == "boid_session" {
				t.Error("GET /auth issued a session cookie; it must not authenticate")
			}
		}
	}
	if pairing.calls != 0 {
		t.Errorf("Redeem called %d times on GET, want 0", pairing.calls)
	}

	// The human then presses the button — that POST is what redeems.
	body := url.Values{"token": {"ABCD-EFGH"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/auth", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("POST /auth status = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	if pairing.calls != 1 {
		t.Errorf("Redeem called %d times after POST, want 1", pairing.calls)
	}
}

func TestPostAuth_InvalidToken(t *testing.T) {
	h := &LoginHandler{
		Pairing: &stubLoginPairing{err: auth.ErrCodeNotFound},
		Signer:  &stubLoginSigner{},
		Store:   &stubLoginDeviceStore{},
		Limiter: &stubLoginRateLimiter{allow: true},
	}
	r := newTestLoginHandler(h)

	body := url.Values{"token": {"XXXX-XXXX"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/auth", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

// The three redeem failures are indistinguishable to the operator today
// ("無効なペアリングコードです" for all of them), which is what made the
// 2026-08-06 incident take an hour to diagnose. Each cause must name itself.
func TestPostLogin_DistinguishesFailureCause(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"expired", auth.ErrCodeExpired, "有効期限"},
		{"consumed", auth.ErrCodeConsumed, "使用済み"},
		{"not found", auth.ErrCodeNotFound, "見つかりません"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &LoginHandler{
				Pairing: &stubLoginPairing{err: tt.err},
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

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if !strings.Contains(w.Body.String(), tt.want) {
				t.Errorf("body does not mention %q; got error text without the cause", tt.want)
			}
		})
	}
}

// GetLogin renders whatever lands in ?error=. It must map the handful of keys
// /auth redirects with to Japanese prose, and never echo an arbitrary
// caller-supplied string back into the page.
func TestGetLogin_MapsErrorKeys(t *testing.T) {
	tests := []struct {
		query   string
		want    string
		wantNot string
	}{
		{"expired", "有効期限", "expired"},
		{"used", "使用済み", "used"},
		{"invalid", "見つかりません", "invalid"},
		{"totally-made-up", "ログインできませんでした", "totally-made-up"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			h := &LoginHandler{
				Pairing: &stubLoginPairing{},
				Signer:  &stubLoginSigner{},
				Store:   &stubLoginDeviceStore{},
				Limiter: &stubLoginRateLimiter{allow: true},
			}
			r := newTestLoginHandler(h)

			req := httptest.NewRequest(http.MethodGet, "/login?error="+url.QueryEscape(tt.query), nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			body := w.Body.String()
			if !strings.Contains(body, tt.want) {
				t.Errorf("body missing %q", tt.want)
			}
			if strings.Contains(body, tt.wantNot) {
				t.Errorf("body echoed the raw error key %q", tt.wantNot)
			}
		})
	}
}

// newPostAuth builds the request the confirmation page's button produces —
// the only shape that redeems a pairing token now that GET /auth is
// side-effect free.
func newPostAuth(token string) *http.Request {
	body := url.Values{"token": {token}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/auth", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:12345"
	return req
}

// newRealLoginHandler builds a handler backed by a real auth.RateLimiter with a fixed clock.
func newRealLoginHandler(pairing loginPairing, now func() time.Time) *chi.Mux {
	h := &LoginHandler{
		Pairing: pairing,
		Signer:  &stubLoginSigner{},
		Store:   &stubLoginDeviceStore{},
		Limiter: auth.NewRateLimiter(now),
	}
	return newTestLoginHandler(h)
}

func TestGetAuth_CFConnectingIP_RateLimit(t *testing.T) {
	now := time.Now()
	r := newRealLoginHandler(&stubLoginPairing{err: auth.ErrCodeNotFound}, func() time.Time { return now })

	sendAuth := func(ip string) int {
		req := newPostAuth("BAD")
		req.Header.Set("CF-Connecting-IP", ip)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	// 5 failures for 1.2.3.4 → locked
	for i := range 5 {
		if got := sendAuth("1.2.3.4"); got == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: got 429 before lock threshold", i+1)
		}
	}
	if got := sendAuth("1.2.3.4"); got != http.StatusTooManyRequests {
		t.Errorf("6th attempt for 1.2.3.4: got %d, want 429", got)
	}
	// Different IP must still be allowed.
	if got := sendAuth("5.6.7.8"); got == http.StatusTooManyRequests {
		t.Errorf("first attempt for 5.6.7.8: got 429, want non-429")
	}
}

func TestGetAuth_XForwardedFor_LeftmostIP(t *testing.T) {
	now := time.Now()
	r := newRealLoginHandler(&stubLoginPairing{err: auth.ErrCodeNotFound}, func() time.Time { return now })

	sendAuth := func(xff string) int {
		req := newPostAuth("BAD")
		req.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	// 5 failures using multi-value XFF; leftmost IP (1.2.3.4) should accumulate.
	for i := range 5 {
		if got := sendAuth("1.2.3.4, 7.7.7.7"); got == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: got 429 before lock threshold", i+1)
		}
	}
	if got := sendAuth("1.2.3.4, 7.7.7.7"); got != http.StatusTooManyRequests {
		t.Errorf("6th attempt: got %d, want 429", got)
	}
}

func TestGetAuth_InvalidXForwardedFor_FallsBackToRemoteAddr(t *testing.T) {
	now := time.Now()
	r := newRealLoginHandler(&stubLoginPairing{err: auth.ErrCodeNotFound}, func() time.Time { return now })

	// 5 failures with an invalid XFF → counts against RemoteAddr 10.0.0.1.
	for i := range 5 {
		req := newPostAuth("BAD")
		req.RemoteAddr = "10.0.0.1:12345"
		req.Header.Set("X-Forwarded-For", "not-an-ip")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: got 429 before lock threshold", i+1)
		}
	}
	req := newPostAuth("BAD")
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "not-an-ip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("6th attempt (fallback IP): got %d, want 429", w.Code)
	}
}

func TestGetAuth_SuccessDoesNotLock(t *testing.T) {
	now := time.Now()
	r := newRealLoginHandler(&stubLoginPairing{label: "phone"}, func() time.Time { return now })

	// 10 successful redeems must not trigger rate limiting.
	for i := range 10 {
		req := newPostAuth("VALID")
		req.Header.Set("CF-Connecting-IP", "1.2.3.4")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Errorf("attempt %d: got 429 on successful redeem", i+1)
		}
	}
}

func TestGetAuth_InvalidToken_LocksAfterThreshold(t *testing.T) {
	now := time.Now()
	r := newRealLoginHandler(&stubLoginPairing{err: auth.ErrCodeNotFound}, func() time.Time { return now })

	sendAuth := func() int {
		req := newPostAuth("BAD")
		req.Header.Set("CF-Connecting-IP", "9.9.9.9")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	for i := range 5 {
		if got := sendAuth(); got == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: got 429 before lock threshold", i+1)
		}
	}
	if got := sendAuth(); got != http.StatusTooManyRequests {
		t.Errorf("6th attempt: got %d, want 429", got)
	}
}

// A server-side failure inside Redeem (DB/IO) is our fault, not the caller's,
// and must not draw down their brute-force budget — otherwise a transient
// SQLite hiccup locks the operator out of their own daemon for 15 minutes.
func TestPostAuth_InternalError_DoesNotRateLimit(t *testing.T) {
	now := time.Now()
	r := newRealLoginHandler(&stubLoginPairing{err: errors.New("disk on fire")}, func() time.Time { return now })

	for i := range 10 {
		req := newPostAuth("SOME-CODE")
		req.Header.Set("CF-Connecting-IP", "3.3.3.3")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: got 429 for a server-side redeem failure", i+1)
		}
	}
}
