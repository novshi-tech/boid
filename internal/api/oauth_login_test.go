package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// fakeOAuthLoginService is a minimal, entirely in-memory OAuthLoginService
// for exercising OAuthLoginHandler without a real apigateway.LoginManager or
// secret store.
type fakeOAuthLoginService struct {
	providers map[string]string // service -> provider

	startResult                               *OAuthLoginStart
	startErr                                  error
	gotNamespace, gotProvider, gotRedirectURI string

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

func (f *fakeOAuthLoginService) StartLogin(namespace, provider, redirectURI string) (*OAuthLoginStart, error) {
	f.gotNamespace, f.gotProvider, f.gotRedirectURI = namespace, provider, redirectURI
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
