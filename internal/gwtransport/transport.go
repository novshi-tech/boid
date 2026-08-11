// Package gwtransport builds the outbound (gateway → upstream)
// http.Transport shared by internal/gitgateway and internal/apigateway.
//
// Both gateways are httputil.ReverseProxy wrappers, and both originally
// constructed their outbound Transport as an http.Transport literal with a
// single field set (ExpectContinueTimeout) and every other field left at
// its zero value. That "zero values == streaming semantics" reasoning is
// correct for the fields that govern body handling, but it silently opted
// out of two connection-liveness settings, and the combination wedges a
// gateway indefinitely against an upstream that stops answering without
// closing its TCP connection:
//
//   - IdleConnTimeout zero means "keep idle pooled connections forever".
//     http.DefaultTransport uses 90s. A connection whose peer has silently
//     gone away (a fly.io machine auto-stopped, a rootless-podman NAT
//     conntrack entry expired, a load balancer dropped its half) still
//     looks ESTABLISHED to the local kernel, so it is handed back out of
//     the pool to every subsequent request.
//
//   - HTTP/2 is enabled implicitly on both gateways (net/http auto-upgrades
//     a Transport whose TLSClientConfig and Dial hooks are all nil), but
//     nothing configured the HTTP/2 health check, and http.HTTP2Config's
//     own docs are explicit that a zero SendPingTimeout means "no health
//     check is performed". So a half-dead h2 connection is never detected
//     — and unlike HTTP/1.1, where a canceled request tears the connection
//     down and the next request redials, an h2 connection survives its
//     canceled streams and keeps absorbing new ones. Every request to that
//     upstream then hangs until its own caller gives up.
//
// This was observed in production on 2026-08-11: API gateway requests to
// one service (a fly.io-hosted upstream) timed out for ~10 minutes while
// the same URL answered in 40ms via curl from the very same daemon
// container, and other services on other upstream hosts were unaffected
// (separate connection pools). The daemon's /proc/net/tcp showed a single
// ESTABLISHED socket to the upstream with empty send/receive queues, and
// service resumed the instant that socket finally died ("unexpected EOF"
// in the gateway log).
//
// New() therefore restores IdleConnTimeout and configures the HTTP/2
// keep-alive ping, while preserving every body-streaming-relevant zero
// value.
package gwtransport

import (
	"net/http"
	"time"
)

const (
	// ExpectContinueTimeout makes the outbound transport actually wait for
	// the upstream's 100-continue before streaming the body, rather than
	// silently ignoring the client's "Expect: 100-continue" header
	// (docs/plans/git-gateway-cutover.md PR3: "Expect: 100-continue と
	// chunked encoding の透過的な扱い"). Carried over verbatim from both
	// gateways' original Transport literals.
	ExpectContinueTimeout = 5 * time.Second

	// IdleConnTimeout matches http.DefaultTransport's own value. It bounds
	// how long a pooled connection that nobody is using survives, which is
	// what eventually retires a connection whose peer vanished while the
	// gateway was idle.
	IdleConnTimeout = 90 * time.Second

	// SendPingTimeout is how long an HTTP/2 connection may go without
	// receiving a frame before the transport sends a keep-alive PING. Zero
	// — net/http's own default — means no health check at all, which is
	// precisely the gap that let a half-dead h2 connection stay in the pool
	// indefinitely.
	SendPingTimeout = 30 * time.Second

	// PingTimeout is how long that PING may go unanswered before the
	// connection is closed and evicted from the pool, so the next request
	// dials a fresh one instead of joining the wedge. Set explicitly rather
	// than relying on net/http's 15s default, so the pairing with
	// SendPingTimeout stays visible at the call site.
	PingTimeout = 15 * time.Second
)

// New returns the outbound Transport both gateways proxy through. Each call
// builds a fresh Transport: a gateway must own its connection pool, so one
// wedged upstream can never be shared across gateways.
//
// Deliberately NOT set, so the change stays scoped to connection liveness:
//
//   - Proxy stays nil (not http.ProxyFromEnvironment, which is what
//     http.DefaultTransport uses). The gateways dial their upstreams
//     directly today; honoring proxy env vars would be a behavior change
//     to the daemon's egress path, not a bug fix.
//   - ResponseHeaderTimeout stays zero. It is an HTTP/1.1-only backstop
//     (the HTTP/2 path does not honor it), and on the h1 path a canceled
//     request already tears its connection down, so h1 self-heals after a
//     single failure without it. Setting it would instead risk breaking
//     legitimately slow upstreams — notably git-upload-pack, which can
//     spend a long time computing a pack before it sends the first
//     response header.
//   - Every body-streaming-relevant field (DisableCompression,
//     WriteBufferSize, ...) stays at its zero value, preserving the
//     unbuffered streaming semantics both gateways rely on
//     (docs/plans/api-gateway.md §5 "無バッファストリーミング転送").
func New() *http.Transport {
	return &http.Transport{
		ExpectContinueTimeout: ExpectContinueTimeout,
		IdleConnTimeout:       IdleConnTimeout,
		// Both gateways already got HTTP/2 implicitly, from net/http's
		// auto-upgrade of a Transport with no TLSClientConfig and no Dial
		// hooks. Stating it explicitly keeps HTTP2 below meaningful if
		// either of those is ever set here, instead of silently
		// downgrading the upstream leg to HTTP/1.1 along with it.
		ForceAttemptHTTP2: true,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: SendPingTimeout,
			PingTimeout:     PingTimeout,
		},
	}
}
