package gitgateway

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- test helpers ---

// newCapturingUpstream starts an httptest.Server standing in for an upstream
// forge; handler observes/responds to the proxied request.
func newCapturingUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// upstreamHost returns the host:port of an httptest.Server, i.e. what the
// gateway route's <host> path segment (and HostForgeConfig.Host) must equal.
func upstreamHost(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// newTestGateway wires a Registry + a CredentialProvider pointed (with
// Scheme "http") at upstream, and returns the gateway's own httptest.Server
// plus the job token and repo key already registered for repo with perm.
// The token is registered under the "default" namespace; tests that care
// about namespace routing build their own Registry/CredentialProvider
// directly instead (see TestServeHTTP_RoutesCredentialsByTokenNamespace).
func newTestGateway(t *testing.T, upstream *httptest.Server, owner, repo string, perm Permission, secretValue string, notifier UpstreamAuthFailureNotifier) (gwURL, token string, host string, key RepoKey) {
	t.Helper()
	host = upstreamHost(t, upstream)
	key = NewRepoKey(host, owner, repo)

	reg := NewRegistry()
	token = reg.Register(map[RepoKey]Permission{key: perm}, "default")

	creds := NewCredentialProvider([]HostForgeConfig{
		{Host: host, Forge: ForgeGitHub, SecretKey: "gh-pat", Scheme: "http"},
	}, func(namespace, key string) (string, error) { return secretValue, nil })

	gw := NewServer(reg, creds, notifier)
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)
	return gwSrv.URL, token, host, key
}

func routePath(token, host, owner, repo, endpoint string) string {
	return fmt.Sprintf("/j/%s/%s/%s/%s/%s", token, host, owner, repo, endpoint)
}

// --- routing / authorization tests ---

func TestServeHTTP_AllowedFetchInjectsCredentialsAndRoutes(t *testing.T) {
	var gotAuth, gotPath, gotHost string
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotHost = r.Host
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("advertisement-body"))
	})
	gwURL, token, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetch, "test-pat", nil)

	resp, err := http.Get(gwURL + routePath(token, host, "owner", "repo.git", "info/refs") + "?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if string(body) != "advertisement-body" {
		t.Fatalf("body = %q, want advertisement-body", body)
	}
	if gotPath != "/owner/repo.git/info/refs" {
		t.Fatalf("upstream saw path %q, want /owner/repo.git/info/refs", gotPath)
	}
	if gotHost != host {
		t.Fatalf("upstream saw Host header %q, want %q", gotHost, host)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:test-pat"))
	if gotAuth != wantAuth {
		t.Fatalf("Authorization = %q, want %q", gotAuth, wantAuth)
	}
}

func TestServeHTTP_AcceptsBothGitSuffixForms(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	gwURL, token, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetch, "tok", nil)

	for _, form := range []string{"repo.git", "repo"} {
		resp, err := http.Get(gwURL + routePath(token, host, "owner", form, "info/refs") + "?service=git-upload-pack")
		if err != nil {
			t.Fatalf("GET (%s): %v", form, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("form %q: status = %d, want 200", form, resp.StatusCode)
		}
	}
}

func TestServeHTTP_ForbiddenRepoReturns403(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be contacted for a forbidden repo")
	})
	gwURL, token, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetch, "tok", nil)

	// "other" repo was never registered for this token.
	resp, err := http.Get(gwURL + routePath(token, host, "owner", "other.git", "info/refs") + "?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestServeHTTP_PushWithoutPushPermissionReturns403(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be contacted for a denied push")
	})
	gwURL, token, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetch, "tok", nil)

	resp, err := http.Post(gwURL+routePath(token, host, "owner", "repo.git", "git-receive-pack"), "application/x-git-receive-pack-request", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestServeHTTP_InvalidTokenReturns401(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be contacted for an invalid token")
	})
	gwURL, _, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetch, "tok", nil)

	resp, err := http.Get(gwURL + routePath("not-a-real-token", host, "owner", "repo.git", "info/refs") + "?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestServeHTTP_UnrecognizedPathReturns404(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be contacted for a malformed path")
	})
	gwURL, token, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetch, "tok", nil)

	resp, err := http.Get(gwURL + "/j/" + token + "/" + host + "/owner/repo.git/HEAD")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestServeHTTP_WrongMethodReturns405(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be contacted when method is rejected")
	})
	gwURL, token, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetchPush, "tok", nil)

	// git-upload-pack must be POST, not GET.
	resp, err := http.Get(gwURL + routePath(token, host, "owner", "repo.git", "git-upload-pack"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestServeHTTP_InfoRefsMissingServiceReturns400(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be contacted when ?service= is missing")
	})
	gwURL, token, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetch, "tok", nil)

	resp, err := http.Get(gwURL + routePath(token, host, "owner", "repo.git", "info/refs"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServeHTTP_NotifiesOnUpstream401(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	})

	var notifiedHost string
	var notifiedRepo RepoKey
	notified := make(chan struct{}, 1)
	notifier := NotifierFuncs{
		UpstreamAuthFailure: func(host string, repo RepoKey) {
			notifiedHost, notifiedRepo = host, repo
			notified <- struct{}{}
		},
	}

	gwURL, token, host, key := newTestGateway(t, upstream, "owner", "repo", PermFetch, "expired-pat", notifier)

	resp, err := http.Get(gwURL + routePath(token, host, "owner", "repo.git", "info/refs") + "?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (relayed from upstream)", resp.StatusCode)
	}

	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UpstreamAuthFailureNotifier to be called")
	}
	if notifiedHost != host {
		t.Fatalf("notified host = %q, want %q", notifiedHost, host)
	}
	if notifiedRepo != key {
		t.Fatalf("notified repo = %q, want %q", notifiedRepo, key)
	}
}

