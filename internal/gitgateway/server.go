package gitgateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// Server is the git gateway's HTTP handler: a thin net/http/httputil.
// ReverseProxy wrapper that does path-based authorization (Registry) and
// forge credential injection (CredentialProvider) around the standard
// library's streaming transport. It never buffers request bodies —
// packfile POSTs are streamed straight through to the upstream forge
// (docs/plans/git-gateway-cutover.md PR3: 「ボディは無バッファ転送必須」).
//
// Server has been wired into the running daemon since PR4
// (docs/plans/git-gateway-cutover.md): internal/server/wire.go constructs
// one (Registry + CredentialProvider + notifier) and Server.Start/Stop own
// its listener lifecycle alongside the daemon's other subservers. PR5 is the
// first PR whose traffic actually reaches it in practice (the runner's
// sandbox-internal clone sequence, gated behind the still-inert
// sandbox.CloneSpec opt-in) — until a caller sets that, this handler serves
// zero real requests.
type Server struct {
	registry    *Registry
	credentials *CredentialProvider
	notifier    UpstreamAuthFailureNotifier
	proxy       *httputil.ReverseProxy

	// tlsHTTPServer is the *http.Server bound by ListenTLS, kept so
	// CloseTLS can gracefully shut it down — including closing keep-alive
	// connections that are idle but otherwise outlive a bare
	// net.Listener.Close() (codex review [Minor 4] on
	// docs/plans/phase6-container-backend.md §PR4). nil until ListenTLS
	// is called.
	tlsHTTPServer *http.Server
}

// routeInfoKey is the context key used to hand the authorized route's
// upstream target (and repo, for the 401 notifier) from ServeHTTP to the
// ReverseProxy's Rewrite/ModifyResponse hooks.
type routeInfoKey struct{}

type routeInfo struct {
	host      string
	repo      RepoKey
	namespace string
}

