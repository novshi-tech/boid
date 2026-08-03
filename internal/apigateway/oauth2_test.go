package apigateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memSecretStore is a small in-memory (namespace, key) -> value store
// standing in for internal/dispatcher.SecretStore in these tests — mutex
// guarded so the singleflight/concurrency tests below can hit it from many
// goroutines safely, and with an optional per-key write-failure injection
// point for the persistence-order test.
type memSecretStore struct {
	mu     sync.Mutex
	data   map[string]string
	failOn map[string]bool
}

func newMemSecretStore() *memSecretStore {
	return &memSecretStore{data: map[string]string{}}
}

func (m *memSecretStore) storeKey(namespace, key string) string {
	return namespace + "\x00" + key
}

func (m *memSecretStore) seed(namespace, key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[m.storeKey(namespace, key)] = value
}

func (m *memSecretStore) failWritesTo(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn == nil {
		m.failOn = map[string]bool{}
	}
	m.failOn[key] = true
}

func (m *memSecretStore) get(namespace, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[m.storeKey(namespace, key)]
	if !ok {
		return "", fmt.Errorf("secret not found: %s/%s", namespace, key)
	}
	return v, nil
}

func (m *memSecretStore) resolver() SecretResolver {
	return func(namespace, key string) (string, error) { return m.get(namespace, key) }
}

func (m *memSecretStore) writer() SecretWriter {
	return func(namespace, key, value string) error {
		m.mu.Lock()
		fail := m.failOn[key]
		m.mu.Unlock()
		if fail {
			return fmt.Errorf("simulated write failure for key %q", key)
		}
		m.seed(namespace, key, value)
		return nil
	}
}

// seedAccessTokenCache/getAccessTokenCache seed and read
// oauthFieldAccessTokenCache's JSON blob directly — access_token and
// expires_at are persisted together under one key (codex review round 3,
// Major finding: see oauthFieldAccessTokenCache's own doc comment for why),
// so tests exercise that combined shape rather than two independent keys.
func seedAccessTokenCache(m *memSecretStore, namespace, provider, accessToken string, expiresAt time.Time) {
	m.seed(namespace, OAuthSecretKey(provider, oauthFieldAccessTokenCache), mustMarshalAccessTokenCache(accessToken, expiresAt))
}

func mustMarshalAccessTokenCache(accessToken string, expiresAt time.Time) string {
	b, err := json.Marshal(oauthAccessTokenCache{AccessToken: accessToken, ExpiresAt: expiresAt.Unix()})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func getAccessTokenCache(m *memSecretStore, namespace, provider string) (oauthAccessTokenCache, error) {
	raw, err := m.get(namespace, OAuthSecretKey(provider, oauthFieldAccessTokenCache))
	if err != nil {
		return oauthAccessTokenCache{}, err
	}
	var c oauthAccessTokenCache
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return oauthAccessTokenCache{}, err
	}
	return c, nil
}

// tokenEndpointStub is a small httptest-backed fake OAuth2 token endpoint
// that counts requests and lets each test script the exact response (or
// error) to return, keyed only by call order (tests here never need more
// than a couple of distinct responses).
type tokenEndpointStub struct {
	srv        *httptest.Server
	calls      int32
	lastForm   url.Values
	respStatus int
	respBody   string
	// gate, when non-nil, is read from once per request before responding —
	// lets a test hold every concurrent request open until it has confirmed
	// they all arrived, widening the race window the singleflight test
	// relies on.
	gate chan struct{}
}

func newTokenEndpointStub(t *testing.T, respStatus int, respBody string) *tokenEndpointStub {
	t.Helper()
	stub := &tokenEndpointStub{respStatus: respStatus, respBody: respBody}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stub.calls, 1)
		if err := r.ParseForm(); err != nil {
			t.Fatalf("token endpoint: parse form: %v", err)
		}
		stub.lastForm = r.PostForm
		if stub.gate != nil {
			<-stub.gate
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.respStatus)
		_, _ = w.Write([]byte(stub.respBody))
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *tokenEndpointStub) callCount() int {
	return int(atomic.LoadInt32(&s.calls))
}

