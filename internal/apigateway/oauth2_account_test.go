package apigateway

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// This file pins PR-2 of docs/plans/api-gateway-credential-accounts.md: the
// credentialID type (§3, D6) and oauth2's account support built on it. See
// oauth2.go's own credentialID doc comment for the rationale; this file
// exercises the reviewer scoring table's PR-2 rows (#1/#2/#4/#5/#6, plus the
// refresh_token persistence-order invariant carried forward with account).

// --- credentialID itself (D2, D6, D1) ---

func TestCredentialID_SecretPrefix_NoAccount_MatchesProviderAlone(t *testing.T) {
	got := credentialID{provider: "freee"}.secretPrefix()
	if got != "freee" {
		t.Errorf("secretPrefix() = %q, want %q (byte-identical to the bare provider name, D2)", got, "freee")
	}
}

func TestCredentialID_SecretPrefix_WithAccount_AppendsAccount(t *testing.T) {
	got := credentialID{provider: "freee", account: "ubs"}.secretPrefix()
	if got != "freee@ubs" {
		t.Errorf("secretPrefix() = %q, want %q (D1 separator)", got, "freee@ubs")
	}
}

func TestCredentialID_CacheKey_NoAccount_MatchesPreAccountSupportFormat(t *testing.T) {
	got := credentialID{provider: "freee"}.cacheKey("ws-a")
	want := "ws-a\x00freee"
	if got != want {
		t.Errorf("cacheKey(\"ws-a\") = %q, want %q (byte-identical to the pre-PR-2 namespace+\"\\x00\"+provider format, D2)", got, want)
	}
}

func TestCredentialID_CacheKey_WithAccount_IncludesAccount(t *testing.T) {
	got := credentialID{provider: "freee", account: "ubs"}.cacheKey("ws-a")
	want := "ws-a\x00freee@ubs"
	if got != want {
		t.Errorf("cacheKey(\"ws-a\") = %q, want %q", got, want)
	}
}

// --- #1 (oauth2 version): account-less requests are byte-identical to
// pre-PR-2 behavior ---

// TestOAuth2TokenSource_AccessToken_NoAccount_UsesUnqualifiedSecretKeys pins
// the reviewer scoring table's #1 for oauth2: an account-less AccessToken
// call must read/write EXACTLY the same secret-store keys as before
// credentialID existed. Seeding/reading via LITERAL key strings (not the
// OAuthSecretKey helper) is deliberate — it proves the actual bytes on the
// wire to the secret store, independent of whether OAuthSecretKey's own
// implementation happens to agree with this test's expectation.
func TestOAuth2TokenSource_AccessToken_NoAccount_UsesUnqualifiedSecretKeys(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "oauth2:freee:refresh_token", "RT-initial")

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT1", "RT2", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	got, err := ts.AccessToken("ws-a", credentialID{provider: "freee"})
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "AT1" {
		t.Errorf("AccessToken = %q, want %q", got, "AT1")
	}
	if v, _ := store.get("ws-a", "oauth2:freee:refresh_token"); v != "RT2" {
		t.Errorf("literal key \"oauth2:freee:refresh_token\" = %q, want rotated value %q", v, "RT2")
	}
	if _, err := store.get("ws-a", "oauth2:freee:access_token_cache"); err != nil {
		t.Errorf("literal key \"oauth2:freee:access_token_cache\" was not written: %v", err)
	}
	// The singleflight/memCache key must also be the pre-PR-2 literal shape
	// — proven indirectly: a second call with the SAME credentialID must hit
	// memCache (no second token-endpoint round trip).
	if _, err := ts.AccessToken("ws-a", credentialID{provider: "freee"}); err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("token endpoint called %d times, want 1 (second call must hit memCache)", n)
	}
}

// --- D6: account-qualified requests use account-qualified keys ---

