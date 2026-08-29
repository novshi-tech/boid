package apigateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/novshi-tech/boid/internal/gwtransport"
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
	service string
	// account is the optional credential-account qualifier (docs/plans/
	// api-gateway-credential-accounts.md) — carried alongside service
	// (which is ALWAYS the base name, account already stripped — D4) so
	// Rewrite's Inject call can resolve the right account-qualified
	// credential (D2/D3) while every authz/BaseURL lookup upstream of it
	// keeps using service alone.
	account   string
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

// recordedServiceName returns the string every RequestRecorder/notifier/
// log call in this file uses to identify "which service" a request
// targeted: service unchanged when account is empty (byte-identical to
// every pre-account-support call site, D2), or "service@account" when an
// account was specified (docs/plans/api-gateway-credential-accounts.md D8
// — RequestRecorder's own signature is unchanged; this is the one place
// account is folded back in before handing off to it). This is
// deliberately NOT what gets passed to CredentialProvider.Resolve/Inject or
// used for Entry.Services/BaseURLFor/AllowsReadOnlyWrite lookups (D4) — all
// of those take service and account as separate arguments/fields.
func recordedServiceName(service, account string) string {
	if account == "" {
		return service
	}
	return service + "@" + account
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
		// The outbound transport is shared with internal/gitgateway via
		// gwtransport.New: ExpectContinueTimeout plus the connection-
		// liveness settings (idle-conn expiry, HTTP/2 keep-alive ping)
		// whose absence wedged this gateway against a silently-vanished
		// upstream in production — see that package's own doc comment.
		// Every body-streaming-relevant field is still left at
		// http.Transport's zero value (== streaming semantics, no request
		// body buffering — docs/plans/api-gateway.md §5 "無バッファ
		// ストリーミング転送"). Wrapped in failFastTransport — see that
		// type's own doc comment.
		Transport: failFastTransport{base: gwtransport.New()},
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
				if err := s.credentials.Inject(pr.Out, info.namespace, info.service, info.account); err != nil {
					recSvc := recordedServiceName(info.service, info.account)
					slog.Warn("apigateway: credential injection failed; aborting request (fail-fast, not forwarding unauthenticated)",
						"service", recSvc, "err", err)
					s.notifier.NotifyCredentialError(recSvc, err)
					injectErr := fmt.Errorf("apigateway: credential injection failed for service %q: %w", recSvc, err)
					pr.Out = pr.Out.WithContext(context.WithValue(pr.Out.Context(), injectionErrorKey{}, injectErr))
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			info, _ := resp.Request.Context().Value(routeInfoKey{}).(routeInfo)
			recSvc := recordedServiceName(info.service, info.account)
			if resp.StatusCode == http.StatusUnauthorized {
				slog.Warn("apigateway: upstream rejected credentials (401); the configured secret may be expired or revoked",
					"service", recSvc)
				s.notifier.NotifyUpstreamAuthFailure(recSvc)
			}
			s.recorder(info.taskID, info.method, recSvc, info.path, resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			info, _ := r.Context().Value(routeInfoKey{}).(routeInfo)
			recSvc := recordedServiceName(info.service, info.account)
			slog.Warn("apigateway: upstream request failed", "service", recSvc, "err", err)
			s.recorder(info.taskID, info.method, recSvc, info.path, http.StatusBadGateway)
			// The response body sent to the SANDBOX never includes err's own
			// text (codex review round 6 finding): a genuine transport
			// failure (DNS lookup, connection refused, TLS handshake) from
			// Go's net/http typically embeds the target host:port verbatim
			// in its error string (e.g. `dial tcp: lookup
			// myapp-internal.example.com: no such host`) — echoing that back
			// would leak base_url's real hostname to the sandbox, directly
			// contradicting docs/plans/api-gateway.md §1's "upstream の実
			// URL は sandbox からは見えない (見せる必要もない)" design
			// principle. The full error (including hostname) is still
			// logged server-side via slog.Warn above for operator
			// diagnosis — only the sandbox-facing response is generic.
			http.Error(w, "bad gateway: upstream request failed for service "+recSvc, http.StatusBadGateway)
		},
	}
	return s
}