func tokenJSON(accessToken, refreshToken string, expiresIn int) string {
	body := map[string]any{"access_token": accessToken, "expires_in": expiresIn}
	if refreshToken != "" {
		body["refresh_token"] = refreshToken
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func TestOAuth2TokenSource_CacheHit_NoRefreshCall(t *testing.T) {
	store := newMemSecretStore()
	ref := time.Now()
	seedAccessTokenCache(store, "ws-a", "freee", "cached-token", ref.Add(1*time.Hour))

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("should-not-be-used", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL, ClientID: "cid"}}, store.resolver(), store.writer())
	ts.Now = func() time.Time { return ref }

	got, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "cached-token" {
		t.Errorf("AccessToken = %q, want %q (cache hit)", got, "cached-token")
	}
	if n := stub.callCount(); n != 0 {
		t.Errorf("token endpoint called %d times, want 0 (cache hit must not refresh)", n)
	}
}

func TestOAuth2TokenSource_NoCachedAccessToken_RefreshesUsingRefreshTokenOnly(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT1", "RT2", 3600))
	ref := time.Now()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL, ClientID: "cid"}}, store.resolver(), store.writer())
	ts.Now = func() time.Time { return ref }

	got, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "AT1" {
		t.Errorf("AccessToken = %q, want %q", got, "AT1")
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("token endpoint called %d times, want 1", n)
	}
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken)); v != "RT2" {
		t.Errorf("persisted refresh_token = %q, want %q", v, "RT2")
	}
	cache, err := getAccessTokenCache(store, "ws-a", "freee")
	if err != nil {
		t.Fatalf("getAccessTokenCache: %v", err)
	}
	if cache.AccessToken != "AT1" {
		t.Errorf("persisted access_token = %q, want %q", cache.AccessToken, "AT1")
	}
	wantExpiry := ref.Add(3600 * time.Second).Unix()
	if cache.ExpiresAt != wantExpiry {
		t.Errorf("persisted expires_at = %d, want %d", cache.ExpiresAt, wantExpiry)
	}
}

func TestOAuth2TokenSource_WithinMargin_Refreshes(t *testing.T) {
	store := newMemSecretStore()
	ref := time.Now()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	// Within the default 5-minute margin (expires in 2 minutes) — must
	// refresh proactively rather than returning the stale cached token.
	seedAccessTokenCache(store, "ws-a", "freee", "stale-token", ref.Add(2*time.Minute))

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("fresh-token", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())
	ts.Now = func() time.Time { return ref }

	got, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "fresh-token" {
		t.Errorf("AccessToken = %q, want %q (must refresh within margin)", got, "fresh-token")
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("token endpoint called %d times, want 1", n)
	}
}

func TestOAuth2TokenSource_RefreshTokenRotation_PersistsNewValue(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-old")
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT", "RT-new", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken)); v != "RT-new" {
		t.Errorf("persisted refresh_token = %q, want rotated value %q", v, "RT-new")
	}
}

func TestOAuth2TokenSource_RefreshTokenNotRotated_KeepsExisting(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-stable")
	// No refresh_token field in the response at all (Google-style: never
	// rotates).
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken)); v != "RT-stable" {
		t.Errorf("persisted refresh_token = %q, want unchanged %q", v, "RT-stable")
	}
}

// TestOAuth2TokenSource_PersistenceOrder_RefreshTokenWriteFailureAborts pins
// docs/plans/api-gateway.md §6's load-bearing ordering: a refresh_token
// persist failure must abort the WHOLE refresh (no access_token/expires_at
// write, no access token handed back to the caller) even though the token
// endpoint already returned a perfectly good response.
func TestOAuth2TokenSource_PersistenceOrder_RefreshTokenWriteFailureAborts(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-old")
	store.failWritesTo(OAuthSecretKey("freee", oauthFieldRefreshToken))

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-should-not-be-used", "RT-new", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	got, err := ts.AccessToken("ws-a", "freee")
	if err == nil {
		t.Fatalf("AccessToken: want error when refresh_token persist fails, got token %q", got)
	}
	if got != "" {
		t.Errorf("AccessToken returned %q on a failed refresh, want empty", got)
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("token endpoint called %d times, want 1 (the round trip itself must still happen)", n)
	}
	// refresh_token must still read as the OLD value — the failed write
	// never landed the rotated one.
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken)); v != "RT-old" {
		t.Errorf("refresh_token = %q after a failed persist, want unchanged %q", v, "RT-old")
	}
	// access_token/expires_at (one combined key) must NEVER have been
	// written — Phase 2 must not run when Phase 1 fails.
	if _, err := getAccessTokenCache(store, "ws-a", "freee"); err == nil {
		t.Error("access_token cache was persisted despite a failed refresh_token persist — persistence order violated")
	}
}

