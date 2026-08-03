package apigateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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
