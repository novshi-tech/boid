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

	"github.com/novshi-tech/boid/internal/gwtransport"
)

// Server is the git gateway's HTTP handler: a thin net/http/httputil.
// ReverseProxy wrapper that does path-based authorization (Registry) and
// forge credential injection (CredentialProvider) around the standard
// library's streaming transport. It never buffers request bodies —
// packfile POSTs stream straight through to the upstream forge.
type Server struct {
	registry    *Registry
	credentials *CredentialProvider
	notifier    UpstreamAuthFailureNotifier
	proxy       *httputil.ReverseProxy

	// tlsHTTPServer is the *http.Server bound by ListenTLS, kept so
	// CloseTLS can gracefully shut it down — including closing keep-alive
	// connections that a bare net.Listener.Close() would leave open. nil
	// until ListenTLS is called.
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
		// Outbound transport shared with internal/apigateway; see
		// gwtransport's doc comment for why (streaming semantics +
		// connection-liveness settings).
		Transport: gwtransport.New(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			info, _ := pr.In.Context().Value(routeInfoKey{}).(routeInfo)
			pr.Out.URL.Scheme = s.credentials.SchemeFor(info.host)
			pr.Out.URL.Host = info.host
			pr.Out.URL.Path = pr.In.URL.Path // set by ServeHTTP before proxying
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = info.host
			if s.credentials != nil {
				// Inject can fail two ways: an unknown host (ServeHTTP's
				// fail-fast pre-check only covers known hosts) or a
				// known-host secret race (nothing unregisters a secret
				// mid-request today, so effectively unreachable). Either
				// way: log, notify, forward unauthenticated — an upstream
				// 401 will still trip ModifyResponse's
				// NotifyUpstreamAuthFailure.
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

	// Entry.Namespace isn't exposed by Authorize's bool-returning signature,
	// so look it up again here. entry is guaranteed present since
	// Unregister only runs at job completion, never racing this handler;
	// if that ever changes, namespace degrades to "" here, which
	// SecretStore.Get's normalizeNamespace turns into "default" rather than
	// crashing or leaking a token from a different namespace.
	entry, _ := s.registry.Lookup(rt.token)
	namespace := entry.Namespace

	// Systemic "no secret resolver at all" case: reject before ever
	// contacting the upstream, distinct from the ordinary per-key-miss path
	// handled by the Resolve pre-check just below. s.credentials == nil is
	// a deliberate no-auth-injection test/upstream mode (see NewServer's
	// doc comment) and is intentionally NOT covered by either check.
	if s.credentials != nil && !s.credentials.Configured() {
		http.Error(w, "service unavailable: git gateway has no secret resolver configured", http.StatusServiceUnavailable)
		return
	}

	// Resolve credentials before ever proxying, so a missing/broken secret
	// returns 502 instead of forwarding unauthenticated and inheriting the
	// upstream's 401 + WWW-Authenticate: Basic — which the sandbox-inner
	// git answers with an interactive credential prompt, hanging the TUI.
	// 502 is deliberate: git treats it as fatal (no prompt).
	//
	// Gated on KnowsHost so an unrecognized host (test upstreams; stray
	// unregistered-forge requests) still forwards unauthenticated with just
	// a notify, rather than always failing closed here.
	//
	// The notifier fires once, here; Rewrite's Inject call below is left in
	// place and expected to succeed on the second resolve, so the cost when
	// credentials are healthy is one extra SecretStore.Get per request.
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
// tlsConfig is expected to require and verify a client certificate (see
// internal/mtls.CA.ServerTLSConfig).
//
// The caller owns the returned listener's lifecycle. Closing the bare
// net.Listener alone stops new connections but leaves already-accepted
// keep-alive connections open (they're owned by the *http.Server driving
// Serve, not the listener); call CloseTLS instead to also close those
// (e.g. on daemon shutdown).
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
