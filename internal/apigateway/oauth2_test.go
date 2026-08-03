package apigateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldAccessToken), "cached-token")
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldExpiresAt), strconv.FormatInt(ref.Add(1*time.Hour).Unix(), 10))

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
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldAccessToken)); v != "AT1" {
		t.Errorf("persisted access_token = %q, want %q", v, "AT1")
	}
	wantExpiry := strconv.FormatInt(ref.Add(3600*time.Second).Unix(), 10)
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldExpiresAt)); v != wantExpiry {
		t.Errorf("persisted expires_at = %q, want %q", v, wantExpiry)
	}
}

func TestOAuth2TokenSource_WithinMargin_Refreshes(t *testing.T) {
	store := newMemSecretStore()
	ref := time.Now()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-initial")
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldAccessToken), "stale-token")
	// Within the default 5-minute margin (expires in 2 minutes) — must
	// refresh proactively rather than returning the stale cached token.
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldExpiresAt), strconv.FormatInt(ref.Add(2*time.Minute).Unix(), 10))

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
	// access_token/expires_at must NEVER have been written — Phase 2 must
	// not run when Phase 1 fails.
	if _, err := store.get("ws-a", OAuthSecretKey("freee", oauthFieldAccessToken)); err == nil {
		t.Error("access_token was persisted despite a failed refresh_token persist — persistence order violated")
	}
	if _, err := store.get("ws-a", OAuthSecretKey("freee", oauthFieldExpiresAt)); err == nil {
		t.Error("expires_at was persisted despite a failed refresh_token persist — persistence order violated")
	}
}

// TestOAuth2TokenSource_AccessTokenPersistFailureIsNotFatal is the mirror
// case: once refresh_token is safely persisted, the grant itself is safe —
// a subsequent access_token/expires_at cache-write failure is logged but
// must not fail the call or discard the freshly obtained access token.
func TestOAuth2TokenSource_AccessTokenPersistFailureIsNotFatal(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-old")
	store.failWritesTo(OAuthSecretKey("freee", oauthFieldAccessToken))

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

func TestOAuth2TokenSource_TokenEndpointErrorResponse_Surfaces(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "RT-revoked")
	body, _ := json.Marshal(oauthErrorResponse{Error: "invalid_grant", ErrorDescription: "Token has been revoked"})
	stub := newTokenEndpointStub(t, http.StatusBadRequest, string(body))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	_, err := ts.AccessToken("ws-a", "freee")
	if err == nil {
		t.Fatal("AccessToken: want error for a token-endpoint error response, got nil")
	}
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error %q does not surface the provider's error/error_description", err.Error())
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
	wantExpiry := strconv.FormatInt(ref.Add(fallbackAccessTokenLifetime).Unix(), 10)
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldExpiresAt)); v != wantExpiry {
		t.Errorf("persisted expires_at = %q, want fallback %q", v, wantExpiry)
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
