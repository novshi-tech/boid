package apigateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// cannedResp is one scripted HTTP response a sequencedServer replays.
type cannedResp struct {
	status int
	body   string
}

// sequencedServer is a small httptest-backed fake that replays a fixed
// sequence of responses in order (repeating the last one for any call past
// the end of the list), capturing each request's parsed form body for
// assertions — used by the device-flow tests here to script a
// device_authorization_endpoint/token_endpoint conversation across
// multiple polls (docs/plans/api-gateway.md §7 device flow).
type sequencedServer struct {
	srv *httptest.Server

	mu    sync.Mutex
	resps []cannedResp
	idx   int
	forms []url.Values
}

func newSequencedServer(t *testing.T, resps ...cannedResp) *sequencedServer {
	t.Helper()
	s := &sequencedServer{resps: resps}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("sequencedServer: parse form: %v", err)
		}
		s.mu.Lock()
		i := s.idx
		if i >= len(s.resps) {
			i = len(s.resps) - 1
		}
		s.idx++
		s.forms = append(s.forms, r.PostForm)
		resp := s.resps[i]
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sequencedServer) url() string { return s.srv.URL }

func (s *sequencedServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idx
}

func (s *sequencedServer) formAt(i int) url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.forms) {
		return nil
	}
	return s.forms[i]
}

func oauthErrorJSON(code string) string {
	b, _ := json.Marshal(map[string]string{"error": code})
	return string(b)
}

func deviceAuthJSON(deviceCode, userCode, verificationURI, verificationURIComplete string, expiresIn, interval int) string {
	body := map[string]any{
		"device_code":      deviceCode,
		"user_code":        userCode,
		"verification_uri": verificationURI,
		"expires_in":       expiresIn,
	}
	if verificationURIComplete != "" {
		body["verification_uri_complete"] = verificationURIComplete
	}
	if interval > 0 {
		body["interval"] = interval
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// waitForStatus polls lm.Status(sessionID) until it stops reporting pending
// or deadline elapses, returning the final observed status — used by every
// device-flow test below, whose background pollDeviceGrant goroutine
// completes asynchronously.
func waitForStatus(t *testing.T, lm *LoginManager, sessionID string, deadline time.Duration) (LoginStatus, string) {
	t.Helper()
	end := time.Now().Add(deadline)
	var status LoginStatus
	var errMsg string
	for time.Now().Before(end) {
		var ok bool
		status, errMsg, ok = lm.Status(sessionID)
		if !ok {
			t.Fatalf("Status(%q): session not found", sessionID)
		}
		if status != LoginStatusPending {
			return status, errMsg
		}
		time.Sleep(20 * time.Millisecond)
	}
	return status, errMsg
}

// --- PKCE / state primitives ---

func TestGenerateCodeVerifier_LengthAndURLSafe(t *testing.T) {
	v, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("generateCodeVerifier: %v", err)
	}
	// RFC 7636 §4.1: 43-128 characters from the unreserved character set.
	if len(v) < 43 || len(v) > 128 {
		t.Errorf("len(verifier) = %d, want 43-128", len(v))
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			t.Fatalf("verifier contains non-unreserved character %q: %s", r, v)
		}
	}
}

func TestGenerateCodeVerifier_Unique(t *testing.T) {
	a, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("generateCodeVerifier: %v", err)
	}
	b, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("generateCodeVerifier: %v", err)
	}
	if a == b {
		t.Error("two calls to generateCodeVerifier produced the same value")
	}
}

// TestCodeChallengeS256_RFC7636AppendixBVector pins codeChallengeS256
// against RFC 7636 Appendix B's own worked example.
func TestCodeChallengeS256_RFC7636AppendixBVector(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const wantChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := codeChallengeS256(verifier); got != wantChallenge {
		t.Errorf("codeChallengeS256(%q) = %q, want %q", verifier, got, wantChallenge)
	}
}

// --- buildAuthorizeURL ---

