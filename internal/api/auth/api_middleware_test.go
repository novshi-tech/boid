package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The TCP API auth middleware is applied only to the TCP listener's handler, so
// these tests exercise the TCP-side logic directly. The UNIX-socket bypass is a
// listener-level property (the unix listener is served by the bare router) and
// is covered by the server integration tests.

func TestTCPAPIAuth_NonAPIPaths_PassThrough(t *testing.T) {
	signer, store := newTestSigner(t)
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	// HTML / auth / static paths are handled by the router's own WebAuth, not
	// gated here. They must pass through even from an external IP.
	for _, path := range []string{"/", "/login", "/auth", "/auth/callback", "/static/app.css", "/tasks/123"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.5:9876"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want %d (non-API path must pass through)", path, w.Code, http.StatusOK)
		}
	}
}

func TestTCPAPIAuth_Health_Public(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()
	// Even with a device registered (bootstrap disabled) and a proxy header,
	// /api/health stays public.
	if err := store.InsertDevice(ctx, "dev-1", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.RemoteAddr = "203.0.113.5:9876"
	req.Header.Set("X-Forwarded-For", "203.0.113.5")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/health: status = %d, want %d (must be public)", w.Code, http.StatusOK)
	}
}

func TestTCPAPIAuth_LoopbackNoDevices_BootstrapPasses(t *testing.T) {
	signer, store := newTestSigner(t)
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	// Genuine local browser before any device is paired: the Web UI's data-API
	// calls must work so the user can reach the pairing flow.
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("loopback+no devices: status = %d, want %d (bootstrap)", w.Code, http.StatusOK)
	}
}

func TestTCPAPIAuth_Tunneled_NoCookie_Unauthorized(t *testing.T) {
	signer, store := newTestSigner(t)
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	// A request forwarded by a reverse proxy / tunnel (cloudflared) reaches the
	// loopback listener but carries proxy headers. IsLoopback==false, so the
	// bootstrap exemption must NOT apply even with zero devices registered.
	for _, hdr := range []string{"X-Forwarded-For", "CF-Connecting-IP", "Forwarded"} {
		t.Run(hdr, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set(hdr, "203.0.113.5")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("tunneled+%s: status = %d, want %d", hdr, w.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestTCPAPIAuth_DevicesRegistered_NoCookie_Unauthorized(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()
	if err := store.InsertDevice(ctx, "dev-1", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	// Once a device is paired the bootstrap window is closed; an unauthenticated
	// loopback caller must be rejected. /api/web/pair is sensitive (mints
	// pairing codes) so it must be gated too.
	for _, path := range []string{"/api/tasks", "/api/secrets", "/api/shutdown", "/api/web/pair"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s (devices registered, no cookie): status = %d, want %d", path, w.Code, http.StatusUnauthorized)
		}
	}
}

func TestTCPAPIAuth_InvalidCookie_Unauthorized_NotRedirect(t *testing.T) {
	signer, store := newTestSigner(t)
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "garbage.value"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	// API clients get 401, not a 302 to /login.
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid cookie: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestTCPAPIAuth_ValidCookie_Passes(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()
	if err := store.InsertDevice(ctx, "dev-1", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	w0 := httptest.NewRecorder()
	if err := signer.Issue(w0, "dev-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	req.RemoteAddr = "203.0.113.5:9876" // external, but a valid session
	for _, c := range w0.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid cookie: status = %d, want %d", w.Code, http.StatusOK)
	}
}