// TestOAuth2TokenSource_AccessToken_WithAccount_UsesQualifiedSecretKeys pins
// D6's secret-store half: an account-qualified request reads/writes
// "oauth2:<provider>@<account>:<field>", asserted as a literal string.
func TestOAuth2TokenSource_AccessToken_WithAccount_UsesQualifiedSecretKeys(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "oauth2:freee@ubs:refresh_token", "RT-ubs-initial")

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-ubs", "RT-ubs-2", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	got, err := ts.AccessToken("ws-a", credentialID{provider: "freee", account: "ubs"})
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "AT-ubs" {
		t.Errorf("AccessToken = %q, want %q", got, "AT-ubs")
	}
	if v, _ := store.get("ws-a", "oauth2:freee@ubs:refresh_token"); v != "RT-ubs-2" {
		t.Errorf("literal key \"oauth2:freee@ubs:refresh_token\" = %q, want rotated value %q", v, "RT-ubs-2")
	}
	if _, err := store.get("ws-a", "oauth2:freee@ubs:access_token_cache"); err != nil {
		t.Errorf("literal key \"oauth2:freee@ubs:access_token_cache\" was not written: %v", err)
	}
	// The unqualified keys must never have been touched.
	if _, err := store.get("ws-a", "oauth2:freee:refresh_token"); err == nil {
		t.Error("the unqualified key \"oauth2:freee:refresh_token\" was written for an account-qualified request — account qualification leaked into the wrong key")
	}
}

// --- #2 (oauth2 version) / D3: no fallback to the unqualified credential ---

// TestOAuth2TokenSource_AccessToken_AccountWithoutQualifiedSecret_FailsClosed
// pins D3: when only the unqualified refresh_token exists, an account-
// qualified request must fail rather than silently use it — the account-less
// request (control case) must keep succeeding, isolating that the failure is
// genuinely about account-scoping and not, say, a broken provider config.
func TestOAuth2TokenSource_AccessToken_AccountWithoutQualifiedSecret_FailsClosed(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "oauth2:freee:refresh_token", "RT-unqualified")
	// Deliberately no seed for "oauth2:freee@ubs:refresh_token".

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT1", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	if _, err := ts.AccessToken("ws-a", credentialID{provider: "freee", account: "ubs"}); err == nil {
		t.Error("AccessToken(freee, account=ubs) with no qualified refresh_token: want error (no fallback to the unqualified credential), got nil")
	}
	if n := stub.callCount(); n != 0 {
		t.Errorf("token endpoint called %d times, want 0 (must fail before ever contacting the endpoint)", n)
	}
	if _, err := ts.AccessToken("ws-a", credentialID{provider: "freee"}); err != nil {
		t.Errorf("AccessToken(freee, account=\"\") should still succeed (control case): %v", err)
	}
}

// --- #4 (most important): singleflight/memCache separate accounts into
// independent entries ---

// accountAwareTokenEndpointStub is a token endpoint fake that reflects the
// incoming refresh_token back into a distinguishable access_token
// (access_token = "AT-for-"+refresh_token), so a test can tell WHICH
// account's grant a given response actually answered — a same-content
// tokenEndpointStub response could not distinguish "the right account's
// refresh ran" from "some other account's refresh ran and got cross-wired
// in", which is exactly the failure mode D6 exists to prevent.
type accountAwareTokenEndpointStub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls int
	// gate, when non-nil, is read from once per request before responding —
	// widens the race window for the concurrency test below, mirroring
	// tokenEndpointStub.gate's own role.
	gate chan struct{}
}