func TestBuildAuthorizeURL_LoopbackIncludesPKCEAndState(t *testing.T) {
	cfg := OAuthProviderConfig{
		Name:                  "google",
		ClientID:              "cid",
		AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
		Scopes:                []string{"a", "b"},
		AuthorizeParams:       map[string]string{"access_type": "offline"},
	}
	raw, err := buildAuthorizeURL(cfg, "http://127.0.0.1:12345/callback", "state123", "challenge456")
	if err != nil {
		t.Fatalf("buildAuthorizeURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := u.Query()
	cases := map[string]string{
		"response_type":         "code",
		"client_id":             "cid",
		"redirect_uri":          "http://127.0.0.1:12345/callback",
		"scope":                 "a b",
		"state":                 "state123",
		"code_challenge":        "challenge456",
		"code_challenge_method": "S256",
		"access_type":           "offline",
	}
	for k, want := range cases {
		if got := q.Get(k); got != want {
			t.Errorf("query[%q] = %q, want %q (url: %s)", k, got, want, raw)
		}
	}
}

func TestBuildAuthorizeURL_ManualHasNoStateOrPKCE(t *testing.T) {
	cfg := OAuthProviderConfig{
		Name:                  "freee",
		ClientID:              "cid",
		AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
	}
	raw, err := buildAuthorizeURL(cfg, "urn:ietf:wg:oauth:2.0:oob", "", "")
	if err != nil {
		t.Fatalf("buildAuthorizeURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := u.Query()
	if q.Get("redirect_uri") != "urn:ietf:wg:oauth:2.0:oob" {
		t.Errorf("redirect_uri = %q, want the OOB urn", q.Get("redirect_uri"))
	}
	if _, present := q["state"]; present {
		t.Error("manual flow's authorize URL must not include state")
	}
	if _, present := q["code_challenge"]; present {
		t.Error("manual flow's authorize URL must not include code_challenge (freee does not support PKCE)")
	}
}

func TestBuildAuthorizeURL_InvalidEndpointRejected(t *testing.T) {
	cfg := OAuthProviderConfig{Name: "bad", ClientID: "cid", AuthorizationEndpoint: "not a url"}
	if _, err := buildAuthorizeURL(cfg, "http://127.0.0.1/callback", "s", "c"); err == nil {
		t.Fatal("want error for a malformed authorization_endpoint, got nil")
	}
}

// --- StartLogin ---

func TestLoginManager_StartLogin_UnknownProvider(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource(nil, store.resolver(), store.writer())
	lm := NewLoginManager(ts)
	if _, err := lm.StartLogin("ws-a", "nope", "", ""); err == nil {
		t.Fatal("want error for an unconfigured provider, got nil")
	}
}

func TestLoginManager_StartLogin_NoFlowConfigured(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{Name: "freee", TokenEndpoint: "https://example.com/token", ClientID: "cid"}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)
	if _, err := lm.StartLogin("ws-a", "freee", "", ""); err == nil {
		t.Fatal("want error when the provider has no Flow configured, got nil")
	}
}

// TestLoginManager_StartLogin_ClientCredentials_RejectedWithDedicatedMessage
// pins docs/plans/api-gateway.md §6-補's requirement that a
// client_credentials provider gets a DEDICATED error from StartLogin — not
// the generic "no flow configured" message every other flow-less provider
// gets (TestLoginManager_StartLogin_NoFlowConfigured, above), which would
// misleadingly suggest setting a flow is the fix.
func TestLoginManager_StartLogin_ClientCredentials_RejectedWithDedicatedMessage(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "sp-client-secret", "shh-its-a-secret")
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "az", TokenEndpoint: "https://example.com/token", ClientID: "sp-client-id", ClientSecretKey: "sp-client-secret",
		Grant: GrantClientCredentials,
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	_, err := lm.StartLogin("ws-a", "az", "", "")
	if err == nil {
		t.Fatal("StartLogin against a client_credentials provider: want error, got nil")
	}
	if strings.Contains(err.Error(), "device/loopback/manual") {
		t.Errorf("error %q reuses the generic flow-selection message instead of a client_credentials-specific one", err.Error())
	}
	if !strings.Contains(err.Error(), "client_credentials") {
		t.Errorf("error %q does not mention client_credentials", err.Error())
	}
}

func TestLoginManager_StartLoopback_RequiresRedirectURI(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "google", ClientID: "cid", TokenEndpoint: "https://example.com/token",
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)
	if _, err := lm.StartLogin("ws-a", "google", "", ""); err == nil {
		t.Fatal("want error when redirect_uri is empty for a loopback provider, got nil")
	}
}

func TestLoginManager_StartLoopback_Success(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "google", ClientID: "cid", TokenEndpoint: "https://example.com/token",
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "google", "http://127.0.0.1:9999/callback", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if start.Flow != LoginFlowLoopback {
		t.Errorf("Flow = %q, want loopback", start.Flow)
	}
	if start.SessionID == "" {
		t.Error("SessionID is empty")
	}
	if start.AuthorizeURL == "" {
		t.Error("AuthorizeURL is empty")
	}
	status, _, ok := lm.Status(start.SessionID)
	if !ok || status != LoginStatusPending {
		t.Errorf("Status = (%v, ok=%v), want (pending, true)", status, ok)
	}
}

func TestLoginManager_StartManual_Success(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", ClientID: "cid", TokenEndpoint: "https://example.com/token",
		Flow: LoginFlowManual, AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "freee", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if start.Flow != LoginFlowManual {
		t.Errorf("Flow = %q, want manual", start.Flow)
	}
	u, err := url.Parse(start.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse AuthorizeURL: %v", err)
	}
	if got := u.Query().Get("redirect_uri"); got != "urn:ietf:wg:oauth:2.0:oob" {
		t.Errorf("redirect_uri = %q, want the OOB urn", got)
	}
}

// --- CompleteLogin (loopback) ---

func TestLoginManager_CompleteLogin_Loopback_Success(t *testing.T) {
	store := newMemSecretStore()
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "google", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "google", "http://127.0.0.1:9999/callback", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, _ := url.Parse(start.AuthorizeURL)
	wantChallenge := u.Query().Get("code_challenge")
	state := u.Query().Get("state")
	if wantChallenge == "" || state == "" {
		t.Fatalf("authorize URL missing code_challenge/state: %s", start.AuthorizeURL)
	}

	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE", state); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	status, _, ok := lm.Status(start.SessionID)
	if !ok || status != LoginStatusComplete {
		t.Fatalf("Status = (%v, ok=%v), want (complete, true)", status, ok)
	}

	form := tokenSrv.formAt(0)
	if form.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", form.Get("grant_type"))
	}
	if form.Get("code") != "AUTH-CODE" {
		t.Errorf("code = %q, want AUTH-CODE", form.Get("code"))
	}
	if form.Get("redirect_uri") != "http://127.0.0.1:9999/callback" {
		t.Errorf("redirect_uri = %q, want the loopback callback URL", form.Get("redirect_uri"))
	}
	verifier := form.Get("code_verifier")
	if verifier == "" {
		t.Fatal("code_verifier missing from the token request")
	}
	if got := codeChallengeS256(verifier); got != wantChallenge {
		t.Errorf("codeChallengeS256(sent code_verifier) = %q, want %q (the authorize URL's own code_challenge)", got, wantChallenge)
	}
	if v, _ := store.get("ws-a", OAuthSecretKey("google", oauthFieldRefreshToken)); v != "RT1" {
		t.Errorf("persisted refresh_token = %q, want RT1", v)
	}
}