// ServeHTTP parses the request path, authorizes it against the registry,
// and — if allowed — rewrites it to the configured service's base URL and
// proxies it with credentials injected. Unrecognized paths get 404; a
// well-formed path whose account qualifier fails validation gets 400 (docs/
// plans/api-gateway-credential-accounts.md D11 — see errInvalidAccount's own
// doc comment for why this is the one parsePath failure that is NOT a 404);
// unknown/expired tokens get 401; well-formed-but-disallowed service
// requests get 403; an allowed service configured with require_account:
// true (docs/plans/api-gateway-credential-accounts.md D5) rejects an
// account-less request with 400; a read-only token attempting a non-GET/HEAD
// method gets 403; an unconfigured (or credential-broken) service gets
// 502/503.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// EscapedPath, not r.URL.Path — see parsePath's own doc comment: net/http
	// decodes %2F into a literal "/" in .Path before a handler ever sees it,
	// which would silently merge two logically-distinct path segments.
	rt, err := parsePath(r.URL.EscapedPath())
	if err != nil {
		if errors.Is(err, errInvalidAccount) {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		http.NotFound(w, r)
		return
	}

	// A SINGLE Lookup — not Authorize (which does its own internal Lookup)
	// followed by a second, independent Lookup for the fields Authorize's
	// bool-returning signature doesn't expose (Namespace/TaskID/ReadOnly).
	// The two-call shape had a real (if narrow) race: Unregister — called
	// from Runner.UnregisterJob at job completion — could land in the
	// window between the two lookups, so the second one would silently
	// return a zero-value Entry (ReadOnly=false, Namespace="") instead of
	// erroring, while `allowed` had already been computed as true from the
	// FIRST (pre-race) lookup. That combination let a since-unregistered
	// token's request proceed using the STALE `allowed=true` verdict
	// together with a zeroed Entry: the read-only gate below
	// (`entry.ReadOnly && ...`) would silently evaluate to false — a
	// request that should have been rejected as a write from a read-only
	// token would instead reach the upstream — and credential resolution
	// would silently fall back to the "default" secret namespace instead
	// of the job's actual workspace-scoped one (codex review finding).
	//
	// Unlike internal/gitgateway.Server.ServeHTTP's own analogous two-Lookup
	// shape (whose doc comment argues the equivalent ABA race there degrades
	// SAFELY to default-namespace credentials — gitgateway's read/write
	// permission is entirely decided inside the single Authorize call, with
	// no separate boolean re-read afterward), this package added a genuinely
	// NEW post-lookup security gate (ReadOnly) that the race could bypass
	// outright, not merely mis-scope. A single Lookup, with allowed derived
	// from THIS SAME entry snapshot, closes the window entirely: Entry is
	// either the one live snapshot both decisions are made from, or the
	// token is correctly reported invalid.
	entry, tokenValid := s.registry.Lookup(rt.token)
	if !tokenValid {
		http.Error(w, "unauthorized: invalid or expired job token", http.StatusUnauthorized)
		return
	}
	// Authorization/BaseURL/readonly-write lookups all key on rt.service —
	// the BASE service name, account already stripped by parsePath (docs/
	// plans/api-gateway-credential-accounts.md D4): a workspace's enabled
	// services list ("freee", never "freee@ubs") governs every account of
	// that service uniformly. recSvc below is the "name@account" form (D8)
	// used ONLY for what the recorder/notifier/log lines and sandbox-facing
	// error text report, never for an authorization or config lookup.
	recSvc := recordedServiceName(rt.service, rt.account)
	allowed := entry.Services[rt.service]

	if !allowed {
		s.recorder(entry.TaskID, r.Method, recSvc, rt.path, http.StatusForbidden)
		http.Error(w, "forbidden: service not permitted for this job token", http.StatusForbidden)
		return
	}

	// ServiceConfig.RequireAccount (docs/plans/api-gateway-credential-
	// accounts.md D5) is checked here — AFTER the authorization gate above,
	// BEFORE every other gate below — deliberately, on both sides of that
	// placement:
	//
	//   - Not before authorization: doing so would leak, to a job token that
	//     cannot even reach this service (entry.Services[rt.service] ==
	//     false), whether the service happens to require an account at all.
	//     A job with no legitimate reason to know this service exists would
	//     learn a fact about its configuration. Checking after `allowed` is
	//     already established means this can only ever fire for a service
	//     the caller is genuinely permitted to use — the same reasoning
	//     that already governs which fields of entry/rt the recorder/error
	//     text below are allowed to mention.
	//   - Before the readonly-write gate (rather than after it): missing an
	//     account is a property of the REQUEST'S SHAPE — it applies
	//     identically to a GET and a POST — whereas the readonly-write gate
	//     is specifically about which HTTP METHODS a readonly token may use.
	//     Resolving the request-shape defect first means a readonly token's
	//     GET without an account to a require_account service gets the same
	//     400 a write request would (not a silent pass-through that only a
	//     later stage would have caught), and a request that supplies both
	//     defects (readonly token, unsafe method, AND no account) reports
	//     the more fundamental "this request cannot possibly succeed as
	//     written" defect rather than the narrower "this token cannot use
	//     this method" one.
	//
	// Every check from here down (BaseURLFor/Configured/Resolve) is either
	// insensitive to account presence or already documented as having no
	// fallback for a missing account-qualified credential (D3) — this gate
	// exists purely so the caller gets a targeted, actionable 400 instead of
	// discovering the same problem several stages later as a 502 from the
	// Resolve pre-check below.
	if rt.account == "" && s.credentials.RequiresAccount(rt.service) {
		s.recorder(entry.TaskID, r.Method, recSvc, rt.path, http.StatusBadRequest)
		http.Error(w, "bad request: service "+rt.service+" requires a credential-account qualifier — request \"/"+rt.service+"@<account>/...\" instead of \"/"+rt.service+"/...\"", http.StatusBadRequest)
		return
	}

	// ServiceConfig.AllowReadOnlyWrite (docs/plans/api-gateway.md §論点,
	// 2026-08-14 追加決定) is a daemon-config-only, per-service opt-out of
	// this gate — see that field's own doc comment for why it must never be
	// settable from project.yaml/task_behaviors.
	if entry.ReadOnly && !isSafeMethod(r.Method) && !s.credentials.AllowsReadOnlyWrite(rt.service) {
		s.recorder(entry.TaskID, r.Method, recSvc, rt.path, http.StatusForbidden)
		http.Error(w, "forbidden: read-only job token may only use GET/HEAD", http.StatusForbidden)
		return
	}

	baseURL, ok := s.credentials.BaseURLFor(rt.service)
	if !ok {
		s.recorder(entry.TaskID, r.Method, recSvc, rt.path, http.StatusBadGateway)
		http.Error(w, "bad gateway: service "+recSvc+" is not configured", http.StatusBadGateway)
		return
	}

	// Systemic "no secret resolver at all" case, distinct from an ordinary
	// per-key miss handled by the Resolve pre-check just below — mirrors
	// internal/gitgateway.Server.ServeHTTP's identical two-tier check.
	if !s.credentials.Configured() {
		s.recorder(entry.TaskID, r.Method, recSvc, rt.path, http.StatusServiceUnavailable)
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
	//
	// account is passed through unmodified (D3): there is no fallback to
	// the account-less credential when an account-qualified one is missing.
	if err := s.credentials.Resolve(entry.Namespace, rt.service, rt.account); err != nil {
		slog.Warn("apigateway: credential resolution failed; refusing to forward (fail-fast)",
			"service", recSvc, "namespace", entry.Namespace, "err", err)
		s.notifier.NotifyCredentialError(recSvc, err)
		s.recorder(entry.TaskID, r.Method, recSvc, rt.path, http.StatusBadGateway)
		http.Error(w,
			"bad gateway: api gateway credential resolution failed for service "+recSvc+": "+err.Error(),
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
	//
	// The string handed to url.Parse is prefixed with baseURL's own
	// "<scheme>://<host>" (codex review finding), not just basePath+rt.path
	// on their own: RFC 3986 treats a string beginning "//" as a
	// network-path reference — everything up to the next "/" becomes the
	// AUTHORITY, not path content. rt.path can legitimately begin with "//"
	// (e.g. a service configured with no base_url path suffix, requested as
	// ".../myapp//tenant/resource" — parsePath's traversal guard has no
	// opinion on a doubled leading slash, only on "."/".." segments), and
	// bare basePath+rt.path concatenation for such a service (basePath =="")
	// would parse "//tenant/resource" as Host="tenant", Path="/resource" —
	// silently swallowing the "tenant" segment entirely, since only
	// upstreamURL.Path/RawPath are read below (Scheme/Host on the outbound
	// request are set from info.baseURL directly, never from this parse).
	// Prefixing with the real scheme+host first means a leading "//" in
	// rt.path is never the first two characters of the string url.Parse
	// sees, so it can only ever land inside .Path/.RawPath.
	basePath := strings.TrimSuffix(baseURL.EscapedPath(), "/")
	upstreamURL, err := url.Parse(baseURL.Scheme + "://" + baseURL.Host + basePath + rt.path)
	if err != nil {
		// Defensive: rt.path was already validated by parsePath to contain
		// no malformed percent-encoding, and basePath comes from a
		// config.yaml base_url that internal/config already validated as a
		// parseable absolute URL — this should be unreachable in practice.
		s.recorder(entry.TaskID, r.Method, recSvc, rt.path, http.StatusBadGateway)
		http.Error(w, "bad gateway: could not construct upstream path: "+err.Error(), http.StatusBadGateway)
		return
	}

	ctx := context.WithValue(r.Context(), routeInfoKey{}, routeInfo{
		service:         rt.service,
		account:         rt.account,
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
