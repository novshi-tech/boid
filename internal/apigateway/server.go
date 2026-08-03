package apigateway

import (
	"context"
	"fmt"
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
	// upstreamPath/upstreamRawPath are the decoded/escaped forms of the
	// full upstream path (base_url's own path prefix + the request tail),
	// pre-computed once in ServeHTTP via url.Parse rather than reconstructed
	// inside Rewrite — see their computation's own comment for why both
	// forms must be set together on the outbound URL.
	upstreamPath    string
	upstreamRawPath string
}

// injectionErrorKey is the context key Rewrite uses to signal a credential-
// injection failure to the custom Transport below, when that failure is
// detected AFTER ServeHTTP's own fail-fast pre-check already succeeded — a
// narrow but real race (e.g. a concurrent `boid secret delete` lands between
// the two resolves). See failFastTransport's own doc comment for why this
// must abort the request rather than forward it unauthenticated.
type injectionErrorKey struct{}

// failFastTransport wraps a real http.RoundTripper so a credential-injection
// failure recorded on the outbound request's context (injectionErrorKey)
// aborts the request BEFORE any network I/O, instead of
// httputil.ReverseProxy's default behavior of proceeding to RoundTrip with
// whatever Rewrite left the request as.
//
// This is a deliberate departure from internal/gitgateway.Server's own
// Rewrite, which logs+notifies and then forwards unauthenticated on the
// exact same race. That fail-open choice is safe for git specifically
// because every upstream forge reliably answers an unauthenticated
// smart-HTTP request with 401 — the sandbox's own git client just sees an
// ordinary auth failure. It is NOT safe here: base_url points at an
// arbitrary, operator-configured REST API whose behavior with no
// Authorization header is unknowable in general — some route might not
// require auth at all and answer with a public/anonymous-tier response the
// caller was never meant to reach unauthenticated, or take an unintended
// anonymous action. Fail-fast (abort, 502) is the only safe default for a
// gateway whose entire purpose is "this request is authenticated, or it
// does not go out."
type failFastTransport struct {
	base http.RoundTripper
}

func (t failFastTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err, failed := req.Context().Value(injectionErrorKey{}).(error); failed {
		return nil, err
	}
	return t.base.RoundTrip(req)
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
		// api-gateway.md §5 "無バッファストリーミング転送"). Wrapped in
		// failFastTransport — see that type's own doc comment.
		Transport: failFastTransport{base: &http.Transport{
			ExpectContinueTimeout: 5 * time.Second,
		}},
		// FlushInterval < 0 flushes immediately after every write instead
		// of batching on a timer — required for SSE (Server-Sent Events)
		// upstreams to stream incrementally rather than arriving in bursts
		// (docs/plans/api-gateway.md §5 "SSE 対応").
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			info, _ := pr.In.Context().Value(routeInfoKey{}).(routeInfo)
			pr.Out.URL.Scheme = info.baseURL.Scheme
			pr.Out.URL.Host = info.baseURL.Host
			// Path/RawPath are both set explicitly (not derived from
			// pr.In.URL.Path) — see routeInfo.upstreamPath's own comment:
			// this is what keeps a "%2F" inside a single request-tail
			// segment byte-preserved on the wire instead of being decoded
			// into an extra path separator.
			pr.Out.URL.Path = info.upstreamPath
			pr.Out.URL.RawPath = info.upstreamRawPath
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
					slog.Warn("apigateway: credential injection failed; aborting request (fail-fast, not forwarding unauthenticated)",
						"service", info.service, "err", err)
					s.notifier.NotifyCredentialError(info.service, err)
					injectErr := fmt.Errorf("apigateway: credential injection failed for service %q: %w", info.service, err)
					pr.Out = pr.Out.WithContext(context.WithValue(pr.Out.Context(), injectionErrorKey{}, injectErr))
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
	// EscapedPath, not r.URL.Path — see parsePath's own doc comment: net/http
	// decodes %2F into a literal "/" in .Path before a handler ever sees it,
	// which would silently merge two logically-distinct path segments.
	rt, err := parsePath(r.URL.EscapedPath())
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
	// otherwise produce. Rewrite's own Inject call re-resolves and, on a
	// failure THERE (the narrow race window between this check and the
	// actual round trip), aborts via failFastTransport rather than
	// forwarding unauthenticated — see that type's own doc comment.
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

	// Compute the full upstream path (base_url's own path prefix + the
	// request tail) ONCE here via url.Parse, rather than string-concatenating
	// pr.In.URL.Path inside Rewrite: url.Parse resolves the combined string
	// into a consistent (Path, RawPath) pair, which is what lets Rewrite set
	// BOTH fields on the outbound request and have Go's own EscapedPath()
	// logic recognize RawPath as a valid encoding of Path (see
	// net/url.URL.EscapedPath's own validity rule) instead of silently
	// re-escaping (and thereby double-encoding, or dropping the distinction
	// entirely) at request-send time.
	basePath := strings.TrimSuffix(baseURL.EscapedPath(), "/")
	upstreamURL, err := url.Parse(basePath + rt.path)
	if err != nil {
		// Defensive: rt.path was already validated by parsePath to contain
		// no malformed percent-encoding, and basePath comes from a
		// config.yaml base_url that internal/config already validated as a
		// parseable absolute URL — this should be unreachable in practice.
		s.recorder(entry.TaskID, r.Method, rt.service, rt.path, http.StatusBadGateway)
		http.Error(w, "bad gateway: could not construct upstream path: "+err.Error(), http.StatusBadGateway)
		return
	}

	ctx := context.WithValue(r.Context(), routeInfoKey{}, routeInfo{
		service:         rt.service,
		namespace:       entry.Namespace,
		taskID:          entry.TaskID,
		method:          r.Method,
		path:            rt.path,
		baseURL:         baseURL,
		upstreamPath:    upstreamURL.Path,
		upstreamRawPath: upstreamURL.RawPath,
	})
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// isSafeMethod reports whether method is exactly "GET" or "HEAD" — the only
// methods a read-only job token may use (docs/plans/api-gateway.md 前提と
// なる決定事項: "task.readonly を HTTP メソッドに写像する"). Deliberately an
// exact, case-SENSITIVE comparison, not a case-folded one: HTTP method
// tokens are case-sensitive (RFC 7230 §3.1.1 defines the method as a
// case-sensitive token, and net/http never normalizes r.Method), so a
// lowercase "get" is a different, non-standard method — accepting it here
// as equivalent to "GET" would let a request using it bypass the read-only
// gate on the (unverifiable) assumption that the upstream treats it
// identically to "GET" too.
func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}