// TestLoginManager_CompleteLogin_Loopback_ConfidentialClient_SendsBothPKCEAndSecret
// pins a deliberate generalization: PKCE presence is decided purely by Flow
// (loopback always uses it), while client_secret presence is decided purely
// by whether ClientSecretKey resolves non-empty — the two are independent
// axes, so a loopback provider configured with a ClientSecretKey (unusual,
// but not rejected by config validation) sends BOTH in the same request.
func TestLoginManager_CompleteLogin_Loopback_ConfidentialClient_SendsBothPKCEAndSecret(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "atlassian-secret", "shh-confidential-secret")
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "atlassian", ClientID: "cid", ClientSecretKey: "atlassian-secret", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://auth.atlassian.com/authorize",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "atlassian", "http://127.0.0.1:9999/callback", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, _ := url.Parse(start.AuthorizeURL)
	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE", u.Query().Get("state")); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	form := tokenSrv.formAt(0)
	if form.Get("client_secret") != "shh-confidential-secret" {
		t.Errorf("client_secret = %q, want shh-confidential-secret", form.Get("client_secret"))
	}
	if form.Get("code_verifier") == "" {
		t.Error("code_verifier missing — loopback flow must always use PKCE regardless of ClientSecretKey")
	}
}

func TestLoginManager_CompleteLogin_Loopback_StateMismatchRejected(t *testing.T) {
	store := newMemSecretStore()
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "google", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "google", "http://127.0.0.1:9999/callback", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE", "wrong-state"); err == nil {
		t.Fatal("want error for a state mismatch, got nil")
	}
	if n := tokenSrv.callCount(); n != 0 {
		t.Errorf("token endpoint called %d times, want 0 (must never exchange on a state mismatch)", n)
	}
	status, errMsg, ok := lm.Status(start.SessionID)
	if !ok || status != LoginStatusFailed {
		t.Errorf("Status = (%v, %q, ok=%v), want (failed, _, true)", status, errMsg, ok)
	}
}

func TestLoginManager_CompleteLogin_MissingCodeRejected(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "google", ClientID: "cid", TokenEndpoint: "https://example.com/token",
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)
	start, err := lm.StartLogin("ws-a", "google", "http://127.0.0.1:9999/callback", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, _ := url.Parse(start.AuthorizeURL)
	if err := lm.CompleteLogin(start.SessionID, "", u.Query().Get("state")); err == nil {
		t.Fatal("want error for an empty code, got nil")
	}
}

func TestLoginManager_CompleteLogin_UnknownSession(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource(nil, store.resolver(), store.writer())
	lm := NewLoginManager(ts)
	if err := lm.CompleteLogin("no-such-session", "code", "state"); err == nil {
		t.Fatal("want error for an unknown session, got nil")
	}
}

func TestLoginManager_CompleteLogin_AlreadyCompletedRejected(t *testing.T) {
	store := newMemSecretStore()
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "google", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)
	start, err := lm.StartLogin("ws-a", "google", "http://127.0.0.1:9999/callback", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, _ := url.Parse(start.AuthorizeURL)
	state := u.Query().Get("state")
	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE", state); err != nil {
		t.Fatalf("first CompleteLogin: %v", err)
	}
	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE-2", state); err == nil {
		t.Fatal("want error completing an already-completed session twice, got nil")
	}
	if n := tokenSrv.callCount(); n != 1 {
		t.Errorf("token endpoint called %d times, want 1 (second complete must not re-exchange)", n)
	}
}

func TestLoginManager_CompleteLogin_ExpiredSessionRejected(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "google", ClientID: "cid", TokenEndpoint: "https://example.com/token",
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
	}}, store.resolver(), store.writer())
	ref := time.Now()
	lm := NewLoginManager(ts)
	lm.Now = func() time.Time { return ref }

	start, err := lm.StartLogin("ws-a", "google", "http://127.0.0.1:9999/callback", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, _ := url.Parse(start.AuthorizeURL)
	state := u.Query().Get("state")

	// Advance the clock past pendingLoginTTL.
	lm.Now = func() time.Time { return ref.Add(pendingLoginTTL + time.Minute) }

	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE", state); err == nil {
		t.Fatal("want error completing an expired session, got nil")
	}
	status, _, ok := lm.Status(start.SessionID)
	if !ok || status != LoginStatusExpired {
		t.Errorf("Status = (%v, ok=%v), want (expired, true)", status, ok)
	}
}

