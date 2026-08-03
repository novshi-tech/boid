package apigateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
