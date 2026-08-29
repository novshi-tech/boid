package apigateway

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedCall captures one RequestRecorder invocation for assertions.
type recordedCall struct {
	taskID, method, service, path string
	status                        int
}

type recordingRecorder struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (r *recordingRecorder) record(taskID, method, service, path string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedCall{taskID, method, service, path, status})
}

func (r *recordingRecorder) last() recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return recordedCall{}
	}
	return r.calls[len(r.calls)-1]
}

func (r *recordingRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestServer_ProxiesWithBearerInjection(t *testing.T) {
	var gotAuth, gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "task-1", false)

	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, stubResolver(map[string]string{"ws-a/myapp-token": "sekret"}))

	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users?x=1", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("upstream saw Authorization = %q, want %q", gotAuth, "Bearer sekret")
	}
	if gotPath != "/v1/users" {
		t.Errorf("upstream saw path = %q, want %q", gotPath, "/v1/users")
	}
	if gotQuery != "x=1" {
		t.Errorf("upstream saw query = %q, want %q", gotQuery, "x=1")
	}

	if got := rec.count(); got != 1 {
		t.Errorf("recorder was called %d times, want exactly 1 (no double-recording a single request)", got)
	}
	call := rec.last()
	if call.taskID != "task-1" || call.method != "GET" || call.service != "myapp" || call.path != "/v1/users" || call.status != http.StatusOK {
		t.Errorf("recorder call = %+v, want {task-1 GET myapp /v1/users 200}", call)
	}
}