// TestLoginManager_CompleteLogin_Loopback_RetryAfterFailureRejected pins
// that a session which already failed (here: a state mismatch) cannot
// later be completed with the CORRECT code/state — CompleteLogin's own doc
// comment states every session is single-shot once it leaves
// LoginStatusPending, in EITHER direction (success or failure), not just
// "already completed".
func TestLoginManager_CompleteLogin_Loopback_RetryAfterFailureRejected(t *testing.T) {
	store := newMemSecretStore()
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "google", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "google", "http://127.0.0.1:9999/callback", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, _ := url.Parse(start.AuthorizeURL)
	correctState := u.Query().Get("state")

	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE", "wrong-state"); err == nil {
		t.Fatal("want error for the first (state-mismatch) attempt, got nil")
	}
	// Retrying with the CORRECT state must still be rejected — the session
	// is already LoginStatusFailed, not LoginStatusPending.
	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE", correctState); err == nil {
		t.Fatal("want error retrying an already-failed session even with the correct state, got nil")
	}
	if n := tokenSrv.callCount(); n != 0 {
		t.Errorf("token endpoint called %d times, want 0 (neither attempt should ever reach the token endpoint)", n)
	}
}

// TestLoginManager_StartDevice_SendsScopesClientIDAndAuthorizeParams pins
// the actual request content postDeviceAuthorizationRequest sends — every
// other device-flow test above only asserts on callCount, not on what was
// actually POSTed.
//
// tokenSrv here is a LOCAL server (never a bare "https://example.com/token"
// placeholder) that terminates the background pollDeviceGrant goroutine
// startDevice always spawns, via an immediate access_denied — every
// device-flow test must let that goroutine reach a terminal state (checked
// here via waitForStatus) before returning, or it keeps running against
// whatever TokenEndpoint the test configured for the rest of this test
// binary's process lifetime. A bare "example.com" placeholder is
// particularly dangerous here: it is a real, live, third-party host, so an
// un-terminated goroutine would make actual outbound network requests to it
// (observed once during development: example.com's real server answered a
// stray POST with a genuine 405, logged mid-test-run purely from unlucky
// timing with an unrelated goroutine — never a test assertion failure, but
// a real, uncontrolled network call a unit test must never make).
func TestLoginManager_StartDevice_SendsScopesClientIDAndAuthorizeParams(t *testing.T) {
	store := newMemSecretStore()
	deviceSrv := newSequencedServer(t, cannedResp{http.StatusOK, deviceAuthJSON("DC1", "USER-CODE", "https://example.com/device", "", 30, 1)})
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusBadRequest, oauthErrorJSON("access_denied")})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "github", ClientID: "gh-client-id", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowDevice, DeviceAuthorizationEndpoint: deviceSrv.url(),
		Scopes:          []string{"repo", "read:user"},
		AuthorizeParams: map[string]string{"custom_param": "custom_value"},
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "github", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	form := deviceSrv.formAt(0)
	if form == nil {
		t.Fatal("device authorization endpoint was never called")
	}
	if form.Get("client_id") != "gh-client-id" {
		t.Errorf("client_id = %q, want gh-client-id", form.Get("client_id"))
	}
	if form.Get("scope") != "repo read:user" {
		t.Errorf("scope = %q, want %q", form.Get("scope"), "repo read:user")
	}
	if form.Get("custom_param") != "custom_value" {
		t.Errorf("custom_param = %q, want custom_value", form.Get("custom_param"))
	}
	if _, present := form["client_secret"]; present {
		t.Error("client_secret must not be sent when ClientSecretKey is unset")
	}

	// Let the background poller terminate (access_denied is terminal)
	// before this test returns — see the doc comment above for why.
	if status, errMsg := waitForStatus(t, lm, start.SessionID, 5*time.Second); status != LoginStatusFailed {
		t.Fatalf("status = %v (%s), want failed", status, errMsg)
	}
}

func TestLoginManager_CompleteLogin_WrongFlowRejected(t *testing.T) {
	store := newMemSecretStore()
	// expires_in=1/interval=1 (rather than a long-lived value): the
	// background pollDeviceGrant goroutine startDevice always spawns is
	// left running past this test's return (its own termination is not
	// what this test is checking), so it must be short-lived — see
	// TestLoginManager_StartDevice_SendsScopesClientIDAndAuthorizeParams's
	// own doc comment for why an un-terminated one is a real hazard.
	deviceSrv := newSequencedServer(t, cannedResp{http.StatusOK, deviceAuthJSON("DC1", "USER-CODE", "https://example.com/device", "", 1, 1)})
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusBadRequest, oauthErrorJSON("authorization_pending")})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "github", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowDevice, DeviceAuthorizationEndpoint: deviceSrv.url(),
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "github", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := lm.CompleteLogin(start.SessionID, "code", ""); err == nil {
		t.Fatal("want error calling CompleteLogin on a device-flow session, got nil")
	}
	// Let the background poller expire (1s) before this test's httptest
	// servers get torn down by t.Cleanup — same reasoning as above.
	waitForStatus(t, lm, start.SessionID, 3*time.Second)
}

// --- CompleteLogin (manual) ---

func TestLoginManager_CompleteLogin_Manual_Success(t *testing.T) {
	store := newMemSecretStore()
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", ClientID: "cid", ClientSecretKey: "freee-secret", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowManual, AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
	}}, store.resolver(), store.writer())
	store.seed("ws-a", "freee-secret", "shh-client-secret")
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "freee", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	// Manual flow never had a state to check — any value (including "")
	// must be accepted.
	if err := lm.CompleteLogin(start.SessionID, "OOB-CODE", ""); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	form := tokenSrv.formAt(0)
	if form.Get("code") != "OOB-CODE" {
		t.Errorf("code = %q, want OOB-CODE", form.Get("code"))
	}
	if form.Get("client_secret") != "shh-client-secret" {
		t.Errorf("client_secret = %q, want shh-client-secret", form.Get("client_secret"))
	}
	if _, present := form["code_verifier"]; present {
		t.Error("manual flow must never send code_verifier (freee does not support PKCE)")
	}
	// RFC 6749 §4.1.3: redirect_uri must be repeated at token-exchange
	// time if it was present in the authorization request — and manual/
	// OOB's authorization request always includes it (the fixed OOB
	// sentinel, oobRedirectURI). Omitting it here is what caused freee's
	// token endpoint to reject the exchange with invalid_client (#936).
	if got := form.Get("redirect_uri"); got != oobRedirectURI {
		t.Errorf("redirect_uri = %q, want %q (RFC 6749 §4.1.3 requires repeating it)", got, oobRedirectURI)
	}
	if v, _ := store.get("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken)); v != "RT1" {
		t.Errorf("persisted refresh_token = %q, want RT1", v)
	}
}

