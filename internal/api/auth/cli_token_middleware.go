package auth

import (
	"crypto/subtle"
	"net/http"
)

// NewCLITokenAuthMiddleware guards the daemon's dedicated CLI TCP listener
// (internal/server's Config.CLIAddr/CLIToken — PR-3 Option 4 host-mode
// redesign, docs/plans/volume-only-daemon.md §論点c, nose directive
// 2026-07-25). Unlike NewTCPAPIAuthMiddleware (the Web UI's own TCP
// listener: session cookie / paired-device Bearer token / a pre-first-
// pairing loopback bootstrap window), this listener has exactly one valid
// credential and no bootstrap exemption at all: a same-host `boid` CLI
// process running in host mode (cmd/host.go) has ALREADY generated or read
// the shared secret from ~/.config/boid/cli-token, and passed the
// identical value to the daemon container as the BOID_CLI_TOKEN env var
// (build/container/compose.yml), before it ever dials this listener — so
// there is no "not paired yet" state to make room for, and (per Option 4's
// own design) no TLS and no loopback-trust fallback on this listener
// either (see cmd/host.go's own doc comment for why host mode makes both
// unnecessary: it owns the daemon container's entire lifecycle already).
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
			next.ServeHTTP(w, r)
		})
	}
}

// cliTokenEqual compares two tokens in constant time, guarding the
// zero-length case explicitly: subtle.ConstantTimeCompare returns 0
// (unequal) for mismatched lengths WITHOUT a timing leak on the length
// check itself, but two empty slices would compare "equal" — never
// reachable in practice here (NewCLITokenAuthMiddleware's own caller
// already rejects token=="" before this is invoked), guarded anyway so
// this helper is correct standalone, not just correct-by-caller-contract.
func cliTokenEqual(a, b string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
