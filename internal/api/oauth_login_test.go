package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// fakeOAuthLoginService is a minimal, entirely in-memory OAuthLoginService
// for exercising OAuthLoginHandler without a real apigateway.LoginManager or
// secret store.
type fakeOAuthLoginService struct {
	providers      map[string]string // service -> provider
	knownProviders map[string]bool   // oauth_providers.<name> that exist

	startResult                                           *OAuthLoginStart
	startErr                                              error
	gotNamespace, gotProvider, gotRedirectURI, gotAccount string

	completeErr                      error
	gotCompleteID, gotCode, gotState string

	statusStatus, statusErrMsg string
	statusOK                   bool
	gotStatusID                string
}

func (f *fakeOAuthLoginService) ProviderForService(service string) (string, bool) {
	p, ok := f.providers[service]
	return p, ok
}

func (f *fakeOAuthLoginService) KnowsProvider(name string) bool {
	return f.knownProviders[name]
}

func (f *fakeOAuthLoginService) StartLogin(namespace, provider, redirectURI, account string) (*OAuthLoginStart, error) {
	f.gotNamespace, f.gotProvider, f.gotRedirectURI, f.gotAccount = namespace, provider, redirectURI, account
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.startResult, nil
}

func (f *fakeOAuthLoginService) CompleteLogin(sessionID, code, state string) error {
	f.gotCompleteID, f.gotCode, f.gotState = sessionID, code, state
	return f.completeErr
}

func (f *fakeOAuthLoginService) LoginStatus(sessionID string) (string, string, bool) {
	f.gotStatusID = sessionID
	return f.statusStatus, f.statusErrMsg, f.statusOK
}

func newOAuthLoginTestServer(svc OAuthLoginService) *httptest.Server {
	h := &OAuthLoginHandler{Service: svc}
	r := chi.NewRouter()
	r.Mount("/api/oauth", h.Routes())
	return httptest.NewServer(r)
}

func TestOAuthLoginHandler_Start_Success(t *testing.T) {
	fake := &fakeOAuthLoginService{
		providers:   map[string]string{"freee": "freee-provider"},
		startResult: &OAuthLoginStart{SessionID: "sess-1", Flow: "manual", AuthorizeURL: "https://example.com/authorize"},
	}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	body, _ := json.Marshal(oauthLoginStartRequest{Service: "freee", Namespace: "ws-a", RedirectURI: "http://127.0.0.1:1234/callback"})
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out oauthLoginStartResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SessionID != "sess-1" || out.Flow != "manual" || out.AuthorizeURL != "https://example.com/authorize" {
		t.Errorf("response = %+v, want session sess-1/manual/authorize url", out)
	}
	if fake.gotNamespace != "ws-a" || fake.gotProvider != "freee-provider" || fake.gotRedirectURI != "http://127.0.0.1:1234/callback" {
		t.Errorf("StartLogin called with (%q, %q, %q), want (ws-a, freee-provider, http://127.0.0.1:1234/callback)",
			fake.gotNamespace, fake.gotProvider, fake.gotRedirectURI)
	}
}

func TestOAuthLoginHandler_Start_DefaultsNamespace(t *testing.T) {
	fake := &fakeOAuthLoginService{
		providers:   map[string]string{"freee": "freee-provider"},
		startResult: &OAuthLoginStart{SessionID: "sess-1", Flow: "manual"},
	}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	body, _ := json.Marshal(oauthLoginStartRequest{Service: "freee"})
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if fake.gotNamespace != "default" {
		t.Errorf("namespace = %q, want default", fake.gotNamespace)
	}
}