func TestLoginManager_CompleteLogin_NoRefreshTokenReturnedAndNoneOnFile_Fails(t *testing.T) {
	store := newMemSecretStore()
	// access_token only, no refresh_token in the response, and none already
	// stored for this (namespace, provider) — the login must be reported as
	// a failure (requireRefreshToken).
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowManual, AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "freee", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := lm.CompleteLogin(start.SessionID, "OOB-CODE", ""); err == nil {
		t.Fatal("want error when the provider returns no refresh_token and none was already on file, got nil")
	}
	status, _, ok := lm.Status(start.SessionID)
	if !ok || status != LoginStatusFailed {
		t.Errorf("Status = (%v, ok=%v), want (failed, true)", status, ok)
	}
}

func TestLoginManager_CompleteLogin_NoRefreshTokenReturnedButAlreadyOnFile_Succeeds(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("google", oauthFieldRefreshToken), "RT-existing")
	// Google's ordinary re-login shape: no refresh_token in THIS response
	// (already granted once before), but one is already on file — must be
	// accepted, not treated as a failure.
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "google", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "google", "http://127.0.0.1:9999/callback", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, _ := url.Parse(start.AuthorizeURL)
	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE", u.Query().Get("state")); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	status, _, ok := lm.Status(start.SessionID)
	if !ok || status != LoginStatusComplete {
		t.Errorf("Status = (%v, ok=%v), want (complete, true)", status, ok)
	}
}

// --- Device flow ---

func TestLoginManager_StartDevice_Success(t *testing.T) {
	store := newMemSecretStore()
	deviceSrv := newSequencedServer(t, cannedResp{http.StatusOK,
		deviceAuthJSON("DC1", "USER-CODE", "https://example.com/device", "https://example.com/device?user_code=USER-CODE", 30, 1)})
	tokenSrv := newSequencedServer(t,
		cannedResp{http.StatusBadRequest, oauthErrorJSON("authorization_pending")},
		cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)},
	)
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "github", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowDevice, DeviceAuthorizationEndpoint: deviceSrv.url(),
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "github", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if start.Flow != LoginFlowDevice {
		t.Errorf("Flow = %q, want device", start.Flow)
	}
	if start.UserCode != "USER-CODE" {
		t.Errorf("UserCode = %q, want USER-CODE", start.UserCode)
	}
	if start.VerificationURIComplete == "" {
		t.Error("VerificationURIComplete is empty")
	}
	if start.IntervalSeconds != 1 {
		t.Errorf("IntervalSeconds = %d, want 1", start.IntervalSeconds)
	}

	status, errMsg := waitForStatus(t, lm, start.SessionID, 5*time.Second)
	if status != LoginStatusComplete {
		t.Fatalf("status = %v (%s), want complete", status, errMsg)
	}
	if v, _ := store.get("ws-a", OAuthSecretKey("github", oauthFieldRefreshToken)); v != "RT1" {
		t.Errorf("persisted refresh_token = %q, want RT1", v)
	}
	if n := deviceSrv.callCount(); n != 1 {
		t.Errorf("device authorization endpoint called %d times, want 1", n)
	}
	if n := tokenSrv.callCount(); n != 2 {
		t.Errorf("token endpoint called %d times, want 2 (pending then success)", n)
	}
}

func TestLoginManager_StartDevice_AccessDeniedFails(t *testing.T) {
	store := newMemSecretStore()
	deviceSrv := newSequencedServer(t, cannedResp{http.StatusOK, deviceAuthJSON("DC1", "USER-CODE", "https://example.com/device", "", 30, 1)})
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusBadRequest, oauthErrorJSON("access_denied")})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "github", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowDevice, DeviceAuthorizationEndpoint: deviceSrv.url(),
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "github", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	status, errMsg := waitForStatus(t, lm, start.SessionID, 5*time.Second)
	if status != LoginStatusFailed {
		t.Fatalf("status = %v (%s), want failed", status, errMsg)
	}
}

func TestLoginManager_StartDevice_SlowDownThenSucceeds(t *testing.T) {
	store := newMemSecretStore()
	deviceSrv := newSequencedServer(t, cannedResp{http.StatusOK, deviceAuthJSON("DC1", "USER-CODE", "https://example.com/device", "", 30, 1)})
	tokenSrv := newSequencedServer(t,
		cannedResp{http.StatusBadRequest, oauthErrorJSON("slow_down")},
		cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)},
	)
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "github", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowDevice, DeviceAuthorizationEndpoint: deviceSrv.url(),
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)
	// Keep the test fast: override the RFC-mandated 5s slow_down backoff.
	lm.SlowDownIncrement = 10 * time.Millisecond

	start, err := lm.StartLogin("ws-a", "github", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	status, errMsg := waitForStatus(t, lm, start.SessionID, 5*time.Second)
	if status != LoginStatusComplete {
		t.Fatalf("status = %v (%s), want complete", status, errMsg)
	}
	if n := tokenSrv.callCount(); n != 2 {
		t.Errorf("token endpoint called %d times, want 2 (slow_down then success)", n)
	}
}

