package auth

import (
	"log/slog"
	"net/http"
	"strings"
)

// NewWebAuthMiddleware returns middleware that enforces session cookie auth for
// web UI routes.
//
// Exempt paths (pass through without any check): /login, /auth*, /static/*.
//
// For non-exempt paths the logic is:
//
//	no cookie + loopback + no devices registered → warn + pass (bootstrap mode)
//	no cookie + anything else                    → 302 /login
//	invalid cookie                               → clear cookie + 302 /login
//	valid cookie                                 → pass (Verify updates last_seen)
func NewWebAuthMiddleware(signer *SessionSigner, store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if webAuthExempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			_, err := r.Cookie(cookieName)
			if err != nil {
				// No session cookie present.
				if IsLoopback(r) {
					has, dbErr := store.HasAnyDevice(r.Context())
					if dbErr == nil && !has {
						slog.Warn("web UI accessed without authentication; run 'boid web pair' to set up a device",
							"remote", r.RemoteAddr)
						next.ServeHTTP(w, r)
						return
					}
				}
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}

			// Cookie present — verify it.
			if signer == nil {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			if _, err := signer.Verify(r); err != nil {
				signer.Clear(w)
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func webAuthExempt(path string) bool {
	return path == "/login" ||
		strings.HasPrefix(path, "/auth") ||
		strings.HasPrefix(path, "/static/")
}