// TestServer_ProxiesWithOAuth2Injection is TestServer_ProxiesWithBearerInjection's
// oauth2-kind counterpart: a real Registry + Server + CredentialProvider,
// wired to a real *OAuth2TokenSource (SetOAuth2TokenSource) fronting a fake
// token endpoint, seeded with only a refresh_token — the exact PR2 dogfood
// starting state (`boid secret set` a refresh_token, nothing else). This
// closes the remaining hop oauth2_test.go/credentials_test.go's own oauth2
// tests don't individually cover: ServeHTTP -> CredentialProvider.Resolve
// (fail-fast pre-check) -> CredentialProvider.Inject (Rewrite) both calling
// through to the SAME OAuth2TokenSource, ending in the upstream actually
// seeing a Bearer access token it never held any static secret for.
func TestServer_ProxiesWithOAuth2Injection(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"live-access-token","expires_in":3600}`))
	}))
	defer tokenEndpoint.Close()

	store := newMemSecretStore()
	store.seed("ws-a", OAuthSecretKey("freee", oauthFieldRefreshToken), "refresh-from-boid-secret-set")

	registry := NewRegistry()
	token := registry.Register([]string{"freee"}, "ws-a", "task-1", false)

	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthOAuth2, Provider: "freee"}},
	}, store.resolver())
	creds.SetOAuth2TokenSource(NewOAuth2TokenSource(
		[]OAuthProviderConfig{{Name: "freee", TokenEndpoint: tokenEndpoint.URL, ClientID: "cid"}},
		store.resolver(), store.writer(),
	))

	srv := NewServer(registry, creds, nil, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/freee/api/1/companies", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer live-access-token" {
		t.Errorf("upstream saw Authorization = %q, want %q", gotAuth, "Bearer live-access-token")
	}
}

// TestServer_RoutesCredentialsByTokenNamespace mirrors
// internal/gitgateway's TestServeHTTP_RoutesCredentialsByTokenNamespace
// (wiring-seams.md #11's own guard for the sibling gateway): a single
// Server, with one shared ServiceConfig/SecretResolver, must route two
// different job tokens — registered under two different Registry
// namespaces — to two different upstream secrets. This is the shape a real
// daemon reaches once two workspaces each set their own
// `boid secret set --namespace <ws> myapp-token <value>`: same service
// config, different secret per workspace, selected purely by which job
// token made the request. TestCredentialProvider_MultiNamespaceIsolation
// (credentials_test.go) already proves CredentialProvider itself resolves
// per-namespace correctly in isolation; this test closes the remaining hop
// — Server.ServeHTTP's post-Authorize Lookup recovering Entry.Namespace and
// threading it into Resolve/Inject — through a real Registry + Server.
func TestServer_RoutesCredentialsByTokenNamespace(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	secretsByNamespace := map[string]string{
		"ws-a/myapp-token": "secret-for-ws-a",
		"ws-b/myapp-token": "secret-for-ws-b",
	}
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, stubResolver(secretsByNamespace))

	registry := NewRegistry()
	tokenA := registry.Register([]string{"myapp"}, "ws-a", "task-a", false)
	tokenB := registry.Register([]string{"myapp"}, "ws-b", "task-b", false)

	srv := NewServer(registry, creds, nil, nil)

	for token, wantSecret := range map[string]string{tokenA: "secret-for-ws-a", tokenB: "secret-for-ws-b"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users", nil)
		srv.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("token %s: status = %d, want 200 (body %q)", token, w.Code, w.Body.String())
		}
		wantAuth := "Bearer " + wantSecret
		if gotAuth != wantAuth {
			t.Errorf("token %s: upstream saw Authorization = %q, want %q", token, gotAuth, wantAuth)
		}
	}
}

func TestServer_StripsInboundAuthHeaders(t *testing.T) {
	var gotAuth, gotCookie, gotProxyAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotProxyAuth = r.Header.Get("Proxy-Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthHeader, Header: "X-Api-Key", SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "v"}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users", nil)
	req.Header.Set("Authorization", "Bearer sandbox-smuggled-token")
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Proxy-Authorization", "Basic xyz")
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if gotAuth != "" {
		t.Errorf("upstream saw inbound Authorization forwarded: %q, want empty (must be stripped)", gotAuth)
	}
	if gotCookie != "" {
		t.Errorf("upstream saw inbound Cookie forwarded: %q, want empty (must be stripped)", gotCookie)
	}
	if gotProxyAuth != "" {
		t.Errorf("upstream saw inbound Proxy-Authorization forwarded: %q, want empty (must be stripped)", gotProxyAuth)
	}
}

func TestServer_UnauthorizedToken(t *testing.T) {
	registry := NewRegistry()
	creds := NewCredentialProvider(nil, stubResolver(nil))
	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/bogus-token/myapp/v1/users", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestServer_ForbiddenService(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"other-app"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(nil))
	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if got := rec.last(); got.status != http.StatusForbidden || got.taskID != "task-1" {
		t.Errorf("recorder call = %+v, want status 403, taskID task-1", got)
	}
}

func TestServer_ReadOnlyRejectsWriteMethods(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "task-1", true /* readOnly */)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "v"}))
	srv := NewServer(registry, creds, nil, nil)

	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/"+token+"/myapp/v1/users", nil)
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s on a read-only token: status = %d, want 403", method, w.Code)
		}
	}

	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/"+token+"/myapp/v1/users", nil)
		srv.ServeHTTP(w, req)
		if w.Code != http.StatusOK && !(method == "HEAD" && w.Code == http.StatusOK) {
			t.Errorf("%s on a read-only token: status = %d, want 200", method, w.Code)
		}
	}
}

// TestServer_ReadOnlyAllowsWriteForAllowlistedService covers the
// docs/plans/api-gateway.md §論点 (2026-08-14 追加決定) escape hatch: an
// operator can mark a specific service AllowReadOnlyWrite: true in
// config.yaml (ServiceConfig, daemon-side — never project.yaml/task_behaviors,
// since only the daemon operator, not the repo, should be able to grant this)
// so a read-only job token may still use non-GET/HEAD methods against THAT
// service. Every other configured service must keep enforcing the ordinary
// read-only gate — this is a per-service opt-in, not a token-wide bypass.
func TestServer_ReadOnlyAllowsWriteForAllowlistedService(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"slack", "myapp"}, "ws-a", "task-1", true /* readOnly */)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "slack", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}, AllowReadOnlyWrite: true},
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "v"}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/"+token+"/slack/v1/chat.postMessage", nil)
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Errorf("POST to allowlisted service on a read-only token: status = %d, want 201", w.Code)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/"+token+"/myapp/v1/users", nil)
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST to non-allowlisted service on a read-only token: status = %d, want 403", w.Code)
	}
}

// TestServer_AccountQualifiedRequest_ReachesUpstreamWithBaseServiceAuthorization
// pins grading item #3 (docs/plans/api-gateway-credential-accounts.md D4):
// a request to "freee@ubs" must be authorized, BaseURL-resolved, and
// credential-resolved using the BASE service name "freee" — the workspace's
// enabled-service set only ever lists "freee", never "freee@ubs", and the
// request must still reach the upstream (not 403) once the account-
// qualified secret exists.
func TestServer_AccountQualifiedRequest_ReachesUpstreamWithBaseServiceAuthorization(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	// Only the BASE name "freee" is in the workspace's enabled-service set —
	// "freee@ubs" is never listed anywhere (D4: no per-account authorization
	// axis in PR-1).
	token := registry.Register([]string{"freee"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}},
	}, stubResolver(map[string]string{"ws-a/freee-token@ubs": "ubs-secret"}))
	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/freee@ubs/api/1/deals", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer ubs-secret" {
		t.Errorf("upstream saw Authorization = %q, want %q", gotAuth, "Bearer ubs-secret")
	}
	// D8: the recorder receives "name@account", not the base name alone.
	if call := rec.last(); call.service != "freee@ubs" {
		t.Errorf("recorder call.service = %q, want %q", call.service, "freee@ubs")
	}
}

// TestServer_AccountQualifiedRequest_ReadOnlyWriteUsesBaseServiceConfig is
// grading item #3's readonly-write half: a readonly job token's POST to
// "freee@ubs" must be allowed exactly when the BASE service "freee" opted
// into AllowReadOnlyWrite — the account qualifier has no config of its own
// to consult (D4).
func TestServer_AccountQualifiedRequest_ReadOnlyWriteUsesBaseServiceConfig(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"freee"}, "ws-a", "task-1", true /* readOnly */)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}, AllowReadOnlyWrite: true},
	}, stubResolver(map[string]string{"ws-a/freee-token@ubs": "ubs-secret"}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/"+token+"/freee@ubs/api/1/deals", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("POST to freee@ubs on a read-only token, base service AllowReadOnlyWrite=true: status = %d, want 201 (body %q)", w.Code, w.Body.String())
	}
}

// TestServer_AccountQualifiedRequest_NoFallbackReturnsBadGateway pins
// grading item #2 at the full-request level (D3): "freee-token@ubs" does
// not exist in the secret store, but the unqualified "freee-token" does —
// the request must fail with 502, never silently proceed using the
// unqualified (wrong-account) credential.
func TestServer_AccountQualifiedRequest_NoFallbackReturnsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // must never be reached
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"freee"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}},
	}, stubResolver(map[string]string{"ws-a/freee-token": "unqualified-secret"})) // no "freee-token@ubs" entry
	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/freee@ubs/api/1/deals", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", w.Code, w.Body.String())
	}
	if call := rec.last(); call.status != http.StatusBadGateway || call.service != "freee@ubs" {
		t.Errorf("recorder call = %+v, want status 502, service freee@ubs", call)
	}
}

// TestServer_RequireAccount_RejectsAccountlessRequest pins grading item #9's
// first half (docs/plans/api-gateway-credential-accounts.md D5): a service
// configured with require_account: true rejects an account-less request
// with 400, and the recorder sees that 400 the same way it sees an existing
// 403/502 rejection (recSvc == the base name, since there is no account to
// fold in).
func TestServer_RequireAccount_RejectsAccountlessRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // must never be reached
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"freee"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}, RequireAccount: true},
	}, stubResolver(map[string]string{"ws-a/freee-token": "unqualified-secret"}))
	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/freee/api/1/deals", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", w.Code, w.Body.String())
	}
	if call := rec.last(); call.status != http.StatusBadRequest || call.service != "freee" {
		t.Errorf("recorder call = %+v, want status 400, service freee", call)
	}
}

// TestServer_RequireAccount_AllowsAccountQualifiedRequest pins grading item
// #9's companion check: an account-QUALIFIED request to the same
// require_account: true service must NOT be rejected — the gate only fires
// on a missing account, never on the presence of one.
func TestServer_RequireAccount_AllowsAccountQualifiedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"freee"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}, RequireAccount: true},
	}, stubResolver(map[string]string{"ws-a/freee-token@ubs": "ubs-secret"}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/freee@ubs/api/1/deals", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
}

// TestServer_RequireAccount_DefaultFalseServiceUnaffected pins grading item
// #9's second half: a service that does NOT set require_account (the
// default, false) must keep accepting an account-less request exactly as it
// did before this field existed — registered side-by-side with a
// require_account: true service so a regression in either direction (the
// flag leaking across services, or the gate firing unconditionally) would
// show up here.
func TestServer_RequireAccount_DefaultFalseServiceUnaffected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"freee", "myapp"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}, RequireAccount: true},
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, stubResolver(map[string]string{
		"ws-a/freee-token@ubs": "ubs-secret",
		"ws-a/myapp-token":     "myapp-secret",
	}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("account-less request to a require_account: false (default) service: status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
}

// TestServer_RequireAccount_UnauthorizedServiceReturns403NotBadRequest pins
// the check-ordering decision in Server.ServeHTTP (docs/plans/
// api-gateway-credential-accounts.md D5 implementation note): the
// require_account gate is checked AFTER authorization, not before, so a job
// token that isn't even permitted to use a require_account: true service
// gets the ordinary 403 "forbidden" — it must never learn, via a 400
// instead, that the service it can't reach happens to require an account.
func TestServer_RequireAccount_UnauthorizedServiceReturns403NotBadRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // must never be reached
	}))
	defer upstream.Close()

	registry := NewRegistry()
	// "freee" is NOT in this token's allowed service set.
	token := registry.Register([]string{"myapp"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}, RequireAccount: true},
	}, stubResolver(nil))
	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/freee/api/1/deals", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", w.Code, w.Body.String())
	}
	if call := rec.last(); call.status != http.StatusForbidden {
		t.Errorf("recorder call = %+v, want status 403", call)
	}
}

// TestServer_RequireAccount_PrecedesReadOnlyWriteGate pins the OTHER half of
// the check-ordering decision in Server.ServeHTTP: require_account is
// checked BEFORE the readonly-write gate, not after. A readonly job token
// issuing an unsafe method (POST) with no account, against a service that
// is BOTH require_account: true AND does NOT opt into AllowReadOnlyWrite,
// gets 400 ("supply an account") rather than 403 ("readonly token can't
// write here") — the missing-account defect applies to the request
// regardless of method, so it is reported first; the narrower
// method-specific gate never even runs for a request this malformed.
func TestServer_RequireAccount_PrecedesReadOnlyWriteGate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // must never be reached
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"freee"}, "ws-a", "task-1", true /* readOnly */)
	creds := NewCredentialProvider([]ServiceConfig{
		// RequireAccount: true, AllowReadOnlyWrite left false (default) —
		// both gates would independently reject this request; the test
		// asserts WHICH one reports first.
		{Name: "freee", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}, RequireAccount: true},
	}, stubResolver(nil))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/"+token+"/freee/api/1/deals", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q) — require_account should be checked before the readonly-write gate", w.Code, w.Body.String())
	}
}

// TestServer_InvalidAccountNameReturnsBadRequest pins D11 at the
// Server.ServeHTTP level: a well-formed /api/<token>/<service>/<tail>
// path whose account name fails validation gets 400, distinct from the
// 404 an unrecognized path shape gets (TestServer_NotFoundForUnmatchedPath)
// — see errInvalidAccount's own doc comment for why the two must not be
// conflated.
func TestServer_InvalidAccountNameReturnsBadRequest(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"freee"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "freee", BaseURL: "https://api.freee.co.jp", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "freee-token"}},
	}, stubResolver(nil))
	srv := NewServer(registry, creds, nil, nil)

	cases := []string{
		"/api/" + token + "/freee@/v1",        // empty account
		"/api/" + token + "/freee@ub.s/v1",    // "." not allowed
		"/api/" + token + "/freee@ubs@nvt/v1", // two "@"
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", p, nil)
			srv.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %q)", w.Code, w.Body.String())
			}
		})
	}
}

func TestServer_UnconfiguredServiceReturnsBadGateway(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"ghost"}, "ws-a", "", false)
	creds := NewCredentialProvider(nil, stubResolver(nil)) // registry allows "ghost" but nothing configures it
	rec := &recordingRecorder{}
	srv := NewServer(registry, creds, nil, rec.record)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/ghost/v1/users", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestServer_TransportErrorDoesNotLeakUpstreamHostToSandbox pins a codex
// review finding (round 6): docs/plans/api-gateway.md §1's design principle
// is "upstream の実 URL は sandbox からは見えない (見せる必要もない)" — but a
// genuine transport failure (DNS lookup, connection refused, TLS handshake)
// from Go's net/http typically embeds the target host:port verbatim in its
// error string, and ErrorHandler used to echo that string straight into the
// sandbox-facing response body. This closes an unreachable upstream
// connection (connection refused, deterministic and network-independent —
// no DNS lookup involved) and asserts the target host:port never appears in
// what the sandbox sees, even though it is still logged server-side.
func TestServer_TransportErrorDoesNotLeakUpstreamHostToSandbox(t *testing.T) {
	// Bind a listener, capture its address, then close it immediately: the
	// OS will refuse connections to that exact address deterministically
	// (no DNS involved, no dependency on external network reachability).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := ln.Addr().String()
	ln.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "http://" + closedAddr, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "v"}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, closedAddr) {
		t.Errorf("response body leaks the upstream host:port %q: %q", closedAddr, body)
	}
	host, _, _ := net.SplitHostPort(closedAddr)
	if host != "" && strings.Contains(body, host) {
		t.Errorf("response body leaks the upstream host %q: %q", host, body)
	}
}

func TestServer_NoResolverConfiguredReturnsServiceUnavailable(t *testing.T) {
	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: "https://myapp.example.com", Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, nil) // no resolver at all
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestServer_CredentialResolutionFailureFailsFastWithoutContactingUpstream(t *testing.T) {
	contacted := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "missing-key"}},
	}, stubResolver(nil)) // secret store has nothing for "missing-key"

	var notifiedService string
	var notifiedErr error
	notifier := NotifierFuncs{CredentialError: func(service string, err error) {
		notifiedService = service
		notifiedErr = err
	}}
	srv := NewServer(registry, creds, notifier, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if contacted {
		t.Error("upstream was contacted despite a fail-fast credential resolution failure")
	}
	if notifiedService != "myapp" || notifiedErr == nil {
		t.Errorf("NotifyCredentialError not called as expected: service=%q err=%v", notifiedService, notifiedErr)
	}
}

func TestServer_UpstreamUnauthorizedNotifies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "v"}))

	var notifiedService string
	notifier := NotifierFuncs{UpstreamAuthFailure: func(service string) { notifiedService = service }}
	srv := NewServer(registry, creds, notifier, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (passed through from upstream)", w.Code)
	}
	if notifiedService != "myapp" {
		t.Errorf("NotifyUpstreamAuthFailure not called with %q, got %q", "myapp", notifiedService)
	}
}

func TestServer_NotFoundForUnmatchedPath(t *testing.T) {
	srv := NewServer(NewRegistry(), NewCredentialProvider(nil, nil), nil, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestServer_TraversalReturnsNotFound is TestServer_NotFoundForUnmatchedPath's
// counterpart for a WELL-FORMED-looking path whose tail is a traversal
// attempt: unlike TestServer_InvalidAccountNameReturnsBadRequest (a
// malformed account segment, which IS a 400 per D11), a traversal attempt
// must stay a 404 — Server.ServeHTTP only maps errors.Is(err,
// errInvalidAccount) to 400, every other parsePath failure (including
// checkForTraversal's) falls through to http.NotFound. Before this test, no
// test exercised a traversal path at the Server.ServeHTTP level at all — only
// parsePath directly (TestParsePath_TraversalCannotEscapeServiceRoot) — so a
// review mutation that made checkForTraversal wrap errInvalidAccount would
// have turned this into a 400 with no test noticing.
func TestServer_TraversalReturnsNotFound(t *testing.T) {
	srv := NewServer(NewRegistry(), NewCredentialProvider(nil, nil), nil, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/tok123/myapp/../../etc/passwd", nil)
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", w.Code, w.Body.String())
	}
}

func TestServer_QueryAuthInjectionPreservesExistingParams(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"legacy"}, "ws-a", "", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "legacy", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthQuery, Query: "api_key", SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "qsecret"}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/legacy/v1/status?foo=bar", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if gotQuery != "api_key=qsecret&foo=bar" {
		t.Errorf("upstream saw query = %q, want %q", gotQuery, "api_key=qsecret&foo=bar")
	}
}

// flakyOnceResolver succeeds on every call except the Nth (1-indexed),
// which fails — used to simulate the narrow race between ServeHTTP's own
// fail-fast Resolve pre-check (the 1st call) and Rewrite's Inject
// (the 2nd call re-resolving the same secret), e.g. a concurrent
// `boid secret delete` landing in between.
func flakyOnceResolver(value string, failOnCall int) SecretResolver {
	var n int
	var mu sync.Mutex
	return func(namespace, key string) (string, error) {
		mu.Lock()
		n++
		callNum := n
		mu.Unlock()
		if callNum == failOnCall {
			return "", errFlakyResolver
		}
		return value, nil
	}
}

var errFlakyResolver = fmt.Errorf("apigateway test: simulated secret-store failure")

// TestServer_RewriteInjectionRaceFailsFastWithoutContactingUpstream pins the
// fix for a race codex review flagged: if credential resolution succeeds at
// ServeHTTP's own pre-check but then FAILS on Rewrite's own re-resolve (the
// narrow window between the two — e.g. a concurrent `boid secret delete`),
// the request must be aborted (502) rather than forwarded to the upstream
// without the Authorization header. Unlike internal/gitgateway (whose
// upstream forge always 401s an unauthenticated smart-HTTP request safely),
// an arbitrary REST API's behavior on missing auth is unknowable, so
// "forward anyway" is not an acceptable fallback here — see
// failFastTransport's own doc comment.
func TestServer_RewriteInjectionRaceFailsFastWithoutContactingUpstream(t *testing.T) {
	contacted := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "", false)
	// Call 1 (ServeHTTP's Resolve pre-check) succeeds; call 2 (Rewrite's
	// Inject) fails — simulating the secret vanishing in between.
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "myapp-token"}},
	}, flakyOnceResolver("sekret", 2))

	var notifiedService string
	notifier := NotifierFuncs{CredentialError: func(service string, err error) { notifiedService = service }}
	srv := NewServer(registry, creds, notifier, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/v1/users", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (fail-fast on Rewrite-time injection failure)", w.Code)
	}
	if contacted {
		t.Error("upstream was contacted despite a Rewrite-time credential injection failure — must abort before any network I/O")
	}
	if notifiedService != "myapp" {
		t.Errorf("NotifyCredentialError not called for the Rewrite-time failure: got service=%q", notifiedService)
	}
}

// TestServer_PercentEncodedSlashWithinSegmentReachesUpstreamUnchanged pins
// the fix for a codex-flagged bug: a "%2F"-encoded slash inside a single
// request-tail segment (common for REST APIs whose resource keys contain
// "/", e.g. object storage / registry APIs) must reach the upstream
// unchanged, not be silently decoded into an extra path separator.
func TestServer_PercentEncodedSlashWithinSegmentReachesUpstreamUnchanged(t *testing.T) {
	var gotRawPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "v"}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/objects/a%2Fb", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if gotRawPath != "/objects/a%2Fb" {
		t.Errorf("upstream saw escaped path = %q, want %q (the %%2F must survive as one segment)", gotRawPath, "/objects/a%2Fb")
	}
}

// TestServer_TrailingSlashReachesUpstreamUnchanged pins the fix for the
// other codex-flagged path-fidelity bug: a request tail's trailing slash
// must reach the upstream unchanged (some REST APIs distinguish "/hooks/"
// from "/hooks" as different routes).
func TestServer_TrailingSlashReachesUpstreamUnchanged(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "v"}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp/hooks/", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if gotPath != "/hooks/" {
		t.Errorf("upstream saw path = %q, want %q (trailing slash must be preserved)", gotPath, "/hooks/")
	}
}

// TestServer_DoubleSlashTailWithPathlessBaseURLIsNotSwallowed pins the fix
// for a codex-flagged bug: a service configured with a pathless base_url
// (BaseURL == "https://host", no path suffix) combined with a request tail
// that itself begins with "//" (e.g. ".../myapp//tenant/resource" — nothing
// in parsePath's traversal guard rejects a doubled leading slash, only "."/
// ".." segments) used to be string-concatenated as basePath+rt.path (""+
// "//tenant/resource" = "//tenant/resource") and handed to url.Parse
// directly. RFC 3986 treats a string BEGINNING "//" as a network-path
// reference: everything up to the next "/" becomes the parsed URL's
// AUTHORITY, not path content — so url.Parse("//tenant/resource") produced
// Host="tenant", Path="/resource", and since only .Path/.RawPath are read
// back out (Scheme/Host on the real outbound request come from info.baseURL
// directly), "tenant" was silently swallowed entirely rather than reaching
// the upstream as part of the path. Fixed by prefixing the string handed to
// url.Parse with baseURL's own "<scheme>://<host>" first, so a leading "//"
// in rt.path is never the first two characters url.Parse sees.
func TestServer_DoubleSlashTailWithPathlessBaseURLIsNotSwallowed(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "", false)
	creds := NewCredentialProvider([]ServiceConfig{
		// upstream.URL is pathless (e.g. "http://127.0.0.1:PORT", no
		// trailing path segment) — the exact shape that triggered the bug.
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "v"}))
	srv := NewServer(registry, creds, nil, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/"+token+"/myapp//tenant/resource", nil)
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if gotPath != "//tenant/resource" {
		t.Errorf("upstream saw path = %q, want %q (the \"tenant\" segment must not be swallowed as a parsed authority)", gotPath, "//tenant/resource")
	}
}

// TestIsSafeMethod_CaseSensitive pins the fix for a codex-flagged bug: HTTP
// method tokens are case-sensitive (RFC 7230 §3.1.1), so a lowercase "get"
// is a DIFFERENT, non-standard method — it must not be treated as
// equivalent to "GET" for the read-only gate.
func TestIsSafeMethod_CaseSensitive(t *testing.T) {
	cases := []struct {
		method string
		want   bool
	}{
		{http.MethodGet, true},
		{http.MethodHead, true},
		{"get", false},
		{"head", false},
		{"Get", false},
		{http.MethodPost, false},
	}
	for _, c := range cases {
		if got := isSafeMethod(c.method); got != c.want {
			t.Errorf("isSafeMethod(%q) = %v, want %v", c.method, got, c.want)
		}
	}
}

// TestServer_SSEResponseStreamsIncrementally proves docs/plans/api-gateway.md
// §5's "SSE 対応" claim as a USER-VISIBLE property: an upstream Server-Sent
// Events response streams through the gateway incrementally rather than
// being buffered until the connection closes.
//
// Note on what this test does and does not isolate: Go's own
// httputil.ReverseProxy already special-cases a "text/event-stream"
// Content-Type (and any response with ContentLength == -1, i.e. chunked/
// unknown-length) to flush immediately REGARDLESS of the configured
// FlushInterval (see reverseproxy.go's flushInterval method) — real SSE
// traffic always takes this shape (no upstream can know an SSE stream's
// final length in advance), so this test's real-world value is confirming
// nothing in this package's own code (an accidental io.ReadAll on the
// response body, a misconfigured Transport, ...) defeats that stdlib
// behavior — not that server.go's own `FlushInterval: -1` field is what
// causes it. TestServer_FlushIntervalNegative_FlushesKnownLengthResponse
// below isolates that field specifically, using a response shape that
// bypasses both of ReverseProxy's own auto-override conditions.
//
// Unlike every other test in this file, this one cannot use
// httptest.NewRecorder() + a direct ServeHTTP call: ResponseRecorder has no
// real network connection for a client to observe partial data over while
// the handler is still running — ServeHTTP simply runs to completion
// synchronously and the test only ever sees the final, fully-buffered
// result, which would pass even if streaming were silently broken. A REAL
// httptest.NewServer(srv) — mirroring internal/gitgateway's own streaming
// proof, TestServeHTTP_StreamsRequestBodyWithoutBuffering — is required to
// observe the ordering property this test is actually about.
//
// The proof: the upstream writes and flushes a first chunk, then blocks
// (holding the connection open) before writing a second chunk and closing.
// If the gateway buffered the response instead of streaming it, the client
// would see nothing until the upstream finally closes — which never happens
// while the upstream is paused — so the test would time out instead of
// observing the first chunk arrive on its own.
func TestServer_SSEResponseStreamsIncrementally(t *testing.T) {
	proceed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter does not implement http.Flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: first\n\n")
		flusher.Flush()
		<-proceed
		fmt.Fprint(w, "data: second\n\n")
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "sekret"}))

	gwSrv := httptest.NewServer(NewServer(registry, creds, nil, nil))
	defer gwSrv.Close()

	resp, err := http.Get(gwSrv.URL + "/api/" + token + "/myapp/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Read exactly the first chunk's bytes off the still-open connection.
	firstChunk := make([]byte, len("data: first\n\n"))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, firstChunk)
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read first chunk: %v", err)
		}
		if string(firstChunk) != "data: first\n\n" {
			t.Fatalf("first chunk = %q, want %q", firstChunk, "data: first\n\n")
		}
		// Observed the first chunk while the upstream is still paused before
		// writing the second one — proves the gateway flushed it through
		// rather than buffering until the response completed.
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first SSE chunk; response appears to be buffered rather than streamed")
	}

	close(proceed)

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest of body: %v", err)
	}
	if string(rest) != "data: second\n\n" {
		t.Fatalf("rest of body = %q, want %q", rest, "data: second\n\n")
	}
}

// TestServer_FlushIntervalNegative_FlushesKnownLengthResponse isolates
// server.go's own `FlushInterval: -1` config value, the specific gap
// TestServer_SSEResponseStreamsIncrementally's doc comment flags: that test's
// upstream response takes a shape (text/event-stream, unknown length) that
// httputil.ReverseProxy special-cases to flush immediately ON ITS OWN,
// regardless of FlushInterval, so it would pass even if this package never
// set that field at all.
//
// This test avoids BOTH of ReverseProxy's auto-override conditions
// (reverseproxy.go's flushInterval method): the upstream response declares
// an ordinary Content-Type (not "text/event-stream") AND a real, known
// Content-Length set before any bytes are written (so the proxy's client
// Transport reports resp.ContentLength as that real value, not -1). With
// both auto-overrides bypassed, the only way the gateway flushes the first
// chunk to the client before the upstream releases the second one is if
// server.go's own configured FlushInterval is honored.
func TestServer_FlushIntervalNegative_FlushesKnownLengthResponse(t *testing.T) {
	const chunk1, chunk2 = "first-chunk-", "second-chunk"
	proceed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream ResponseWriter does not implement http.Flusher")
		}
		w.Header().Set("Content-Type", "text/plain") // deliberately NOT text/event-stream
		w.Header().Set("Content-Length", strconv.Itoa(len(chunk1)+len(chunk2)))
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, chunk1)
		flusher.Flush()
		<-proceed
		io.WriteString(w, chunk2)
	}))
	defer upstream.Close()

	registry := NewRegistry()
	token := registry.Register([]string{"myapp"}, "ws-a", "task-1", false)
	creds := NewCredentialProvider([]ServiceConfig{
		{Name: "myapp", BaseURL: upstream.URL, Auth: ServiceAuth{Kind: AuthBearer, SecretKey: "k"}},
	}, stubResolver(map[string]string{"ws-a/k": "sekret"}))

	gwSrv := httptest.NewServer(NewServer(registry, creds, nil, nil))
	defer gwSrv.Close()

	resp, err := http.Get(gwSrv.URL + "/api/" + token + "/myapp/plain")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Sanity check that this response really does bypass both of
	// ReverseProxy's own auto-flush triggers — a positive ContentLength
	// (not -1) and a non-SSE Content-Type — so a pass here can only be
	// attributed to server.go's own FlushInterval.
	if resp.ContentLength != int64(len(chunk1)+len(chunk2)) {
		t.Fatalf("resp.ContentLength = %d, want %d (test setup invalid: proxy did not see a known length)", resp.ContentLength, len(chunk1)+len(chunk2))
	}
	if ct := resp.Header.Get("Content-Type"); ct == "text/event-stream" {
		t.Fatal("test setup invalid: Content-Type is text/event-stream, which would trigger ReverseProxy's own unconditional auto-flush")
	}

	firstChunk := make([]byte, len(chunk1))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, firstChunk)
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read first chunk: %v", err)
		}
		if string(firstChunk) != chunk1 {
			t.Fatalf("first chunk = %q, want %q", firstChunk, chunk1)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first chunk; a known-Content-Length, non-SSE response is not being flushed immediately — FlushInterval may have been dropped")
	}

	close(proceed)

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest of body: %v", err)
	}
	if string(rest) != chunk2 {
		t.Fatalf("rest of body = %q, want %q", rest, chunk2)
	}
}

// TestServer_UpstreamTransportHasConnectionLivenessTimeouts pins the
// 2026-08-11 production incident (internal/gwtransport's own package doc
// comment): this Server's outbound Transport was an http.Transport literal
// with every connection-liveness field left at zero, so a pooled HTTP/2
// connection to an upstream that stopped answering — without closing its
// TCP connection — was reused forever and every request to that one
// service hung until its caller gave up, while other services (other
// upstream hosts, other pools) stayed healthy.
func TestServer_UpstreamTransportHasConnectionLivenessTimeouts(t *testing.T) {
	s := NewServer(NewRegistry(), nil, nil, nil)

	ff, ok := s.proxy.Transport.(failFastTransport)
	if !ok {
		t.Fatalf("proxy.Transport is %T, want failFastTransport", s.proxy.Transport)
	}
	tr, ok := ff.base.(*http.Transport)
	if !ok {
		t.Fatalf("failFastTransport.base is %T, want *http.Transport", ff.base)
	}

	if tr.IdleConnTimeout == 0 {
		t.Error("IdleConnTimeout is 0: a pooled connection whose peer silently vanished never expires (use gwtransport.New)")
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout == 0 {
		t.Error("no HTTP/2 keep-alive ping configured: HTTP/2 is negotiated implicitly, and a zero SendPingTimeout means net/http never health-checks a half-dead connection (use gwtransport.New)")
	}
	if tr.ExpectContinueTimeout == 0 {
		t.Error("ExpectContinueTimeout is 0: the client's Expect: 100-continue would be silently ignored")
	}
}