func TestLoginManager_StartDevice_ExpiresWithoutCompletion(t *testing.T) {
	store := newMemSecretStore()
	// expires_in=1s, interval=1s: the poll never succeeds, and the
	// deviceAuthorization's own 1-second expiry should win the race.
	deviceSrv := newSequencedServer(t, cannedResp{http.StatusOK, deviceAuthJSON("DC1", "USER-CODE", "https://example.com/device", "", 1, 1)})
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusBadRequest, oauthErrorJSON("authorization_pending")})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "github", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowDevice, DeviceAuthorizationEndpoint: deviceSrv.url(),
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "github", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	status, errMsg := waitForStatus(t, lm, start.SessionID, 5*time.Second)
	if status != LoginStatusExpired {
		t.Fatalf("status = %v (%s), want expired", status, errMsg)
	}
}

func TestLoginManager_StartDevice_MissingDeviceAuthorizationEndpoint(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "github", ClientID: "cid", TokenEndpoint: "https://example.com/token", Flow: LoginFlowDevice,
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)
	if _, err := lm.StartLogin("ws-a", "github", "", ""); err == nil {
		t.Fatal("want error when device_authorization_endpoint is unset, got nil")
	}
}

// TestLoginManager_StartLogin_EmptyNamespaceSharesMemCacheWithDefault pins
// that StartLogin normalizes "" to "default" itself (mirroring
// AccessToken's own normalizeNamespace entry point exactly — see that
// function's own doc comment), extending the codex review round-5 fix
// (TestOAuth2TokenSource_EmptyAndDefaultNamespace_ShareSingleflightAndMemCache,
// oauth2_test.go) to this second entry point. A real caller never actually
// reaches LoginManager with namespace=="" (both internal/api/oauth_login.go's
// Start handler and cmd/secret_oauth.go's --namespace flag default it to
// "default" first) — this test exists purely to keep the invariant honest
// for any caller that somehow does.
//
// This is NOT observable by checking which secret-store ROW a login
// persists to: dispatcher.SecretStore.Get/Set (and this test's own
// memSecretStore.resolver/writer, which do not by themselves normalize —
// see memSecretStore's own doc comment in oauth2_test.go) still land on
// the identical row whether or not StartLogin normalizes first, AS LONG AS
// every read/write for THIS grant consistently uses whatever value
// StartLogin decided on — a bug here would not corrupt the persisted
// refresh_token at all. The only place a missing normalization is
// observable is OAuth2TokenSource's OWN in-process memCache key: a
// login-obtained entry cached under the UNNORMALIZED "" key is invisible to
// a subsequent AccessToken("default", ...) call (which always normalizes
// before checking memCache), forcing an extra, unnecessary refresh
// immediately after a successful login — exactly what this test's call-count
// assertion below catches.
func TestLoginManager_StartLogin_EmptyNamespaceSharesMemCacheWithDefault(t *testing.T) {
	store := newMemSecretStore()
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowManual, AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("", "freee", "", "")
	if err != nil {
		t.Fatalf("StartLogin(\"\"): %v", err)
	}
	if err := lm.CompleteLogin(start.SessionID, "OOB-CODE", ""); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if n := tokenSrv.callCount(); n != 1 {
		t.Fatalf("after login: token endpoint called %d times, want 1", n)
	}

	// AccessToken("default", ...) must find the SAME memCache entry the
	// login above just populated — not trigger a second, unnecessary
	// refresh because it was cached under a different ("") key.
	got, err := ts.AccessToken("default", credentialID{provider: "freee"})
	if err != nil {
		t.Fatalf("AccessToken(\"default\"): %v", err)
	}
	if got != "AT1" {
		t.Errorf("AccessToken(\"default\") = %q, want AT1", got)
	}
	if n := tokenSrv.callCount(); n != 1 {
		t.Errorf("after AccessToken(\"default\"): token endpoint called %d times, want still 1 (StartLogin(\"\") and AccessToken(\"default\") must share one memCache key)", n)
	}
}

// --- --account (docs/plans/api-gateway-credential-accounts.md D9, PR-3) ---
//
// This section pins the reviewer scoring table's #10 ("login で取得した
// grant が oauth2:<provider>@<account>:refresh_token に書かれることがテスト
// で示されているか") for all three flows, since PR-3's task description
// specifically calls out the device flow as the one most likely to lose
// account somewhere in its asynchronous (StartLogin returns immediately;
// pollDeviceGrant persists later, from a background goroutine)
// pendingLogin round trip. It also pins D7 (client_secret_key is never
// account-qualified) and D3 (an account-qualified login never touches — and
// never falls back to — the unqualified key) alongside each flow's
// account-qualified persistence, and StartLogin's own account-name
// validation (D11, shared with parsePath — route_test.go's
// TestValidateAccountName_MatchesParsePathAccountValidation pins that the
// RULE itself is identical; the test below pins that StartLogin actually
// APPLIES it).