// TestOAuth2TokenSource_AccessTokenPersistFailureIsNotFatal is the mirror
// case: once refresh_token is safely persisted, the grant itself is safe —
// a subsequent access_token/expires_at cache-write failure is logged but
// must not fail the call or discard the freshly obtained access token.
func TestOAuth2TokenSource_AccessTokenPersistFailureIsNotFatal(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-old")
	store.failWritesTo(OAuthSecretKey("freee", oauthFieldAccessTokenCache))

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-fresh", "RT-new", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	got, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("AccessToken: want no error (refresh_token succeeded), got %v", err)
	}
	if got != "AT-fresh" {
		t.Errorf("AccessToken = %q, want %q", got, "AT-fresh")
	}
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken)); v != "RT-new" {
		t.Errorf("refresh_token = %q, want rotated %q", v, "RT-new")
	}
}

// TestOAuth2TokenSource_NonRotatingProvider_RefreshTokenWriteFailureIsNotFatal
// pins the codex review round-3 Major fix: a NON-rotating provider (no
// refresh_token in the response, e.g. Google) must not have its unchanged
// refresh_token re-persisted at all — so a write failure injected on that
// key must never fail the call, since the write is never even attempted.
// Before this fix, the same (unchanged) value was unconditionally
// re-written every refresh and a failure there was fatal, discarding a
// perfectly good access token the token endpoint had just returned.
func TestOAuth2TokenSource_NonRotatingProvider_RefreshTokenWriteFailureIsNotFatal(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-stable")
	store.failWritesTo(OAuthSecretKey("freee", oauthFieldRefreshToken))

	// No refresh_token in the response at all (non-rotating provider).
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-fresh", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	got, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("AccessToken: want no error (refresh_token was never rotated, so the failing write is never attempted), got %v", err)
	}
	if got != "AT-fresh" {
		t.Errorf("AccessToken = %q, want %q", got, "AT-fresh")
	}
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken)); v != "RT-stable" {
		t.Errorf("refresh_token = %q, want unchanged %q", v, "RT-stable")
	}
}

// TestOAuth2TokenSource_RotatingProvider_ExplicitlySameValue_SkipsWrite pins
// that "not rotated" also covers a provider that echoes back the exact same
// refresh_token value (rather than omitting the field) — both are "no
// actual change", so both skip the redundant write the same way.
func TestOAuth2TokenSource_RotatingProvider_ExplicitlySameValue_SkipsWrite(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-stable")
	store.failWritesTo(OAuthSecretKey("freee", oauthFieldRefreshToken))

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-fresh", "RT-stable", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("AccessToken: want no error (refresh_token value unchanged), got %v", err)
	}
}

func TestOAuth2TokenSource_Singleflight_ConcurrentCallsCoalesce(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-coalesced", "RT-next", 3600))
	stub.gate = make(chan struct{})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	const n = 20
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = ts.AccessToken("ws-a", "freee")
		}(i)
	}
	// Give every goroutine a chance to reach the token endpoint (or at
	// least start racing toward it) before releasing the single request
	// the stub should actually see.
	time.Sleep(50 * time.Millisecond)
	close(stub.gate)
	wg.Wait()

	if n := stub.callCount(); n != 1 {
		t.Errorf("token endpoint called %d times, want exactly 1 (singleflight must coalesce concurrent refreshes)", n)
	}
	for i := range results {
		if errs[i] != nil {
			t.Errorf("goroutine %d: AccessToken error: %v", i, errs[i])
		}
		if results[i] != "AT-coalesced" {
			t.Errorf("goroutine %d: AccessToken = %q, want %q", i, results[i], "AT-coalesced")
		}
	}
}

func TestOAuth2TokenSource_SingleflightKeyReleasedAfterCompletion(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT1", "", 1)) // expires almost immediately
	ref := time.Now()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())
	ts.Now = func() time.Time { return ref }

	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("first AccessToken: %v", err)
	}
	if n := stub.callCount(); n != 1 {
		t.Fatalf("after first call: token endpoint called %d times, want 1", n)
	}

	// Advance the clock well past the 1-second expiry + margin so a second
	// call must refresh again — if the singleflight map entry were never
	// released, this call would hang forever waiting on a call that already
	// finished.
	ts.Now = func() time.Time { return ref.Add(1 * time.Hour) }
	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}
	if n := stub.callCount(); n != 2 {
		t.Errorf("after second call: token endpoint called %d times, want 2 (singleflight key must be released between refreshes)", n)
	}
}

