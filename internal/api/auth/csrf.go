package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	csrfCookieName = "csrf_token"
	csrfHeaderName = "X-CSRF-Token"
)

// CSRFMiddleware implements double-submit cookie CSRF protection.
//
// GET/HEAD/OPTIONS/TRACE: issues csrf_token cookie if absent, then passes.
// POST/PUT/PATCH/DELETE: compares X-CSRF-Token header with cookie; 403 on mismatch.
//
// Exempt paths:
//   - /auth and /auth/* (protected by one-time pairing token)
//   - /api/* (programmatic CLI access via UNIX socket)
func CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if csrfExempt(r) {
			next.ServeHTTP(w, r)
			return
		}

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
			if _, err := r.Cookie(csrfCookieName); err != nil {
				http.SetCookie(w, &http.Cookie{
					Name:     csrfCookieName,
					Value:    generateCSRFToken(),
					Path:     "/",
					Secure:   true,
					SameSite: http.SameSiteStrictMode,
				})
			}
			next.ServeHTTP(w, r)

		default:
			cookie, err := r.Cookie(csrfCookieName)
			if err != nil || cookie.Value == "" {
				http.Error(w, "CSRF token missing", http.StatusForbidden)
				return
			}
			if r.Header.Get(csrfHeaderName) != cookie.Value {
				http.Error(w, "CSRF token mismatch", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}

func csrfExempt(r *http.Request) bool {
	p := r.URL.Path
	return p == "/auth" || strings.HasPrefix(p, "/auth/") || strings.HasPrefix(p, "/api/")
}

func generateCSRFToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