func TestLoginManager_CompleteLogin_Loopback_WithAccount_PersistsAccountQualifiedKey(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "atlassian-secret", "shh-confidential-secret")
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "atlassian", ClientID: "cid", ClientSecretKey: "atlassian-secret", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowLoopback, AuthorizationEndpoint: "https://auth.atlassian.com/authorize",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "atlassian", "http://127.0.0.1:9999/callback", "ubs")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, _ := url.Parse(start.AuthorizeURL)
	if err := lm.CompleteLogin(start.SessionID, "AUTH-CODE", u.Query().Get("state")); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	// Literal key strings (not OAuthSecretKey/credentialID) — see this
	// section's own doc comment.
	if v, _ := store.get("ws-a", "oauth2:atlassian@ubs:refresh_token"); v != "RT1" {
		t.Errorf("persisted refresh_token at oauth2:atlassian@ubs:refresh_token = %q, want RT1", v)
	}
	if v, _ := store.get("ws-a", "oauth2:atlassian:refresh_token"); v != "" {
		t.Errorf("unqualified key oauth2:atlassian:refresh_token = %q, want empty (account-qualified login must not touch it, D3)", v)
	}

	// D7: client_secret_key resolution is never account-qualified. If
	// exchangeAndPersist had qualified this lookup by mistake, it would have
	// missed the seeded "atlassian-secret" entry (memSecretStore.resolver
	// errors on a missing key) and CompleteLogin above would have failed
	// outright, rather than silently sending a wrong value — so this
	// assertion is really pinning "the login succeeded with the value seeded
	// under the UNQUALIFIED key", which is only possible if the resolver was
	// queried with that exact key.
	if got := tokenSrv.formAt(0).Get("client_secret"); got != "shh-confidential-secret" {
		t.Errorf("client_secret = %q, want shh-confidential-secret (D7: client_secret_key must not be account-qualified)", got)
	}
}

// TestLoginManager_CompleteLogin_Manual_WithAccount_PriorReadIsAccountQualified
// pins docs/plans/api-gateway-credential-accounts.md review item #3:
// exchangeAndPersist's priorRefreshToken read (used ONLY to detect "the
// provider did not rotate, skip the write" — persistGrant's own doc comment)
// must be keyed by the ACCOUNT-QUALIFIED credential
// (OAuthSecretKey(cred.secretPrefix(), ...)), never by cfg.Name/provider
// alone. If that read were mistakenly keyed by the unqualified provider name
// instead, a fresh --account login whose provider happens to return the
// exact same refresh_token already sitting under the UNQUALIFIED key (seeded
// here to simulate a prior unqualified login) would look like "unrotated",
// and persistGrant would skip writing the account-qualified key entirely —
// the account-qualified credential would then never be written at all, even
// though CompleteLogin reports success. This regression is otherwise
// invisible: every existing WithAccount test's provider only ever returns a
// refresh_token that has never been seen at any OTHER key before.
func TestLoginManager_CompleteLogin_Manual_WithAccount_PriorReadIsAccountQualified(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "oauth2:freee:refresh_token", "RT1")
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowManual, AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "freee", "", "ubs")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := lm.CompleteLogin(start.SessionID, "OOB-CODE", ""); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	if v, _ := store.get("ws-a", "oauth2:freee@ubs:refresh_token"); v != "RT1" {
		t.Errorf("persisted refresh_token at oauth2:freee@ubs:refresh_token = %q, want RT1 (must not be suppressed by the UNQUALIFIED key already holding the same value)", v)
	}
}

func TestLoginManager_CompleteLogin_Manual_WithAccount_PersistsAccountQualifiedKey(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "freee-secret", "shh-client-secret")
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", ClientID: "cid", ClientSecretKey: "freee-secret", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowManual, AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "freee", "", "ubs")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := lm.CompleteLogin(start.SessionID, "OOB-CODE", ""); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	if v, _ := store.get("ws-a", "oauth2:freee@ubs:refresh_token"); v != "RT1" {
		t.Errorf("persisted refresh_token at oauth2:freee@ubs:refresh_token = %q, want RT1", v)
	}
	if v, _ := store.get("ws-a", "oauth2:freee:refresh_token"); v != "" {
		t.Errorf("unqualified key oauth2:freee:refresh_token = %q, want empty (account-qualified login must not touch it, D3)", v)
	}
	if got := tokenSrv.formAt(0).Get("client_secret"); got != "shh-client-secret" {
		t.Errorf("client_secret = %q, want shh-client-secret (D7: client_secret_key must not be account-qualified)", got)
	}
}

// TestLoginManager_StartDevice_WithAccount_PersistsAccountQualifiedKey is the
// device-flow half of this section — the one PR-3's task description flags
// as most likely to lose account, since it must survive being carried
// through pendingLogin into a background goroutine (pollDeviceGrant) that
// runs well after StartLogin itself has returned.
func TestLoginManager_StartDevice_WithAccount_PersistsAccountQualifiedKey(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "gh-secret", "shh-gh-secret")
	deviceSrv := newSequencedServer(t, cannedResp{http.StatusOK, deviceAuthJSON("DC1", "USER-CODE", "https://example.com/device", "", 30, 1)})
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "github", ClientID: "cid", ClientSecretKey: "gh-secret", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowDevice, DeviceAuthorizationEndpoint: deviceSrv.url(),
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "github", "", "ubs")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	status, errMsg := waitForStatus(t, lm, start.SessionID, 5*time.Second)
	if status != LoginStatusComplete {
		t.Fatalf("status = %v (%s), want complete", status, errMsg)
	}

	if v, _ := store.get("ws-a", "oauth2:github@ubs:refresh_token"); v != "RT1" {
		t.Errorf("persisted refresh_token at oauth2:github@ubs:refresh_token = %q, want RT1", v)
	}
	if v, _ := store.get("ws-a", "oauth2:github:refresh_token"); v != "" {
		t.Errorf("unqualified key oauth2:github:refresh_token = %q, want empty (account-qualified login must not touch it, D3)", v)
	}
	if got := tokenSrv.formAt(0).Get("client_secret"); got != "shh-gh-secret" {
		t.Errorf("client_secret = %q, want shh-gh-secret (D7: client_secret_key must not be account-qualified)", got)
	}
}