func newAccountAwareTokenEndpointStub(t *testing.T) *accountAwareTokenEndpointStub {
	t.Helper()
	stub := &accountAwareTokenEndpointStub{}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("token endpoint: parse form: %v", err)
		}
		stub.mu.Lock()
		stub.calls++
		stub.mu.Unlock()
		if stub.gate != nil {
			<-stub.gate
		}
		rt := r.PostForm.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// No refresh_token in the response (non-rotating) keeps this stub
		// simple — the rotation-specific invariant has its own dedicated
		// test below.
		_, _ = w.Write([]byte(tokenJSON("AT-for-"+rt, "", 3600)))
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *accountAwareTokenEndpointStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestOAuth2TokenSource_AccessToken_DifferentAccounts_DoNotShareCache is the
// sequential half of #4: refreshing one account must not populate (or be
// satisfied by) another account's cache entry, in either direction.
func TestOAuth2TokenSource_AccessToken_DifferentAccounts_DoNotShareCache(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "oauth2:freee@ubs:refresh_token", "RT-ubs")
	store.seed("ws-a", "oauth2:freee@nvt:refresh_token", "RT-nvt")

	stub := newAccountAwareTokenEndpointStub(t)
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	gotUBS, err := ts.AccessToken("ws-a", credentialID{provider: "freee", account: "ubs"})
	if err != nil {
		t.Fatalf("AccessToken(ubs): %v", err)
	}
	if gotUBS != "AT-for-RT-ubs" {
		t.Errorf("AccessToken(ubs) = %q, want %q", gotUBS, "AT-for-RT-ubs")
	}

	gotNVT, err := ts.AccessToken("ws-a", credentialID{provider: "freee", account: "nvt"})
	if err != nil {
		t.Fatalf("AccessToken(nvt): %v", err)
	}
	if gotNVT != "AT-for-RT-nvt" {
		t.Errorf("AccessToken(nvt) = %q, want %q (must NOT be ubs's cached token)", gotNVT, "AT-for-RT-nvt")
	}
	if n := stub.callCount(); n != 2 {
		t.Fatalf("token endpoint called %d times, want 2 (ubs and nvt must each trigger their own refresh)", n)
	}

	// Re-checking ubs must hit its OWN cache entry (still "AT-for-RT-ubs"),
	// not nvt's — and must not trigger a third network call.
	gotUBSAgain, err := ts.AccessToken("ws-a", credentialID{provider: "freee", account: "ubs"})
	if err != nil {
		t.Fatalf("second AccessToken(ubs): %v", err)
	}
	if gotUBSAgain != "AT-for-RT-ubs" {
		t.Errorf("second AccessToken(ubs) = %q, want unchanged %q (must not have been overwritten by nvt's refresh)", gotUBSAgain, "AT-for-RT-ubs")
	}
	if n := stub.callCount(); n != 2 {
		t.Errorf("token endpoint called %d times after re-checking ubs, want still 2 (cache hit)", n)
	}
}

// TestOAuth2TokenSource_AccessToken_DifferentAccounts_SingleflightDoesNotCoalesce
// is the concurrent half of #4 — the specific race D6 calls out as most
// dangerous: concurrent callers for DIFFERENT accounts of the same provider
// must NOT be coalesced by singleflight into a single token-endpoint call
// (which would hand one account's response to the other's caller). Mirrors
// TestOAuth2TokenSource_Singleflight_ConcurrentCallsCoalesce's gate-based
// shape, but splits the goroutines across two accounts and asserts exactly
// 2 token-endpoint calls (one per account) with no cross-contamination in
// any goroutine's result.
func TestOAuth2TokenSource_AccessToken_DifferentAccounts_SingleflightDoesNotCoalesce(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "oauth2:freee@ubs:refresh_token", "RT-ubs")
	store.seed("ws-a", "oauth2:freee@nvt:refresh_token", "RT-nvt")

	stub := newAccountAwareTokenEndpointStub(t)
	stub.gate = make(chan struct{})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	const perAccount = 10
	var wg sync.WaitGroup
	ubsResults := make([]string, perAccount)
	ubsErrs := make([]error, perAccount)
	nvtResults := make([]string, perAccount)
	nvtErrs := make([]error, perAccount)

	for i := 0; i < perAccount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ubsResults[i], ubsErrs[i] = ts.AccessToken("ws-a", credentialID{provider: "freee", account: "ubs"})
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			nvtResults[i], nvtErrs[i] = ts.AccessToken("ws-a", credentialID{provider: "freee", account: "nvt"})
		}(i)
	}
	// Give every goroutine a chance to reach the token endpoint before
	// releasing the (at most two) requests the stub should actually see.
	time.Sleep(50 * time.Millisecond)
	close(stub.gate)
	wg.Wait()

	if n := stub.callCount(); n != 2 {
		t.Errorf("token endpoint called %d times, want exactly 2 (one per account — accounts must not coalesce with each other, but concurrent callers WITHIN an account still must, per the existing singleflight test)", n)
	}
	for i := range ubsResults {
		if ubsErrs[i] != nil {
			t.Errorf("ubs goroutine %d: AccessToken error: %v", i, ubsErrs[i])
		}
		if ubsResults[i] != "AT-for-RT-ubs" {
			t.Errorf("ubs goroutine %d: AccessToken = %q, want %q (credential mixing: got another account's token)", i, ubsResults[i], "AT-for-RT-ubs")
		}
	}
	for i := range nvtResults {
		if nvtErrs[i] != nil {
			t.Errorf("nvt goroutine %d: AccessToken error: %v", i, nvtErrs[i])
		}
		if nvtResults[i] != "AT-for-RT-nvt" {
			t.Errorf("nvt goroutine %d: AccessToken = %q, want %q (credential mixing: got another account's token)", i, nvtResults[i], "AT-for-RT-nvt")
		}
	}
}

