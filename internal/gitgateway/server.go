package gitgateway

import (
	"context"
	"log/slog"
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
// Server is inert in this PR: nothing constructs or listens on one from the
// running daemon yet (that wiring, plus config.yaml/SandboxRuntimeInfo
// plumbing, is PR4).
type Server struct {
	registry    *Registry
	credentials *CredentialProvider
	notifier    UpstreamAuthFailureNotifier
	proxy       *httputil.ReverseProxy
}

// routeInfoKey is the context key used to hand the authorized route's
// upstream target (and repo, for the 401 notifier) from ServeHTTP to the
// ReverseProxy's Rewrite/ModifyResponse hooks.
type routeInfoKey struct{}

type routeInfo struct {
	host string
	repo RepoKey
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
				if err := s.credentials.Inject(pr.Out, info.host); err != nil {
					slog.Warn("gitgateway: credential injection failed; forwarding without auth", "host", info.host, "err", err)
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

	// Rewrite the request path in place to the upstream's canonical
	// (".git"-suffixed) form; Rewrite reads it back off pr.In.URL.Path.
	r.URL.Path = rt.upstreamPath()

	ctx := context.WithValue(r.Context(), routeInfoKey{}, routeInfo{host: rt.host, repo: repo})
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}