// TestLoginManager_StartDevice_WithAccount_PriorReadIsAccountQualified is
// TestLoginManager_CompleteLogin_Manual_WithAccount_PriorReadIsAccountQualified's
// device-flow counterpart (docs/plans/api-gateway-credential-accounts.md
// review item #3) — pollDeviceGrant has its OWN, independent
// priorRefreshToken read (it does not go through exchangeAndPersist at all),
// so a regression there is a SEPARATE mutation from the manual/loopback one
// and needs its own test to catch it.
func TestLoginManager_StartDevice_WithAccount_PriorReadIsAccountQualified(t *testing.T) {
	store := newMemSecretStore()
	store.seed("ws-a", "oauth2:github:refresh_token", "RT1")
	deviceSrv := newSequencedServer(t, cannedResp{http.StatusOK, deviceAuthJSON("DC1", "USER-CODE", "https://example.com/device", "", 30, 1)})
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "github", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowDevice, DeviceAuthorizationEndpoint: deviceSrv.url(),
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "github", "", "ubs")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	status, errMsg := waitForStatus(t, lm, start.SessionID, 5*time.Second)
	if status != LoginStatusComplete {
		t.Fatalf("status = %v (%s), want complete", status, errMsg)
	}

	if v, _ := store.get("ws-a", "oauth2:github@ubs:refresh_token"); v != "RT1" {
		t.Errorf("persisted refresh_token at oauth2:github@ubs:refresh_token = %q, want RT1 (must not be suppressed by the UNQUALIFIED key already holding the same value)", v)
	}
}

// TestLoginManager_CompleteLogin_NoAccount_PersistsLiteralUnqualifiedRefreshTokenKey
// pins that an account-LESS login (the overwhelmingly common case, and the
// only case that existed before this feature) still writes to EXACTLY the
// same key it always did — asserted against a hardcoded literal string,
// deliberately NOT built via OAuthSecretKey/credentialID, so a latent bug in
// either of those two helpers could not hide a regression here the way it
// would in every other test in this file that (correctly, for their own
// purposes) goes through them.
func TestLoginManager_CompleteLogin_NoAccount_PersistsLiteralUnqualifiedRefreshTokenKey(t *testing.T) {
	store := newMemSecretStore()
	tokenSrv := newSequencedServer(t, cannedResp{http.StatusOK, tokenJSON("AT1", "RT1", 3600)})
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", ClientID: "cid", TokenEndpoint: tokenSrv.url(),
		Flow: LoginFlowManual, AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	start, err := lm.StartLogin("ws-a", "freee", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := lm.CompleteLogin(start.SessionID, "OOB-CODE", ""); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if v, _ := store.get("ws-a", "oauth2:freee:refresh_token"); v != "RT1" {
		t.Errorf("persisted refresh_token at literal key oauth2:freee:refresh_token = %q, want RT1", v)
	}
}

// TestLoginManager_StartLogin_InvalidAccountRejected pins the server-side
// half of PR-3's account validation requirement (the CLI-side half is
// route_test.go's TestValidateAccountName_MatchesParsePathAccountValidation
// plus cmd/secret_oauth_test.go's
// TestSecretOAuthLogin_InvalidAccountRejectedClientSide): StartLogin rejects
// an account name parsePath's own splitServiceAccount would also reject,
// wrapping the exact same errInvalidAccount sentinel — not a
// separately-worded validation error that happens to also fail.
func TestLoginManager_StartLogin_InvalidAccountRejected(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource([]OAuthProviderConfig{{
		Name: "freee", ClientID: "cid", TokenEndpoint: "https://example.com/token",
		Flow: LoginFlowManual, AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
	}}, store.resolver(), store.writer())
	lm := NewLoginManager(ts)

	// "" is deliberately excluded from this table: StartLogin treats an
	// empty account as "no account requested" and never calls
	// validateAccountName on it at all (StartLogin's own doc comment) — the
	// exact same carve-out ValidateAccountName's doc comment documents for
	// its own callers. Only non-empty, malformed names belong here.
	cases := []string{"not valid!", "ub s", "ub@s", strings.Repeat("a", 65)}
	for _, account := range cases {
		t.Run(account, func(t *testing.T) {
			if _, err := lm.StartLogin("ws-a", "freee", "", account); err == nil {
				t.Fatalf("StartLogin with account %q: want error, got nil", account)
			} else if !errors.Is(err, errInvalidAccount) {
				t.Errorf("StartLogin with account %q: error = %v, want it to wrap errInvalidAccount (same rule parsePath enforces)", account, err)
			}
		})
	}
}

// --- Status ---

func TestLoginManager_Status_UnknownSession(t *testing.T) {
	store := newMemSecretStore()
	ts := NewOAuth2TokenSource(nil, store.resolver(), store.writer())
	lm := NewLoginManager(ts)
	if _, _, ok := lm.Status("no-such-session"); ok {
		t.Error("Status for an unknown session should report ok=false")
	}
}
