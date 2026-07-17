package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/api/auth"
)

// NormalizePublicURL parses raw as an HTTPS origin, strips path/query/fragment,
// lowercases and IPv6-brackets the host, and returns the canonical
// `https://host[:port]` form. It exists so wire.go can validate
// cfg.Web.PublicURL once at daemon startup and hand the canonical form to
// DeviceAuthHandler.PublicURL — the value that ends up in the POST
// /api/auth/device response, which the CLI (PR2) will byte-compare against
// its saved profile URL on every subsequent request. An un-normalized
// value (path, trailing slash, `HOST.EXAMPLE.COM`, plain http, empty host
// like `https://:443`) would either fail the CLI's origin-bind check
// later or, worse, get accepted and silently misroute future requests.
//
// This is also the exact same normalization applied to the request Host
// header fallback inside DeviceAuthHandler.canonicalURL — see the code
// path there for the "fallback must not bypass this validator" rule.
//
// Empty raw is not an error (PublicURL is optional at boot — the handler
// falls back to the request's Host header). A non-empty raw that fails
// validation is returned as an error so the operator sees the misconfig
// at startup instead of at the first pair attempt.
func NormalizePublicURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	// docs/plans/cli-remote-connection.md 決定事項 4 pins the whole remote
	// device-auth flow to https:// — plain http:// is unsupported (localhost
	// debug takes the unix socket instead), so a non-https public_url is a
	// misconfig, not a variant to accept.
	if u.Scheme != "https" {
		return "", fmt.Errorf("scheme %q, want https", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("userinfo not allowed in public_url")
	}
	// url.URL.Hostname/Port cannot be used to split u.Host: they treat
	// the LAST colon as the port separator, which mangles bracket-less
	// IPv6 literals like `2001:db8::1` (→ hostname="2001:db8:" port="1")
	// and multi-colon garbage like `example.com:80:443`
	// (→ hostname="example.com:80" port="443"). splitAuthority is the
	// strict version — it rejects both classes outright instead of
	// silently reshaping them into a bogus canonical origin.
	hostname, port, err := splitAuthority(u.Host)
	if err != nil {
		return "", err
	}
	hostname = strings.ToLower(hostname)
	if hostname == "" {
		// Catches "https://" alone, "https://:443", "https:///path", etc.
		return "", fmt.Errorf("missing host")
	}
	// url.Parse only rejects a non-digit port; a numeric out-of-range
	// port like "65536" or "0" passes through as a string, and
	// splitAuthority propagates it verbatim. Reject those here so the
	// canonical origin cannot promise a TCP endpoint that could never
	// exist.
	if port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("port %q out of range 1..65535", port)
		}
	}
	// A hostname that survived splitAuthority yet still contains a colon
	// can only be an IPv6 literal (there is no other legal form). Validate
	// it as one before re-bracketing — a malformed literal must not slip
	// through as a canonical origin.
	if strings.ContainsRune(hostname, ':') {
		ip := net.ParseIP(hostname)
		if ip == nil || ip.To4() != nil {
			return "", fmt.Errorf("host %q is not a valid IPv6 literal", hostname)
		}
		hostname = "[" + hostname + "]"
	}
	hostport := hostname
	if port != "" {
		hostport += ":" + port
	}
	normalized := &url.URL{Scheme: "https", Host: hostport}
	return normalized.String(), nil
}

// splitAuthority splits an HTTP authority (`host`, `host:port`, `[ipv6]`,
// `[ipv6]:port`) into hostname and port. It reports an error for
// ambiguous forms that url.URL silently mangles — bracket-less IPv6
// (`2001:db8::1`) and multi-colon garbage (`example.com:80:443`). Port
// is returned as-is (no numeric validation — url.Parse already rejected
// non-digit ports upstream; range validation happens in the caller).
func splitAuthority(authority string) (host, port string, err error) {
	if h, p, e := net.SplitHostPort(authority); e == nil {
		return h, p, nil
	}
	// SplitHostPort failed — decide whether the input is a legal
	// port-less form or genuine malformed garbage.
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		// Bracketed IPv6 with no port, e.g. "[::1]".
		return authority[1 : len(authority)-1], "", nil
	}
	if strings.ContainsRune(authority, ':') {
		// Unbracketed colons + no successful split = ambiguous.
		return "", "", fmt.Errorf("authority %q: ambiguous host:port", authority)
	}
	// Plain DNS name with no port.
	return authority, "", nil
}