func TestOAuthLoginHandler_Start_MissingServiceRejected(t *testing.T) {
	fake := &fakeOAuthLoginService{providers: map[string]string{}}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	body, _ := json.Marshal(oauthLoginStartRequest{})
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOAuthLoginHandler_Start_UnknownServiceReturns404(t *testing.T) {
	fake := &fakeOAuthLoginService{providers: map[string]string{}}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	body, _ := json.Marshal(oauthLoginStartRequest{Service: "nonexistent"})
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestOAuthLoginHandler_Start_LoginManagerErrorReturns400(t *testing.T) {
	fake := &fakeOAuthLoginService{
		providers: map[string]string{"freee": "freee-provider"},
		startErr:  errors.New("no flow configured"),
	}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	body, _ := json.Marshal(oauthLoginStartRequest{Service: "freee"})
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] != "no flow configured" {
		t.Errorf("error = %q, want %q", out["error"], "no flow configured")
	}
}

func TestOAuthLoginHandler_Start_NilServiceReturns503(t *testing.T) {
	srv := newOAuthLoginTestServer(nil)
	defer srv.Close()

	body, _ := json.Marshal(oauthLoginStartRequest{Service: "freee"})
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestOAuthLoginHandler_Status_Success(t *testing.T) {
	fake := &fakeOAuthLoginService{statusStatus: "pending", statusOK: true}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/oauth/login/sess-1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out oauthLoginStatusResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Status != "pending" {
		t.Errorf("Status = %q, want pending", out.Status)
	}
	if fake.gotStatusID != "sess-1" {
		t.Errorf("LoginStatus called with %q, want sess-1", fake.gotStatusID)
	}
}

func TestOAuthLoginHandler_Status_UnknownSessionReturns404(t *testing.T) {
	fake := &fakeOAuthLoginService{statusOK: false}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/oauth/login/no-such-session")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestOAuthLoginHandler_Complete_Success(t *testing.T) {
	fake := &fakeOAuthLoginService{}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	body, _ := json.Marshal(oauthLoginCompleteRequest{Code: "AUTH-CODE", State: "state123"})
	resp, err := http.Post(srv.URL+"/api/oauth/login/sess-1/complete", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if fake.gotCompleteID != "sess-1" || fake.gotCode != "AUTH-CODE" || fake.gotState != "state123" {
		t.Errorf("CompleteLogin called with (%q, %q, %q), want (sess-1, AUTH-CODE, state123)",
			fake.gotCompleteID, fake.gotCode, fake.gotState)
	}
}

func TestOAuthLoginHandler_Complete_ErrorReturns400(t *testing.T) {
	fake := &fakeOAuthLoginService{completeErr: errors.New("state mismatch")}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	body, _ := json.Marshal(oauthLoginCompleteRequest{Code: "AUTH-CODE", State: "wrong"})
	resp, err := http.Post(srv.URL+"/api/oauth/login/sess-1/complete", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestOAuthLoginHandler_Start_ForwardsAccount pins docs/plans/
// api-gateway-credential-accounts.md D9: the request's optional "account"
// field reaches StartLogin unchanged.
func TestOAuthLoginHandler_Start_ForwardsAccount(t *testing.T) {
	fake := &fakeOAuthLoginService{
		providers:   map[string]string{"freee": "freee-provider"},
		startResult: &OAuthLoginStart{SessionID: "sess-1", Flow: "manual"},
	}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	body, _ := json.Marshal(oauthLoginStartRequest{Service: "freee", Account: "ubs"})
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if fake.gotAccount != "ubs" {
		t.Errorf("StartLogin called with account %q, want ubs", fake.gotAccount)
	}
}

// TestOAuthLoginHandler_Start_OmittedAccountFieldMeansUnqualified pins the
// HTTP API backward-compatibility half of D9 (PR-3 task description: "account
// 無しのリクエスト（フィールド欠落）が現行と完全に同じ挙動になること"): a
// request whose raw JSON body has NO "account" key at all — exactly what
// every CLI build older than this feature sends — must reach StartLogin
// with account == "", identical to an explicit empty string.
func TestOAuthLoginHandler_Start_OmittedAccountFieldMeansUnqualified(t *testing.T) {
	fake := &fakeOAuthLoginService{
		providers:   map[string]string{"freee": "freee-provider"},
		startResult: &OAuthLoginStart{SessionID: "sess-1", Flow: "manual"},
	}
	srv := newOAuthLoginTestServer(fake)
	defer srv.Close()

	body := `{"service":"freee"}`
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if fake.gotAccount != "" {
		t.Errorf("StartLogin called with account %q, want empty (the request body never had an \"account\" field)", fake.gotAccount)
	}
}

// TestOAuthLoginHandler_Start_AcceptsProviderName pins that the login
// argument may name an oauth_providers.<name> directly, not only a
// services.<name> entry.
//
// The grant this whole flow obtains is stored PER PROVIDER — StartLogin
// takes a provider and never sees a service (apigateway.LoginManager's own
// doc comment) — so a service name is nothing but an indirect way to spell
// one. Requiring it made `boid secret oauth login google` fail for a
// config with six google-backed services (gmail-api, drive-api, ...) even
// though "google" is the exact thing being authorized, and the user has to
// know that picking any ONE of the six is what unlocks all of them.
func TestOAuthLoginHandler_Start_AcceptsProviderName(t *testing.T) {
	svc := &fakeOAuthLoginService{
		providers:      map[string]string{"gmail-api": "google"},
		knownProviders: map[string]bool{"google": true},
		startResult:    &OAuthLoginStart{SessionID: "s1", Flow: "loopback", AuthorizeURL: "https://example.com/auth"},
	}
	srv := newOAuthLoginTestServer(svc)
	defer srv.Close()

	body := `{"service":"google","namespace":"default","redirect_uri":"http://127.0.0.1:1/callback"}`
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if svc.gotProvider != "google" {
		t.Errorf("StartLogin provider = %q, want %q", svc.gotProvider, "google")
	}
}

// TestOAuthLoginHandler_Start_ServiceNameWinsOverProviderName pins the
// resolution order: services.<name> is tried first. A config with both a
// service and a provider called "x" must keep resolving "x" the way it
// always did — the provider fallback is additive, never an override.
func TestOAuthLoginHandler_Start_ServiceNameWinsOverProviderName(t *testing.T) {
	svc := &fakeOAuthLoginService{
		providers:      map[string]string{"collide": "service-side-provider"},
		knownProviders: map[string]bool{"collide": true},
		startResult:    &OAuthLoginStart{SessionID: "s1", Flow: "loopback", AuthorizeURL: "https://example.com/auth"},
	}
	srv := newOAuthLoginTestServer(svc)
	defer srv.Close()

	body := `{"service":"collide","namespace":"default","redirect_uri":"http://127.0.0.1:1/callback"}`
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if svc.gotProvider != "service-side-provider" {
		t.Errorf("StartLogin provider = %q, want the service-resolved one", svc.gotProvider)
	}
}

// TestOAuthLoginHandler_Start_UnknownNameIs404 pins that a name matching
// neither a service nor a provider is still rejected — the provider
// fallback must not turn a typo into an attempted login.
func TestOAuthLoginHandler_Start_UnknownNameIs404(t *testing.T) {
	svc := &fakeOAuthLoginService{
		providers:      map[string]string{"gmail-api": "google"},
		knownProviders: map[string]bool{"google": true},
	}
	srv := newOAuthLoginTestServer(svc)
	defer srv.Close()

	body := `{"service":"google-api","namespace":"default"}`
	resp, err := http.Post(srv.URL+"/api/oauth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if svc.gotProvider != "" {
		t.Errorf("StartLogin was called with provider %q; an unknown name must not start a login", svc.gotProvider)
	}
}