// NewServer builds a Server. credentials may be nil (requests are proxied
// without auth injection — useful for tests against an upstream that
// doesn't require it); notifier may be nil (defaults to NoopNotifier).
func NewServer(registry *Registry, credentials *CredentialProvider, notifier UpstreamAuthFailureNotifier) *Server {
	if notifier == nil {
		notifier = NoopNotifier
	}
	s := &Server{
		registry:    registry,
		credentials: credentials,
		notifier:    notifier,
	}

	s.proxy = &httputil.ReverseProxy{
		// ExpectContinueTimeout makes the outbound transport actually wait
		// for the upstream's 100-continue before streaming the body,
		// rather than silently ignoring the client's "Expect:
		// 100-continue" header (docs/plans/git-gateway-cutover.md PR3:
		// "Expect: 100-continue と chunked encoding の透過的な扱い"). All
		// other Transport fields are left at http.Transport's zero values
		// (== streaming semantics, no body buffering).
		Transport: &http.Transport{
			ExpectContinueTimeout: 5 * time.Second,
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			info, _ := pr.In.Context().Value(routeInfoKey{}).(routeInfo)
			pr.Out.URL.Scheme = s.credentials.SchemeFor(info.host)
			pr.Out.URL.Host = info.host
			pr.Out.URL.Path = pr.In.URL.Path // set by ServeHTTP before proxying
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = info.host
			if s.credentials != nil {
				// Two Inject-failure shapes reach this callback:
				//
				//   - Unknown host (not in config.Gateway.HostConfigs()):
				//     ServeHTTP's pre-check is gated on KnowsHost, so an
				//     unknown host bypasses fail-fast and lands here with
				//     Inject returning "no forge configured for host". This
				//     is the pre-PR-B fail-open + notify path preserved
				//     verbatim — test upstreams (httptest.Server dynamic
				//     ports) and stray unregistered-forge requests both
				//     take this shape.
				//
				//   - Known host + secret race: any Inject failure for a
				//     known host is a race between pre-check and Rewrite
				//     (currently no code path unregisters a secret
				//     mid-request, so effectively unreachable). Same
				//     fail-open behavior applies; notifier double-fire is
				//     not a concern because the pre-check already fired
				//     the primary signal in the non-race case.
				//
				// Either way: log for observability, notify (unchanged from
				// pre-PR-B), and forward unauthenticated. Upstream 401 will
				// still trip ModifyResponse's NotifyUpstreamAuthFailure.
				if err := s.credentials.Inject(pr.Out, info.host, info.namespace); err != nil {
					slog.Warn("gitgateway: credential injection failed; forwarding without auth", "host", info.host, "err", err)
					s.notifier.NotifyCredentialError(info.host, info.repo, err)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusUnauthorized {
				info, _ := resp.Request.Context().Value(routeInfoKey{}).(routeInfo)
				slog.Warn("gitgateway: upstream rejected credentials (401); token may be expired or revoked", "host", info.host, "repo", info.repo)
				s.notifier.NotifyUpstreamAuthFailure(info.host, info.repo)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("gitgateway: upstream request failed", "err", err)
			http.Error(w, "bad gateway: "+err.Error(), http.StatusBadGateway)
		},
	}
	return s
}

// ServeHTTP parses the request path, authorizes it against the registry,
// and — if allowed — rewrites it to the upstream forge URL and proxies it
// with credentials injected. Unrecognized paths get 404; unknown/expired
// tokens get 401; well-formed-but-disallowed repo/operation combinations get
// 403; anything else about the request shape (missing/bad ?service=, wrong
// method) gets 400/405.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, err := parsePath(r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if strings.ToUpper(r.Method) != methodForEndpoint(rt.endpoint) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	op, err := operationForEndpoint(rt.endpoint, r.URL.Query().Get("service"))
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	repo := rt.repoKey()
	allowed, tokenValid := s.registry.Authorize(rt.token, repo, op)
	if !tokenValid {
		http.Error(w, "unauthorized: invalid or expired job token", http.StatusUnauthorized)
		return
	}
	if !allowed {
		http.Error(w, "forbidden: repo/operation not permitted for this job token", http.StatusForbidden)
		return
	}

	// This second Lookup recovers Entry.Namespace, which Authorize's
	// bool-returning signature doesn't expose — namespace scopes the
	// credential resolution below (post-cutover 改善 §1 workspace-scoped
	// PAT namespace). entry is guaranteed present under normal token
	// lifetime: Unregister runs only at job completion (via
	// Runner.UnregisterJob), never from a peer request, so there is no
	// caller racing this handler for the same token. The theoretical ABA
	// race (Authorize sees the entry, an Unregister slips in, this Lookup
	// misses) can only fire if that lifetime rule is ever broken; if it
	// does, `namespace` degrades to "" here, which SecretStore.Get's
	// normalizeNamespace turns into "default" — the request still proxies
	// safely with default-namespace credentials rather than crashing or
	// leaking a token from a different namespace.
	entry, _ := s.registry.Lookup(rt.token)
	namespace := entry.Namespace

	// Systemic "no secret resolver at all" case (docs/plans/git-gateway-cutover.md
	// PR5 review): reject before ever contacting the upstream or invoking
	// the notifier, distinct from the ordinary per-key-miss path handled by
	// the Resolve pre-check just below. s.credentials == nil is a deliberate
	// no-auth-injection test/upstream mode (see NewServer's doc comment) and
	// is intentionally NOT covered by either this check or the Resolve
	// pre-check.
	if s.credentials != nil && !s.credentials.Configured() {
		http.Error(w, "service unavailable: git gateway has no secret resolver configured", http.StatusServiceUnavailable)
		return
	}

	// Per-key credential-resolution failure (docs/plans/gitgateway-credential-fail-fast.md
	// PR-B): call Resolve before ever proxying so that a missing / broken
	// secret returns 502 instead of forwarding the request unauthenticated
	// and inheriting the upstream's 401 + WWW-Authenticate: Basic — which
	// the sandbox-inner git would answer with an interactive credential
	// prompt, hanging the whole TUI (`Username for 'http://10.0.2.2:...':`
	// with no way out but Ctrl-C).
	//
	// 502 (Bad Gateway) is the intentional shape: git treats it as fatal
	// (no prompt), and it semantically matches "gateway itself could not
	// reach the upstream on your behalf" — which is exactly what a
	// misconfigured secret means from the client's point of view.
	//
	// The pre-check is gated on KnowsHost: pre-PR-B, an unknown host also
	// took the fail-open + notify path (Rewrite's Inject would return
	// "no forge configured for host" and forward unauthenticated). Test
	// upstreams (httptest.Server dynamic ports) never appear in
	// config.Gateway.HostConfigs() and rely on that pre-existing shape —
	// gating on KnowsHost preserves it while still fail-fast'ing the
	// intended case (known host + resolver miss, the actual hang trigger
	// captured in [[gitgateway-credential-fail-hangs-sandbox]]).
	//
	// This reverses the pre-cutover fail-open + NotifyCredentialError
	// behavior (`docs/plans/git-gateway-cutover.md` PR3/PR4: 「gateway 自体は
	// 落とさない」) ONLY for the known-host-with-secret-miss case. That
	// principle held while the gateway was still inert (PR3/PR4) and the
	// only visible consequence of forwarding-without-auth was a 401 in the
	// log; once PR5+ made real sandbox clients depend on this path, that
	// same forwarding started producing the TUI hang above — a much worse
	// failure mode than the honest 502 we now return.
	//
	// The notifier fires exactly once (here), so callers such as
	// internal/server/gitgateway_notify.go still see the same
	// per-request signal they did before; only the proxy path has changed.
	// Rewrite's Inject call below is left in place — it will succeed on
	// the second resolve for any request that made it past this pre-check,
	// so the cost of the extra lookup is one SecretStore.Get per request
	// when credentials are healthy (cheap: an in-process DB read).
	if s.credentials != nil && s.credentials.KnowsHost(rt.host) {
		if _, _, err := s.credentials.Resolve(rt.host, namespace); err != nil {
			slog.Warn("gitgateway: credential resolution failed; refusing to forward (fail-fast)",
				"host", rt.host, "namespace", namespace, "err", err)
			s.notifier.NotifyCredentialError(rt.host, repo, err)
			http.Error(w,
				"bad gateway: git gateway credential resolution failed for host "+
					rt.host+" (namespace "+namespace+"): "+err.Error(),
				http.StatusBadGateway)
			return
		}
	}

	// Rewrite the request path in place to the upstream's canonical
	// (".git"-suffixed) form; Rewrite reads it back off pr.In.URL.Path.
	r.URL.Path = rt.upstreamPath()

	ctx := context.WithValue(r.Context(), routeInfoKey{}, routeInfo{host: rt.host, repo: repo, namespace: namespace})
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// ListenTLS binds a TCP+mTLS listener at addr and serves s on it in a
// background goroutine, returning immediately once the listener is bound.
// This is the git gateway's TCP(mTLS) counterpart to the plaintext
// loopback listener internal/server.Server already binds for the userns
// backend (docs/plans/phase6-container-backend.md §PR4/§決定5) — purely
// additive, so that existing loopback+10.0.2.2 path is unaffected by
// calling this. tlsConfig is expected to require and verify a client
// certificate (see internal/mtls.CA.ServerTLSConfig).
//
// The caller owns the returned listener's lifecycle: closing it (directly,
// or by calling CloseTLS) stops the background http.Serve goroutine (which
// returns http.ErrServerClosed, swallowed here exactly like
// internal/server.Server already does for its other listeners). Closing
// the bare net.Listener alone only stops new connections from being
// accepted — it does not tear down already-accepted keep-alive
// connections, since those are owned by the *http.Server driving Serve,
// not the listener. Callers that need existing connections closed too
// (e.g. on daemon shutdown) should call CloseTLS instead of ln.Close().
func (s *Server) ListenTLS(addr string, tlsConfig *tls.Config) (net.Listener, error) {
	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("gitgateway: listen tls: %w", err)
	}
	srv := &http.Server{Handler: s}
	s.tlsHTTPServer = srv
	go func() {
		_ = srv.Serve(ln) // returns http.ErrServerClosed when ln (or srv) is closed; caller owns lifecycle
	}()
	return ln, nil
}

// CloseTLS gracefully shuts down the http.Server bound by ListenTLS: it
// stops accepting new connections (like ln.Close() alone would) and also
// closes idle keep-alive connections, waiting up to ctx's deadline for
// in-flight requests to finish before returning. A no-op (nil error) if
// ListenTLS was never called.
func (s *Server) CloseTLS(ctx context.Context) error {
	if s.tlsHTTPServer == nil {
		return nil
	}
	return s.tlsHTTPServer.Shutdown(ctx)
}