// TestServeHTTP_CredentialResolutionFailure_FailsFastWith502 proves the
// gateway distinguishes a config problem (credential injection itself fails)
// from an upstream 401, AND that the config-problem path returns 502 without
// ever contacting the upstream — the fail-fast behavior introduced in
// docs/plans/gitgateway-credential-fail-fast.md PR-B, reversing the pre-PR-B
// fail-open policy from docs/plans/git-gateway-cutover.md PR4.
//
// The SecretResolver here deliberately errors — standing in for a
// misconfigured/missing secret_key reference (e.g. `boid secret set -n <ws>
// BB_TOKEN` never ran for the workspace, the real-world hang trigger
// captured in memo [[gitgateway-credential-fail-hangs-sandbox]]) — so
// Resolve fails before the request ever reaches upstream. Under the pre-PR-B
// contract this would forward-without-auth and let the upstream respond
// 401 (the shape that then produced the sandbox TUI credential prompt hang);
// under PR-B the gateway 502s and the upstream handler must never be called.
//
// NotifyCredentialError must still fire exactly once (the observability
// contract is unchanged, only the proxy behavior is). NotifyUpstreamAuthFailure
// must not fire since the upstream is never contacted.
func TestServeHTTP_CredentialResolutionFailure_FailsFastWith502(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be contacted when credential resolution fails (fail-fast), got request to %s", r.URL.Path)
	})
	host := upstreamHost(t, upstream)
	key := NewRepoKey(host, "owner", "repo")

	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{key: PermFetch}, "khi")
	creds := NewCredentialProvider([]HostForgeConfig{
		{Host: host, Forge: ForgeBitbucket, SecretKey: "BB_TOKEN", Scheme: "http"},
	}, func(string, string) (string, error) { return "", fmt.Errorf("sql: no rows in result set") })

	var credErrHost string
	var credErrRepo RepoKey
	var credErr error
	credNotified := make(chan struct{}, 1)
	var upstreamNotified bool
	notifier := NotifierFuncs{
		UpstreamAuthFailure: func(string, RepoKey) { upstreamNotified = true },
		CredentialError: func(host string, repo RepoKey, err error) {
			credErrHost, credErrRepo, credErr = host, repo, err
			credNotified <- struct{}{}
		},
	}

	gw := NewServer(reg, creds, notifier)
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)

	resp, err := http.Get(gwSrv.URL + routePath(token, host, "owner", "repo.git", "info/refs") + "?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 502 (fail-fast: credential resolution failed, upstream must not be contacted); body=%s", resp.StatusCode, body)
	}

	// The 502 body carries diagnostic hints so nose can locate the missing
	// secret without cross-referencing boid.log: host + namespace + secret
	// key. These are the same three fields the wrapped Resolve error itself
	// contains (asserted in credentials_test.go TestResolveResolverError),
	// re-surfaced through the response body for direct observability.
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{host, "khi", "BB_TOKEN"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("response body = %q, missing %q", body, want)
		}
	}

	select {
	case <-credNotified:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for NotifyCredentialError to be called")
	}
	if credErrHost != host {
		t.Fatalf("credential error host = %q, want %q", credErrHost, host)
	}
	if credErrRepo != key {
		t.Fatalf("credential error repo = %q, want %q", credErrRepo, key)
	}
	if credErr == nil {
		t.Fatal("credential error should be non-nil")
	}
	if upstreamNotified {
		t.Fatal("NotifyUpstreamAuthFailure should not fire (upstream was never contacted)")
	}
}

