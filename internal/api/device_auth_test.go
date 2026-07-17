package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api/auth"
)

var errArbitraryDB = errors.New("simulated pairing store failure")

// fakeRateLimiter never blocks unless told to — used to exercise the happy
// path deterministically and, in one test, to pin the 429 branch.
type fakeRateLimiter struct {
	allowed         bool
	recordedFailure bool
}

func (f *fakeRateLimiter) Allowed(string) bool { return f.allowed }
func (f *fakeRateLimiter) RecordFailure(string) {
	f.recordedFailure = true
}

func newTestDeviceAuthStore(t *testing.T) *auth.Store {
	t.Helper()
	return newTestAuthStore(t) // defined in web_management_test.go, same package
}

func newDeviceAuthRouter(h *DeviceAuthHandler) http.Handler {
	r := chi.NewRouter()
	r.Mount("/api/auth", h.Routes())
	return r
}

func TestDeviceAuthHandler_PostDevice_ValidCode_ReturnsToken(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	pairing := auth.NewPairingManager(store)
	code, err := pairing.Issue(context.Background(), "my-cli")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	h := &DeviceAuthHandler{
		Pairing: pairing,
		Store:   store,
		Limiter: &fakeRateLimiter{allowed: true},
	}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": code, "device_name": "my-laptop"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp deviceAuthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DeviceID == "" {
		t.Error("device_id is empty")
	}
	if resp.Token == "" {
		t.Error("token is empty")
	}
	if got, want := resp.Token[:len(auth.DeviceTokenPrefix)], auth.DeviceTokenPrefix; got != want {
		t.Errorf("token prefix = %q, want %q", got, want)
	}

	// The device must actually be findable via the hash of the returned
	// token (the whole point of the endpoint).
	d, err := store.GetDeviceByTokenHash(context.Background(), auth.HashToken(resp.Token))
	if err != nil {
		t.Fatalf("GetDeviceByTokenHash: %v", err)
	}
	if d == nil {
		t.Fatal("GetDeviceByTokenHash: got nil, want device")
	}
	if d.ID != resp.DeviceID {
		t.Errorf("stored device ID = %q, want %q", d.ID, resp.DeviceID)
	}
	if d.Label != "my-laptop" {
		t.Errorf("label = %q, want %q (device_name should win over pair label)", d.Label, "my-laptop")
	}
}

func TestDeviceAuthHandler_PostDevice_NoDeviceName_FallsBackToPairLabel(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	pairing := auth.NewPairingManager(store)
	code, err := pairing.Issue(context.Background(), "pair-label")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	h := &DeviceAuthHandler{Pairing: pairing, Store: store, Limiter: &fakeRateLimiter{allowed: true}}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp deviceAuthResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	d, _ := store.GetDeviceByTokenHash(context.Background(), auth.HashToken(resp.Token))
	if d == nil || d.Label != "pair-label" {
		t.Errorf("label = %+v, want pair-label", d)
	}
}

func TestDeviceAuthHandler_PostDevice_InvalidCode_Unauthorized(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	limiter := &fakeRateLimiter{allowed: true}
	h := &DeviceAuthHandler{
		Pairing: auth.NewPairingManager(store),
		Store:   store,
		Limiter: limiter,
	}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": "ZZZZ-9999"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !limiter.recordedFailure {
		t.Error("expected RecordFailure to be called for an invalid code")
	}
}

