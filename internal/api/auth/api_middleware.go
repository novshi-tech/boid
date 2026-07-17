package auth

import (
	"encoding/json"
	"net/http"
	"strings"
)

// NewTCPAPIAuthMiddleware guards the data/control JSON API when it is reached
// over the (potentially externally-exposed) TCP listener.
//
// The UNIX socket is trusted: it is filesystem-permission protected and only
// reachable by the local user (CLI and in-sandbox agents dial it via
// BOID_SOCKET). It is served by the bare router WITHOUT this middleware, so
// CLI/agent access is never gated. This middleware is applied ONLY to the TCP
// listener's handler.
//
// Over TCP, every /api/* path except the public ones (see apiAuthRequired)
// requires either a valid Bearer device token or a valid session cookie
// (docs/plans/cli-remote-connection.md Phase 3 PR0). An
// `Authorization: Bearer <token>` header, when present, is a hard commitment
// to that path: success or failure is decided on the Bearer token alone,
// with no fallback to the cookie check below even if a valid session cookie
// is also attached to the same request. This keeps the two paths'
// pass/fail semantics independent and makes the priority unambiguous
// (Bearer over cookie) rather than needing to reconcile which one "wins"
// when both are present. When no Bearer header is present at all, the logic
// below is byte-for-byte the pre-PR0 cookie/bootstrap behavior — see 決定事項
// 「既存 cookie 経路は 100% 温存」.
//
// The loopback-bootstrap exemption (no cookie + genuine loopback + zero
// registered devices) is preserved so the Web UI is usable before the first
// device is paired. Requests that arrived via a reverse proxy / tunnel
// (IsLoopback==false because of proxy headers) get no bootstrap and must
// authenticate.
//
// Failures return 401 JSON (not a 302 redirect to /login) because callers here
// are API clients. Non-/api paths (HTML pages, /login, /auth, /static) fall
// through to the router, which applies its own WebAuthMiddleware.
func NewTCPAPIAuthMiddleware(signer *SessionSigner, store *Store) func(http.Handler) http.Handler {
	var bearer *BearerVerifier
	if store != nil {
		bearer = NewBearerVerifier(store)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !apiAuthRequired(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			if _, present, _ := ExtractBearerToken(r); present {
				// Once an Authorization: Bearer header is present in ANY
				// form (case-insensitive scheme, malformed, empty, etc.),
				// this request has committed to the Bearer path. Do NOT
				// fall through to cookie auth below — a client that meant
				// to authenticate as Bearer device A must never be
				// silently authenticated as cookie device B just because
				// its Bearer header happens to be malformed. See
				// docs/plans/cli-remote-connection.md PR0 codex review.
				if bearer == nil {
					writeAPIUnauthorized(w)
					return
				}
				deviceID, err := bearer.Verify(r)
				if err != nil {
					writeAPIUnauthorized(w)
					return
				}
				ctx := WithDeviceID(r.Context(), deviceID)
				ctx = WithAuthMethod(ctx, AuthMethodBearer)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			if _, err := r.Cookie(cookieName); err != nil {
				// No session cookie. Allow only the pre-pairing bootstrap
				// window (a genuine local browser with no devices yet); reject
				// everything else — including proxied/tunneled requests, which
				// IsLoopback already rejects — with 401.
				if IsLoopback(r) && store != nil {
					if has, dbErr := store.HasAnyDevice(r.Context()); dbErr == nil && !has {
						next.ServeHTTP(w, r)
						return
					}
				}
				writeAPIUnauthorized(w)
				return
			}

			if signer == nil {
				writeAPIUnauthorized(w)
				return
			}
			deviceID, err := signer.Verify(r)
			if err != nil {
				writeAPIUnauthorized(w)
				return
			}

			ctx := WithDeviceID(r.Context(), deviceID)
			ctx = WithAuthMethod(ctx, AuthMethodCookie)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// apiAuthRequired reports whether the path is a data/control API endpoint that
// must be authenticated over the TCP transport.
//
//   - /api/health is intentionally public (liveness probe / tunnel health).
//   - /api/auth/device is intentionally public (docs/plans/cli-remote-connection.md
//     Phase 3 PR0): it is the Bearer-token-issuing endpoint itself — a caller
//     with no token yet redeems a one-time pairing code there to get one. It
//     carries its own rate limiting (DeviceAuthHandler.PostDevice) since it
//     is reachable unauthenticated. Only chi's POST route exists at this
//     exact path, so exempting the path exempts only that method in
//     practice — matching every other exemption in this function, which is
//     path-based, not method-based.
//   - Non-/api paths (HTML, /login, /auth, /static) are not gated here; the
//     router's own WebAuthMiddleware handles them.
func apiAuthRequired(path string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	switch path {
	case "/api/health", "/api/auth/device":
		return false
	}
	return true
}

func writeAPIUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
}
