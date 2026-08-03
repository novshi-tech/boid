package apigateway

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Server is the API gateway's HTTP handler: a thin net/http/httputil.
// ReverseProxy wrapper that does path-based authorization (Registry) and
// per-service credential injection (CredentialProvider) around the standard
// library's streaming transport — the same shape as
// internal/gitgateway.Server, generalized from git smart HTTP to arbitrary
// HTTP APIs (docs/plans/api-gateway.md PR1).
type Server struct {
	registry    *Registry
	credentials *CredentialProvider
	notifier    UpstreamAuthFailureNotifier
	recorder    RequestRecorder
	proxy       *httputil.ReverseProxy
}

// routeInfoKey is the context key used to hand the authorized route's
// resolved upstream target (and the fields the recorder/notifier need) from
// ServeHTTP to the ReverseProxy's Rewrite/ModifyResponse hooks.
type routeInfoKey struct{}

type routeInfo struct {
	service   string
	namespace string
	taskID    string
	method    string
	path      string
	baseURL   *url.URL
}

// NewServer builds a Server. notifier may be nil (defaults to NoopNotifier);
// recorder may be nil (defaults to a no-op).
func NewServer(registry *Registry, credentials *CredentialProvider, notifier UpstreamAuthFailureNotifier, recorder RequestRecorder) *Server {
	if notifier == nil {
		notifier = NoopNotifier
	}
	if recorder == nil {
		recorder = noopRecorder
	}
	s := &Server{
		registry:    registry,
		credentials: credentials,
		notifier:    notifier,
		recorder:    recorder,
	}

	s.proxy = &httputil.ReverseProxy{
		// ExpectContinueTimeout mirrors internal/gitgateway.Server's own
		// Transport: all other fields stay at http.Transport's zero values
		// (== streaming semantics, no request body buffering — docs/plans/
		// api-gateway.md §5 "無バッファストリーミング転送").
		Transport: &http.Transport{
			ExpectContinueTimeout: 5 * time.Second,
		},
		// FlushInterval < 0 flushes immediately after every write instead
		// of batching on a timer — required for SSE (Server-Sent Events)
		// upstreams to stream incrementally rather than arriving in bursts
		// (docs/plans/api-gateway.md §5 "SSE 対応").
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			info, _ := pr.In.Context().Value(routeInfoKey{}).(routeInfo)
			pr.Out.URL.Scheme = info.baseURL.Scheme
			pr.Out.URL.Host = info.baseURL.Host
			pr.Out.URL.Path = pr.In.URL.Path // set by ServeHTTP before proxying
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = info.baseURL.Host

			// inbound ヘッダ剥がし (docs/plans/api-gateway.md §5): a
			// sandbox-supplied Authorization/Cookie/Proxy-Authorization must
			// never reach the upstream — it would either collide with the
			// gateway's own injected credential or let the sandbox smuggle
			// an entirely different credential through the gateway
			// unmodified.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Proxy-Authorization")

			if s.credentials != nil {
				if err := s.credentials.Inject(pr.Out, info.service, info.namespace); err != nil {
					slog.Warn("apigateway: credential injection failed; forwarding without auth",
						"service", info.service, "err", err)
					s.notifier.NotifyCredentialError(info.service, err)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			info, _ := resp.Request.Context().Value(routeInfoKey{}).(routeInfo)
			if resp.StatusCode == http.StatusUnauthorized {
				slog.Warn("apigateway: upstream rejected credentials (401); the configured secret may be expired or revoked",
					"service", info.service)
				s.notifier.NotifyUpstreamAuthFailure(info.service)
			}
			s.recorder(info.taskID, info.method, info.service, info.path, resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			info, _ := r.Context().Value(routeInfoKey{}).(routeInfo)
			slog.Warn("apigateway: upstream request failed", "service", info.service, "err", err)
			s.recorder(info.taskID, info.method, info.service, info.path, http.StatusBadGateway)
			http.Error(w, "bad gateway: "+err.Error(), http.StatusBadGateway)
		},
	}
	return s
}

// ServeHTTP parses the request path, authorizes it against the registry,
// and — if allowed — rewrites it to the configured service's base URL and
// proxies it with credentials injected. Unrecognized paths get 404;
// unknown/expired tokens get 401; well-formed-but-disallowed service
// requests get 403; a read-only token attempting a non-GET/HEAD method gets
// 403; an unconfigured (or credential-broken) service gets 502/503.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, err := parsePath(r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	allowed, tokenValid := s.registry.Authorize(rt.token, rt.service)
	if !tokenValid {
		http.Error(w, "unauthorized: invalid or expired job token", http.StatusUnauthorized)
		return
	}

	// entry is guaranteed present: Authorize already did a successful
	// Lookup to determine tokenValid==true. This second Lookup recovers the
	// fields Authorize's bool-returning signature doesn't expose
	// (Namespace/TaskID/ReadOnly) — the same pattern
	// internal/gitgateway.Server.ServeHTTP uses for Entry.Namespace.
	entry, _ := s.registry.Lookup(rt.token)

	if !allowed {
		s.recorder(entry.TaskID, r.Method, rt.service, rt.path, http.StatusForbidden)
		http.Error(w, "forbidden: service not permitted for this job token", http.StatusForbidden)
		return
	}

	if entry.ReadOnly && !isSafeMethod(r.Method) {
		s.recorder(entry.TaskID, r.Method, rt.service, rt.path, http.StatusForbidden)
		http.Error(w, "forbidden: read-only job token may only use GET/HEAD", http.StatusForbidden)
		return
	}

	baseURL, ok := s.credentials.BaseURLFor(rt.service)
	if !ok {
		s.recorder(entry.TaskID, r.Method, rt.service, rt.path, http.StatusBadGateway)
		http.Error(w, "bad gateway: service "+rt.service+" is not configured", http.StatusBadGateway)
		return
	}

	// Systemic "no secret resolver at all" case, distinct from an ordinary
	// per-key miss handled by the Resolve pre-check just below — mirrors
	// internal/gitgateway.Server.ServeHTTP's identical two-tier check.
	if !s.credentials.Configured() {
		s.recorder(entry.TaskID, r.Method, rt.service, rt.path, http.StatusServiceUnavailable)
		http.Error(w, "service unavailable: api gateway has no secret resolver configured", http.StatusServiceUnavailable)
		return
	}

	// Fail-fast credential pre-check (docs/plans/gitgateway-credential-fail-fast.md
	// pattern, generalized): resolve before ever proxying, so a missing or
	// broken secret returns 502 instead of forwarding the request
	// unauthenticated (or not at all, for kinds that have no unauthenticated
	// fallback) and inheriting whatever confusing failure the upstream would
	// otherwise produce.
	if err := s.credentials.Resolve(rt.service, entry.Namespace); err != nil {
		slog.Warn("apigateway: credential resolution failed; refusing to forward (fail-fast)",
			"service", rt.service, "namespace", entry.Namespace, "err", err)
		s.notifier.NotifyCredentialError(rt.service, err)
		s.recorder(entry.TaskID, r.Method, rt.service, rt.path, http.StatusBadGateway)
		http.Error(w,
			"bad gateway: api gateway credential resolution failed for service "+rt.service+": "+err.Error(),
			http.StatusBadGateway)
		return
	}

	// Rewrite the request path in place to the upstream's own path
	// (service base_url's own path prefix, if any, joined with the cleaned
	// request tail); Rewrite reads it back off pr.In.URL.Path.
	basePath := strings.TrimSuffix(baseURL.Path, "/")
	r.URL.Path = basePath + rt.path

	ctx := context.WithValue(r.Context(), routeInfoKey{}, routeInfo{
		service:   rt.service,
		namespace: entry.Namespace,
		taskID:    entry.TaskID,
		method:    r.Method,
		path:      rt.path,
		baseURL:   baseURL,
	})
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// isSafeMethod reports whether method is GET or HEAD — the only methods a
// read-only job token may use (docs/plans/api-gateway.md 前提となる決定事項:
// "task.readonly を HTTP メソッドに写像する").
func isSafeMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead:
		return true
	default:
		return false
	}
}