func TestDeviceAuthHandler_PostDevice_MissingCode_BadRequest(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	h := &DeviceAuthHandler{
		Pairing: auth.NewPairingManager(store),
		Store:   store,
		Limiter: &fakeRateLimiter{allowed: true},
	}
	router := newDeviceAuthRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestDeviceAuthHandler_PostDevice_RateLimited(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	h := &DeviceAuthHandler{
		Pairing: auth.NewPairingManager(store),
		Store:   store,
		Limiter: &fakeRateLimiter{allowed: false},
	}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": "AAAA-1111"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
}

func TestDeviceAuthHandler_PostDevice_CanonicalURL_PrefersPublicURL(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	pairing := auth.NewPairingManager(store)
	code, _ := pairing.Issue(context.Background(), "")

	h := &DeviceAuthHandler{
		Pairing:   pairing,
		Store:     store,
		Limiter:   &fakeRateLimiter{allowed: true},
		PublicURL: "https://boid.example.com",
	}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	req.Host = "10.0.0.5:8080"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var resp deviceAuthResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.CanonicalURL != "https://boid.example.com" {
		t.Errorf("canonical_url = %q, want configured public URL", resp.CanonicalURL)
	}
}

func TestNormalizePublicURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty is allowed", "", "", false},
		{"plain https origin", "https://example.com", "https://example.com", false},
		{"uppercase host is lowercased", "https://EXAMPLE.Com", "https://example.com", false},
		{"path is stripped", "https://example.com/path/x", "https://example.com", false},
		{"query is stripped", "https://example.com?x=1", "https://example.com", false},
		{"fragment is stripped", "https://example.com#f", "https://example.com", false},
		{"trailing slash is stripped", "https://example.com/", "https://example.com", false},
		{"port is preserved", "https://example.com:8443", "https://example.com:8443", false},
		{"IPv6 with brackets round-trips", "https://[::1]:8080", "https://[::1]:8080", false},
		{"IPv6 without port keeps brackets", "https://[2001:db8::1]", "https://[2001:db8::1]", false},
		{"leading/trailing whitespace is trimmed", "  https://example.com  ", "https://example.com", false},
		{"http scheme is rejected", "http://example.com", "", true},
		{"no scheme is rejected", "example.com", "", true},
		{"scheme-only is rejected", "https://", "", true},
		{"port-only host is rejected", "https://:443", "", true},
		{"userinfo does not become host", "https://user:pw@", "", true},
		// Adversarial authority forms that url.URL's own Hostname/Port
		// would silently mangle. splitAuthority must reject these outright.
		{"bracket-less IPv6 is rejected", "https://2001:db8::1", "", true},
		{"multi-colon authority is rejected", "https://example.com:80:443", "", true},
		{"bracketed non-IPv6 host is rejected", "https://[not-ipv6]:8080", "", true},
		{"userinfo before host is rejected", "https://alice@example.com", "", true},
		{"bracketed IPv6 with port round-trips", "https://[fe80::1]:8080", "https://[fe80::1]:8080", false},
		{"bracketed IPv6 without port keeps brackets", "https://[fe80::1]", "https://[fe80::1]", false},
		// Numeric port range: url.Parse accepts these as syntactically
		// valid, but they can never resolve to a real TCP endpoint —
		// reject at normalize time so canonical_url does not promise a
		// bogus origin.
		{"port zero is rejected", "https://example.com:0", "", true},
		{"port above max is rejected", "https://example.com:65536", "", true},
		{"port at max is accepted", "https://example.com:65535", "https://example.com:65535", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePublicURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("NormalizePublicURL(%q) = %q, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Errorf("NormalizePublicURL(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizePublicURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDeviceAuthHandler_PostDevice_ForwardedHeader_DoesNotBypassRateLimit(t *testing.T) {
	// The public token-issuance endpoint MUST NOT trust forwarded
	// headers as the rate-limit key — otherwise a directly-connected
	// attacker could rotate CF-Connecting-IP / X-Forwarded-For between
	// each guess and consume no bucket at all. This test pins that
	// peerIPForPublicEndpoint (not remoteIP) is the sole key source:
	// varying the forwarded headers on the same peer must NOT reset
	// the limiter's failure counter.
	store := newTestDeviceAuthStore(t)
	limiter := &rateLimitKeyRecorder{allowed: true}
	h := &DeviceAuthHandler{
		Pairing:   auth.NewPairingManager(store),
		Store:     store,
		Limiter:   limiter,
		PublicURL: "https://x.example",
	}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": "bad-code"})
	makeReq := func(cfIP, xff string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
		req.RemoteAddr = "203.0.113.5:9876"
		if cfIP != "" {
			req.Header.Set("CF-Connecting-IP", cfIP)
		}
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		return req
	}

	// Same TCP peer, different forwarded headers each hit. The
	// limiter must observe the exact same key every time.
	for _, hdrs := range [][2]string{
		{"1.2.3.4", ""},
		{"5.6.7.8", ""},
		{"", "9.9.9.9"},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, makeReq(hdrs[0], hdrs[1]))
	}

	if len(limiter.keysSeen) == 0 {
		t.Fatal("Allowed was never called")
	}
	first := limiter.keysSeen[0]
	for _, k := range limiter.keysSeen[1:] {
		if k != first {
			t.Errorf("rate-limit key varied across requests from the same peer: got %q vs %q — forwarded headers must not influence keying", first, k)
		}
	}
	if strings.Contains(first, "1.2.3.4") || strings.Contains(first, "5.6.7.8") || strings.Contains(first, "9.9.9.9") {
		t.Errorf("rate-limit key %q contains a forwarded-header value; must be derived only from RemoteAddr", first)
	}
}

// rateLimitKeyRecorder captures every key the handler feeds into the
// limiter so a test can assert forwarded-header spoofing does not
// influence keying.
type rateLimitKeyRecorder struct {
	allowed  bool
	keysSeen []string
}

func (r *rateLimitKeyRecorder) Allowed(key string) bool {
	r.keysSeen = append(r.keysSeen, key)
	return r.allowed
}
func (r *rateLimitKeyRecorder) RecordFailure(string) {}

func TestDeviceAuthHandler_PostDevice_CanonicalURL_FallbackNormalizesHost(t *testing.T) {
	// The Host header fallback path must go through NormalizePublicURL —
	// otherwise an uppercase / trailing-slashy / IPv6-bracketed Host
	// would leak into canonical_url unnormalized and break the CLI's
	// byte-equality origin bind check.
	store := newTestDeviceAuthStore(t)
	pairing := auth.NewPairingManager(store)
	code, _ := pairing.Issue(context.Background(), "")

	h := &DeviceAuthHandler{Pairing: pairing, Store: store, Limiter: &fakeRateLimiter{allowed: true}}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	req.Host = "WORK.Example.COM"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var resp deviceAuthResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.CanonicalURL != "https://work.example.com" {
		t.Errorf("canonical_url = %q, want %q (host must be lowercased through NormalizePublicURL)",
			resp.CanonicalURL, "https://work.example.com")
	}
}

func TestDeviceAuthHandler_PostDevice_CanonicalURL_FallbackRejectsGarbageHost(t *testing.T) {
	// If the request Host header is unusable as an origin (bare port,
	// unparseable), the handler must NOT fabricate a garbage
	// canonical_url — it must 500 with the pairing code still unspent.
	store := newTestDeviceAuthStore(t)
	pairing := auth.NewPairingManager(store)
	code, _ := pairing.Issue(context.Background(), "")

	h := &DeviceAuthHandler{Pairing: pairing, Store: store, Limiter: &fakeRateLimiter{allowed: true}}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	req.Host = ":443" // no hostname, just a port
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}

	// Retry once the origin is fixable — the code should still be redeemable.
	h.PublicURL = "https://boid.example.com"
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
}

func TestDeviceAuthHandler_PostDevice_CanonicalURL_FallsBackToHostHeader(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	pairing := auth.NewPairingManager(store)
	code, _ := pairing.Issue(context.Background(), "")

	h := &DeviceAuthHandler{Pairing: pairing, Store: store, Limiter: &fakeRateLimiter{allowed: true}}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	req.Host = "work.example.com"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var resp deviceAuthResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.CanonicalURL != "https://work.example.com" {
		t.Errorf("canonical_url = %q, want %q", resp.CanonicalURL, "https://work.example.com")
	}
}

// fakePairing lets a test drive Redeem to return either a real label/success
// or an arbitrary non-sentinel error (simulating a DB / IO failure inside
// the PairingManager). Used to prove that the handler distinguishes
// "invalid code" (client fault, 401 + RecordFailure) from "internal error"
// (server fault, 500 + no RecordFailure).
type fakePairing struct {
	label string
	err   error
}

func (f *fakePairing) Redeem(context.Context, string) (string, error) {
	return f.label, f.err
}

func TestDeviceAuthHandler_PostDevice_InternalRedeemError_500_NoRateLimit(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	limiter := &fakeRateLimiter{allowed: true}
	h := &DeviceAuthHandler{
		Pairing: &fakePairing{err: errArbitraryDB},
		Store:   store,
		Limiter: limiter,
	}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": "AAAA-1111"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if limiter.recordedFailure {
		t.Error("RecordFailure was called for an internal error — must only fire on the code-guessing sentinels")
	}
}

func TestDeviceAuthHandler_PostDevice_CanonicalURLUnavailable_500_CodeUnconsumed(t *testing.T) {
	// PublicURL is unset and we clear the request Host header. The handler
	// must fail fast BEFORE burning the pairing code, so the operator can
	// fix web.public_url and retry with the same code.
	store := newTestDeviceAuthStore(t)
	pairing := auth.NewPairingManager(store)
	code, err := pairing.Issue(context.Background(), "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	h := &DeviceAuthHandler{Pairing: pairing, Store: store, Limiter: &fakeRateLimiter{allowed: true}}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	req.Host = ""
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}

	// A second attempt with the SAME code must still succeed once the
	// operator has set web.public_url — proving the code wasn't consumed.
	h.PublicURL = "https://boid.example.com"
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
}

func TestDeviceAuthHandler_PostDevice_OversizeBody_Rejects(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	h := &DeviceAuthHandler{
		Pairing:   auth.NewPairingManager(store),
		Store:     store,
		Limiter:   &fakeRateLimiter{allowed: true},
		PublicURL: "https://x.example",
	}
	router := newDeviceAuthRouter(h)

	// 8 KiB payload — 2x the MaxBytesReader cap — inside a valid JSON
	// shape so the failure is the size cap, not a JSON syntax error.
	huge := bytes.Repeat([]byte("A"), 8*1024)
	body, _ := json.Marshal(map[string]string{"code": string(huge)})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 400 or 413", w.Code)
	}
}

