package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// NewCLITokenAuthMiddleware guards the daemon's dedicated CLI TCP listener
// (internal/server's Config.CLIAddr/CLIToken). Unlike NewTCPAPIAuthMiddleware
// (the Web UI's own TCP listener: session cookie / paired-device Bearer
// token / a pre-first-pairing loopback bootstrap window), this listener
// has exactly one valid credential and no bootstrap exemption at all: a
// same-host `boid` CLI process running in host mode (cmd/host.go) has
// ALREADY generated or read the shared secret from
// ~/.config/boid/cli-token, and passed the identical value to the daemon
// container as the BOID_CLI_TOKEN env var (build/container/compose.yml),
// before it ever dials this listener — so there is no "not paired yet"
// state to make room for, and no TLS and no loopback-trust fallback on
// this listener either (see cmd/host.go's own doc comment for why host
// mode makes both unnecessary: it owns the daemon container's entire
// lifecycle already).
//
// token=="" (BOID_CLI_TOKEN unset at daemon startup — cmd/start.go's own
// doc comment covers when this is or isn't a misconfiguration worth
// warning about) rejects every request unconditionally: an unset token
// must never silently degrade to an open, unauthenticated TCP listener.
//
// The comparison is constant-time (crypto/subtle.ConstantTimeCompare) —
// unlike a paired device's token (compared via its SHA-256 hash,
// bearer_verifier.go's own HashToken), BOID_CLI_TOKEN is compared to its
// own raw value directly (there is exactly one shared secret, not a set of
// per-device hashed rows to look up), so equal-time comparison is this
// middleware's own responsibility rather than falling out of a hash-then-
// lookup design the way the device-token path's does.
func NewCLITokenAuthMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !apiAuthRequired(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			got, present, ok := ExtractBearerToken(r)
			if !present || !ok || token == "" || !cliTokenEqual(got, token) {
				writeAPIUnauthorized(w)
				return
			}
			// Mark the request as CLI-token-authenticated (see
			// WithCLITokenAuthenticated's own doc comment): downstream
			// handlers sharing this router with the
			// UNIX socket / Web UI TCP listener (e.g. WSAttachHandler) must
			// not re-verify this same Bearer header as a device-pair token
			// — it never was one.
			next.ServeHTTP(w, r.WithContext(WithCLITokenAuthenticated(r.Context())))
		})
	}
}

// cliTokenEqual compares two tokens in constant time. Both are hashed
// (SHA-256) before the comparison so
// subtle.ConstantTimeCompare always runs over two fixed-length 32-byte
// digests, regardless of a/b's own lengths: subtle.ConstantTimeCompare
// itself returns 0 immediately, WITHOUT running its constant-time loop at
// all, whenever the two inputs' lengths differ — comparing raw token bytes
// directly (the previous implementation) meant a mismatched-length probe
// short-circuited in a way that is, in principle, distinguishable in time
// from a same-length mismatch. In practice this was low-impact (generated
// tokens — loadOrCreateCLIToken — are always exactly 64 hex chars, so a
// genuine token is never a different length from an attacker's guess
// unless the guess is already wrong on length alone, which leaks nothing
// interesting), but hashing first makes the comparison satisfy the
// constant-time claim unconditionally rather than relying on that
// generation-time invariant.
//
// The zero-length guard stays explicit: two empty slices would hash equal
// (sha256("") == sha256("")) — never reachable in practice
// (NewCLITokenAuthMiddleware's own caller already rejects token=="" before
// this is invoked), guarded anyway so this helper is correct standalone,
// not just correct-by-caller-contract.
func cliTokenEqual(a, b string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}