func TestOAuth2TokenSource_UnknownProvider_Error(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource(nil, store.resolver(), store.writer())
	if _, err := ts.AccessToken("ws-a", "nonexistent"); err == nil {
		t.Fatal("AccessToken for an unconfigured provider: want error, got nil")
	} else if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error %q does not mention the provider being unconfigured", err.Error())
	}
}

func TestOAuth2TokenSource_NoRefreshTokenConfigured_Error(t *testing.T) {
	store := newMemSecretStore()
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	if _, err := ts.AccessToken("ws-a", "freee"); err == nil {
		t.Fatal("AccessToken with no refresh_token seeded: want error, got nil")
	} else if !strings.Contains(err.Error(), "refresh_token") {
		t.Errorf("error %q does not mention refresh_token", err.Error())
	}
	if n := stub.callCount(); n != 0 {
		t.Errorf("token endpoint called %d times, want 0 (must fail before ever contacting the endpoint)", n)
	}
}

// TestOAuth2TokenSource_TokenEndpointErrorResponse_Surfaces pins the codex
// review round-4 fix alongside its original intent: the RFC 6749 §5.2 error
// CODE (a fixed, non-secret enum — invalid_grant, invalid_client, ...) is
// still surfaced to the caller, but error_description (a provider-authored
// free-text field with no such guarantee) must NOT be — it must only be
// logged server-side, never returned in an error a sandbox-facing 502 could
// echo (Server.ServeHTTP's existing fail-fast precheck response does
// exactly that for a Resolve failure).
func TestOAuth2TokenSource_TokenEndpointErrorResponse_Surfaces(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-revoked")
	body, _ := json.Marshal(oauthErrorResponse{Error: "invalid_grant", ErrorDescription: "Token has been revoked for account nose@example.com"})
	stub := newTokenEndpointStub(t, http.StatusBadRequest, string(body))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	_, err := ts.AccessToken("ws-a", "freee")
	if err == nil {
		t.Fatal("AccessToken: want error for a token-endpoint error response, got nil")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error %q does not surface the provider's error code", err.Error())
	}
	if strings.Contains(err.Error(), "revoked") || strings.Contains(err.Error(), "nose@example.com") {
		t.Errorf("error %q leaks error_description (provider-authored free text) to the caller — it must be logged server-side only", err.Error())
	}
}

// TestOAuth2TokenSource_TransportErrorDoesNotLeakTokenEndpointHost pins the
// codex review round-4 Major finding directly: a raw transport failure
// (connection refused here) must never surface the token endpoint's real
// host in the error AccessToken returns — that error is exactly what
// Server.ServeHTTP's Resolve fail-fast precheck echoes into the SANDBOX-
// facing 502 response body (internal/apigateway/server.go), and
// cfg.TokenEndpoint's real hostname is upstream infrastructure detail the
// gateway's whole design principle (docs/plans/api-gateway.md §1) says must
// never reach the sandbox.
func TestOAuth2TokenSource_TransportErrorDoesNotLeakTokenEndpointHost(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	// A closed listener: dialing it fails with a transport error whose
	// message embeds this exact host:port.
	closedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	deadTokenEndpoint := "http://" + closedListener.Addr().String() + "/token"
	closedListener.Close()

	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: deadTokenEndpoint}}, store.resolver(), store.writer())
	_, gotErr := ts.AccessToken("ws-a", "freee")
	if gotErr == nil {
		t.Fatal("AccessToken: want error for a transport failure, got nil")
	}
	host := closedListener.Addr().String()
	if strings.Contains(gotErr.Error(), host) {
		t.Errorf("error %q leaks the token endpoint's real host %q to the caller", gotErr.Error(), host)
	}
}

func TestOAuth2TokenSource_ExpiresInMissing_FallsBackConservatively(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	stub := newTokenEndpointStub(t, http.StatusOK, `{"access_token":"AT1"}`) // no expires_in at all
	ref := time.Now()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())
	ts.Now = func() time.Time { return ref }

	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	wantExpiry := ref.Add(fallbackAccessTokenLifetime).Unix()
	cache, err := getAccessTokenCache(store, "ws-a", "freee")
	if err != nil {
		t.Fatalf("getAccessTokenCache: %v", err)
	}
	if cache.ExpiresAt != wantExpiry {
		t.Errorf("persisted expires_at = %d, want fallback %d", cache.ExpiresAt, wantExpiry)
	}
}