// --- #6 / D7: client_secret_key is never account-qualified ---

// resolverSpy wraps a SecretResolver and records every (namespace, key) pair
// it was asked to resolve, in order — used below to assert the LITERAL key
// string client_secret was resolved from, not just the resulting value
// (asserting only the value could not distinguish "resolved from the right
// unqualified key" from "coincidentally resolved from a qualified key that
// happens to hold the same value").
type resolverSpy struct {
	inner SecretResolver
	mu    sync.Mutex
	calls []string // "namespace/key"
}

func (s *resolverSpy) resolver() SecretResolver {
	return func(namespace, key string) (string, error) {
		s.mu.Lock()
		s.calls = append(s.calls, namespace+"/"+key)
		s.mu.Unlock()
		return s.inner(namespace, key)
	}
}

func (s *resolverSpy) calledWith(namespace, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := namespace + "/" + key
	for _, c := range s.calls {
		if c == want {
			return true
		}
	}
	return false
}

// TestOAuth2TokenSource_ClientSecretKey_NotAccountQualified_EvenWithAccount
// pins #6/D7: an account-qualified refresh_token grant must still resolve
// client_secret from the UNQUALIFIED client_secret_key — asserted by
// checking the literal key string the resolver was actually invoked with.
func TestOAuth2TokenSource_ClientSecretKey_NotAccountQualified_EvenWithAccount(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "oauth2:freee@ubs:refresh_token", "RT-ubs")
	store.seed("ws-a", "freee-client-secret", "shh-its-a-secret")
	spy := &resolverSpy{inner: store.resolver()}

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-ubs", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", TokenEndpoint: stub.srv.URL, ClientID: "cid", ClientSecretKey: "freee-client-secret",
	}}, spy.resolver(), store.writer())

	if _, err := ts.AccessToken("ws-a", credentialID{provider: "freee", account: "ubs"}); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got := stub.lastForm.Get("client_secret"); got != "shh-its-a-secret" {
		t.Errorf("form client_secret = %q, want %q", got, "shh-its-a-secret")
	}
	if !spy.calledWith("ws-a", "freee-client-secret") {
		t.Errorf("resolver was never called with the unqualified key %q; calls = %v", "freee-client-secret", spy.calls)
	}
	if spy.calledWith("ws-a", "freee-client-secret@ubs") {
		t.Error("resolver was called with an account-qualified client_secret_key — client_secret must never be account-qualified (D7)")
	}
}

