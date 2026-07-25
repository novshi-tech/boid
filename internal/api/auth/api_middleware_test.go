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

func TestTCPAPIAuth_AuthDevice_Public(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()
	// Even with a device registered (bootstrap disabled) and no credentials
	// at all, POST /api/auth/device stays public — it is the endpoint a
	// brand-new CLI uses to redeem a pairing code for its first token.
	if err := store.InsertDevice(ctx, "dev-1", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", nil)
	req.RemoteAddr = "203.0.113.5:9876"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/auth/device: status = %d, want %d (must be public)", w.Code, http.StatusOK)
	}
}

func TestTCPAPIAuth_AuthDevicesID_StillGated(t *testing.T) {
	// /api/auth/devices/{id} (plural, DELETE) is a distinct path from
	// /api/auth/device (singular, POST) and must NOT be swept up by the new
	// exemption — it requires Bearer auth just like any other /api/* route.
	signer, store := newTestSigner(t)
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodDelete, "/api/auth/devices/dev-1", nil)
	req.RemoteAddr = "203.0.113.5:9876"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/auth/devices/{id}: status = %d, want %d (must stay gated)", w.Code, http.StatusUnauthorized)
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

func TestTCPAPIAuth_ValidBearer_Passes(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()
	token, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	if err := store.InsertDeviceToken(ctx, "dev-bearer", "cli", HashToken(token)); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(deviceIDEchoHandler())

	req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	req.RemoteAddr = "203.0.113.5:9876" // external, no cookie at all
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("valid bearer: status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Body.String(); got != "dev-bearer" {
		t.Errorf("device id in context = %q, want %q", got, "dev-bearer")
	}
}

func TestTCPAPIAuth_InvalidBearer_Unauthorized(t *testing.T) {
	signer, store := newTestSigner(t)
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	req.RemoteAddr = "203.0.113.5:9876"
	req.Header.Set("Authorization", "Bearer boid_pat_does-not-exist")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid bearer: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestTCPAPIAuth_RevokedBearer_Unauthorized(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()
	token, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	if err := store.InsertDeviceToken(ctx, "dev-revoked", "", HashToken(token)); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}
	if err := store.RevokeDevice(ctx, "dev-revoked"); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	req.RemoteAddr = "203.0.113.5:9876"
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked bearer: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestTCPAPIAuth_BearerHeaderPresent_IgnoresCookieFallback pins the priority
// decision: when an Authorization: Bearer header is present, it is the sole
// auth path — even a request that ALSO carries a valid session cookie must
// be judged on the (here: invalid) Bearer token alone, not fall back to the
// cookie. This keeps the two paths' failure semantics independent and
// predictable (docs/plans/cli-remote-connection.md PR0: "一貫していれば OK"
// — Bearer-first is the chosen, documented order).
func TestTCPAPIAuth_BearerHeaderPresent_IgnoresCookieFallback(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()
	if err := store.InsertDevice(ctx, "dev-cookie", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	w0 := httptest.NewRecorder()
	if err := signer.Issue(w0, "dev-cookie"); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	req.RemoteAddr = "203.0.113.5:9876"
	for _, c := range w0.Result().Cookies() {
		req.AddCookie(c)
	}
	req.Header.Set("Authorization", "Bearer boid_pat_garbage")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bearer header present but invalid (valid cookie also present): status = %d, want %d (no cookie fallback)", w.Code, http.StatusUnauthorized)
	}
}

func TestTCPAPIAuth_NoBearerHeader_CookiePathUnchanged(t *testing.T) {
	// Sanity check that omitting the Authorization header entirely leaves
	// the pre-PR0 cookie behavior byte-for-byte intact (bootstrap window
	// still applies with zero devices registered).
	signer, store := newTestSigner(t)
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(http.HandlerFunc(okHandler))

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("loopback+no devices+no bearer header: status = %d, want %d (bootstrap)", w.Code, http.StatusOK)
	}
}

func deviceIDEchoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := DeviceIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(id))
	})
}

// authMethodEchoHandler writes the AuthMethod tag the middleware placed in
// the request context, so tests can prove the middleware→handler
// propagation still works end-to-end (device_auth_test.go injects the tag
// by hand — a regression there would be masked).
func authMethodEchoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, _ := AuthMethodFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(m))
	})
}

func TestTCPAPIAuth_ValidBearer_PropagatesAuthMethodBearer(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()
	token, err := GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	if err := store.InsertDeviceToken(ctx, "dev-bearer-am", "cli", HashToken(token)); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(authMethodEchoHandler())

	req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	req.RemoteAddr = "203.0.113.5:9876"
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got, want := w.Body.String(), string(AuthMethodBearer); got != want {
		t.Errorf("auth method in context = %q, want %q", got, want)
	}
}

func TestTCPAPIAuth_ValidCookie_PropagatesAuthMethodCookie(t *testing.T) {
	signer, store := newTestSigner(t)
	ctx := context.Background()
	if err := store.InsertDevice(ctx, "dev-cookie-am", "", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	w0 := httptest.NewRecorder()
	if err := signer.Issue(w0, "dev-cookie-am"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(authMethodEchoHandler())

	req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	req.RemoteAddr = "203.0.113.5:9876"
	for _, c := range w0.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got, want := w.Body.String(), string(AuthMethodCookie); got != want {
		t.Errorf("auth method in context = %q, want %q", got, want)
	}
}

// TestTCPAPIAuth_BootstrapLoopback_LeavesAuthMethodUnset guards the
// unauthenticated bootstrap-loopback pass-through (no cookie + genuine
// loopback + zero devices registered) — a handler MUST see no AuthMethod
// tag in that case, so a downstream check like DeleteDevice's
// `method != AuthMethodBearer → 403` fires instead of silently accepting
// a bootstrap request as some default identity.
func TestTCPAPIAuth_BootstrapLoopback_LeavesAuthMethodUnset(t *testing.T) {
	signer, store := newTestSigner(t)
	mw := NewTCPAPIAuthMiddleware(signer, store)
	handler := mw(authMethodEchoHandler())

	req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	// loopback source, no cookie, no bearer, zero devices → bootstrap path.
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bootstrap pass)", w.Code)
	}
	if got := w.Body.String(); got != "" {
		t.Errorf("auth method = %q, want empty (bootstrap does not set it)", got)
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
