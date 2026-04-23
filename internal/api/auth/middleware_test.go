package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func setupMiddlewareTest(t *testing.T) (*SessionSigner, *Store) {
	t.Helper()
	return newTestSigner(t)
}

func TestWebAuthMiddleware_ValidCookie(t *testing.T) {
	signer, store := setupMiddlewareTest(t)
	ctx := context.Background()

	if err := store.InsertDevice(ctx, "dev-1", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	// Issue a valid session cookie.
	w0 := httptest.NewRecorder()
	if err := signer.Issue(w0, "dev-1"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	mw := NewWebAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range w0.Result().Cookies() {
		req.AddCookie(c)
	}
	req.RemoteAddr = "1.2.3.4:5678"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("valid cookie: status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestWebAuthMiddleware_InvalidCookie_Redirects(t *testing.T) {
	signer, store := setupMiddlewareTest(t)

	mw := NewWebAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "invalid.cookie"})
	req.RemoteAddr = "1.2.3.4:5678"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("invalid cookie: status = %d, want %d", w.Code, http.StatusFound)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestWebAuthMiddleware_LoopbackNoDevices_Passes(t *testing.T) {
	signer, store := setupMiddlewareTest(t)

	mw := NewWebAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("loopback+no devices: status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestWebAuthMiddleware_ExternalIPNoDevices_Redirects(t *testing.T) {
	signer, store := setupMiddlewareTest(t)

	mw := NewWebAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:9876"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("external+no devices: status = %d, want %d", w.Code, http.StatusFound)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestWebAuthMiddleware_LoopbackWithDevices_Redirects(t *testing.T) {
	signer, store := setupMiddlewareTest(t)
	ctx := context.Background()

	if err := store.InsertDevice(ctx, "dev-x", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	mw := NewWebAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("loopback+devices+no cookie: status = %d, want %d", w.Code, http.StatusFound)
	}
}

func TestWebAuthMiddleware_ExemptPaths(t *testing.T) {
	signer, store := setupMiddlewareTest(t)
	mw := NewWebAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	for _, path := range []string{"/login", "/auth", "/auth/callback", "/static/style.css"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.5:9876"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want %d (exempt path)", path, w.Code, http.StatusOK)
		}
	}
}

func TestWebAuthMiddleware_UnixSocket_NoDevices_Passes(t *testing.T) {
	signer, store := setupMiddlewareTest(t)

	mw := NewWebAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "@" // UNIX socket

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unix socket+no devices: status = %d, want %d", w.Code, http.StatusOK)
	}
}