// TestOAuth2TokenSource_ClientCredentialsGrant_WithAccount_QualifiesCacheButNotClientSecret
// extends #6/D7 to the client_credentials grant (which has no refresh_token
// at all, but DOES have an access_token_cache to qualify per D6, and a
// client_secret_key that must stay unqualified exactly like the
// authorization_code grant's).
func TestOAuth2TokenSource_ClientCredentialsGrant_WithAccount_QualifiesCacheButNotClientSecret(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "sp-client-secret", "shh-its-a-secret")
	spy := &resolverSpy{inner: store.resolver()}

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT1", "", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "az", TokenEndpoint: stub.srv.URL, ClientID: "sp-client-id", ClientSecretKey: "sp-client-secret",
		Grant: GrantClientCredentials,
	}}, spy.resolver(), store.writer())

	got, err := ts.AccessToken("ws-a", credentialID{provider: "az", account: "tenant2"})
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "AT1" {
		t.Errorf("AccessToken = %q, want %q", got, "AT1")
	}
	if !spy.calledWith("ws-a", "sp-client-secret") {
		t.Errorf("resolver was never called with the unqualified key %q; calls = %v", "sp-client-secret", spy.calls)
	}
	if spy.calledWith("ws-a", "sp-client-secret@tenant2") {
		t.Error("resolver was called with an account-qualified client_secret_key — client_secret must never be account-qualified (D7), even for the client_credentials grant")
	}
	if _, err := store.get("ws-a", "oauth2:az@tenant2:access_token_cache"); err != nil {
		t.Errorf("literal key \"oauth2:az@tenant2:access_token_cache\" was not written: %v", err)
	}
}

// --- refresh_token persistence order (D6/existing invariant) survives
// account qualification ---

// TestOAuth2TokenSource_PersistenceOrder_WithAccount_RefreshTokenWriteFailureAborts
// mirrors TestOAuth2TokenSource_PersistenceOrder_RefreshTokenWriteFailureAborts
// (oauth2_test.go) with account="ubs": the "refresh_token persists before
// access_token is trusted" invariant (docs/plans/api-gateway.md §6) must
// hold per-credential, not just for the unqualified case — a persist
// failure on account ubs's qualified key must abort ubs's call exactly the
// same way, and must never touch nvt's (or the unqualified) keys.
func TestOAuth2TokenSource_PersistenceOrder_WithAccount_RefreshTokenWriteFailureAborts(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "oauth2:freee@ubs:refresh_token", "RT-ubs-old")
	store.failWritesTo("oauth2:freee@ubs:refresh_token")

	stub := newTokenEndpointStub(t, http.StatusOK, tokenJSON("AT-should-not-be-used", "RT-ubs-new", 3600))
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: stub.srv.URL}}, store.resolver(), store.writer())

	got, err := ts.AccessToken("ws-a", credentialID{provider: "freee", account: "ubs"})
	if err == nil {
		t.Fatalf("AccessToken: want error when the qualified refresh_token persist fails, got token %q", got)
	}
	if got != "" {
		t.Errorf("AccessToken returned %q on a failed refresh, want empty", got)
	}
	if n := stub.callCount(); n != 1 {
		t.Errorf("token endpoint called %d times, want 1 (the round trip itself must still happen)", n)
	}
	if v, _ := store.get("ws-a", "oauth2:freee@ubs:refresh_token"); v != "RT-ubs-old" {
		t.Errorf("refresh_token = %q after a failed persist, want unchanged %q", v, "RT-ubs-old")
	}
	if _, err := store.get("ws-a", "oauth2:freee@ubs:access_token_cache"); err == nil {
		t.Error("access_token cache was persisted despite a failed refresh_token persist — persistence order violated for an account-qualified credential")
	}
	// The unqualified key must never have been created by this
	// account-qualified call at all.
	if _, err := store.get("ws-a", "oauth2:freee:refresh_token"); err == nil {
		t.Error("the unqualified key \"oauth2:freee:refresh_token\" was written by an account-qualified (ubs) call")
	}
}
