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
// requires a valid session cookie. The loopback-bootstrap exemption
// (no cookie + genuine loopback + zero registered devices) is preserved so the
// Web UI is usable before the first device is paired. Requests that arrived via
// a reverse proxy / tunnel (IsLoopback==false because of proxy headers) get no
// bootstrap and must authenticate.
//
// Failures return 401 JSON (not a 302 redirect to /login) because callers here
// are API clients. Non-/api paths (HTML pages, /login, /auth, /static) fall
// through to the router, which applies its own WebAuthMiddleware.
func NewTCPAPIAuthMiddleware(signer *SessionSigner, store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !apiAuthRequired(r.URL.Path) {
				next.ServeHTTP(w, r)
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

			next.ServeHTTP(w, r.WithContext(WithDeviceID(r.Context(), deviceID)))
		})
	}
}

// apiAuthRequired reports whether the path is a data/control API endpoint that
// must be authenticated over the TCP transport.
//
//   - /api/health is intentionally public (liveness probe / tunnel health).
//   - Non-/api paths (HTML, /login, /auth, /static) are not gated here; the
//     router's own WebAuthMiddleware handles them.
func apiAuthRequired(path string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	switch path {
	case "/api/health":
		return false
	}
	return true
}

func writeAPIUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
}
