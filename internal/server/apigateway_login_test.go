package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/apigateway"
)

// apigateway_login_test.go pins apiGatewayLoginAdapter — the translation
// layer between apigateway.CredentialProvider/apigateway.LoginManager and
// api.OAuthLoginService (docs/plans/api-gateway.md §7, PR3) — against a
// REAL apigateway.CredentialProvider + apigateway.LoginManager, mirroring
// TestNewAPIGatewayRecorder_RecordsActionWithExpectedPayload's own "the one
// hop none of internal/apigateway's own tests can reach" rationale
// (apigateway_notify_test.go). Field-by-field, not just Name/ordering — the
// same lesson seam #23's TestAPIGatewayOAuthProviders_FieldMapping already
// encodes (a SortedByName-shaped test would not catch a dropped or
// mismapped field in this translation).

// memSecretStore is a tiny (namespace, key) -> value map standing in for
// internal/dispatcher.SecretStore — this test has no concurrent access, so
// no mutex is needed (unlike internal/apigateway's own memSecretStore,
// which its singleflight tests do need).
type loginAdapterMemSecretStore struct {
	data map[string]string
}

func newLoginAdapterMemSecretStore() *loginAdapterMemSecretStore {
	return &loginAdapterMemSecretStore{data: map[string]string{}}
}

func (m *loginAdapterMemSecretStore) key(namespace, key string) string {
	return namespace + "\x00" + key
}

func (m *loginAdapterMemSecretStore) resolver() apigateway.SecretResolver {
	return func(namespace, key string) (string, error) {
		v, ok := m.data[m.key(namespace, key)]
		if !ok {
			return "", errNotFound
		}
		return v, nil
	}
}

func (m *loginAdapterMemSecretStore) writer() apigateway.SecretWriter {
	return func(namespace, key, value string) error {
		m.data[m.key(namespace, key)] = value
		return nil
	}
}

var errNotFound = &loginAdapterSecretNotFoundError{}

type loginAdapterSecretNotFoundError struct{}

func (*loginAdapterSecretNotFoundError) Error() string { return "secret not found" }

func newTestLoginAdapter(t *testing.T, tokenEndpoint string) (*apiGatewayLoginAdapter, *loginAdapterMemSecretStore) {
	t.Helper()
	store := newLoginAdapterMemSecretStore()
	creds := apigateway.NewCredentialProvider([]apigateway.ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: apigateway.ServiceAuth{Kind: apigateway.AuthOAuth2, Provider: "freee-provider"}},
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: apigateway.ServiceAuth{Kind: apigateway.AuthBearer, SecretKey: "myapp-token"}},
	}, store.resolver())
	tokens := apigateway.NewOAuth2TokenSource([]apigateway.OAuthProviderConfig{
		{
			Name: "freee-provider", ClientID: "cid", TokenEndpoint: tokenEndpoint,
			Flow: apigateway.LoginFlowManual, AuthorizationEndpoint: "https://accounts.secure.freee.co.jp/public_api/authorize",
		},
	}, store.resolver(), store.writer())
	creds.SetOAuth2TokenSource(tokens)
	logins := apigateway.NewLoginManager(tokens)
	return &apiGatewayLoginAdapter{creds: creds, logins: logins}, store
}

func TestAPIGatewayLoginAdapter_ProviderForService(t *testing.T) {
	adapter, _ := newTestLoginAdapter(t, "https://example.com/token")

	if provider, ok := adapter.ProviderForService("freee"); !ok || provider != "freee-provider" {
		t.Errorf("ProviderForService(freee) = (%q, %v), want (freee-provider, true)", provider, ok)
	}
	if _, ok := adapter.ProviderForService("myapp"); ok {
		t.Error("ProviderForService(myapp) should be false — myapp is not an oauth2-kind service")
	}
	if _, ok := adapter.ProviderForService("nonexistent"); ok {
		t.Error("ProviderForService(nonexistent) should be false")
	}
}

// TestAPIGatewayLoginAdapter_StartLogin_FieldMapping pins that EVERY field
// of apigateway.LoginStart survives the translation into api.OAuthLoginStart
// — the same "not just Name/ordering" lesson as seam #23's
// TestAPIGatewayOAuthProviders_FieldMapping.
func TestAPIGatewayLoginAdapter_StartLogin_FieldMapping(t *testing.T) {
	adapter, _ := newTestLoginAdapter(t, "https://example.com/token")

	start, err := adapter.StartLogin("ws-a", "freee-provider", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if start.SessionID == "" {
		t.Error("SessionID is empty")
	}
	if start.Flow != "manual" {
		t.Errorf("Flow = %q, want manual", start.Flow)
	}
	if start.AuthorizeURL == "" {
		t.Error("AuthorizeURL is empty")
	}
	// device-only fields must be their zero values for a manual-flow start.
	if start.UserCode != "" || start.VerificationURI != "" || start.VerificationURIComplete != "" {
		t.Errorf("device-only fields non-empty for a manual flow: %+v", start)
	}
	if start.IntervalSeconds != 0 || start.ExpiresInSeconds != 0 {
		t.Errorf("device-only numeric fields non-zero for a manual flow: %+v", start)
	}
}

func TestAPIGatewayLoginAdapter_StartLogin_UnknownProviderError(t *testing.T) {
	adapter, _ := newTestLoginAdapter(t, "https://example.com/token")
	if _, err := adapter.StartLogin("ws-a", "nonexistent-provider", ""); err == nil {
		t.Fatal("want error for an unconfigured provider, got nil")
	}
}

func TestAPIGatewayLoginAdapter_CompleteLoginAndStatus_Success(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "AT1", "refresh_token": "RT1", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	adapter, store := newTestLoginAdapter(t, tokenSrv.URL)
	start, err := adapter.StartLogin("ws-a", "freee-provider", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	status, _, ok := adapter.LoginStatus(start.SessionID)
	if !ok || status != "pending" {
		t.Fatalf("LoginStatus before complete = (%q, ok=%v), want (pending, true)", status, ok)
	}

	if err := adapter.CompleteLogin(start.SessionID, "OOB-CODE", ""); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	status, errMsg, ok := adapter.LoginStatus(start.SessionID)
	if !ok || status != "complete" {
		t.Fatalf("LoginStatus after complete = (%q, %q, ok=%v), want (complete, \"\", true)", status, errMsg, ok)
	}
	if v, _ := store.resolver()("ws-a", apigateway.OAuthSecretKey("freee-provider", "refresh_token")); v != "RT1" {
		t.Errorf("persisted refresh_token = %q, want RT1", v)
	}
}

func TestAPIGatewayLoginAdapter_LoginStatus_UnknownSession(t *testing.T) {
	adapter, _ := newTestLoginAdapter(t, "https://example.com/token")
	if _, _, ok := adapter.LoginStatus("no-such-session"); ok {
		t.Error("LoginStatus for an unknown session should report ok=false")
	}
}

func TestAPIGatewayLoginAdapter_CompleteLogin_ErrorPropagates(t *testing.T) {
	adapter, _ := newTestLoginAdapter(t, "https://example.com/token")
	start, err := adapter.StartLogin("ws-a", "freee-provider", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := adapter.CompleteLogin(start.SessionID, "", ""); err == nil {
		t.Fatal("want error for an empty code, got nil")
	}
}
