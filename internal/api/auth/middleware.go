package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

type contextKey string

const (
	deviceIDCtxKey   contextKey = "deviceID"
	authMethodCtxKey contextKey = "authMethod"
)

// AuthMethod is the auth path a request came through, recorded in the
// context by NewTCPAPIAuthMiddleware and NewWebAuthMiddleware so handlers
// can enforce method-specific policy (e.g. DeviceAuthHandler.DeleteDevice
// only permits self-revoke for AuthMethodBearer callers).
type AuthMethod string

const (
	AuthMethodBearer AuthMethod = "bearer"
	AuthMethodCookie AuthMethod = "cookie"
)

// WithDeviceID returns ctx with deviceID embedded. Used by NewWebAuthMiddleware
// and in tests to inject a device identity without running the full middleware.
func WithDeviceID(ctx context.Context, deviceID string) context.Context {
	return context.WithValue(ctx, deviceIDCtxKey, deviceID)
}

// DeviceIDFromContext returns the authenticated device ID stored in ctx by
// NewWebAuthMiddleware. Returns ("", false) for unauthenticated requests.
func DeviceIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(deviceIDCtxKey).(string)
	return id, ok && id != ""
}

// WithAuthMethod returns ctx tagged with the auth method used to authenticate
// this request. Called by NewTCPAPIAuthMiddleware (bearer or cookie branch)
// and NewWebAuthMiddleware (cookie branch); handlers read it via
// AuthMethodFromContext.
func WithAuthMethod(ctx context.Context, method AuthMethod) context.Context {
	return context.WithValue(ctx, authMethodCtxKey, method)
}

// AuthMethodFromContext returns the AuthMethod tag stored in ctx by one of
// the auth middlewares. Returns ("", false) for unauthenticated requests or
// for authenticated requests that ran through a middleware that predates
// the tag (bootstrap loopback path — no device identity either).
func AuthMethodFromContext(ctx context.Context) (AuthMethod, bool) {
	m, ok := ctx.Value(authMethodCtxKey).(AuthMethod)
	return m, ok && m != ""
}

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
//	valid cookie                                 → pass (Verify updates last_seen, deviceID stored in ctx)
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
			deviceID, err := signer.Verify(r)
			if err != nil {
				signer.Clear(w)
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}

			ctx := WithDeviceID(r.Context(), deviceID)
			ctx = WithAuthMethod(ctx, AuthMethodCookie)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func webAuthExempt(path string) bool {
	return path == "/login" ||
		strings.HasPrefix(path, "/auth") ||
		strings.HasPrefix(path, "/static/")
}
