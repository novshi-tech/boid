package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DeviceTokenPrefix marks every Bearer device token minted by
// POST /api/auth/device (docs/plans/cli-remote-connection.md Phase 3 PR0
// 決定事項 8). It carries no security weight on its own — the token's
// entropy is the random bytes that follow — it exists purely so a token is
// recognizable at a glance (logs, `boid login` prompts) and distinguishable
// from a session cookie value or a pairing code.
const DeviceTokenPrefix = "boid_pat_"

// deviceTokenRandBytes is the amount of crypto/rand entropy encoded into
// every device token, matching the plan doc's "32 byte crypto/rand" decision.
const deviceTokenRandBytes = 32

// GenerateDeviceToken returns a new raw Bearer device token:
// DeviceTokenPrefix followed by 32 bytes of crypto/rand, URL-safe base64
// (no padding) encoded. The raw token is handed back to the caller exactly
// once (the POST /api/auth/device response body) — only its HashToken hash
// is ever persisted (web_devices.token_hash).
func GenerateDeviceToken() (string, error) {
	buf := make([]byte, deviceTokenRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate device token: %w", err)
	}
	return DeviceTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken returns the SHA-256 hash of a raw Bearer token — the value
// stored in web_devices.token_hash. Mirrors HashCode's role for pairing
// codes (pairing.go): the raw secret never touches the database, only its
// hash does.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// bearerScheme is the RFC 6750 Bearer auth scheme name. HTTP auth schemes
// are case-insensitive (RFC 7235 §2.1), so we compare with strings.EqualFold
// rather than a fixed-case prefix — the previous "Bearer " prefix check
// silently mis-classified `bearer <tok>` / `BEARER <tok>` etc. as "no
// Authorization scheme", which then let the request fall through to cookie
// auth with a different device identity. See docs/plans/cli-remote-connection.md
// PR0 codex review.
const bearerScheme = "Bearer"
const authorizationHeader = "Authorization"

// ExtractBearerToken parses an `Authorization: Bearer <token>` header on r.
// Returns:
//
//   - token, true, true — a Bearer scheme header carrying a syntactically
//     usable token (non-empty after trimming leading whitespace);
//   - "", true, false — a Bearer scheme header that is malformed (missing
//     token part, empty after trimming, etc.);
//   - "", false, false — no Authorization header, or a header using a
//     different scheme (Basic, Digest, …).
//
// The three-way return exists so callers (NewTCPAPIAuthMiddleware,
// WSAttachHandler.authenticateDevice) can implement the plan doc's PR0
// rule "an Authorization: Bearer header, when present, is a hard commitment
// to the Bearer path" — a present-but-malformed Bearer header must fail
// authentication outright, NOT silently fall back to cookie auth (which
// could resolve to a different device identity than the caller intended).
// Scheme matching is case-insensitive.
func ExtractBearerToken(r *http.Request) (token string, present bool, ok bool) {
	// A single request MUST NOT carry more than one Authorization header —
	// with multiple values `Header.Get` would silently prefer the first
	// (as when a client sends `Basic ...` followed by `Bearer ...`), which
	// would let a caller sneak a valid cookie-flavored value past the
	// Bearer-scheme check by placing a non-Bearer value first. Treat any
	// multi-value case as an explicitly present-but-invalid Bearer request
	// so the middleware hard-fails without falling back to cookie auth.
	values := r.Header.Values(authorizationHeader)
	if len(values) > 1 {
		return "", true, false
	}
	h := ""
	if len(values) == 1 {
		h = values[0]
	}
	if h == "" {
		return "", false, false
	}
	sepIdx := strings.IndexAny(h, " \t")
	if sepIdx < 0 {
		// Single-token header like "Bearer" alone — if that single token
		// is the scheme name itself (case-insensitive), treat it as a
		// malformed Bearer header (present, not ok) so the caller
		// hard-fails instead of silently falling through to cookie auth.
		if strings.EqualFold(h, bearerScheme) {
			return "", true, false
		}
		return "", false, false
	}
	scheme := h[:sepIdx]
	rest := h[sepIdx+1:]
	if !strings.EqualFold(scheme, bearerScheme) {
		return "", false, false
	}
	token = strings.TrimLeft(rest, " \t")
	if token == "" {
		return "", true, false
	}
	return token, true, true
}

// BearerVerifier validates a raw Bearer device token against web_devices —
// the Authorization-header counterpart to SessionSigner's cookie
// verification. Both converge on the same device identity model (a
// web_devices row, revocable via Store.RevokeDevice); see
// docs/plans/cli-remote-connection.md 決定事項: 「既存 web_devices テーブル
// を拡張 (別テーブル案は却下)」.
type BearerVerifier struct {
	store *Store
}

// NewBearerVerifier builds a BearerVerifier backed by store. store must not
// be nil.
func NewBearerVerifier(store *Store) *BearerVerifier {
	return &BearerVerifier{store: store}
}

// Verify extracts and hashes the Bearer token from r, looks up the owning
// device (revoked_at IS NULL only — Store.GetDeviceByTokenHash), and on
// success updates its last_seen_at exactly as SessionSigner.Verify does for
// cookies. Every failure mode (missing header, malformed header, unknown
// hash, revoked device) collapses to ErrInvalidSession so callers cannot
// distinguish "no token" from "bad token" from the error alone — the same
// posture SessionSigner.Verify takes for cookies.
func (v *BearerVerifier) Verify(r *http.Request) (string, error) {
	token, _, ok := ExtractBearerToken(r)
	if !ok {
		return "", ErrInvalidSession
	}

	device, err := v.store.GetDeviceByTokenHash(r.Context(), HashToken(token))
	if err != nil {
		return "", fmt.Errorf("get device by token: %w", err)
	}
	if device == nil {
		return "", ErrInvalidSession
	}

	if err := v.store.UpdateDeviceLastSeen(r.Context(), device.ID, time.Now()); err != nil {
		return "", fmt.Errorf("update last seen: %w", err)
	}

	return device.ID, nil
}