// peerIPForPublicEndpoint returns the TCP peer IP for use as a rate-limit
// bucket key on unauthenticated endpoints reachable over the TCP listener
// (POST /api/auth/device today). It intentionally does NOT consult
// forwarded headers like CF-Connecting-IP / X-Forwarded-For — those are
// caller-controlled bytes on any request that does NOT arrive via a
// trusted reverse proxy, and honoring them here would let a directly
// connected attacker rotate the header per request and drain no bucket
// at all.
//
// This is deliberately a stricter policy than internal/api/web.go's
// remoteIP: the latter opts into fairness (per-real-client buckets when
// behind Cloudflare, at the cost of spoofability on direct connections)
// for the cookie-based /login and /auth flows. Bearer device pairing is
// stricter because it is the token-issuance boundary — a spoofable rate
// limit here would render the "public, rate-limited" contract in
// docs/plans/cli-remote-connection.md PR0 effectively fictitious.
// Introducing trusted-proxy CIDR config so remoteIP itself becomes
// spoof-resistant across all public endpoints is tracked as an unresolved
// point in the plan doc; this helper is the endpoint-scoped fix for PR0.
func peerIPForPublicEndpoint(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// deviceAuthStore is the persistence surface DeviceAuthHandler needs: mint a
// Bearer-authenticated device row and revoke one. Satisfied by *auth.Store.
type deviceAuthStore interface {
	InsertDeviceToken(ctx context.Context, id, label string, tokenHash []byte) error
	RevokeDevice(ctx context.Context, id string) error
}

// DeviceAuthHandler serves the Bearer device-auth endpoints used by remote
// CLI clients (docs/plans/cli-remote-connection.md Phase 3 PR0):
//
//   - POST   /api/auth/device       — public, rate-limited. Redeems a
//     pairing code (the same one `boid web pair` issues for the cookie
//     flow) for a long-lived Bearer device token.
//   - DELETE /api/auth/devices/{id} — Bearer-authenticated, self-revoke
//     only.
//
// It is deliberately a separate surface from WebManagementHandler (mounted
// at /api/web/*, UNIX-socket-only, trusted local-admin surface with no
// ownership check: /api/web/pair, /api/web/devices) — this handler is
// reachable over TCP by a caller who starts out holding nothing but a valid
// pairing code, so PostDevice carries its own rate limiting and DeleteDevice
// enforces "only revoke yourself".
type DeviceAuthHandler struct {
	Pairing   loginPairing
	Store     deviceAuthStore
	Limiter   loginRateLimiter
	Registry  *auth.ConnectionRegistry
	PublicURL string
}

func (h *DeviceAuthHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/device", h.PostDevice)
	r.Delete("/devices/{id}", h.DeleteDevice)
	return r
}

type deviceAuthRequest struct {
	Code       string `json:"code"`
	DeviceName string `json:"device_name,omitempty"`
}

type deviceAuthResponse struct {
	DeviceID     string `json:"device_id"`
	Token        string `json:"token"`
	CanonicalURL string `json:"canonical_url,omitempty"`
}

// deviceAuthMaxBodyBytes caps the JSON body size for POST /api/auth/device.
// The endpoint is unauthenticated so anyone reachable to the TCP listener
// can hit it; without a cap, an attacker could exhaust memory by pumping a
// giant "code" string past the rate-limiter (which only counts REDEEM
// failures, not decode failures). 4 KiB is ~200x the largest legitimate
// body (an 8-char code + a 256-char device_name + json framing) yet stays
// small enough that even a fully-consumed request costs nothing.
const deviceAuthMaxBodyBytes = 4 * 1024

// deviceAuthMaxCodeLen is a defence-in-depth cap on the pairing code field
// itself. GeneratePairingCode emits 9 chars ("XXXX-XXXX"); anything an
// order of magnitude larger cannot be a real code, so reject cheaply
// without touching the pairing store.
const deviceAuthMaxCodeLen = 64

// deviceAuthMaxDeviceNameLen is a defence-in-depth cap on the caller-supplied
// device_name label. It ends up in web_devices.label (a human-readable
// identifier), so anything longer than a few hundred bytes is either
// abusive or a client bug.
const deviceAuthMaxDeviceNameLen = 256

// PostDevice redeems a one-time pairing code (auth.PairingManager — the same
// manager instance the cookie-based /login and /auth flows share) for a
// long-lived Bearer device token. Unlike those flows, no session cookie is
// ever set here: the raw token is returned exactly once, in the response
// body, and only its SHA-256 hash (auth.HashToken) is ever persisted
// (Store.InsertDeviceToken).
func (h *DeviceAuthHandler) PostDevice(w http.ResponseWriter, r *http.Request) {
	// Use the spoof-resistant peer IP (NOT remoteIP) so header rotation
	// cannot bypass the rate limit on this token-issuance endpoint. See
	// peerIPForPublicEndpoint's godoc for why this diverges from the
	// cookie-based /login and /auth flows.
	ip := peerIPForPublicEndpoint(r)
	if h.Limiter != nil && !h.Limiter.Allowed(ip) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	// Resolve the daemon's externally-reachable origin BEFORE consuming
	// the pairing code: if we cannot produce a canonical_url (no
	// web.public_url configured AND no Host header on the request), the
	// CLI's origin-bind check in PR2 has nothing to persist and the whole
	// login would be pointless. Fail loudly with the pairing code still
	// unspent so the operator can retry after fixing the configuration.
	canonicalURL := h.canonicalURL(r)
	if canonicalURL == "" {
		http.Error(w, "canonical URL unavailable", http.StatusInternalServerError)
		return
	}

	if r.Body == nil {
		writeDeviceAuthError(w, http.StatusBadRequest, "request body required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, deviceAuthMaxBodyBytes)
	var req deviceAuthRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeDeviceAuthError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// json.Decoder consumes only the first top-level JSON value; anything
	// after it (a second value, trailing garbage, another object) would be
	// silently ignored, bypassing DisallowUnknownFields and the size cap
	// on the same request. Reject those explicitly.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeDeviceAuthError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeDeviceAuthError(w, http.StatusBadRequest, "code is required")
		return
	}
	if len(code) > deviceAuthMaxCodeLen {
		writeDeviceAuthError(w, http.StatusBadRequest, "code too long")
		return
	}
	if len(req.DeviceName) > deviceAuthMaxDeviceNameLen {
		writeDeviceAuthError(w, http.StatusBadRequest, "device_name too long")
		return
	}

	// Generate the token BEFORE consuming the pairing code — a crypto/rand
	// failure at this point must not burn the code (5-minute single-use
	// codes are precious; forcing the operator to run `boid web pair` again
	// on a server-side RNG hiccup is a bad trade). This ordering means the
	// only remaining "code consumed but device not created" window is a
	// SQLite failure between Redeem and InsertDeviceToken — SQLite Exec on
	// a healthy filesystem is essentially never the failure mode here, so
	// making the code-consume + device-insert a single tx would be more
	// mechanism than the residual risk warrants.
	token, err := auth.GenerateDeviceToken()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	label, err := h.Pairing.Redeem(r.Context(), code)
	if err != nil {
		// Rate-limit failure-tracking is meant to slow down code-guessing
		// attackers, so only count the "the code was wrong" sentinels; a
		// database/IO failure inside Redeem is our problem, not the
		// caller's, and must not be double-punished with a 401 or a rate
		// limit strike.
		if errors.Is(err, auth.ErrCodeNotFound) ||
			errors.Is(err, auth.ErrCodeExpired) ||
			errors.Is(err, auth.ErrCodeConsumed) {
			if h.Limiter != nil {
				h.Limiter.RecordFailure(ip)
			}
			writeDeviceAuthError(w, http.StatusUnauthorized, "invalid or expired pairing code")
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	deviceName := strings.TrimSpace(req.DeviceName)
	if deviceName == "" {
		deviceName = label
	}

	deviceID := uuid.New().String()
	if err := h.Store.InsertDeviceToken(r.Context(), deviceID, deviceName, auth.HashToken(token)); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(deviceAuthResponse{ // best-effort; client may have disconnected
		DeviceID:     deviceID,
		Token:        token,
		CanonicalURL: canonicalURL,
	})
}

// canonicalURL resolves the daemon's externally-reachable origin, echoed
// back so the CLI's `boid login` (PR2) can bind the saved token to the URL
// it actually paired against (docs/plans/cli-remote-connection.md 決定事項
// 9: cross-origin token reuse is a hard error at request time, not a
// warning — this response field is what a future PR2 persists for that
// check).
//
// Precedence:
//
//   - `web.public_url` (already validated + normalized by NormalizePublicURL
//     at wire.go startup — this branch trusts the stored value verbatim)
//   - request Host header, wrapped as `https://<host>` and put through the
//     SAME NormalizePublicURL validator so a garbage Host (`::garbage`,
//     bare port, uppercase, missing) is rejected exactly as a misconfigured
//     `public_url` would be. Skipping validation here would let a caller
//     induce arbitrary canonical_url values by spoofing the Host header,
//     defeating the CLI-side origin-bind check.
//
// Returns "" if neither source produces a valid canonical URL. The caller
// (PostDevice) treats "" as a hard 500 so no pairing code is consumed
// against an unbindable response.
func (h *DeviceAuthHandler) canonicalURL(r *http.Request) string {
	if h.PublicURL != "" {
		return h.PublicURL
	}
	if r.Host == "" {
		return ""
	}
	normalized, err := NormalizePublicURL("https://" + r.Host)
	if err != nil {
		return ""
	}
	return normalized
}

// DeleteDevice revokes the caller's own device only, and only when the
// caller authenticated via Bearer token. The {id} path param must match
// the Bearer-authenticated device ID the auth middleware placed in the
// request context (auth.DeviceIDFromContext + auth.AuthMethodFromContext).
//
// The auth-method check exists because a session-cookie caller has other,
// richer paths for managing their own devices (the WebManagementHandler
// /api/web/devices UNIX-socket surface used by `boid web revoke`) — this
// endpoint is explicitly the *Bearer* self-revoke path used by
// `boid logout`, and mixing the two auth models on the same route would
// muddle the responsibility split.
//
// This is intentionally stricter than WebManagementHandler.DeleteDevice
// (UNIX-socket-only local admin, no ownership check) — this endpoint is
// reachable by any Bearer-holding remote caller, so it must not let one
// device revoke another's token.
func (h *DeviceAuthHandler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	method, methodOK := auth.AuthMethodFromContext(r.Context())
	callerID, idOK := auth.DeviceIDFromContext(r.Context())
	if !methodOK || method != auth.AuthMethodBearer || !idOK || callerID != id {
		writeDeviceAuthError(w, http.StatusForbidden, "can only revoke your own device")
		return
	}
	if err := h.Store.RevokeDevice(r.Context(), id); err != nil {
		if errors.Is(err, auth.ErrDeviceNotFound) {
			writeDeviceAuthError(w, http.StatusNotFound, "device not found")
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if h.Registry != nil {
		h.Registry.RevokeDevice(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeDeviceAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