// TestOAuth2TokenSource_ExplicitNonPositiveExpiresIn_TreatedAsAlreadyExpired
// pins the codex review round-6 Major fix: an explicit expires_in: 0 (the
// token endpoint itself declaring the access token already invalid) must be
// treated as already-expired, NOT given fallbackAccessTokenLifetime's
// generous benefit of the doubt the way a genuinely OMITTED expires_in is —
// the two are indistinguishable through a plain (non-pointer) flexibleInt
// field, which was the bug.
func TestOAuth2TokenSource_ExplicitNonPositiveExpiresIn_TreatedAsAlreadyExpired(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	stub := newTokenEndpointStub(t, http.StatusOK, `{"access_token":"AT-questionable","expires_in":0}`)
	ref := time.Now()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())
	ts.Now = func() time.Time { return ref }

	// The first call still gets the token the endpoint actually returned —
	// this isn't rejected outright as an error.
	got, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "AT-questionable" {
		t.Errorf("AccessToken = %q, want %q", got, "AT-questionable")
	}
	if n := stub.callCount(); n != 1 {
		t.Fatalf("after first call: token endpoint called %d times, want 1", n)
	}

	// A SECOND call, at the exact same instant (no time has passed), must
	// refresh again rather than treating this token as valid for
	// fallbackAccessTokenLifetime — proving expires_in:0 did not fall into
	// the same branch as a genuinely omitted expires_in.
	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}
	if n := stub.callCount(); n != 2 {
		t.Errorf("after second call (same instant): token endpoint called %d times, want 2 (expires_in:0 must not be cached as fresh)", n)
	}
}

func TestOAuth2TokenSource_ClientSecretResolvedAndSent(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	store.seed("ws-a", "freee-client-secret", "shh-its-a-secret")
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT1", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", TokenEndpoint: stub.srv.URL, ClientID: "the-client-id", ClientSecretKey: "freee-client-secret",
	}}, store.resolver(), store.writer())

	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got := stub.lastForm.Get("client_id"); got != "the-client-id" {
		t.Errorf("form client_id = %q, want %q", got, "the-client-id")
	}
	if got := stub.lastForm.Get("client_secret"); got != "shh-its-a-secret" {
		t.Errorf("form client_secret = %q, want %q", got, "shh-its-a-secret")
	}
	if got := stub.lastForm.Get("grant_type"); got != "refresh_token" {
		t.Errorf("form grant_type = %q, want %q", got, "refresh_token")
	}
	if got := stub.lastForm.Get("refresh_token"); got != "RT-initial" {
		t.Errorf("form refresh_token = %q, want %q", got, "RT-initial")
	}
	if stub.lastForm.Has("scope") {
		t.Error("form must not include a scope parameter (PR2 scope note: refresh_token grant never sends scope)")
	}
}

func TestOAuth2TokenSource_NoClientSecretKey_OmitsClientSecretParam(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT1", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL, ClientID: "cid"}}, store.resolver(), store.writer())

	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if stub.lastForm.Has("client_secret") {
		t.Error("form must not include client_secret when ClientSecretKey is empty (public client)")
	}
}

// TestOAuth2TokenSource_MemCache_AvoidsRedundantRefreshAfterAccessTokenPersistFailure
// pins the codex review round-1 Major fix: when the access_token/expires_at
// secret-store persist (Phase 2) fails, a SUBSEQUENT AccessToken call for the
// same (namespace, provider) — e.g. Inject's own call, right after Resolve's
// precheck triggered this very refresh (docs/plans/api-gateway.md §6,
// internal/apigateway/credentials.go's Resolve/Inject shape) — must NOT
// trigger a second token-endpoint round trip. Before this fix it would: with
// no access_token persisted, cachedAccessToken had nothing to fall back on.
func TestOAuth2TokenSource_MemCache_AvoidsRedundantRefreshAfterAccessTokenPersistFailure(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	store.failWritesTo(OAuthSecretKey("freee", oauthFieldAccessTokenCache))

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-fresh", "RT-new", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	got1, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("first AccessToken: %v", err)
	}
	if got1 != "AT-fresh" {
		t.Fatalf("first AccessToken = %q, want %q", got1, "AT-fresh")
	}
	if n := stub.callCount(); n != 1 {
		t.Fatalf("after first call: token endpoint called %d times, want 1", n)
	}

	// access_token was never actually persisted (write failure injected
	// above) — a secret-store-only cache would find nothing here and
	// refresh again. memCache must short-circuit that.
	got2, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}
	if got2 != "AT-fresh" {
		t.Errorf("second AccessToken = %q, want %q (from memCache)", got2, "AT-fresh")
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("after second call: token endpoint called %d times, want still 1 (memCache must avoid a redundant refresh)", n)
	}
}