func TestDeviceAuthHandler_PostDevice_UnknownField_Rejects(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	h := &DeviceAuthHandler{
		Pairing:   auth.NewPairingManager(store),
		Store:     store,
		Limiter:   &fakeRateLimiter{allowed: true},
		PublicURL: "https://x.example",
	}
	router := newDeviceAuthRouter(h)

	body := []byte(`{"code":"AAAA-1111","evil":"payload"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestDeviceAuthHandler_PostDevice_CodeTooLong_Rejects(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	h := &DeviceAuthHandler{
		Pairing:   auth.NewPairingManager(store),
		Store:     store,
		Limiter:   &fakeRateLimiter{allowed: true},
		PublicURL: "https://x.example",
	}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{"code": string(bytes.Repeat([]byte("X"), 128))})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestDeviceAuthHandler_PostDevice_DeviceNameTooLong_Rejects(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	h := &DeviceAuthHandler{
		Pairing:   auth.NewPairingManager(store),
		Store:     store,
		Limiter:   &fakeRateLimiter{allowed: true},
		PublicURL: "https://x.example",
	}
	router := newDeviceAuthRouter(h)

	body, _ := json.Marshal(map[string]string{
		"code":        "AAAA-1111",
		"device_name": string(bytes.Repeat([]byte("N"), 512)),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestDeviceAuthHandler_DeleteDevice_CookieAuth_Forbidden(t *testing.T) {
	// A cookie-authenticated caller must NOT be able to hit the Bearer
	// self-revoke endpoint even if they name their own device ID. Cookie
	// callers manage devices via WebManagementHandler (/api/web/devices,
	// UNIX-socket-only), not this Bearer-scoped surface.
	store := newTestDeviceAuthStore(t)
	if err := store.InsertDevice(context.Background(), "dev-cookie", "browser", []byte("cookiehash")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	h := &DeviceAuthHandler{Store: store}
	router := newDeviceAuthRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/api/auth/devices/dev-cookie", nil)
	req = req.WithContext(auth.WithAuthMethod(auth.WithDeviceID(req.Context(), "dev-cookie"), auth.AuthMethodCookie))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	d, err := store.GetDevice(context.Background(), "dev-cookie")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d == nil {
		t.Error("cookie device was revoked via the Bearer endpoint — must be 403 with no side effects")
	}
}

func TestDeviceAuthHandler_DeleteDevice_SelfRevoke_NoContent(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	if err := store.InsertDeviceToken(context.Background(), "dev-self", "", []byte("h")); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	h := &DeviceAuthHandler{Store: store}
	router := newDeviceAuthRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/api/auth/devices/dev-self", nil)
	req = req.WithContext(auth.WithAuthMethod(auth.WithDeviceID(req.Context(), "dev-self"), auth.AuthMethodBearer))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	d, err := store.GetDevice(context.Background(), "dev-self")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d != nil {
		t.Errorf("device still active after self-revoke: %+v", d)
	}
}

func TestDeviceAuthHandler_DeleteDevice_OtherDevice_Forbidden(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	if err := store.InsertDeviceToken(context.Background(), "dev-victim", "", []byte("h")); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	h := &DeviceAuthHandler{Store: store}
	router := newDeviceAuthRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/api/auth/devices/dev-victim", nil)
	// Caller is authenticated as a DIFFERENT device.
	req = req.WithContext(auth.WithAuthMethod(auth.WithDeviceID(req.Context(), "dev-attacker"), auth.AuthMethodBearer))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	d, err := store.GetDevice(context.Background(), "dev-victim")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d == nil {
		t.Error("victim device was revoked by a non-owner caller")
	}
}

func TestDeviceAuthHandler_DeleteDevice_NoDeviceIDInContext_Forbidden(t *testing.T) {
	// Simulates a request that reached this handler with no auth context at
	// all (e.g. over the UNIX socket, which never runs the Bearer/cookie
	// middleware) — must not be treated as an implicit match.
	store := newTestDeviceAuthStore(t)
	if err := store.InsertDeviceToken(context.Background(), "dev-x", "", []byte("h")); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	h := &DeviceAuthHandler{Store: store}
	router := newDeviceAuthRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/api/auth/devices/dev-x", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestDeviceAuthHandler_DeleteDevice_NotFound(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	h := &DeviceAuthHandler{Store: store}
	router := newDeviceAuthRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/api/auth/devices/no-such", nil)
	req = req.WithContext(auth.WithAuthMethod(auth.WithDeviceID(req.Context(), "no-such"), auth.AuthMethodBearer))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDeviceAuthHandler_DeleteDevice_RevokesConnectionRegistry(t *testing.T) {
	store := newTestDeviceAuthStore(t)
	if err := store.InsertDeviceToken(context.Background(), "dev-conn", "", []byte("h")); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}
	reg := auth.NewConnectionRegistry()
	revokeCh, release := reg.Register("dev-conn")
	defer release()

	h := &DeviceAuthHandler{Store: store, Registry: reg}
	router := newDeviceAuthRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/api/auth/devices/dev-conn", nil)
	req = req.WithContext(auth.WithAuthMethod(auth.WithDeviceID(req.Context(), "dev-conn"), auth.AuthMethodBearer))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	select {
	case <-revokeCh:
		// expected: channel closed by RevokeDevice
	default:
		t.Error("ConnectionRegistry was not revoked for the deleted device")
	}
}