// TestServeHTTP_NoResolverConfiguredRejectsWithoutNotifying is the guard for
// the "KeyFilePath 未設定時の CredentialError 抑制" fix
// (docs/plans/git-gateway-cutover.md PR5 review, flagged in the PR4 review):
// when the daemon has no secret store at all (internal/server/wire.go's
// gwCreds is built with a nil resolver in that case), a valid, authorized
// request must be rejected outright — no upstream contact, no
// NotifyCredentialError spam — rather than falling into the ordinary
// per-key-miss fail-open + notify path exercised by
// TestServeHTTP_NotifiesCredentialErrorDistinctlyFromUpstream401 above.
func TestServeHTTP_NoResolverConfiguredRejectsWithoutNotifying(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be contacted when no secret resolver is configured")
	})
	host := upstreamHost(t, upstream)
	key := NewRepoKey(host, "owner", "repo")

	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{key: PermFetch}, "default")
	// hosts is non-empty (a real gateway config would declare the forge for
	// this host) but resolver is nil — exactly wire.go's KeyFilePath-unset
	// shape.
	creds := NewCredentialProvider([]HostForgeConfig{
		{Host: host, Forge: ForgeGitHub, SecretKey: "gh-pat", Scheme: "http"},
	}, nil)

	var credNotified, upstreamNotified bool
	notifier := NotifierFuncs{
		CredentialError:     func(string, RepoKey, error) { credNotified = true },
		UpstreamAuthFailure: func(string, RepoKey) { upstreamNotified = true },
	}

	gw := NewServer(reg, creds, notifier)
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)

	resp, err := http.Get(gwSrv.URL + routePath(token, host, "owner", "repo.git", "info/refs") + "?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (rejected before contacting upstream)", resp.StatusCode)
	}
	if credNotified {
		t.Error("NotifyCredentialError should not fire for the systemic no-resolver case")
	}
	if upstreamNotified {
		t.Error("NotifyUpstreamAuthFailure should not fire (upstream was never contacted)")
	}
}

// TestServeHTTP_NilCredentialsStillProxiesUnauthenticated proves the
// deliberate no-auth-injection test/upstream mode (Server.credentials ==
// nil, per NewServer's doc comment) is NOT affected by the no-resolver
// rejection above: it must keep forwarding requests unauthenticated exactly
// as before PR5.
func TestServeHTTP_NilCredentialsStillProxiesUnauthenticated(t *testing.T) {
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	host := upstreamHost(t, upstream)
	key := NewRepoKey(host, "owner", "repo")

	reg := NewRegistry()
	token := reg.Register(map[RepoKey]Permission{key: PermFetch}, "default")

	gw := NewServer(reg, nil, nil)
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)

	resp, err := http.Get(gwSrv.URL + routePath(token, host, "owner", "repo.git", "info/refs") + "?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// A nil CredentialProvider makes SchemeFor default to "https", so this
	// http-only test upstream will refuse the TLS handshake the proxy
	// attempts — the point of this test is only that the request reaches
	// the proxy at all (no premature 503), which a non-404/401/403 outcome
	// here (bad gateway from the scheme mismatch) already demonstrates.
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want anything but 503 (nil credentials must not trigger the no-resolver rejection)", resp.StatusCode)
	}
}

// --- streaming / transport-transparency tests ---