// TestOAuth2TokenSource_MemCache_AvoidsRedundantRefreshAfterSingleflightWindowCloses
// pins the codex review round-1 Major fix for the OTHER gap the same
// memCache addition closes: a caller arriving strictly AFTER another
// goroutine's refresh for the same (namespace, provider) already completed
// (and therefore outside singleflightGroup's in-flight coalescing window —
// singleflight, by design, only coalesces OVERLAPPING calls) must still find
// the just-refreshed token via memCache instead of unconditionally
// refreshing again.
func TestOAuth2TokenSource_MemCache_AvoidsRedundantRefreshAfterSingleflightWindowCloses(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-fresh", "RT-new", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("first AccessToken: %v", err)
	}
	if n := stub.callCount(); n != 1 {
		t.Fatalf("after first call: token endpoint called %d times, want 1", n)
	}

	// The singleflight map entry for this key is already gone (Do deletes
	// it once fn returns) — a second call here exercises a completely fresh
	// AccessToken -> cachedAccessToken path, not a coalesced wait.
	got, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}
	if got != "AT-fresh" {
		t.Errorf("second AccessToken = %q, want %q", got, "AT-fresh")
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("after second call: token endpoint called %d times, want still 1", n)
	}
}

// TestOAuth2TokenSource_TokenEndpointRedirect_Refused pins the codex review
// round-1 Major (security) fix: the token endpoint responding with a 307/308
// redirect must NOT be followed — Go's default http.Client redirect policy
// preserves method+body across 307/308 (RFC 7538), which would repost
// refresh_token (and client_secret, for a confidential client) to WHATEVER
// URL the Location header names, including an http:// (plaintext) one,
// silently defeating config-load's "token_endpoint must be https"
// guarantee. The redirect TARGET server below must never be contacted.
func TestOAuth2TokenSource_TokenEndpointRedirect_Refused(t *testing.T) {
	var redirectTargetHits int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&redirectTargetHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"should-never-be-used","expires_in":3600}`))
	}))
	defer redirectTarget.Close()

	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", redirectTarget.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer tokenEndpoint.Close()

	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: tokenEndpoint.URL}}, store.resolver(), store.writer())

	_, err := ts.AccessToken("ws-a", "freee")
	if err == nil {
		t.Fatal("AccessToken: want error when the token endpoint redirects, got nil")
	}
	if n := atomic.LoadInt32(&redirectTargetHits); n != 0 {
		t.Errorf("redirect target was contacted %d times, want 0 (the client must refuse to follow the redirect)", n)
	}
}

// TestOAuth2TokenSource_ShortLivedToken_ConsideredFreshImmediatelyAfterRefresh
// pins the codex review round-2 Major fix: a provider whose access token
// lifetime (expires_in) is shorter than (or comparable to) RefreshMargin
// must still be considered fresh immediately after being obtained — not
// judged "already needs refresh" the instant it's minted, which would
// otherwise force every single AccessToken call (including the very next
// one, e.g. Inject's call right after Resolve triggered this refresh) to
// refresh again.
func TestOAuth2TokenSource_ShortLivedToken_ConsideredFreshImmediatelyAfterRefresh(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	const expiresInSeconds = 60 // shorter than DefaultOAuth2RefreshMargin (5m)
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-short-lived", "", expiresInSeconds))
	ref := time.Now()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())
	ts.Now = func() time.Time { return ref }

	got1, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("first AccessToken: %v", err)
	}
	if got1 != "AT-short-lived" {
		t.Fatalf("first AccessToken = %q, want %q", got1, "AT-short-lived")
	}
	if n := stub.callCount(); n != 1 {
		t.Fatalf("after first call: token endpoint called %d times, want 1", n)
	}

	// Immediately after (same instant — ts.Now is frozen at ref): a
	// secret-store-only cache (margin unclamped) would already consider
	// this token "within the 5-minute margin of expiring" (since it only
	// lives 60s total) and refresh again. The effective (clamped) margin
	// computed at refresh time must prevent that.
	got2, err := ts.AccessToken("ws-a", "freee")
	if err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}
	if got2 != "AT-short-lived" {
		t.Errorf("second AccessToken = %q, want %q (must reuse the just-obtained token)", got2, "AT-short-lived")
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("after second call: token endpoint called %d times, want still 1 (short-lived token must not trigger an immediate re-refresh)", n)
	}

	// Once genuinely close to ITS OWN expiry (not the original 5-minute
	// margin), it must still refresh — proving this isn't just "never
	// refresh a short-lived token again".
	ts.Now = func() time.Time { return ref.Add(55 * time.Second) } // 5s remaining
	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("third AccessToken: %v", err)
	}
	if n := stub.callCount(); n != 2 {
		t.Errorf("after third call (near actual expiry): token endpoint called %d times, want 2", n)
	}
}

// TestOAuth2TokenSource_NormalLifetimeToken_MarginUnaffectedByShortLivedClamp
// pins that the clamp added for short-lived tokens is a no-op for a normal
// (long-lived) provider — the regression
// TestOAuth2TokenSource_WithinMargin_Refreshes already covers this for a
// secret-store-only (unclamped) cache read; this test covers the SAME
// invariant for a token that went through a real refresh (memCache path,
// where the clamp computation actually runs).
func TestOAuth2TokenSource_NormalLifetimeToken_MarginUnaffectedByShortLivedClamp(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT1", "", 3600)) // 1h, comfortably above the 5m margin
	ref := time.Now()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())
	ts.Now = func() time.Time { return ref }

	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("first AccessToken: %v", err)
	}

	// 10 minutes remaining (outside the 5-minute margin) — must still be
	// considered fresh.
	ts.Now = func() time.Time { return ref.Add(50 * time.Minute) }
	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("with 10m remaining (outside margin): token endpoint called %d times, want still 1", n)
	}

	// 2 minutes remaining (inside the 5-minute margin) — must refresh, same
	// as the plain (unclamped) case: expiresIn(3600s)/2 = 1800s, comfortably
	// larger than the 5-minute margin, so the clamp never engages here.
	ts.Now = func() time.Time { return ref.Add(58 * time.Minute) }
	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("third AccessToken: %v", err)
	}
	if n := stub.callCount(); n != 2 {
		t.Errorf("with 2m remaining (inside margin): token endpoint called %d times, want 2 (must still refresh)", n)
	}
}

// TestOAuth2TokenSource_SingleflightRecheck_AvoidsRefreshWhenAnotherGoroutineAlreadyRefreshed
// pins the codex review round-2 Major fix directly at the singleflight
// boundary: a goroutine that decides "needs refresh" from a stale read, then
// enters t.sf.Do AFTER a different goroutine's refresh for the same key has
// ALREADY completed and released the singleflight slot (so it does NOT get
// coalesced — singleflight only coalesces overlapping calls), must still
// avoid a redundant network round trip by re-checking the (now fresh)
// cache inside the closure before calling refresh.
func TestOAuth2TokenSource_SingleflightRecheck_AvoidsRefreshWhenAnotherGoroutineAlreadyRefreshed(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-fresh", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	// Goroutine A's refresh runs to completion (and releases the
	// singleflight slot) BEFORE we simulate goroutine B's late arrival by
	// directly invoking Do with the same key — the shape AccessToken itself
	// would produce for a caller that stalled between its own cache check
	// and entering t.sf.Do.
	if _, err := ts.AccessToken("ws-a", "freee"); err != nil {
		t.Fatalf("goroutine A's AccessToken: %v", err)
	}
	if n := stub.callCount(); n != 1 {
		t.Fatalf("after A: token endpoint called %d times, want 1", n)
	}

	// refreshWithRecheck is EXACTLY what AccessToken's t.sf.Do closure runs
	// — calling it directly here (bypassing t.sf.Do itself, whose map entry
	// for this key is already gone since A's call completed) simulates a
	// caller that would have "won" a brand-new singleflight slot (since
	// there is no in-flight call to join) but must still re-check the
	// cache before actually refreshing.
	got, err := ts.refreshWithRecheck("ws-a", OAuthProviderConfig{Name: "freee", TokenEndpoint: stub.srv.URL})
	if err != nil {
		t.Fatalf("refreshWithRecheck: %v", err)
	}
	if got != "AT-fresh" {
		t.Errorf("refreshWithRecheck = %q, want %q (A's cached token, not a fresh one)", got, "AT-fresh")
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("token endpoint called %d times, want still 1 (B must reuse A's freshly cached token via the recheck)", n)
	}
}

// TestOAuth2TokenSource_EmptyAndDefaultNamespace_ShareSingleflightAndMemCache
// pins the codex review round-5 Major fix: internal/dispatcher.SecretStore
// treats namespace "" and "default" as the exact same row
// (normalizeNamespace), so this package's own singleflight/memCache keys
// must agree — a caller passing "" and a caller passing "default" for the
// same provider must be coalesced onto ONE refresh, never two independent
// ones racing against the same refresh_token.
func TestOAuth2TokenSource_EmptyAndDefaultNamespace_ShareSingleflightAndMemCache(t *testing.T) {
	store := newMemSecretStore()
	// Seeded under "default" — the resolver/writer below always normalize
	// "" to "default" too (mirroring the real SecretStore), so this is the
	// one row either namespace argument actually reads/writes.
	store.seed("default", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-fresh", "", 3600))

	normalizingResolver := func(namespace, key string) (string, error) {
		return store.get(normalizeNamespace(namespace), key)
	}
	normalizingWriter := func(namespace, key, value string) error {
		return store.writer()(normalizeNamespace(namespace), key, value)
	}
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, normalizingResolver, normalizingWriter)

	if _, err := ts.AccessToken("", "freee"); err != nil {
		t.Fatalf("AccessToken(\"\"): %v", err)
	}
	if n := stub.callCount(); n != 1 {
		t.Fatalf("after AccessToken(\"\"): token endpoint called %d times, want 1", n)
	}

	// A second call under the EXPLICIT "default" namespace must reuse the
	// same cached token — it is, per SecretStore's own normalization, the
	// identical row "" just populated.
	got, err := ts.AccessToken("default", "freee")
	if err != nil {
		t.Fatalf("AccessToken(\"default\"): %v", err)
	}
	if got != "AT-fresh" {
		t.Errorf("AccessToken(\"default\") = %q, want %q", got, "AT-fresh")
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("after AccessToken(\"default\"): token endpoint called %d times, want still 1 (\"\" and \"default\" must share one cache/singleflight key)", n)
	}
}

// TestOAuth2TokenSource_TokenEndpointErrorResponse_UnrecognizedCodeNotEchoed
// pins the codex review round-5 Major fix: an "error" value that is not a
// genuine RFC 6749 §5.2 enum member (a non-compliant or compromised token
// endpoint could put anything there — a hostname, PII, ...) must never be
// echoed into the error AccessToken returns, even though the round-4 fix
// already stopped echoing error_description.
func TestOAuth2TokenSource_TokenEndpointErrorResponse_UnrecognizedCodeNotEchoed(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	body, _ := json.Marshal(oauthErrorResponse{Error: "internal-host-leak.example.com: connection details", ErrorDescription: "irrelevant"})
	stub := newTokenEndpointStub(t, http.StatusBadRequest, string(body))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	_, err := ts.AccessToken("ws-a", "freee")
	if err == nil {
		t.Fatal("AccessToken: want error, got nil")
	}
	if strings.Contains(err.Error(), "internal-host-leak.example.com") {
		t.Errorf("error %q echoes an unrecognized, unvalidated error code to the caller", err.Error())
	}
}

func TestOAuth2TokenSource_NilReceiver_ReturnsError(t *testing.T) {
	var ts *OAuth2TokenSource
	if _, err := ts.AccessToken("ws-a", "freee"); err == nil {
		t.Error("nil *OAuth2TokenSource.AccessToken: want error, got nil")
	}
}

func TestOAuthSecretKey_Format(t *testing.T) {
	if got, want := OAuthSecretKey("freee", "refresh_token"), "oauth2:freee:refresh_token"; got != want {
		t.Errorf("OAuthSecretKey = %q, want %q", got, want)
	}
}

func TestFlexibleInt_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		json string
		want int64
	}{
		{`3600`, 3600},
		{`"3600"`, 3600},
		{`0`, 0},
		{`null`, 0},
	}
	for _, c := range cases {
		var f flexibleInt
		if err := json.Unmarshal([]byte(c.json), &f); err != nil {
			t.Errorf("Unmarshal(%s): %v", c.json, err)
			continue
		}
		if int64(f) != c.want {
			t.Errorf("Unmarshal(%s) = %d, want %d", c.json, f, c.want)
		}
	}
	var f flexibleInt
	if err := json.Unmarshal([]byte(`"not-a-number"`), &f); err == nil {
		t.Error("Unmarshal(\"not-a-number\"): want error, got nil")
	}
}