// TestServeHTTP_StreamsRequestBodyWithoutBuffering proves the gateway does
// not read the full request body into memory before forwarding it upstream
// (docs/plans/git-gateway-cutover.md PR3: "ボディは無バッファ転送必須...
// packfile の大 POST を io.ReadAll しない"). It also exercises chunked
// Transfer-Encoding end to end, since the io.Pipe request body used here has
// no known Content-Length and so the client sends it chunked.
//
// The proof: the writer deliberately blocks after sending the first chunk
// (holding the pipe, and therefore the client request, open) and does not
// send the second chunk or close the body until the upstream handler has
// already observed the first chunk. If the gateway buffered the whole body
// before forwarding (e.g. via io.ReadAll), the upstream would see nothing
// until EOF — which never arrives while the writer is paused — so the test
// would time out instead of observing a partial read.
func TestServeHTTP_StreamsRequestBodyWithoutBuffering(t *testing.T) {
	const chunkSize = 512 * 1024
	chunk1 := bytes.Repeat([]byte{0xA1}, chunkSize)
	chunk2 := bytes.Repeat([]byte{0xB2}, chunkSize)

	sawPartial := make(chan struct{}, 1)
	var totalReceived int

	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, chunkSize)
		n, err := io.ReadFull(r.Body, buf)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		totalReceived += n
		select {
		case sawPartial <- struct{}{}:
		default:
		}

		rest, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		totalReceived += len(rest)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "%d", totalReceived)
	})
	gwURL, token, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetchPush, "tok", nil)

	pr, pw := io.Pipe()
	proceed := make(chan struct{})

	go func() {
		if _, err := pw.Write(chunk1); err != nil {
			return
		}
		<-proceed
		if _, err := pw.Write(chunk2); err != nil {
			return
		}
		pw.Close()
	}()

	type result struct {
		status int
		body   string
		err    error
	}
	respCh := make(chan result, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, gwURL+routePath(token, host, "owner", "repo.git", "git-receive-pack"), pr)
		if err != nil {
			respCh <- result{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			respCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		respCh <- result{status: resp.StatusCode, body: string(body)}
	}()

	select {
	case <-sawPartial:
		// Upstream received the first chunk while the writer is still
		// paused before chunk2/EOF: proves streaming, not full buffering.
	case res := <-respCh:
		t.Fatalf("request completed (status=%d, body=%s, err=%v) before upstream observed a partial read; body was buffered", res.status, res.body, res.err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for upstream to observe the first chunk")
	}

	close(proceed)

	select {
	case res := <-respCh:
		if res.err != nil {
			t.Fatalf("request error: %v", res.err)
		}
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", res.status, res.body)
		}
		wantTotal := fmt.Sprintf("%d", 2*chunkSize)
		if res.body != wantTotal {
			t.Fatalf("upstream reported %s bytes total, want %s", res.body, wantTotal)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for request to complete after releasing the writer")
	}
}

// TestServeHTTP_Expect100ContinuePassthrough exercises a POST with
// "Expect: 100-continue" through the gateway, confirming the request
// completes normally (the header is neither dropped nor causes the transfer
// to hang) and the full body arrives upstream intact.
func TestServeHTTP_Expect100ContinuePassthrough(t *testing.T) {
	var receivedBody []byte
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		receivedBody = b
		w.WriteHeader(http.StatusOK)
	})
	gwURL, token, host, _ := newTestGateway(t, upstream, "owner", "repo", PermFetchPush, "tok", nil)

	body := bytes.Repeat([]byte("pack"), 5000)
	req, err := http.NewRequest(http.MethodPost, gwURL+routePath(token, host, "owner", "repo.git", "git-receive-pack"), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Expect", "100-continue")
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")

	client := &http.Client{
		Transport: &http.Transport{ExpectContinueTimeout: 3 * time.Second},
		Timeout:   10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(receivedBody, body) {
		t.Fatalf("upstream received %d bytes, want %d", len(receivedBody), len(body))
	}
}

// --- workspace-scoped PAT namespace (post-cutover 改善 §1) ---

// TestServeHTTP_RoutesCredentialsByTokenNamespace is the end-to-end guard for
// post-cutover 改善 §1 (workspace-scoped PAT namespace): a single gateway
// Server, with one shared HostForgeConfig/SecretResolver, must route two
// different job tokens — registered under two different Registry namespaces
// — to two different upstream Basic-auth credentials. This is the shape a
// real daemon reaches once two workspaces each `boid secret set --namespace
// <ws> gh-pat <PAT>`: same gateway host config, different PAT per workspace,
// selected purely by which job token made the request.
func TestServeHTTP_RoutesCredentialsByTokenNamespace(t *testing.T) {
	var gotAuth string
	upstream := newCapturingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	host := upstreamHost(t, upstream)
	key := NewRepoKey(host, "owner", "repo")

	secretsByNamespace := map[string]string{
		"ws-a": "pat-for-ws-a",
		"ws-b": "pat-for-ws-b",
	}
	resolver := func(namespace, key string) (string, error) {
		if key != "gh-pat" {
			t.Fatalf("resolver called with unexpected key %q", key)
		}
		v, ok := secretsByNamespace[namespace]
		if !ok {
			t.Fatalf("resolver called with unexpected namespace %q", namespace)
		}
		return v, nil
	}
	creds := NewCredentialProvider([]HostForgeConfig{
		{Host: host, Forge: ForgeGitHub, SecretKey: "gh-pat", Scheme: "http"},
	}, resolver)

	reg := NewRegistry()
	tokenA := reg.Register(map[RepoKey]Permission{key: PermFetch}, "ws-a")
	tokenB := reg.Register(map[RepoKey]Permission{key: PermFetch}, "ws-b")

	gw := NewServer(reg, creds, nil)
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)

	for token, wantNamespace := range map[string]string{tokenA: "ws-a", tokenB: "ws-b"} {
		resp, err := http.Get(gwSrv.URL + routePath(token, host, "owner", "repo.git", "info/refs") + "?service=git-upload-pack")
		if err != nil {
			t.Fatalf("GET (namespace %s): %v", wantNamespace, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("namespace %s: status = %d, want 200", wantNamespace, resp.StatusCode)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+secretsByNamespace[wantNamespace]))
		if gotAuth != wantAuth {
			t.Fatalf("namespace %s: upstream saw Authorization = %q, want %q", wantNamespace, gotAuth, wantAuth)
		}
	}
}
